// config.go 加载 JSON 配置（支持注释与尾逗号）+ 环境变量覆盖。
//
// 为什么自带一个 JSONC 解析：运维配置文件需要写注释（「这个值为什么是 30s」），
// 而标准库的 encoding/json 不认注释。用一个 20 行的预处理（去 // 与 /* */、
// 去尾逗号）比引入第三方依赖更可控——出问题时能一眼看懂。
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Config 是网关的全部配置。
//
// 分段原则：能热改的放前面（api_key / cooldown / models），装配期依赖的放后面
// （listen / upstream / auth_dir）。面板保存后只对热改段立即生效。
type Config struct {
	// Listen 监听地址，如 ":7867" 或 "0.0.0.0:7867"。
	Listen string `json:"listen"`
	// APIKey 是 /v1/* 与面板的密钥；空 = 不鉴权（仅本机/私网）。
	APIKey string `json:"api_key"`
	// AuthDir 账号文件目录（一个账号一个 JSON）。
	AuthDir string `json:"auth_dir"`
	// StateFile 池状态（冷却/统计）持久化路径。
	StateFile string `json:"state_file"`
	// DefaultModel 请求未给 model 时使用。
	DefaultModel string `json:"default_model"`
	// Aliases 把客户端写死的通用模型名映射到上游 id。
	Aliases map[string]string `json:"aliases"`

	Upstream struct {
		LLMBase    string `json:"llm_base"`
		AuthBase   string `json:"auth_base"`
		PointsBase string `json:"points_base"`
		Origin     string `json:"origin"`
		// TimeoutSeconds 短请求（续期/目录/签到/余额）总超时。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds 等响应头超时（也是流式请求的首字节超时）。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds 流内空闲上限（0 = 不限制）。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// UserAgent 出站 UA（空 = 内置）。
		UserAgent string `json:"user_agent"`
		// ClientVersion 出站 x-client-version（空 = 内置 v1.1.0，可跟官方升级）。
		ClientVersion string `json:"client_version"`
		// CatalogCacheFile 模型目录持久化缓存（上游不可达时兜底）。
		CatalogCacheFile string `json:"catalog_cache_file"`
	} `json:"upstream"`

	Cooldown struct {
		// PlanCredit 额度不足冷却（"0" = 到次日 04:00）。
		PlanCredit string `json:"plan_credit"`
		// SoftRate 限流软冷却基数（连续触发指数退避）。
		SoftRate string `json:"soft_rate"`
		// SoftRateMax 软冷却封顶。
		SoftRateMax string `json:"soft_rate_max"`
	} `json:"cooldown"`

	Pool struct {
		Strategy string `json:"strategy"`
		// MaxInFlight 单账号在途软上限。
		//
		// 实测上游单账号并发 ≤10 全成功、12 起回 429 limiter_request_error。
		// 默认取 10（贴着实测上限跑），配合 QueueWait 吸收突发；要更稳可降到 6。
		// 注意：这里是「贴着上限」，不是「留余量」——号少又想稳，就往回调。
		MaxInFlight int `json:"max_in_flight"`
		// QueueWait 全部账号在途满时的最长等待（"0" = 不等待）。
		QueueWait          string  `json:"queue_wait"`
		BreakerThreshold   int     `json:"breaker_threshold"`
		BreakerCooldown    string  `json:"breaker_cooldown"`
		BreakerCooldownMax string  `json:"breaker_cooldown_max"`
		DegradeThreshold   int     `json:"degrade_threshold"`
		DegradeCooldown    string  `json:"degrade_cooldown"`
		IdleWeightPerHour  float64 `json:"idle_weight_per_hour"`
		IdleWeightMax      float64 `json:"idle_weight_max"`
	} `json:"pool"`

	Schedule struct {
		CheckinHours          []int `json:"checkin_hours"`
		KeepaliveHours        []int `json:"keepalive_hours"`
		CheckinEnabled        bool  `json:"checkin_enabled"`
		KeepaliveEnabled      bool  `json:"keepalive_enabled"`
		BalanceRefreshEnabled bool  `json:"balance_refresh_enabled"`
		BalanceRefreshMinutes int   `json:"balance_refresh_minutes"`
		// IncludeDisabledInTasks 让签到/保活/余额对已禁用账号也执行。
		IncludeDisabledInTasks bool `json:"include_disabled_in_tasks"`
		// NewAccountGraceDays 新号静默期（天）。
		NewAccountGraceDays int `json:"new_account_grace_days"`
		// Concurrency 单轮任务并发账号数。
		Concurrency int `json:"concurrency"`
	} `json:"schedule"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`
		TTL        string `json:"ttl"`
		GCInterval string `json:"gc_interval"`
		// PrefixAffinity 没有显式会话 id 时也按消息前缀粘性路由（默认 false）。
		// 打开后同一 system 的所有请求会钉在同一个号上——风控特征更像真人，
		// 但多号池会退化；单用户 / 单客户端场景建议保持关闭。
		PrefixAffinity bool `json:"prefix_affinity"`
	} `json:"session_sticky"`

	Prompt struct {
		Mode string `json:"mode"`
		File string `json:"file"`
	} `json:"prompt"`

	Logging struct {
		RequestArchiveEnabled bool `json:"request_archive_enabled"`
		RequestRetentionDays  int  `json:"request_retention_days"`
		RequestArchiveMaxMB   int  `json:"request_archive_max_mb"`
		RequestClientInfo     bool `json:"request_client_info"`
	} `json:"logging"`

	Server struct {
		ReadTimeout string `json:"read_timeout"`
		// MinOutputTokens 出站 max_tokens 下限（默认 2048，负数 = 不抬升）。
		// 上游的思考 token 算在 max_tokens 里：客户端给 16/512 时思考会把预算
		// 吃光、正文为空（真号实测）。不想改客户端意图的可以设 0 关掉。
		MinOutputTokens int `json:"min_output_tokens"`
	} `json:"server"`

	Panel struct {
		Title string `json:"title"`
	} `json:"panel"`

	// Import 是「从本机客户端目录捡账号」的配置。
	Import struct {
		// ClientDirs 是额外的扫描路径（文件或目录）。留空时按平台查标准位置：
		//   macOS   ~/Library/Application Support/office-raccoon/agent/config/auth.json
		//   Windows %APPDATA%\office-raccoon\agent\config\auth.json
		//   Linux   ~/.config/office-raccoon/agent/config/auth.json
		// 服务器部署时本机没有客户端，这里是空的属于正常。
		ClientDirs []string `json:"client_dirs"`
	} `json:"import"`

	Login struct {
		Enabled bool   `json:"enabled"`
		AppName string `json:"app_name"`
		// CallbackBase 是浏览器授权完成后的回调基址（空 = 按 listen 自动推导）。
		//
		// 本机跑：留空即可，自动得到 http://127.0.0.1:<port>。
		// 线上部署：填公网地址，如 "https://xhx2api.1task.cn"——上游 authorize 页
		// 实测接受域名 redirect（2026-10-26 验证：页面原样透传，不回退 office-raccoon://）。
		// 必须是 http(s)://host[:port] 形状，回调路径固定 /auth/callback。
		CallbackBase string `json:"callback_base"`
	} `json:"login"`

	// Images 是生图通道（/v1/images/generations）。
	Images struct {
		// Enabled 默认 true；显式 false 关掉整条通道。
		Enabled *bool `json:"enabled"`
		// Model 指定上游图片模型（空 = 上游默认组 raccoon-image-gen）。
		// 实测 fallback 组里有 raccoon-4eb26a / raccoon-0c119c / raccoon-ecc5fd，
		// 但只有默认组是稳定入口，除非你确知要哪一个，否则留空。
		Model string `json:"model"`
		// TimeoutSeconds 单张图超时（默认 600）。
		// 实测 1024 档 49-143s、4K 档 150s，但超限尺寸被压缩后单次能到 267s
		// （2026-10-26 线上全形态验收），300s 的余量只剩 33 秒，故放到 600。
		TimeoutSeconds int `json:"timeout_seconds"`
		// ReturnURL 控制是否提供可直接访问的图片 url（默认**开**）。
		//
		// 上游只回 base64（实测传 response_format=url 也照样返回 url:null），
		// 而一张 4K 图的 base64 有 8MB+，塞给前端、工作流或消息队列都很难用。
		// 所以网关默认把图落盘（或传对象存储）并提供 /v1/images/<id>。
		// 显式 false 关掉：响应只回 base64，客户端点名要 url 会被 400。
		ReturnURL *bool `json:"return_url"`
		// ResponseFormat 是客户端**没传** response_format 时的默认形态：
		//
		//	"url"（默认）—— 只回 url，省掉几 MB base64；
		//	"b64_json"    —— 只回 base64（旧行为，给只认 b64 的客户端）；
		//	"both"        —— url 与 b64_json 都给。
		//
		// 客户端显式传 response_format 时以它为准（只接受 url / b64_json）。
		ResponseFormat string `json:"response_format"`
		// MaxTotalMB 是落盘目录的总量上限（省略 = 2048；0 = 不限制）。
		//
		// 默认可访问 url 就意味着每张图都落盘：4K PNG 单张 5MB，不设上限迟早撑满磁盘。
		// 超过上限后按 mtime 从旧到新删，降到 90% 为止——文件名是内容哈希，
		// 删掉的旧链接会 404，别把这里当永久存储。
		MaxTotalMB *int `json:"max_total_mb"`
		// PublicBase 是写进 url 的对外基址（如 https://xhx2api.1task.cn）。
		// 空 = 按请求的 Host 推导；反向代理场景建议显式填，否则拿到的是内网地址。
		PublicBase string `json:"public_base"`
		// Dir 是图片落盘目录（空 = state 文件同目录下的 images/）。
		Dir string `json:"dir"`
		// COS 把图传腾讯云对象存储，url 用 COS 地址（上传失败回落本地盘）。
		//
		// 为什么要它：本地盘在容器/多副本部署下不共享，域名也可能对不上；
		// 对象存储给的是稳定外链，直接能塞进前端 img、消息体和数据库。
		// 开了它就不必再单独开 return_url（装配时自动视为开启）。
		COS struct {
			Enabled   bool   `json:"enabled"`
			SecretID  string `json:"secret_id"`
			SecretKey string `json:"secret_key"`
			// Bucket 形如 mybucket-1250000000（含 APPID）。
			Bucket string `json:"bucket"`
			// Region 形如 ap-guangzhou。
			Region string `json:"region"`
			// Prefix 是对象键前缀（如 "xhx/prod"，空 = 桶根）。
			Prefix string `json:"prefix"`
			// BaseURL 是写进 url 的对外基址（CDN/自定义域名，空 = COS 默认域名）。
			BaseURL string `json:"base_url"`
			// PublicRead 上传时带 x-cos-acl: public-read（私有桶想直接给链接就得开）。
			PublicRead bool `json:"public_read"`
			// SignedTTLSeconds > 0 时返回带签名的临时链接（私有桶、不想开公有读）。
			SignedTTLSeconds int `json:"signed_ttl_seconds"`
		} `json:"cos"`
		// Aliases 是额外接受并暴露的生图模型名，默认 gpt-image-* / dall-e-*。
		//
		// 上游 /images/gen 忽略 model 字段（见 upstream.ImageAliases 注释），
		// 所以这些名字纯属客户端便利：让 Cherry Studio 之类硬编码
		// gpt-image-1 / dall-e-3 的客户端能直接选中并出图，而不是 400。
		// 省略该字段 = 用默认表；显式 [] = 一个别名都不暴露。
		Aliases []string `json:"aliases"`
	} `json:"images"`

	Models struct {
		// Hidden 模型黑名单（面板/接口都不再列出，点名请求直接拒）。
		Hidden []string `json:"hidden"`
		// IncludeUpstreamHidden 让 /v1/models 也列出「上游 visible=false」的模型。
		//
		// 默认 true。上游的 visible 只影响它自己官网的模型选择器，不代表不可调用：
		// 实测三个 raccoon-* 全被标成 false，而默认模型就是其中之一。
		// 设 false 可以回到「只列上游展示的模型」。
		IncludeUpstreamHidden *bool `json:"include_upstream_hidden"`
	} `json:"models"`

	ShutdownTimeoutSeconds int `json:"shutdown_timeout_seconds"`

	// ---- 解析后的派生值（不参与序列化） ----
	path            string
	queueWait       time.Duration
	softRateDur     time.Duration
	softRateMaxDur  time.Duration
	planCreditDur   time.Duration
	breakerCooldown time.Duration
	breakerMax      time.Duration
	degradeCooldown time.Duration
	sessionTTL      time.Duration
	sessionGC       time.Duration
	readTimeout     time.Duration
	upstreamShort   time.Duration
	upstreamHeader  time.Duration
	upstreamIdle    time.Duration
}

// defaults 返回推荐默认值（与 config.example.json 一致）。
// boolPtr 返回指向 v 的指针（用于 config 里的「未设置」三态 bool）。
func boolPtr(v bool) *bool { return &v }

func intPtr(v int) *int { return &v }

func defaults() *Config {
	c := &Config{
		Listen:       "0.0.0.0:7867",
		AuthDir:      "./auths",
		StateFile:    "./data/state.json",
		DefaultModel: "raccoon-8c4485",
		Aliases: map[string]string{
			"gpt-4o":                     "raccoon-chat-ml-5-5",
			"gpt-4o-mini":                "raccoon-chat-ml-5-5",
			"gpt-4":                      "raccoon-8c4485",
			"gpt-4.1":                    "raccoon-8c4485",
			"gpt-4.1-mini":               "raccoon-8c4485",
			"gpt-5":                      "raccoon-8c4485",
			"gpt-5-mini":                 "raccoon-8c4485",
			"claude-sonnet-4-20250514":   "raccoon-chat-ml-5-5",
			"claude-3-5-sonnet-20241022": "raccoon-chat-ml-5-5",
			"claude-3-7-sonnet-20250219": "raccoon-chat-ml-5-5",
			"o3":                         "raccoon-8c4485",
			"o4-mini":                    "raccoon-8c4485",
		},
	}
	c.Upstream.TimeoutSeconds = 120
	c.Upstream.HeaderTimeoutSeconds = 120
	c.Upstream.IdleTimeoutSeconds = 300
	c.Upstream.CatalogCacheFile = "./data/model_catalog.json"
	c.Cooldown.PlanCredit = "12h"
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.SoftRateMax = "2h"
	c.Pool.Strategy = "round_robin"
	// 上游单账号硬上限是 5 个在途（2026-11-01 三账号两模型实测：第 6 个起
	// 0.1s 内直接 429，不排队）。网关这边对齐到上限，超发只会把号打进
	// 上游的频率惩罚（全模型 429，实测 ≥30 分钟），比排队等待贵得多。
	c.Pool.MaxInFlight = 5
	c.Pool.QueueWait = "2s"
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "10m"
	c.Pool.BreakerCooldownMax = "2h"
	c.Pool.DegradeThreshold = 5
	c.Pool.DegradeCooldown = "10m"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5
	c.Schedule.CheckinHours = []int{9}
	c.Schedule.KeepaliveHours = []int{3}
	c.Schedule.CheckinEnabled = true
	c.Schedule.KeepaliveEnabled = true
	c.Schedule.BalanceRefreshEnabled = true
	c.Schedule.BalanceRefreshMinutes = 5
	c.Schedule.NewAccountGraceDays = 3
	c.Schedule.Concurrency = 2
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	c.Prompt.Mode = "passthrough"
	c.Logging.RequestArchiveEnabled = true
	c.Logging.RequestRetentionDays = 7
	c.Logging.RequestArchiveMaxMB = 100
	c.Logging.RequestClientInfo = true
	c.Server.ReadTimeout = "300s"
	c.Panel.Title = "小浣熊网关"
	c.Login.Enabled = true
	c.Login.AppName = "办公小浣熊客户端"
	c.ShutdownTimeoutSeconds = 10
	return c
}

// Load 读取并校验配置文件；不存在时返回 fs.ErrNotExist 供 main 决定是否自动生成。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.path = path
	return cfg, nil
}

// ParseConfig 解析原始字节（JSONC）。
func ParseConfig(raw []byte) (*Config, error) {
	stripped := stripJSONC(raw)
	// 先解成 map，摘掉 "_" 开头的注释性键（_comment 之类不该被严格校验拒绝）。
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(stripped, &probe); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	for key := range probe {
		if strings.HasPrefix(key, "_") {
			delete(probe, key)
		}
	}
	clean, err := json.Marshal(probe)
	if err != nil {
		return nil, err
	}
	cfg := defaults()
	dec := json.NewDecoder(bytes.NewReader(clean))
	// 未知键报错：写错键名（比如 "cooldwon"）静默忽略会让人排查半天。
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("配置项错误: %w", err)
	}
	applyDefaults(cfg)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	applyEnv(cfg)
	cfg.computeDerived()
	return cfg, nil
}

// parseConfigLenient 与 ParseConfig 相同，但**不拒绝未知键**。
//
// 落盘回读用它：磁盘上的配置可能带未来版本新增的键，严格模式会让面板保存
// 直接失败，而这与用户当前这次提交无关。
func parseConfigLenient(raw []byte) (*Config, error) {
	stripped := stripJSONC(raw)
	cfg := defaults()
	dec := json.NewDecoder(bytes.NewReader(stripped))
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("配置项错误: %w", err)
	}
	applyDefaults(cfg)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.computeDerived()
	return cfg, nil
}

// applyDefaults 把「零值 = 未配置」的字段补上默认（bool 需要单独处理）。
func applyDefaults(c *Config) {
	d := defaults()
	if c.Listen == "" {
		c.Listen = d.Listen
	}
	if c.AuthDir == "" {
		c.AuthDir = d.AuthDir
	}
	if c.StateFile == "" {
		c.StateFile = d.StateFile
	}
	if c.Models.IncludeUpstreamHidden == nil {
		c.Models.IncludeUpstreamHidden = boolPtr(true)
	}
	if c.Images.Enabled == nil {
		c.Images.Enabled = boolPtr(true)
	}
	if c.Images.TimeoutSeconds <= 0 {
		c.Images.TimeoutSeconds = 600
	}
	if c.Images.ReturnURL == nil {
		c.Images.ReturnURL = boolPtr(true)
	}
	if f := strings.ToLower(strings.TrimSpace(c.Images.ResponseFormat)); f == "" {
		c.Images.ResponseFormat = "url"
	} else {
		c.Images.ResponseFormat = f
	}
	if c.Images.MaxTotalMB == nil {
		c.Images.MaxTotalMB = intPtr(2048)
	}
	if c.DefaultModel == "" {
		c.DefaultModel = d.DefaultModel
	}
	if len(c.Aliases) == 0 {
		c.Aliases = d.Aliases
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = d.Upstream.TimeoutSeconds
	}
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = d.Upstream.HeaderTimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds < 0 {
		c.Upstream.IdleTimeoutSeconds = 0
	}
	if c.Upstream.CatalogCacheFile == "" {
		c.Upstream.CatalogCacheFile = d.Upstream.CatalogCacheFile
	}
	if c.Cooldown.PlanCredit == "" {
		c.Cooldown.PlanCredit = d.Cooldown.PlanCredit
	}
	if c.Cooldown.SoftRate == "" {
		c.Cooldown.SoftRate = d.Cooldown.SoftRate
	}
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = d.Cooldown.SoftRateMax
	}
	if c.Pool.Strategy == "" {
		c.Pool.Strategy = d.Pool.Strategy
	}
	if c.Pool.QueueWait == "" {
		c.Pool.QueueWait = d.Pool.QueueWait
	}
	if c.Pool.MaxInFlight <= 0 {
		c.Pool.MaxInFlight = d.Pool.MaxInFlight
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = d.Pool.BreakerThreshold
	}
	if c.Pool.BreakerCooldown == "" {
		c.Pool.BreakerCooldown = d.Pool.BreakerCooldown
	}
	if c.Pool.BreakerCooldownMax == "" {
		c.Pool.BreakerCooldownMax = d.Pool.BreakerCooldownMax
	}
	if c.Pool.DegradeThreshold <= 0 {
		c.Pool.DegradeThreshold = d.Pool.DegradeThreshold
	}
	if c.Pool.DegradeCooldown == "" {
		c.Pool.DegradeCooldown = d.Pool.DegradeCooldown
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = d.Pool.IdleWeightPerHour
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = d.Pool.IdleWeightMax
	}
	if len(c.Schedule.CheckinHours) == 0 {
		c.Schedule.CheckinHours = d.Schedule.CheckinHours
	}
	if len(c.Schedule.KeepaliveHours) == 0 {
		c.Schedule.KeepaliveHours = d.Schedule.KeepaliveHours
	}
	if c.Schedule.BalanceRefreshMinutes <= 0 {
		c.Schedule.BalanceRefreshMinutes = d.Schedule.BalanceRefreshMinutes
	}
	if c.Schedule.Concurrency <= 0 {
		c.Schedule.Concurrency = d.Schedule.Concurrency
	}
	if c.SessionSticky.TTL == "" {
		c.SessionSticky.TTL = d.SessionSticky.TTL
	}
	if c.SessionSticky.GCInterval == "" {
		c.SessionSticky.GCInterval = d.SessionSticky.GCInterval
	}
	if c.Prompt.Mode == "" {
		c.Prompt.Mode = d.Prompt.Mode
	}
	if c.Logging.RequestRetentionDays <= 0 {
		c.Logging.RequestRetentionDays = d.Logging.RequestRetentionDays
	}
	if c.Logging.RequestArchiveMaxMB <= 0 {
		c.Logging.RequestArchiveMaxMB = d.Logging.RequestArchiveMaxMB
	}
	if c.Server.ReadTimeout == "" {
		c.Server.ReadTimeout = d.Server.ReadTimeout
	}
	if c.Server.MinOutputTokens == 0 {
		c.Server.MinOutputTokens = 2048
	}
	if c.Panel.Title == "" {
		c.Panel.Title = d.Panel.Title
	}
	if c.Login.AppName == "" {
		c.Login.AppName = d.Login.AppName
	}
	if c.ShutdownTimeoutSeconds <= 0 {
		c.ShutdownTimeoutSeconds = d.ShutdownTimeoutSeconds
	}
}

// validate 校验必填项与格式。
func (c *Config) validate() error {
	// 生图的默认响应形态：写错值必须报错，不能静默回落——
	// 用户以为配了 both，实际拿到 url，排查起来毫无头绪。
	switch c.Images.ResponseFormat {
	case "", "url", "b64_json", "both":
	default:
		return fmt.Errorf(`images.response_format 只能是 "url" / "b64_json" / "both"，得到 %q`, c.Images.ResponseFormat)
	}
	if c.Images.MaxTotalMB != nil && *c.Images.MaxTotalMB < 0 {
		return fmt.Errorf("images.max_total_mb 不能为负（0 = 不限制）")
	}

	if !strings.Contains(c.Listen, ":") {
		return fmt.Errorf("listen 必须是 host:port 形式（当前 %q）", c.Listen)
	}
	for name, raw := range map[string]string{
		"cooldown.plan_credit":       c.Cooldown.PlanCredit,
		"cooldown.soft_rate":         c.Cooldown.SoftRate,
		"cooldown.soft_rate_max":     c.Cooldown.SoftRateMax,
		"pool.breaker_cooldown":      c.Pool.BreakerCooldown,
		"pool.breaker_cooldown_max":  c.Pool.BreakerCooldownMax,
		"pool.degrade_cooldown":      c.Pool.DegradeCooldown,
		"pool.queue_wait":            c.Pool.QueueWait,
		"session_sticky.ttl":         c.SessionSticky.TTL,
		"session_sticky.gc_interval": c.SessionSticky.GCInterval,
		"server.read_timeout":        c.Server.ReadTimeout,
	} {
		if _, err := time.ParseDuration(raw); err != nil {
			return fmt.Errorf("%s 不是合法时长（如 60s / 10m / 2h）：%q", name, raw)
		}
	}
	for _, h := range append(append([]int{}, c.Schedule.CheckinHours...), c.Schedule.KeepaliveHours...) {
		if h < 0 || h > 23 {
			return fmt.Errorf("schedule 小时必须在 0-23 之间（当前 %d）", h)
		}
	}
	if c.Prompt.Mode != "" && c.Prompt.Mode != "passthrough" && c.Prompt.Mode != "custom" {
		return fmt.Errorf("prompt.mode 只能是 passthrough 或 custom（当前 %q）", c.Prompt.Mode)
	}
	return nil
}

// applyEnv 用环境变量覆盖（前缀 XHX_，嵌套用 _ 分隔）。
//
// 只覆盖「敏感/易变」的标量字段，不做通用反射——通用映射会让人猜不到变量名，
// 而且改起来容易把嵌套结构写错。
func applyEnv(c *Config) {
	setStr := func(env string, dst *string) {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
	}
	setInt := func(env string, dst *int) {
		if v := os.Getenv(env); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	setStr("XHX_LISTEN", &c.Listen)
	setStr("XHX_API_KEY", &c.APIKey)
	setStr("XHX_AUTH_DIR", &c.AuthDir)
	setStr("XHX_STATE_FILE", &c.StateFile)
	setStr("XHX_DEFAULT_MODEL", &c.DefaultModel)
	setStr("XHX_UPSTREAM_LLM_BASE", &c.Upstream.LLMBase)
	setStr("XHX_UPSTREAM_AUTH_BASE", &c.Upstream.AuthBase)
	setStr("XHX_UPSTREAM_POINTS_BASE", &c.Upstream.PointsBase)
	setStr("XHX_UPSTREAM_ORIGIN", &c.Upstream.Origin)
	setStr("XHX_UPSTREAM_USER_AGENT", &c.Upstream.UserAgent)
	setStr("XHX_UPSTREAM_CLIENT_VERSION", &c.Upstream.ClientVersion)
	setInt("XHX_UPSTREAM_TIMEOUT_SECONDS", &c.Upstream.TimeoutSeconds)
	setInt("XHX_UPSTREAM_HEADER_TIMEOUT_SECONDS", &c.Upstream.HeaderTimeoutSeconds)
	setInt("XHX_UPSTREAM_IDLE_TIMEOUT_SECONDS", &c.Upstream.IdleTimeoutSeconds)
	if v := os.Getenv("XHX_MODELS_HIDDEN"); v != "" {
		parts := strings.Split(v, ",")
		c.Models.Hidden = c.Models.Hidden[:0]
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				c.Models.Hidden = append(c.Models.Hidden, p)
			}
		}
	}
}

// computeDerived 把字符串时长解析成 Duration（解析失败已在 validate 拦过）。
// ImageURLEnabled 表示是否具备「给得出 url」的能力：return_url 默认开，
// 对象存储启用时也一定具备（那条链路本来就会返回外链）。
func (c *Config) ImageURLEnabled() bool {
	return c.Images.COS.Enabled || c.Images.ReturnURL == nil || *c.Images.ReturnURL
}

// ImageDefaultFormat 归一「客户端没传 response_format」时的形态。
//
// 没有 url 能力时一律回落 b64_json：与其回一个 url:null 让客户端拿到空值，
// 不如老实给 base64。写错的值也会回落到 url（validate 会先拦）。
func (c *Config) ImageDefaultFormat() string {
	switch strings.ToLower(strings.TrimSpace(c.Images.ResponseFormat)) {
	case "b64_json":
		return "b64_json"
	case "both":
		if c.ImageURLEnabled() {
			return "both"
		}
		return "b64_json"
	default: // "url"（默认）
		if c.ImageURLEnabled() {
			return "url"
		}
		return "b64_json"
	}
}

// ImageMaxTotalMB 取图片目录上限（0 = 不限制）。
func (c *Config) ImageMaxTotalMB() int {
	if c.Images.MaxTotalMB == nil {
		return 2048
	}
	if *c.Images.MaxTotalMB > 0 {
		return *c.Images.MaxTotalMB
	}
	return 0
}

func (c *Config) computeDerived() {
	parse := func(s string) time.Duration {
		d, _ := time.ParseDuration(s)
		return d
	}
	c.queueWait = parse(c.Pool.QueueWait)
	c.softRateDur = parse(c.Cooldown.SoftRate)
	c.softRateMaxDur = parse(c.Cooldown.SoftRateMax)
	c.planCreditDur = parse(c.Cooldown.PlanCredit)
	c.breakerCooldown = parse(c.Pool.BreakerCooldown)
	c.breakerMax = parse(c.Pool.BreakerCooldownMax)
	c.degradeCooldown = parse(c.Pool.DegradeCooldown)
	c.sessionTTL = parse(c.SessionSticky.TTL)
	c.sessionGC = parse(c.SessionSticky.GCInterval)
	c.readTimeout = parse(c.Server.ReadTimeout)
	c.upstreamShort = time.Duration(c.Upstream.TimeoutSeconds) * time.Second
	c.upstreamHeader = time.Duration(c.Upstream.HeaderTimeoutSeconds) * time.Second
	c.upstreamIdle = time.Duration(c.Upstream.IdleTimeoutSeconds) * time.Second
}

// WriteDefault 生成一份推荐配置（含随机 api_key）并落盘，返回生成的密钥。
//
// 首次运行自动生成：双击二进制/裸跑 docker 即开，不需要先手工复制样例。
// 随机密钥而不是空密钥：公网部署忘记设 key 是这类网关最常见的事故。
func WriteDefault(path string) (string, error) {
	cfg := defaults()
	key := randomKey()
	cfg.APIKey = key
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", err
	}
	return key, nil
}

// randomKey 生成 sk-xhx-<32 hex>。
func randomKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "sk-xhx-" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return "sk-xhx-" + hex.EncodeToString(b[:])
}

// SaveConfig 校验、落盘并热应用面板提交的配置（合并语义）。
//
// 落盘前先与磁盘上的原文件做深合并：面板表单只提交它管理的键，用户手写的注释性
// 字段（"_comment" 之类）与未来版本新增的键不该被洗掉。
//
// 合并语义的代价是**删不掉键**：提交里没有的键会被磁盘旧值补回来。所以集合型
// 配置（`aliases`）单独按「提交即全量」处理，JSON 编辑器另走 ReplaceConfig。
func SaveConfig(path string, raw []byte) (*Config, []string, error) {
	return saveConfig(path, raw, false)
}

// ReplaceConfig 以提交内容为准落盘：已知键没提到就是删除。
//
// 给面板的 JSON 编辑器用——那里显示的是整份配置，用户删掉一行就该真的消失。
// 未知键（"_comment"、未来版本字段）照旧保留，免得注释被吃掉。
func ReplaceConfig(path string, raw []byte) (*Config, []string, error) {
	return saveConfig(path, raw, true)
}

func saveConfig(path string, raw []byte, replace bool) (*Config, []string, error) {
	var incoming map[string]any
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, nil, fmt.Errorf("提交内容不是合法 JSON: %w", err)
	}
	current := map[string]any{}
	oldRaw, readErr := os.ReadFile(path)
	if readErr == nil {
		stripped := stripJSONC(oldRaw)
		_ = json.Unmarshal(stripped, &current)
	}
	// 先严格校验「本次提交的内容」：未知键要在这里被抓住。
	// 不能先合并再校验——磁盘上可能留着未来版本的键，合并后会被严格模式误杀。
	if _, err := ParseConfig(raw); err != nil {
		return nil, nil, err
	}
	var merged map[string]any
	if replace {
		if len(incoming) == 0 {
			// 空对象在 replace 语义下等于「全删」：照做会把 api_key 重新随机生成，
			// 所有客户端当场鉴权失败。宁可拒绝。
			return nil, nil, errors.New("提交内容为空：整份覆盖模式至少要保留 api_key")
		}
		merged = replaceMaps(current, incoming)
	} else {
		merged = mergeMaps(current, incoming)
	}
	// 兜底：提交里丢了 api_key 就沿用磁盘上的旧值。
	// 删掉 api_key ≠ 关闭鉴权（空 = 不鉴权），它会走 applyDefaults 重新随机生成，
	// 结果是全客户端 401——这种「静默自锁」不该由一次手滑触发。
	if _, ok := merged["api_key"]; !ok {
		if old, ok := current["api_key"]; ok {
			merged["api_key"] = old
		}
	}
	mergedRaw, err := json.Marshal(merged)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := parseConfigLenient(mergedRaw)
	if err != nil {
		return nil, nil, err
	}
	cfg.path = path
	// 只列出「本次真的改了」的需重启字段：恒返回全量清单会让用户每次保存都
	// 看到一串提示，久了就无视它，真正需要重启时反而错过。
	oldCfg, _ := parseConfigLenient(oldRaw)
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, nil, err
	}
	return cfg, changedRestartFields(oldCfg, cfg), nil
}

// changedRestartFields 比较新旧配置，只返回**发生变化**且需要重启的字段。
func changedRestartFields(oldCfg, newCfg *Config) []string {
	all := restartRequiredFields(newCfg)
	if oldCfg == nil {
		return all
	}
	type pair struct {
		name     string
		oldValue string
		newValue string
	}
	values := []pair{
		{"listen", oldCfg.Listen, newCfg.Listen},
		{"auth_dir", oldCfg.AuthDir, newCfg.AuthDir},
		{"state_file", oldCfg.StateFile, newCfg.StateFile},
		{"upstream.timeout_seconds", itoa(oldCfg.Upstream.TimeoutSeconds), itoa(newCfg.Upstream.TimeoutSeconds)},
		{"upstream.header_timeout_seconds", itoa(oldCfg.Upstream.HeaderTimeoutSeconds), itoa(newCfg.Upstream.HeaderTimeoutSeconds)},
		{"upstream.idle_timeout_seconds", itoa(oldCfg.Upstream.IdleTimeoutSeconds), itoa(newCfg.Upstream.IdleTimeoutSeconds)},
		{"upstream.user_agent", oldCfg.Upstream.UserAgent, newCfg.Upstream.UserAgent},
		{"upstream.llm_base", oldCfg.Upstream.LLMBase, newCfg.Upstream.LLMBase},
		{"upstream.auth_base", oldCfg.Upstream.AuthBase, newCfg.Upstream.AuthBase},
		{"upstream.points_base", oldCfg.Upstream.PointsBase, newCfg.Upstream.PointsBase},
		{"upstream.origin", oldCfg.Upstream.Origin, newCfg.Upstream.Origin},
		{"session_sticky.ttl", oldCfg.SessionSticky.TTL, newCfg.SessionSticky.TTL},
		{"session_sticky.gc_interval", oldCfg.SessionSticky.GCInterval, newCfg.SessionSticky.GCInterval},
		{"logging.request_archive_enabled", boolStr(oldCfg.Logging.RequestArchiveEnabled), boolStr(newCfg.Logging.RequestArchiveEnabled)},
		{"logging.request_retention_days", itoa(oldCfg.Logging.RequestRetentionDays), itoa(newCfg.Logging.RequestRetentionDays)},
		{"logging.request_archive_max_mb", itoa(oldCfg.Logging.RequestArchiveMaxMB), itoa(newCfg.Logging.RequestArchiveMaxMB)},
		{"server.read_timeout", oldCfg.Server.ReadTimeout, newCfg.Server.ReadTimeout},
		{"login.app_name", oldCfg.Login.AppName, newCfg.Login.AppName},
		{"models.include_upstream_hidden", optionalBool(oldCfg.Models.IncludeUpstreamHidden), optionalBool(newCfg.Models.IncludeUpstreamHidden)},
		{"images.enabled", optionalBool(oldCfg.Images.Enabled), optionalBool(newCfg.Images.Enabled)},
		{"images.timeout_seconds", itoa(oldCfg.Images.TimeoutSeconds), itoa(newCfg.Images.TimeoutSeconds)},
		{"images.return_url", optionalBool(oldCfg.Images.ReturnURL), optionalBool(newCfg.Images.ReturnURL)},
		{"images.response_format", oldCfg.ImageDefaultFormat(), newCfg.ImageDefaultFormat()},
		{"images.max_total_mb", itoa(oldCfg.ImageMaxTotalMB()), itoa(newCfg.ImageMaxTotalMB())},
		{"images.dir", oldCfg.Images.Dir, newCfg.Images.Dir},
		{"images.public_base", oldCfg.Images.PublicBase, newCfg.Images.PublicBase},
	}
	changed := map[string]bool{}
	for _, v := range values {
		if v.oldValue != v.newValue {
			changed[v.name] = true
		}
	}
	out := make([]string, 0, len(changed))
	for _, name := range all {
		if changed[name] {
			out = append(out, name)
		}
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

// optionalBool 把三态 bool 渲染成可比较的字符串（nil = 未配置）。
func optionalBool(p *bool) string {
	if p == nil {
		return "unset"
	}
	return boolStr(*p)
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// setSectionKeys 是「用户可以在面板里自由增删行」的集合型配置段。
//
// 这类段提交即全量：深合并会把用户删掉的行用旧值补回来，表现就是
// 「删了保存又出现」。所以它们整段替换，不做递归合并。
var setSectionKeys = map[string]bool{
	"aliases": true,
}

// mergeMaps 深合并（嵌套对象逐键覆盖），集合型段整段替换。
func mergeMaps(cur, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if setSectionKeys[k] {
			cur[k] = v
			continue
		}
		if inMap, ok := v.(map[string]any); ok {
			if curMap, ok := cur[k].(map[string]any); ok {
				cur[k] = mergeMaps(curMap, inMap)
				continue
			}
		}
		cur[k] = v
	}
	return cur
}

// knownTopKeys / knownSubKeys 按结构体 tag 反推「已知键」：用于区分
// 「用户不想要了」与「面板不管的键」，replaceMaps 只对后者做保留。
var knownTopKeys, knownSubKeys = configSchemaKeys()

func configSchemaKeys() (top map[string]bool, sub map[string]map[string]bool) {
	top = map[string]bool{}
	sub = map[string]map[string]bool{}
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		top[name] = true
		if f.Type.Kind() != reflect.Struct {
			continue
		}
		keys := map[string]bool{}
		for j := 0; j < f.Type.NumField(); j++ {
			n, _, _ := strings.Cut(f.Type.Field(j).Tag.Get("json"), ",")
			if n != "" && n != "-" {
				keys[n] = true
			}
		}
		sub[name] = keys
	}
	return top, sub
}

// replaceMaps 以提交内容为准：已知键没提到就是删除，未知键（"_comment"、
// 未来版本字段）连同一层子键原样保留。
func replaceMaps(cur, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(incoming)+len(cur))
	for k, v := range cur {
		if knownTopKeys[k] {
			continue // 已知段交给提交内容决定去留
		}
		out[k] = v
	}
	for k, v := range incoming {
		inMap, isMap := v.(map[string]any)
		curMap, curIsMap := cur[k].(map[string]any)
		if sub, ok := knownSubKeys[k]; ok && isMap && curIsMap {
			// 保留段内未知子键（如 upstream 下的 "_comment"），已知子键以提交为准。
			section := make(map[string]any, len(inMap))
			for ck, cv := range curMap {
				if !sub[ck] {
					section[ck] = cv
				}
			}
			for ik, iv := range inMap {
				section[ik] = iv
			}
			out[k] = section
			continue
		}
		out[k] = v
	}
	return out
}

// restartRequiredFields 返回需要重启才能生效的字段清单。
//
// 这些值在装配期被对象捕获（监听地址、HTTP client Transport、目录句柄），
// 运行期改不了；如实告诉用户比「显示已保存但没生效」好。
func restartRequiredFields(c *Config) []string {
	return []string{
		"listen", "auth_dir", "state_file",
		"upstream.timeout_seconds", "upstream.header_timeout_seconds",
		"upstream.idle_timeout_seconds", "upstream.user_agent",
		"upstream.llm_base", "upstream.auth_base", "upstream.points_base", "upstream.origin",
		"session_sticky.ttl", "session_sticky.gc_interval",
		"logging.request_archive_enabled", "logging.request_retention_days",
		"logging.request_archive_max_mb", "server.read_timeout",
		"login.app_name",
		// 只影响 /v1/models 的返回内容，但它是启动时注入 handler 的。
		"models.include_upstream_hidden",
		"images.enabled", "images.timeout_seconds",
		// 下面几个都是启动时注入 handler 的（url 能力、默认形态、落盘目录、对外基址）。
		"images.return_url", "images.response_format", "images.max_total_mb",
		"images.dir", "images.public_base",
	}
}

// ---------------------------------------------------------------------------
// JSONC 预处理

// stripJSONC 去掉 // 与 /* */ 注释、尾逗号，并把字符串内容原样保留。
//
// 不能用正则：注释符号可能出现在字符串里（比如一个含 "http://" 的 UA），
// 正则会把字符串里的 // 也当注释切掉。必须逐字符扫描并跟踪「是否在字符串内」。
func stripJSONC(src []byte) []byte {
	out := make([]byte, 0, len(src))
	inString := false
	escaped := false
	for i := 0; i < len(src); i++ {
		ch := src[i]
		if inString {
			out = append(out, ch)
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		switch {
		case ch == '"':
			inString = true
			out = append(out, ch)
		case ch == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			if i < len(src) {
				out = append(out, '\n')
			}
		case ch == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i++ // 跳过 '/'
		default:
			out = append(out, ch)
		}
	}
	return stripTrailingCommas(out)
}

// stripTrailingCommas 去掉 } / ] 前的逗号（同样要跳过字符串）。
func stripTrailingCommas(src []byte) []byte {
	out := make([]byte, 0, len(src))
	inString := false
	escaped := false
	for i := 0; i < len(src); i++ {
		ch := src[i]
		if inString {
			out = append(out, ch)
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			out = append(out, ch)
			continue
		}
		if ch == ',' {
			// 往后看第一个非空白字符：是 } 或 ] 就吞掉这个逗号。
			j := i + 1
			for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r') {
				j++
			}
			if j < len(src) && (src[j] == '}' || src[j] == ']') {
				continue
			}
		}
		out = append(out, ch)
	}
	return out
}

// maskKey 在日志里脱敏 api_key。
func maskKey(key string) string {
	if key == "" {
		return "(空：不鉴权)"
	}
	if len(key) <= 10 {
		return "***"
	}
	return key[:7] + "…" + key[len(key)-4:]
}

// errConfigNotFound 供 main 判断是否自动生成。
var errConfigNotFound = errors.New("config not found")
