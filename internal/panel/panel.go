// Package panel 内嵌式 Web 管理面板：账号总览与运维、模型目录、请求日志、
// 用量统计、登录导入、配置热改。
//
// 设计约束（与网关本体一致）：
//
//   - 前端 go:embed 内嵌，无构建步骤、无 node_modules，单二进制即开；
//   - 鉴权复用网关 api_key（Bearer / ?key=），与 /v1/* 同一把钥匙；
//     面板 HTML 本身不含秘密，可匿名加载，密钥只发给 /panel/api/*；
//   - 所有运维操作落到 pool / scheduler / auth 已有入口，不另写一套状态机；
//   - 面板不做「代用户调上游」的事，除了用户明确点的按钮（签到/刷新余额）。
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
	"github.com/rsfree/xhx2api/internal/livecfg"
	"github.com/rsfree/xhx2api/internal/pool"
	"github.com/rsfree/xhx2api/internal/reqlog"
	"github.com/rsfree/xhx2api/internal/scheduler"
	"github.com/rsfree/xhx2api/internal/session"
	"github.com/rsfree/xhx2api/internal/upstream"
	"github.com/rsfree/xhx2api/internal/usage"

	qrcode "github.com/skip2/go-qrcode"
)

// Config 是面板依赖（main 装配注入）。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	Scheduler *scheduler.Scheduler
	// AuthDir 是账号文件目录（导入/删除都落在这里）。
	AuthDir string
	// APIKey 空 = 不鉴权；与 Live 同时给出时 Live 优先。
	APIKey string
	Live   *livecfg.Holder
	// Version 面板展示用。
	Version string
	// ConfigPath / LoadConfig / SaveConfig 用于配置页（nil 时返回 501）。
	ConfigPath string
	LoadConfig func() (any, error)
	// SaveConfig 落盘配置。replace=false 走合并（表单只提交它管的键），
	// replace=true 以提交内容为准（JSON 编辑器整份提交，删掉的键要真删）。
	SaveConfig func(raw []byte, replace bool) ([]string, error)
	// Usage / RequestLog 可观测数据（nil 时对应接口返回空）。
	Usage      *usage.Recorder
	RequestLog *reqlog.Recorder
	// SessionStats 返回粘性会话统计（nil = 关闭）。
	SessionStats func() session.Stats
	// CallbackBase 是环回授权的回调基址（如 http://127.0.0.1:7867）。
	CallbackBase string
	// AppName 是授权页上报的客户端名。
	AppName string
	// ClientDirs 是额外要扫描的客户端凭证路径/目录（config.import.client_dirs）。
	ClientDirs []string
	// DefaultModel 用于模型页标「默认」；Aliases 展开成卡片上的别名标签。
	// 少了这两个，模型页看起来完整、实际上说不清「我该填哪个名字」。
	DefaultModel string
	Aliases      map[string]string
	// Logs 运行日志环形缓冲。
	Logs *Ring
	// StartedAt 进程启动时刻。
	StartedAt time.Time
}

// Panel 是面板 handler（挂载在 /panel/ 前缀，本包路由带全前缀）。
type Panel struct {
	cfg    Config
	mux    *http.ServeMux
	logins *loginManager
	logs   *Ring

	// smsPending 记住「刚刚发出去的验证码属于哪条通道」。
	//
	// 两条登录通道（IAM / 桌面端）的验证码不通用，而发码和验码是两个请求：
	// 不记住通道就会出现「IAM 发码 + 桌面端验码」→ 用户填对了也报
	// sms_code_verify_error（实测踩过）。
	smsMu      sync.Mutex
	smsPending map[string]smsPending
}

// smsPending 是一次未完成的短信登录。
type smsPending struct {
	verificationID string // IAM 通道才有
	via            string // "iam" / "desktop"
	inviter        string
	at             time.Time
}

// New 装配面板。
func New(cfg Config) *Panel {
	if cfg.Logs == nil {
		cfg.Logs = NewRing(500)
	}
	p := &Panel{cfg: cfg, mux: http.NewServeMux(), logins: newLoginManager(), logs: cfg.Logs}
	p.routes()
	return p
}

// Logs 返回日志缓冲（main 用 io.MultiWriter 镜像 log 输出进来）。
func (p *Panel) Logs() *Ring { return p.logs }

// routes 注册全部端点。
func (p *Panel) routes() {
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)
	p.mux.HandleFunc("GET /panel/style.css", p.styleSheet)

	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
	p.mux.HandleFunc("GET /panel/api/models", p.withAuth(p.models))
	p.mux.HandleFunc("POST /panel/api/models/refresh", p.withAuth(p.modelsRefresh))
	p.mux.HandleFunc("GET /panel/api/logs", p.withAuth(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/requests", p.withAuth(p.requests))
	p.mux.HandleFunc("GET /panel/api/usage", p.withAuth(p.usageHandler))
	p.mux.HandleFunc("POST /panel/api/usage/reset", p.withAuth(p.usageReset))
	p.mux.HandleFunc("GET /panel/api/config", p.withAuth(p.getConfig))
	p.mux.HandleFunc("POST /panel/api/config", p.withAuth(p.saveConfig))

	p.mux.HandleFunc("POST /panel/api/accounts/import", p.withAuth(p.importAccount))
	p.mux.HandleFunc("GET /panel/api/accounts/scan", p.withAuth(p.scanClient))
	p.mux.HandleFunc("POST /panel/api/accounts/scan/import", p.withAuth(p.importScanned))
	p.mux.HandleFunc("GET /panel/api/login/sms/config", p.withAuth(p.smsConfig))
	p.mux.HandleFunc("POST /panel/api/login/sms/send", p.withAuth(p.smsSend))
	p.mux.HandleFunc("POST /panel/api/login/sms/verify", p.withAuth(p.smsLogin))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/disable", p.withAuth(p.accountDisable))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/enable", p.withAuth(p.accountEnable))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/pause", p.withAuth(p.accountPause))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/resume", p.withAuth(p.accountResume))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/revive", p.withAuth(p.accountRevive))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/remove", p.withAuth(p.accountRemove))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/priority", p.withAuth(p.accountPriority))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/checkin", p.withAuth(p.accountCheckin))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/bind-device", p.withAuth(p.accountBindDevice))
	p.mux.HandleFunc("POST /panel/api/accounts/bind-device-all", p.withAuth(p.accountsBindDeviceAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/balance", p.withAuth(p.accountBalance))
	p.mux.HandleFunc("GET /panel/api/accounts/{uid}/bills", p.withAuth(p.accountBills))
	p.mux.HandleFunc("GET /panel/api/points", p.withAuth(p.pointsOverview))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/points/claim", p.withAuth(p.pointsClaim))

	p.mux.HandleFunc("POST /panel/api/checkin_all", p.withAuth(p.checkinAll))
	p.mux.HandleFunc("POST /panel/api/keepalive_all", p.withAuth(p.keepaliveAll))
	p.mux.HandleFunc("POST /panel/api/balance_all", p.withAuth(p.balanceAll))

	p.mux.HandleFunc("POST /panel/api/login/qr/start", p.withAuth(p.loginQrStart))
	p.mux.HandleFunc("GET /panel/api/login/qr/poll", p.withAuth(p.loginQrPoll))
	p.mux.HandleFunc("GET /panel/api/login/qr/png", p.withAuth(p.loginQrPng))
	p.mux.HandleFunc("POST /panel/api/login/device/start", p.withAuth(p.loginDeviceStart))
	p.mux.HandleFunc("GET /panel/api/login/device/poll", p.withAuth(p.loginDevicePoll))
	p.mux.HandleFunc("POST /panel/api/login/code", p.withAuth(p.loginCode))

	// 浏览器环回授权的回调：不在 /panel 前缀下（授权页把 code 打到这里）。
	p.mux.HandleFunc("GET /auth/callback", p.authCallback)
}

// ServeHTTP 统一入口：先写安全响应头。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	p.mux.ServeHTTP(w, r)
}

// setSecurityHeaders 给面板全部响应加安全头（含 401 与静态资源）。
func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	// CSP 只放行一个外部来源：阿里云验证码（手机号登录要用）。
	//
	// 收得紧是默认值，但「script-src 'self'」会直接掐掉
	// o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js，
	// 表现就是面板里永远显示「人机验证不可用」——实测踩过。
	// 放行范围按阿里云验证码实际用到的域给，不多给一条：
	//   *.alicdn.com                    控件脚本与样式（o/g/x 三个域名都会用到）
	//   *.captcha-open.aliyun.com       验证接口
	//   *.captcha-open.alibaba.com      同上（国际站）
	//   *.aliyuncs.com                  验证码服务端下发资源
	h.Set("Content-Security-Policy",
		"default-src 'self'; "+
			// 验证码控件会从三个 CDN 域名拉脚本与样式（实测：o/g/x.alicdn.com），
			// 只放行 o 会报 "violates ... script-src" 并让控件 onError。
			"script-src 'self' https://*.alicdn.com; "+
			"style-src 'self' 'unsafe-inline' https://*.alicdn.com; "+
			"img-src 'self' data: https://*.alicdn.com https://*.aliyuncs.com; "+
			"font-src 'self' data: https://*.alicdn.com; "+
			"connect-src 'self' https://*.alicdn.com https://*.captcha-open.aliyun.com https://*.captcha-open.alibaba.com https://*.aliyuncs.com; "+
			"frame-src https://*.captcha-open.aliyun.com https://*.captcha-open.alibaba.com")
}

// withAuth 与网关同口径的鉴权（支持 Bearer / ?key=）。
func (p *Panel) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.authOK(r) {
			writeErr(w, http.StatusUnauthorized, "invalid_api_key")
			return
		}
		next(w, r)
	}
}

func (p *Panel) authOK(r *http.Request) bool {
	key := p.apiKey()
	if key == "" {
		return true
	}
	token := ""
	if raw := strings.TrimSpace(r.Header.Get("Authorization")); len(raw) > 7 && strings.EqualFold(raw[:7], "bearer ") {
		token = strings.TrimSpace(raw[7:])
	}
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get("key"))
	}
	if token == "" {
		return false
	}
	return constantTimeEqual(token, key)
}

func (p *Panel) apiKey() string {
	if p.cfg.Live != nil {
		return p.cfg.Live.Load().APIKey
	}
	return p.cfg.APIKey
}

// ---------------------------------------------------------------------------
// 静态资源（go:embed）

func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	raw, err := panelFS.ReadFile("index.html")
	if err != nil {
		http.Error(w, "panel assets missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(raw)
}

func (p *Panel) appScript(w http.ResponseWriter, r *http.Request) {
	raw, err := panelFS.ReadFile("app.js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(raw)
}

func (p *Panel) styleSheet(w http.ResponseWriter, r *http.Request) {
	raw, err := panelFS.ReadFile("style.css")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(raw)
}

// panelFS 由 index.go 里的 go:embed 提供。
var _ fs.FS = panelFS

// ---------------------------------------------------------------------------
// 总览

// overview 返回池状态 + 账号列表 + 调度器状况。
func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	entries := p.cfg.Pool.List()
	counts := p.cfg.Pool.Counts()
	payload := map[string]any{
		"version":        p.cfg.Version,
		"started_at":     p.cfg.StartedAt.Format(time.RFC3339),
		"uptime_s":       int(time.Since(p.cfg.StartedAt).Seconds()),
		"counts":         counts,
		"accounts":       entries,
		"login_sessions": p.logins.sessionsCount(),
		"callback_base":  p.cfg.CallbackBase,
	}
	if h, a := p.cfg.Upstream.AgentHosts(); h != "" {
		payload["upstream"] = map[string]string{"llm_base": h, "auth_base": a}
	}
	if p.cfg.Scheduler != nil {
		payload["scheduler"] = map[string]any{
			"config":  p.cfg.Scheduler.Config(),
			"results": p.cfg.Scheduler.Results(),
		}
	}
	if p.cfg.SessionStats != nil {
		payload["session"] = p.cfg.SessionStats()
	}
	if p.cfg.RequestLog != nil {
		payload["requests"] = requestsSummary(p.cfg.RequestLog.Metrics())
	}
	writeJSON(w, http.StatusOK, payload)
}

// requestsSummary 是面板「请求」页顶部的指标快照。
//
// 抽成函数而不是在 payload 里手写 map：新增指标（比如 unmetered）漏一行，
// 面板上就是「数据显示不出来」——2026-11-21 真踩过：reqlog 已经加了
// usage_missing，这里没加 unmetered，「N 条未记账」永远不显示。
func requestsSummary(m reqlog.Metrics) map[string]any {
	return map[string]any{
		"total": m.Total, "success": m.Success, "failure": m.Failure,
		"in_flight": m.InFlight, "rpm": m.RPM, "tokens_total": m.TokensTotal,
		"latency_p50_ms": m.LatencyP50, "latency_p95_ms": m.LatencyP95,
		"latency_avg_ms": m.LatencyAvg,
		// 上游没回 usage 的条数：Token 总量偏低时要能解释成「没记账」而不是「省」。
		"unmetered": m.Unmetered,
	}
}

// ---------------------------------------------------------------------------
// 模型

// aliasesAndDefault 取别名表与默认模型：热配置优先。
//
// 面板保存别名后模型页要立刻反映，否则用户刚加的别名在页面上看不到，
// 会以为没保存成功。
func (p *Panel) aliasesAndDefault() (map[string]string, string) {
	aliases, def := p.cfg.Aliases, p.cfg.DefaultModel
	if p.cfg.Live != nil {
		if s := p.cfg.Live.Load(); len(s.Aliases) > 0 {
			aliases = s.Aliases
			if s.DefaultModel != "" {
				def = s.DefaultModel
			}
		}
	}
	return aliases, def
}

func (p *Panel) models(w http.ResponseWriter, r *http.Request) {
	catalog := p.cfg.Upstream.Catalog()
	aliasTable, defaultModel := p.aliasesAndDefault()
	// 每个模型的最近实测耗时（中位数）。上游同一个模型波动极大——
	// 实测「只回复两个字」raccoon-8c4485 能从 12s 漂到 137s，同期
	// raccoon-19b265 只要 5.6s。不带这个数字，用户只会觉得「面板卡死」。
	latency, latFailures := p.recentLatencyByModel()
	// 反向索引：目标模型 → 指向它的别名。别名是网关侧的概念，上游目录里没有。
	byTarget := map[string][]string{}
	for alias, target := range aliasTable {
		byTarget[target] = append(byTarget[target], alias)
	}
	inCatalog := make(map[string]bool, len(catalog))
	out := make([]map[string]any, 0, len(catalog))
	for _, mi := range catalog {
		inCatalog[mi.ID] = true
		aliases := byTarget[mi.ID]
		if aliases == nil {
			// 没有别名时给空数组而不是 null：前端少一处防御，契约也更稳。
			aliases = []string{}
		}
		sort.Strings(aliases)
		out = append(out, map[string]any{
			"id": mi.ID, "name": mi.Name, "category": mi.Category,
			"context_length": mi.ContextWindow, "max_output_tokens": mi.MaxOutput,
			"reasoning": mi.Reasoning, "vision": mi.VL,
			"credits": mi.Credits, "billing_status": mi.BillingStatus,
			"free_note": mi.FreeNote, "base_credits": mi.BaseCredits,
			"visible": mi.Visible,
			// 上游没列出的（visible=false）也照实呈现：它可调用，其中一个还是默认模型。
			"unlisted": !mi.Visible,
			"hidden":   p.hidden(mi.ID),
			"default":  mi.ID == defaultModel,
			"aliases":  aliases,
			// 0 = 还没有成功样本（新模型或刚重启）。
			"recent_ms":       latency[mi.ID],
			"recent_failures": latFailures[mi.ID],
		})
	}
	// 别名指向了目录里没有的模型：实测这种目标往往仍可调用（如 raccoon-chat-ml-5-5），
	// 但用户从列表里看不到它，只会觉得「gpt-4o 怎么映射到一个不存在的模型」。
	orphans := make([]map[string]string, 0, 4)
	for alias, target := range aliasTable {
		if !inCatalog[target] {
			orphans = append(orphans, map[string]string{"alias": alias, "target": target})
		}
	}
	sort.Slice(orphans, func(i, j int) bool {
		return orphans[i]["target"] < orphans[j]["target"] || (orphans[i]["target"] == orphans[j]["target"] && orphans[i]["alias"] < orphans[j]["alias"])
	})
	refreshed := ""
	if age := p.cfg.Upstream.CatalogAge(); !age.IsZero() {
		refreshed = age.Format(time.RFC3339)
	}
	// 生图通道在模型页也要有一张卡：上游目录里没有它，用户只看目录会以为
	// 「这里不能生图」。别名跟着配置走（热改），列表顺序与 /v1/models 一致。
	imageModels := upstream.ImageModelIDs(nil)
	if p.cfg.Live != nil {
		imageModels = upstream.ImageModelIDs(p.cfg.Live.Load().ImageAliases)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models": out, "refreshed_at": refreshed, "count": len(out),
		"default_model": defaultModel, "orphans": orphans,
		"image_models": imageModels,
	})
}

// hidden 判断模型是否被配置拉黑（Live 快照，热改即生效）。
func (p *Panel) hidden(model string) bool {
	if p.cfg.Live == nil {
		return false
	}
	return p.cfg.Live.Load().Hidden(model)
}

// recentLatencyByModel 返回每个模型的最近实测耗时（中位数，毫秒）与失败次数。
//
// 只统计成功的对话类请求算耗时：生图 20-200s、失败请求的耗时都不可比。
// 用中位数而不是平均值——上游波动极大（同一模型 5.6s→137s），平均会被极值带跑。
//
// 失败次数单独给：上游劣化时请求会挂在 100s+ 直到客户端断开（499），
// 这时 recent_ms 可能还是上一轮的好数据，不看失败数就会误判「模型没问题」。
//
// 存在的理由是「用户以为面板卡死」：默认模型 raccoon-8c4485 实测能到 137s，
// 而同期 raccoon-19b265 只要 5.6s。没有这个数字，用户只能靠猜。
func (p *Panel) recentLatencyByModel() (median map[string]int64, failures map[string]int) {
	median = map[string]int64{}
	failures = map[string]int{}
	if p.cfg.RequestLog == nil {
		return median, failures
	}
	buckets := map[string][]int64{}
	for _, ev := range p.cfg.RequestLog.Recent(500) {
		if ev.Model == "" {
			continue
		}
		switch ev.Protocol {
		case "chat", "messages", "responses":
		default:
			continue
		}
		if ev.Status != 200 || ev.ErrorKind != "" {
			failures[ev.Model]++
			continue
		}
		if ev.DurationMS <= 0 {
			continue
		}
		buckets[ev.Model] = append(buckets[ev.Model], ev.DurationMS)
	}
	for model, ds := range buckets {
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		median[model] = ds[len(ds)/2]
	}
	return median, failures
}

func (p *Panel) modelsRefresh(w http.ResponseWriter, r *http.Request) {
	a := p.cfg.Pool.FirstAvailable()
	if a == nil {
		writeErr(w, http.StatusServiceUnavailable, "没有可用账号，无法刷新目录")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	models, err := p.cfg.Upstream.RefreshModels(ctx, a, true)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "刷新失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(models)})
}

// ---------------------------------------------------------------------------
// 日志与可观测

func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 200)
	writeJSON(w, http.StatusOK, map[string]any{"lines": p.logs.Snapshot(limit)})
}

func (p *Panel) requests(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeJSON(w, http.StatusOK, map[string]any{"metrics": nil, "events": []any{}})
		return
	}
	limit := queryInt(r, "limit", 100)
	writeJSON(w, http.StatusOK, map[string]any{
		"metrics": p.cfg.RequestLog.Metrics(),
		"events":  p.cfg.RequestLog.Recent(limit),
	})
}

func (p *Panel) usageHandler(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeJSON(w, http.StatusOK, usage.Snapshot{})
		return
	}
	writeJSON(w, http.StatusOK, p.cfg.Usage.Snapshot())
}

func (p *Panel) usageReset(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage != nil {
		p.cfg.Usage.Reset()
	}
	if p.cfg.RequestLog != nil {
		p.cfg.RequestLog.Reset()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 配置

func (p *Panel) getConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadConfig == nil {
		writeErr(w, http.StatusNotImplemented, "配置读写未启用")
		return
	}
	cfg, err := p.cfg.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": p.cfg.ConfigPath, "config": cfg})
}

func (p *Panel) saveConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "配置读写未启用")
		return
	}
	body, err := readAll(r, 2<<20)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	restart, err := p.cfg.SaveConfig(body, r.URL.Query().Get("mode") == "replace")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart_required": restart})
}

// ---------------------------------------------------------------------------
// 账号运维

// importAccount 导入账号：接受账号 JSON / access_token / 授权回调链接。
func (p *Panel) importAccount(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Content string `json:"content"`
	}
	if err := decodeJSON(r, &payload); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	content := strings.TrimSpace(payload.Content)
	if content == "" {
		writeErr(w, http.StatusBadRequest, "内容为空")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	// 先看是不是授权回调链接。
	if code, _, ok := upstream.ParseCallbackCode(content); ok && looksLikeCallback(content) {
		account, err := p.cfg.Upstream.PrepareAccount(ctx, map[string]any{"code": code})
		if err != nil {
			writeErr(w, http.StatusBadRequest, "授权码换票失败: "+err.Error())
			return
		}
		p.admit(w, account)
		return
	}
	// 否则按账号 JSON / 裸 token 解析。
	parsed, err := auth.Parse([]byte(content))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	account, err := p.cfg.Upstream.PrepareAccount(ctx, map[string]any{
		"access_token":  parsed.AccessToken,
		"refresh_token": parsed.RefreshToken,
		"uid":           parsed.UID,
		"nickname":      parsed.Nickname,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, "凭证校验失败: "+err.Error())
		return
	}
	p.admit(w, account)
}

// looksLikeCallback 判断内容是否像回调链接（而不是含 "://" 的奇怪 token）。
func looksLikeCallback(content string) bool {
	return strings.Contains(content, "office-raccoon://") ||
		strings.Contains(content, "/auth/callback")
}

// mergeExisting 让「覆盖导入」不丢能力。
//
// 场景：同一个号先用浏览器授权导入（有 refresh_token，可续期），后来又用
// 「客户端目录」扫了一次（IAM 票，不带 refresh_token）。直接覆盖会让账号
// 从「能续期」退化成「30 天后失效」——这不是用户的本意，他只想刷新票。
// 所以新凭证缺 refresh_token 时沿用旧的。
func mergeExisting(account *auth.Auth, existing *auth.Auth) {
	if existing == nil {
		return
	}
	if existing.FilePath != "" {
		account.FilePath = existing.FilePath
	}
	if account.RefreshToken == "" {
		if refresh := existing.RefreshTokenValue(); refresh != "" {
			account.RefreshToken = refresh
		}
	}
	snap := existing.Snapshot()
	if account.Nickname == "" {
		account.Nickname = snap.Nickname
	}
	// 快照字段一起沿用：这些是「查询结果」不是「凭证」，覆盖导入时新对象里
	// 恒为空，不沿用就会把余额/签到状态/进池时间全抹掉（实测踩过：导入后
	// 面板显示 0 分、未签到，得手动刷一遍余额才回来）。
	if account.Credit == 0 {
		account.Credit = snap.Credit
	}
	if account.CreditAt == "" {
		account.CreditAt = snap.CreditAt
	}
	if account.CheckedIn == "" {
		account.CheckedIn = snap.CheckedIn
	}
	if account.ImportedAt == "" {
		account.ImportedAt = snap.ImportedAt
	}
	if account.RegisterAt == "" {
		account.RegisterAt = snap.RegisterAt
	}
	// 设备绑定态也是「查询结果」：覆盖导入（重新登录/重贴 token）时新对象里
	// 恒为空，不沿用就等于把一个已知 unbound 的号打回「未知」，
	// 下一次签到又要多打一次 user_info，面板也会白闪一屏「未签」。
	if account.DeviceBound == "" {
		account.DeviceBound = snap.DeviceBound
	}
	if account.DeviceCheckedAt == "" {
		account.DeviceCheckedAt = snap.DeviceCheckedAt
	}
}

// claimWelcome 新号入池就领一次性奖励（桌面端登录 + 移动端登录）。
//
// 这两笔各 3000，都是幂等的（领过回 granted=false）。以前只挂在「签到」流程里，
// 结果是新号要等到下一个签到时段才补上，面板上长时间显示「可补领」。
// 实测：移动端那一笔在注册当天就能领（6000 → 9000），别等。
func (p *Panel) claimWelcome(account *auth.Auth) {
	if account == nil || account.UID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		desktop, derr := p.cfg.Upstream.CheckinClaim(ctx, account)
		mobile, merr := p.cfg.Upstream.ClaimMobileGrant(ctx, account)
		if derr != nil && merr != nil {
			return
		}
		if !desktop && !mobile {
			return // 都领过了，正常情况（覆盖导入）
		}
		gained := int64(0)
		if desktop {
			gained += 3000
		}
		if mobile {
			gained += 3000
		}
		if remain, _, _, err := p.cfg.Upstream.UserEntUsage(ctx, account); err == nil {
			p.cfg.Pool.NoteCredits(account.UID, remain)
		}
		if p.cfg.Logs != nil {
			p.cfg.Logs.Add("新号入池礼包：" + account.UID + " +" + strconv.FormatInt(gained, 10) + " 分")
		}
		log.Printf("[panel] 新号入池礼包 %s +%d 分", account.UID, gained)
	}()
}

// admit 把账号落盘并入池。
func (p *Panel) admit(w http.ResponseWriter, account *auth.Auth) {
	if account == nil || account.UID == "" {
		writeErr(w, http.StatusBadRequest, "无法确定账号 uid")
		return
	}
	// 已存在：沿用原文件路径，覆盖凭证（重新登录场景），但不丢可续期能力。
	mergeExisting(account, p.cfg.Pool.AuthByUID(account.UID))
	if err := account.Save(p.cfg.AuthDir); err != nil {
		writeErr(w, http.StatusInternalServerError, "落盘失败: "+err.Error())
		return
	}
	p.cfg.Pool.Add(account)
	p.claimWelcome(account)
	log.Printf("[panel] 导入账号 %s（%s）", account.Nickname, account.UID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": account.UID, "nickname": account.Nickname,
		"file": account.FilePath,
	})
}

func (p *Panel) accountDisable(w http.ResponseWriter, r *http.Request) {
	p.mutate(w, r, func(uid string) bool { return p.cfg.Pool.SetDisabled(uid, true) }, "已禁用")
}

func (p *Panel) accountEnable(w http.ResponseWriter, r *http.Request) {
	p.mutate(w, r, func(uid string) bool { return p.cfg.Pool.Revive(uid) }, "已启用")
}

func (p *Panel) accountPause(w http.ResponseWriter, r *http.Request) {
	p.mutate(w, r, func(uid string) bool { return p.cfg.Pool.SetPaused(uid, true) }, "已暂停")
}

func (p *Panel) accountResume(w http.ResponseWriter, r *http.Request) {
	p.mutate(w, r, func(uid string) bool { return p.cfg.Pool.SetPaused(uid, false) }, "已恢复")
}

func (p *Panel) accountRevive(w http.ResponseWriter, r *http.Request) {
	p.mutate(w, r, func(uid string) bool { return p.cfg.Pool.Revive(uid) }, "已解冻")
}

// accountRemove 移除账号：池 + 文件一起删（文件先备份到 auths/removed/）。
func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	// 备份一份到 removed/，误删还能捡回来。
	if a.FilePath != "" {
		if backup, err := backupAuthFile(a.FilePath, p.cfg.AuthDir); err == nil {
			log.Printf("[panel] 账号 %s 已移除，凭证备份在 %s", uid, backup)
		}
	}
	if err := a.Remove(); err != nil {
		writeErr(w, http.StatusInternalServerError, "删除文件失败: "+err.Error())
		return
	}
	p.cfg.Pool.Remove(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// backupAuthFile 把凭证文件复制到 auths/removed/（保留原文件名 + 时间戳）。
func backupAuthFile(src, authDir string) (string, error) {
	raw, err := readFile(src)
	if err != nil {
		return "", err
	}
	dir := authDir + "/removed"
	if err := ensureDir(dir); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s/%s.%s", dir, baseName(src), time.Now().Format("20060102-150405"))
	if err := writeFile(name, raw); err != nil {
		return "", err
	}
	return name, nil
}

func (p *Panel) accountPriority(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	var payload struct {
		Priority int `json:"priority"`
	}
	if err := decodeJSON(r, &payload); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !p.cfg.Pool.SetPriority(uid, payload.Priority) {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "priority": payload.Priority})
}

func (p *Panel) accountCheckin(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "调度器未启用")
		return
	}
	uid := r.PathValue("uid")
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	outcome, err := p.cfg.Scheduler.CheckinOne(ctx, uid)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, outcome)
}

// accountBindDevice 给单个账号补绑可信设备。
//
// 为什么需要这个入口：上游把「有没有每日积分资格」绑在 trusted_device.bound 上，
// 而 device_id 只能由服务端通过 /devices_current_bind 下发。接码导入的号
// 天生没有设备（bound=false），这种号**永远**拿不到每日积分。
// 以前只能在官方客户端登录一次，现在在这里点一下就能补上。
func (p *Panel) accountBindDevice(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	deviceID, err := p.cfg.Upstream.BindDevice(ctx, a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// 绑定成功后上游的 device_id 只在这里见过一次，落盘失败就等于白绑
	// （下次会换新的 client_device_id，反而制造「不断声明新设备」）。
	a.DeviceCheckedAt = time.Now().Format(time.RFC3339)
	if serr := a.SaveAtomic(); serr != nil {
		log.Printf("WARN: 账号 %s 绑定设备后落盘失败（重启后需重新绑定）: %v", uid, serr)
	}
	note := "已绑定"
	if deviceID == "" {
		note = "此前已绑定，此次无需重复"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uid": uid, "device_id": deviceID, "device_bound": a.DeviceBound, "note": note,
	})
}

// accountsBindDeviceAll 给所有「还没有可信设备」的账号补绑。
//
// 只处理 unbound 与未查过的：已 bound 的号重复调没有意义，还会平白多几条
// 上游请求。串行 + 每个 40s 上限：并发绑一串设备本身就是可疑行为。
func (p *Panel) accountsBindDeviceAll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	type result struct {
		UID      string `json:"uid"`
		OK       bool   `json:"ok"`
		DeviceID string `json:"device_id,omitempty"`
		Note     string `json:"note,omitempty"`
		Error    string `json:"error,omitempty"`
		Skipped  bool   `json:"skipped,omitempty"`
	}

	var (
		results []result
		okCount int
	)
	for _, uid := range p.cfg.Pool.UIDs() {
		a := p.cfg.Pool.AuthByUID(uid)
		if a == nil {
			continue
		}
		if a.DeviceBound == "bound" {
			// 已 bound 但没记绑定时刻（老凭证）：顺手补上。
			// 签到要靠它区分「刚绑上、等上游发分」和「绑了很久也没发分」，
			// 缺了就只能在 pending 与 ineligible 之间瞎猜。
			if a.DeviceBoundAt == "" {
				a.DeviceBoundAt = time.Now().Format(time.RFC3339)
				if serr := a.SaveAtomic(); serr != nil {
					log.Printf("WARN: 账号 %s 补写绑定时刻失败: %v", uid, serr)
				}
			}
			results = append(results, result{UID: uid, Skipped: true})
			continue
		}
		deviceID, err := p.cfg.Upstream.BindDevice(ctx, a)
		if err != nil {
			results = append(results, result{UID: uid, Error: err.Error()})
			continue
		}
		a.DeviceCheckedAt = time.Now().Format(time.RFC3339)
		if serr := a.SaveAtomic(); serr != nil {
			log.Printf("WARN: 账号 %s 绑定设备后落盘失败: %v", uid, serr)
		}
		results = append(results, result{UID: uid, OK: true, DeviceID: deviceID, Note: deviceBindNote(deviceID)})
		okCount++
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": okCount, "results": results})
}

// deviceBindNote 给「上游说已经绑过」一个能看懂的说明。
//
// 上游对已绑过的会话回 HTTP 400 + code 100002。我们把它当成功（本地状态
// 确实要补成 bound），但要让人看得出「这次没有真的新建设备」。
func deviceBindNote(deviceID string) string {
	if deviceID == "" {
		return "此前已绑定"
	}
	return ""
}

func (p *Panel) accountBalance(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "调度器未启用")
		return
	}
	uid := r.PathValue("uid")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	usageInfo, err := p.cfg.Scheduler.RefreshOne(ctx, uid)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, usageInfo)
}

func (p *Panel) accountBills(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	rows, err := p.cfg.Upstream.RecentBills(ctx, a, queryInt(r, "limit", 50))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bills": rows})
}

// mutate 是「按 uid 执行一个池操作」的公共壳。
func (p *Panel) mutate(w http.ResponseWriter, r *http.Request, fn func(uid string) bool, okMessage string) {
	uid := r.PathValue("uid")
	if !fn(uid) {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": okMessage})
}

// ---------------------------------------------------------------------------
// 批量任务

func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "调度器未启用")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	writeJSON(w, http.StatusOK, p.cfg.Scheduler.CheckinAll(ctx))
}

func (p *Panel) keepaliveAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "调度器未启用")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	writeJSON(w, http.StatusOK, p.cfg.Scheduler.KeepaliveAll(ctx))
}

func (p *Panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "调度器未启用")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	writeJSON(w, http.StatusOK, p.cfg.Scheduler.RefreshBalances(ctx))
}

// ---------------------------------------------------------------------------
// 登录

func (p *Panel) loginQrStart(w http.ResponseWriter, r *http.Request) {
	id, content, err := p.logins.StartQr(p.cfg.Upstream)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "content": content, "status": "等待扫码"})
}

func (p *Panel) loginQrPoll(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少 id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	status, creds, err := p.logins.PollQr(ctx, id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if creds != nil {
		// 扫码成功：直接入池，面板不必再多一步。
		if aerr := p.admitQuiet(creds); aerr != nil {
			writeErr(w, http.StatusInternalServerError, "入池失败: "+aerr.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "已入池", "done": true,
			"uid": creds.UID, "nickname": creds.Nickname})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "done": false})
}

// loginQrPng 把扫码会话的 URL 渲染成 PNG（img 标签带不了 Bearer 头，所以
// 这个端点支持 ?key= 查询参数鉴权）。
func (p *Panel) loginQrPng(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	content := p.logins.QrContent(id)
	if content == "" {
		http.Error(w, "扫码会话不存在或已过期", http.StatusNotFound)
		return
	}
	png, err := qrcode.Encode(content, qrcode.Medium, 512)
	if err != nil {
		http.Error(w, "二维码生成失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func (p *Panel) loginDeviceStart(w http.ResponseWriter, r *http.Request) {
	base := p.cfg.CallbackBase
	if base == "" {
		writeErr(w, http.StatusNotImplemented, "未配置回调基址（面板可能监听在非 127.0.0.1）")
		return
	}
	id, authURL, err := p.logins.StartDevice(p.cfg.Upstream, base, p.cfg.AppName)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "url": authURL})
}

func (p *Panel) loginDevicePoll(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少 id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	creds, done, err := p.logins.PollDevice(ctx, id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if !done || creds == nil {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "status": "等待浏览器授权"})
		return
	}
	if aerr := p.admitQuiet(creds); aerr != nil {
		writeErr(w, http.StatusInternalServerError, "入池失败: "+aerr.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"done": true, "status": "已入池",
		"uid": creds.UID, "nickname": creds.Nickname})
}

func (p *Panel) loginCode(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(r, &payload); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	code, _, ok := upstream.ParseCallbackCode(payload.Code)
	if !ok {
		// 也允许直接粘一段账号 JSON（用户可能点错入口）。
		p.importAccount(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	creds, err := p.logins.ExchangeCode(ctx, p.cfg.Upstream, code)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := p.admitQuiet(creds); err != nil {
		writeErr(w, http.StatusInternalServerError, "入池失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": creds.UID, "nickname": creds.Nickname})
}

// authCallback 是环回授权的回调页。
//
// 官方 authorize 页会把浏览器 302 到 http://127.0.0.1:<port>/auth/callback?code=…
// 我们收下 code（交给会话换票），并给用户一个「可以关掉这个页面」的提示。
func (p *Panel) authCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	if code == "" {
		code = r.URL.Query().Get("authorization_code")
	}
	if code == "" || state == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(callbackPage("授权失败", "回调里缺少 code 或 state，请回面板重新发起。")))
		return
	}
	if err := p.logins.AcceptCallback(state, code); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(callbackPage("授权失败", err.Error())))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(callbackPage("授权成功", "凭据已收到，正在换票入池。可以关闭本页，回到面板查看。")))
}

// callbackPage 是回调提示页（内联样式，不依赖外部资源）。
func callbackPage(title, message string) string {
	return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>` + title + `</title>
<style>body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;
display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;
background:#f6f7f9;color:#1f2937}main{background:#fff;padding:40px 48px;border-radius:14px;
box-shadow:0 8px 30px rgba(0,0,0,.06);text-align:center}h1{font-size:20px;margin:0 0 12px}
p{color:#6b7280;margin:0;font-size:14px;line-height:1.6}</style></head>
<body><main><h1>` + title + `</h1><p>` + message + `</p></main></body></html>`
}

// admitQuiet 与 admit 同逻辑，但不写 HTTP 响应（供登录流程复用）。
func (p *Panel) admitQuiet(account *auth.Auth) error {
	if account == nil || account.UID == "" {
		return errors.New("无法确定账号 uid")
	}
	mergeExisting(account, p.cfg.Pool.AuthByUID(account.UID))
	if err := account.Save(p.cfg.AuthDir); err != nil {
		return err
	}
	p.cfg.Pool.Add(account)
	log.Printf("[panel] 登录入池 %s（%s）", account.Nickname, account.UID)
	p.claimWelcome(account)
	return nil
}

// ---------------------------------------------------------------------------
// 辅助

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func queryInt(r *http.Request, key string, fallback int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// 账号列表按昵称排序（面板稳定展示）。
func sortEntries(entries []pool.Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Nickname) < strings.ToLower(entries[j].Nickname)
	})
}
