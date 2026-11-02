// Package server 暴露 OpenAI / Anthropic 兼容的 HTTP 接口，内部驱动 pool 选号 +
// upstream 转发。
//
// 三条入站协议共用同一条链路（选号 → 续期 → 转发 → 记账 → 换号重试）：
//
//	POST /v1/chat/completions   OpenAI Chat Completions（原生）
//	POST /v1/messages           Anthropic Messages（Claude Code 的入口）
//	POST /v1/responses          OpenAI Responses（Codex 的入口）
//
// 协议桥只做「翻译形状」，不参与选号与重试；所以三个入口的账号轮转、冷却、
// 用量口径完全一致，不会出现「用 Claude Code 时账号策略不一样」这种怪事。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/rsfree/xhx2api/internal/livecfg"
	"github.com/rsfree/xhx2api/internal/pool"
	"github.com/rsfree/xhx2api/internal/prompt"
	"github.com/rsfree/xhx2api/internal/reqlog"
	"github.com/rsfree/xhx2api/internal/session"
	"github.com/rsfree/xhx2api/internal/upstream"
	"github.com/rsfree/xhx2api/internal/usage"
)

// Config 是 handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 空 = 不鉴权；Live 同时给出时 Live 优先
	MaxRotate int    // 单请求最多换号次数
	// Session 会话粘性路由器（nil = 关闭）。
	Session *session.Router
	// StickyByPrefix 让「没有显式会话 id」的请求也按消息前缀粘性路由。
	//
	// 默认关闭：同一个用户的编码代理每轮 system 都一样，前缀指纹恒定，
	// 打开后会把该用户的全部流量钉死在一个号上（多号池退化成单号）。
	// 需要「同前缀尽量同号」以降低风控特征时再打开。
	StickyByPrefix bool
	// Live 热配置（api_key / 黑名单 / 提示词模式）。
	Live *livecfg.Holder
	// Usage 逐请求用量记录（nil = 不记录）。
	Usage *usage.Recorder
	// RequestLog 请求指标与归档（nil = 不记录）。
	RequestLog *reqlog.Recorder
	// Panel 管理面板（nil = 不挂载）。
	Panel http.Handler
	// DefaultModel 请求未给 model 时使用。
	DefaultModel string
	// Aliases 客户端通用模型名 → 上游 id（如 gpt-4o → raccoon-chat-ml-5-5）。
	Aliases map[string]string
	// PromptMode / PromptText 提示词策略。
	PromptMode string
	PromptText string
	// RefreshSkew token 提前刷新窗口。
	RefreshSkew time.Duration
	// Version 版本号（/status 展示）。
	Version string
	// MaxBodyBytes 入站请求体上限（默认 32MiB）。
	MaxBodyBytes int64
	// ExcludeUpstreamHidden 让 /v1/models 只列「上游 visible=true」的模型。
	//
	// 默认 false（= 全列）。用「排除」而不是「包含」是为了让零值就是正确行为：
	// 布尔字段在构造函数里分不清「没设」和「显式 false」，一旦用「包含」，
	// 任何不经 config 的构造（测试、嵌入式使用）都会静默把模型过滤掉。
	ExcludeUpstreamHidden bool
	// QueueWait 是所有账号在途满时的最长等待（0 = 不等待，直接软回退）。
	//
	// 上游单账号并发实测：≤10 全成功，12 起 429 limiter_request_error。
	// 等几百毫秒拿到刚归还的名额，比打过去换 60 秒冷却便宜。
	QueueWait time.Duration
	// DisableImages 关掉 /v1/images/generations（零值 = 开启）。
	// 用「禁用」而不是「启用」，理由同 ExcludeUpstreamHidden：零值要是正确行为。
	DisableImages bool
	// ImageModel 指定上游图片模型（空 = 上游默认组 raccoon-image-gen）。
	ImageModel string
	// ImageReturnURL 表示 url 能力是否可用（配置里 return_url 默认开）。
	// 关掉时显式要 response_format=url 会 400，默认形态回落 b64_json。
	ImageReturnURL bool
	// ImageDefaultFormat 是客户端没传 response_format 时的默认形态：
	// "url"（默认）/ "b64_json" / "both"。空值按 "url" 处理。
	ImageDefaultFormat string
	// ImagePublicBase 是写进 url 的对外基址（空 = 按请求 Host 推导）。
	ImagePublicBase string
	// ImageDir 是图片落盘目录（空 = 不落盘，也就不产生 url）。
	ImageDir string
	// ImageMaxTotalMB 是落盘目录的总量上限（0 = 不限制）。
	// 超限后按 mtime 删最旧的，免得默认开 url 之后把磁盘写满。
	ImageMaxTotalMB int
	// ImageUploader 非 nil 时图片优先传对象存储，url 用存储地址；
	// 上传失败自动回落本地落盘（ImageDir 有值时）。
	ImageUploader ImageUploader
	// ImageTimeout 单张生图的超时（0 = 5 分钟）。
	//
	// 实测 20-200s，比对话慢一个数量级；给 5 分钟是为了覆盖高分辨率档位。
	ImageTimeout time.Duration
	// MinOutputTokens 出站 max_tokens 下限（0 = 不抬升；默认 1024）。
	// 思考 token 算在 max_tokens 里，客户端给 16/512 会被思考吃光、正文回空。
	MinOutputTokens int
}

// Handler 是网关主 handler。
type Handler struct {
	cfg Config
	mux *http.ServeMux
}

// NewHandler 装配路由。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 32 << 20
	}
	// 负数 = 显式关闭；0/未设 = 用默认下限。
	if cfg.MinOutputTokens == 0 {
		cfg.MinOutputTokens = defaultMinOutputTokens
	} else if cfg.MinOutputTokens < 0 {
		cfg.MinOutputTokens = 0
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = upstream.DefaultModel
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/messages", h.withAnthropicAuth(h.messages))
	h.mux.HandleFunc("POST /v1/messages/count_tokens", h.withAnthropicAuth(h.countTokens))
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responses))
	h.mux.HandleFunc("POST /v1/images/generations", h.withAuth(h.imageGenerations))
	h.mux.HandleFunc("POST /v1/images/edits", h.withAuth(h.imageEdits))
	// 落盘图片的读取入口：故意不鉴权，见 imageFile 的注释。
	h.mux.HandleFunc("GET /v1/images/{name}", h.imageFile)
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.Panel != nil {
		// /panel 不带斜杠时 ServeMux 自动 301 到 /panel/。
		h.mux.Handle("/panel/", cfg.Panel)
		// 浏览器环回授权的回调不在 /panel 前缀下（上游 authorize 页只会把
		// code 打到 http://127.0.0.1:<port>/auth/callback），必须在这里转交。
		h.mux.Handle("/auth/callback", cfg.Panel)
	}
	return h
}

// ServeHTTP 统一入口：请求日志埋点 + 分发。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.cfg.RequestLog == nil || !h.traced(r) {
		h.mux.ServeHTTP(w, r)
		return
	}
	h.cfg.RequestLog.Begin()
	obs := &responseObserver{ResponseWriter: w}
	trace := &requestTrace{
		id: reqlog.NewRequestID(), start: time.Now(),
		path: r.URL.Path, protocol: protocolOf(r.URL.Path),
	}
	if h.requestLogClientInfo() {
		trace.clientIP = clientIP(r)
		trace.userAgent = r.Header.Get("User-Agent")
	}
	w.Header().Set("X-Request-Id", trace.id)
	r = r.WithContext(context.WithValue(r.Context(), traceKey{}, trace))
	defer func() {
		status := obs.status
		if status == 0 {
			status = http.StatusOK
		}
		ev := trace.event(status)
		ev.Stream = trace.stream
		ev.TTFBMS = trace.ttfbMS
		ev.Attempts = trace.attempts
		ev.Rotations = trace.rotations
		h.cfg.RequestLog.Record(ev)
	}()
	h.mux.ServeHTTP(obs, r)
}

// traced 只对模型调用埋点；/v1/models、/status 这类读接口不进请求账本。
func (h *Handler) traced(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/v1/chat/completions", "/v1/messages", "/v1/responses", "/v1/images/generations", "/v1/images/edits":
		return true
	}
	return false
}

func protocolOf(path string) string {
	switch path {
	case "/v1/chat/completions":
		return "chat"
	case "/v1/messages", "/v1/messages/count_tokens":
		return "messages"
	case "/v1/responses":
		return "responses"
	case "/v1/images/generations", "/v1/images/edits":
		return "images"
	}
	return ""
}

func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); v != "" {
		if i := strings.Index(v, ","); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type traceKey struct{}

// requestTrace 是一个请求的埋点上下文。
type requestTrace struct {
	id         string
	start      time.Time
	path       string
	protocol   string
	model      string
	uid        string
	account    string
	stream     bool
	attempts   int
	rotations  int
	ttfbMS     int64
	errKind    string
	errMsg     string
	prompt     int64
	completion int64
	total      int64
	clientIP   string
	userAgent  string
}

func traceFrom(ctx context.Context) *requestTrace {
	tr, _ := ctx.Value(traceKey{}).(*requestTrace)
	return tr
}

func (t *requestTrace) event(status int) reqlog.Event {
	if t == nil {
		return reqlog.Event{Status: status}
	}
	return reqlog.Event{
		ID: t.id, Time: t.start, Method: http.MethodPost, Path: t.path,
		Protocol: t.protocol, Model: t.model, UID: t.uid, Account: t.account,
		Status: status, ErrorKind: t.errKind, Error: t.errMsg,
		DurationMS: time.Since(t.start).Milliseconds(), TTFBMS: t.ttfbMS,
		Prompt: t.prompt, Completion: t.completion, Total: t.total,
		Stream: t.stream, Attempts: t.attempts, Rotations: t.rotations,
		ClientIP: t.clientIP, UserAgent: t.userAgent,
	}
}

// responseObserver 记录状态码与「是否已写过」。
type responseObserver struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (o *responseObserver) WriteHeader(code int) {
	if !o.wrote {
		o.status = code
		o.wrote = true
	}
	o.ResponseWriter.WriteHeader(code)
}

func (o *responseObserver) Write(p []byte) (int, error) {
	if !o.wrote {
		o.status = http.StatusOK
		o.wrote = true
	}
	return o.ResponseWriter.Write(p)
}

func (o *responseObserver) Flush() {
	if f, ok := o.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ---------------------------------------------------------------------------
// 鉴权

// withAuth 是 OpenAI 侧鉴权（Bearer）。
func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.authOK(r) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

// withAnthropicAuth 兼容 SDK 的 x-api-key 头（官方 SDK 不用 Bearer）。
func (h *Handler) withAnthropicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			if k := r.Header.Get("x-api-key"); k != "" {
				r.Header.Set("Authorization", "Bearer "+k)
			}
		}
		if !h.authOK(r) {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

// authOK 校验密钥（空 key = 放行，仅本机/私网）。
func (h *Handler) authOK(r *http.Request) bool {
	key := h.apiKey()
	if key == "" {
		return true
	}
	token := bearerOf(r)
	if token == "" {
		return false
	}
	// 常量时间比较，避免逐字节计时侧信道。
	return constantTimeEqual(token, key)
}

// apiKey 当前生效密钥（Live 优先）。
func (h *Handler) apiKey() string {
	if h.cfg.Live != nil {
		return h.cfg.Live.Load().APIKey
	}
	return h.cfg.APIKey
}

func bearerOf(r *http.Request) string {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(raw) > 7 && strings.EqualFold(raw[:7], "bearer ") {
		return strings.TrimSpace(raw[7:])
	}
	return ""
}

// constantTimeEqual 定长比较（长度不同也走完整循环）。
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

// ---------------------------------------------------------------------------
// 健康检查与状态

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().Format(time.RFC3339)})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	counts := h.cfg.Pool.Counts()
	payload := map[string]any{
		"version": h.cfg.Version,
		"accounts": map[string]int{
			"total": counts.Total, "healthy": counts.Healthy,
			"cooling": counts.Cooling, "disabled": counts.Disabled,
			"in_flight": counts.InFlight,
		},
		"models":        len(h.cfg.Upstream.Catalog()),
		"catalog_age":   h.cfg.Upstream.CatalogAge().Format(time.RFC3339),
		"uptime_s":      int(time.Since(startedAt).Seconds()),
		"default_model": h.cfg.DefaultModel,
	}
	if h.cfg.Session != nil {
		payload["session_sticky"] = h.cfg.Session.Stats()
	}
	if h.cfg.Usage != nil {
		payload["usage"] = h.cfg.Usage.Snapshot().Lifetime
	}
	if h.cfg.RequestLog != nil {
		payload["requests"] = h.cfg.RequestLog.Metrics()
	}
	writeJSON(w, http.StatusOK, payload)
}

var startedAt = time.Now()

// ---------------------------------------------------------------------------
// 模型列表

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("refresh") == "1" || r.URL.Query().Get("refresh") == "true"
	if force {
		// 有可用号就刷新一次（失败不影响返回旧目录）。
		if a := h.cfg.Pool.FirstAvailable(); a != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			if _, err := h.cfg.Upstream.RefreshModels(ctx, a, true); err != nil {
				log.Printf("WARN: [models] 刷新目录失败: %v", err)
			}
			cancel()
		}
	} else if time.Since(h.cfg.Upstream.CatalogAge()) > 10*time.Minute {
		// 目录过期：后台刷一次，本次先用旧目录回（不让 /v1/models 等上游）。
		if a := h.cfg.Pool.FirstAvailable(); a != nil {
			auth := a
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
				defer cancel()
				if _, err := h.cfg.Upstream.RefreshModels(ctx, auth, true); err != nil {
					log.Printf("WARN: [models] 后台刷新目录失败: %v", err)
				}
			}()
		}
	}
	data := make([]map[string]any, 0, 16)
	seen := map[string]bool{}
	for _, mi := range h.cfg.Upstream.Catalog() {
		// 上游的 visible=false 只表示「不在官网模型选择器里」，不代表不可调用：
		// 实测三个 raccoon-* 都被标成 false，而其中一个正是网关的默认模型。
		// 按 visible 过滤会让客户端（靠 /v1/models 发现模型）永远看不到默认模型。
		if (!mi.Visible && h.cfg.ExcludeUpstreamHidden) || h.hidden(mi.ID) || seen[mi.ID] {
			continue
		}
		seen[mi.ID] = true
		entry := map[string]any{
			"id": mi.ID, "object": "model", "created": 0, "owned_by": "raccoon",
			"name": mi.Name, "context_length": mi.ContextWindow,
			"max_output_tokens": mi.MaxOutput,
			"capabilities": map[string]any{
				"vision": mi.VL, "reasoning": mi.Reasoning, "tools": true, "stream": true,
			},
		}
		if mi.Credits != "" {
			entry["credits"] = mi.Credits
		}
		if mi.BillingStatus != "" {
			entry["billing_status"] = mi.BillingStatus
			entry["free_note"] = mi.FreeNote
			entry["base_credits"] = mi.BaseCredits
		}
		// 上游没在目录里列出、但实际可调用：客户端应当知道这一点，
		// 否则「为什么默认模型不在列表里」永远说不清。
		if !mi.Visible {
			entry["unlisted"] = true
		}
		data = append(data, entry)
	}
	// 生图模型是网关合成的：上游目录里不列图片模型（model_catalog 只有对话），
	// 但客户端需要有个名字可填，否则根本发现不了这条能力。
	//
	// 另外把 gpt-image-* / dall-e-* 这些客户端硬编码的名字也列出来（默认开着）。
	// 它们不是真的换模型——实测上游 /images/gen 完全忽略 model 字段——只是让
	// 「默认填 gpt-image-1 的客户端」不必手工改配置就能选中。别在描述里假装
	// 是 OpenAI 的模型：alias_of 指回合成 id，note 说明实际走的通道。
	if !h.cfg.DisableImages {
		for i, id := range upstream.ImageModelIDs(h.imageAliases()) {
			if h.hidden(id) {
				continue
			}
			entry := map[string]any{
				"id": id, "object": "model", "created": 0, "owned_by": "raccoon",
				"name": "文生图 / 图生图（OpenAI Images 兼容）",
				"capabilities": map[string]any{
					"image": true, "edit": true, "chat": false, "stream": false,
				},
			}
			if i > 0 {
				entry["alias_of"] = upstream.ImageModelID
				entry["note"] = "别名：上游只有一条生图通道，model 字段不参与路由（size/quality 才生效）"
			}
			data = append(data, entry)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// hidden 判断模型是否被配置隐藏。
func (h *Handler) hidden(model string) bool {
	if h.cfg.Live != nil {
		return h.cfg.Live.Load().Hidden(model)
	}
	return false
}

// ---------------------------------------------------------------------------
// chat 主链路

// chatRequest 是从入站体里抽出来的调度所需字段（其余原样透传）。
type chatRequest struct {
	Model  string
	Stream bool
	// TurnTitle 用于积分对账（上游把它写进账单）。
	TurnTitle string
}

// parseChatRequest 轻量解析（不重建 body，避免改变上游看到的字节）。
func parseChatRequest(body []byte) (chatRequest, error) {
	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return chatRequest{}, err
	}
	return chatRequest{Model: strings.TrimSpace(probe.Model), Stream: probe.Stream, TurnTitle: probe.Title}, nil
}

// rewriteModel 只改 model 字段，其余字节原样保留（数字精度、字段顺序都不动）。
func rewriteModel(body []byte, model string) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	payload["model"] = raw
	return json.Marshal(payload)
}

// resolveModel 把客户端模型名归一到上游 id。
//
// 顺序：空 → 默认；黑名单 → 拒绝；别名表 → 目标；目录精确命中 → 用它；
// 目录非空且名字不像上游 id → 拒绝（避免把 gpt-5 打上去白扣一次额度）；
// 目录为空（上游不可达）→ 原样透传，让上游自己判。
func (h *Handler) resolveModel(name string) (string, error) {
	// 生图模型不能用于对话：与其让它打到上游换一个看不懂的报错，
	// 不如在这里说清楚该调哪个地址。
	//
	// 别名（gpt-image-2 / dall-e-3 …）同样拦：客户端把图片模型写进聊天
	// 请求是常见误配，放过去只会白扣一次额度。
	if upstream.IsImageModel(name, h.imageAliases()) {
		return "", &modelError{Status: http.StatusBadRequest, Code: "invalid_model",
			Msg: fmt.Sprintf("%s 是生图模型，请调用 POST /v1/images/generations（不支持对话）", strings.TrimSpace(name))}
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = h.defaultModel()
	}
	if h.hidden(name) {
		return "", &modelError{Status: http.StatusForbidden, Code: "model_hidden",
			Msg: fmt.Sprintf("模型 %s 已被网关禁用", name)}
	}
	if target, ok := h.alias(name); ok {
		return target, nil
	}
	if _, ok := h.cfg.Upstream.ResolveModel(name); ok {
		return name, nil
	}
	catalog := h.cfg.Upstream.Catalog()
	// 兜底目录只有一条：说明从没成功拉到过真目录，此时不校验（上游可能认识这个名字）。
	if len(catalog) <= 1 {
		return name, nil
	}
	// 目录可用但没命中：上游新增的模型（raccoon- 前缀）放行，通用名 400。
	for _, mi := range catalog {
		if strings.EqualFold(mi.ID, name) {
			return mi.ID, nil
		}
	}
	if strings.HasPrefix(strings.ToLower(name), "raccoon-") || strings.HasPrefix(strings.ToLower(name), "sn-") {
		return name, nil
	}
	return "", &modelError{Status: http.StatusBadRequest, Code: "model_not_found",
		Msg: fmt.Sprintf("模型 %s 不在小浣熊目录中；可用模型见 GET /v1/models", name)}
}

// modelError 是模型解析错误（带 HTTP 状态）。
type modelError struct {
	Status int
	Code   string
	Msg    string
}

func (e *modelError) Error() string { return e.Msg }

// alias 查别名表：热配置优先，空表回退启动时注入的静态表。
func (h *Handler) alias(name string) (string, bool) {
	if h.cfg.Live != nil {
		if s := h.cfg.Live.Load(); len(s.Aliases) > 0 {
			return s.Alias(name)
		}
	}
	return lookupAlias(h.cfg.Aliases, name)
}

// imageAliases 取当前生效的生图模型别名表（热配置优先）。
//
// 返回的切片已做去重与大写归一，且**不含**合成 id——需要完整列表时用
// upstream.ImageModelIDs（它会自己拼上合成 id）。
func (h *Handler) imageAliases() []string {
	if h.cfg.Live != nil {
		return upstream.EffectiveImageAliases(h.cfg.Live.Load().ImageAliases)
	}
	return upstream.EffectiveImageAliases(nil)
}

// defaultModel 取「请求未给 model」时用的名字：热配置优先。
func (h *Handler) defaultModel() string {
	if h.cfg.Live != nil {
		if v := h.cfg.Live.Load().DefaultModel; v != "" {
			return v
		}
	}
	return h.cfg.DefaultModel
}

func lookupAlias(aliases map[string]string, name string) (string, bool) {
	if len(aliases) == 0 {
		return "", false
	}
	if v, ok := aliases[name]; ok {
		return v, true
	}
	lower := strings.ToLower(name)
	for k, v := range aliases {
		if strings.ToLower(k) == lower {
			return v, true
		}
	}
	return "", false
}

// chatCompletions 是 OpenAI chat 入口，也是另外两个协议入口的共用底座。
func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	tr := traceFrom(r.Context())
	body, err := readBody(r, h.cfg.MaxBodyBytes)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	req, err := parseChatRequest(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
		return
	}
	if tr != nil {
		tr.model = req.Model
		tr.stream = req.Stream
	}
	model, err := h.resolveModel(req.Model)
	if err != nil {
		var me *modelError
		if errors.As(err, &me) {
			writeOpenAIError(w, me.Status, me.Code, me.Msg)
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_model", err.Error())
		return
	}
	if tr != nil {
		tr.model = model
	}
	// clientModel 是客户端眼里的模型名：响应里的 model 字段必须回显它。
	// 上游会把自己的 id 塞进每一帧，客户端不认识 raccoon-*，部分实现还会
	// 按这个字段校验模型是否匹配；记账仍用上游 id，否则别名会把统计拆成十几条。
	clientModel := strings.TrimSpace(req.Model)
	if clientModel == "" {
		clientModel = h.defaultModel()
	}
	// 提示词改写（passthrough 时零成本返回原 body）。
	body, _ = prompt.Apply(body, prompt.Options{Mode: h.promptMode(), Text: h.promptText()})
	body, err = rewriteModel(body, model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "rewrite model: "+err.Error())
		return
	}
	// title 只走 Header（积分对账），body 里必须摘掉：上游对未知字段严格校验。
	body = stripTurnTitle(body)
	// temperature 一律不外发：上游对思考型模型只接受 1，客户端带的 0.7
	// 会被直接 400（2026-11-02 线上 57% 的失败来自这一条）。
	body = stripTemperature(body)
	// 思考 token 算在 max_tokens 里，小额度会让正文回空——统一抬到下限。
	body = liftMaxTokens(body, h.cfg.MinOutputTokens)
	body = upstream.PrepareBody(body)

	// 会话键：显式 id 优先，其次消息前缀指纹。
	sessionKey, prefix := session.Key(r.Header.Get, body)
	if h.cfg.Session != nil && prefix != "" {
		h.cfg.Session.NotePrefix(prefix)
	}
	if sessionKey == "" && prefix != "" && h.cfg.StickyByPrefix {
		sessionKey = prefix
	}

	var lastErr error
	// escalated 表示已经为「思考吃光额度」加过一次额度，避免反复重试。
	escalated := false
	// clamped 表示已经为「max_tokens 超上限」收过一次，避免同一份 body 反复撞。
	clamped := false
	excluded := map[string]bool{}
	for attempt := 0; attempt < h.cfg.MaxRotate; attempt++ {
		if tr != nil {
			tr.attempts = attempt + 1
		}
		lease, perr := h.pickForRequest(r.Context(), sessionKey, excluded)
		if perr != nil {
			// 已经有过上游错误时不要用「没有可用账号」盖掉它：单账号部署下，
			// 第一次 5xx 会把唯一的号排除，第二次 Pick 必然 no_account，
			// 用户看到的就成了误导性的「没有可用账号」（实测踩过）。
			if lastErr == nil {
				lastErr = perr
			}
			break
		}
		a := lease.Auth()
		if a == nil {
			lease.ReleaseSuccess(0)
			lastErr = pool.ErrNoAccount
			break
		}
		if tr != nil {
			tr.uid = a.UID
			tr.account = a.Snapshot().Nickname
		}
		// 续期：临期主动刷，避免把 401 打到上游。
		if _, rerr := h.cfg.Upstream.RefreshTokenIfNeeded(r.Context(), a, h.cfg.RefreshSkew); rerr != nil {
			log.Printf("WARN: [refresh] %s: %v", a.UID, rerr)
		}
		stream, status, rawErr, cerr := h.cfg.Upstream.ChatStream(r.Context(), a, body, req.TurnTitle)
		latency := time.Since(started)
		if cerr != nil {
			kind := classifyErr(cerr, status, string(rawErr))
			msg := cerr.Error()
			// 输出额度超出上游区间：报错里给了 [lo, hi]，收进区间再重试。
			// 只收不发明额度，同号重试、不吃惩罚——这是客户端填大了或填了
			// 负数（当「不限」用），不是账号有问题。
			if kind == "client" && !clamped {
				if lo, hi := maxTokensRangeFromError(msg); hi > 0 {
					if fixed, changed := clampMaxTokens(body, lo, hi); changed {
						clamped = true
						body = fixed
						lease.ReleaseSuccess(latency)
						if tr != nil {
							tr.rotations++
						}
						log.Printf("INFO: [chat] uid=%s model=%s max_tokens 超出 [%d,%d]，收进区间重试", a.UID, model, lo, hi)
						continue
					}
				}
			}
			// 登录态失效先试着续期，续上了就**不罚号**：401 往往只是 token 恰好
			// 过期，账号本身完全正常。先罚 2 分钟冷却再去续期，会让这个号在
			// 冷却窗口内不可用（实测踩过：单号部署直接 503）。
			if kind == "session_dead" && a.CanRefresh() {
				if rerr := h.cfg.Upstream.RefreshToken(r.Context(), a); rerr == nil {
					lease.ReleaseSuccess(latency)
					lastErr = cerr
					if tr != nil {
						tr.rotations++
					}
					continue
				}
			}
			// 首次 429 不罚号：上游的速率闸是秒级恢复的，罚号会把一次瞬时
			// 过载放大成整池长时间冷却（见 chatRateRetryDelay 注释）。
			// 连续第二次才按普通错误处理——那时换号 + 短冷却才是对的。
			if kind == "soft_rate" && attempt == 0 {
				lease.ReleaseSuccess(latency)
				lastErr = cerr
				if tr != nil {
					tr.rotations++
				}
				log.Printf("INFO: [chat] uid=%s model=%s 429 瞬时过载，%s 后原地重试", a.UID, model, chatRateRetryDelay)
				select {
				case <-time.After(chatRateRetryDelay):
				case <-r.Context().Done():
					writeOpenAIError(w, http.StatusRequestTimeout, "client_closed", "客户端已断开")
					return
				}
				continue
			}
			lease.Release(&kind, msg, latency)
			lastErr = cerr
			log.Printf("WARN: [chat] uid=%s model=%s attempt=%d kind=%s: %v", a.UID, model, attempt+1, kind, cerr)
			if !shouldRotate(kind) {
				break
			}
			excluded[a.UID] = true
			if h.cfg.Session != nil {
				h.cfg.Session.Forget(sessionKey)
			}
			if tr != nil {
				tr.rotations++
			}
			continue
		}
		// 选号成功：绑定会话。
		if h.cfg.Session != nil {
			h.cfg.Session.Bind(sessionKey, a.UID)
		}
		if req.Stream {
			h.serveStream(w, r, stream, clientModel, model, lease, latency, tr)
			return
		}
		// 非流式：在循环内聚合，让「空流 / 流内错误」也能换号重试——
		// 这两类错误只有读完流才知道，必须留在重试循环里。
		obj, aerr := upstream.Aggregate(stream, clientModel)
		_ = stream.Close()
		if aerr != nil {
			kind := classifyErr(aerr, 0, aerr.Error())
			lease.Release(&kind, aerr.Error(), time.Since(started))
			lastErr = aerr
			if tr != nil {
				tr.errKind = kind
				tr.errMsg = aerr.Error()
			}
			log.Printf("WARN: [chat] uid=%s model=%s attempt=%d 聚合失败 kind=%s: %v", a.UID, model, attempt+1, kind, aerr)
			if kind == "server" || kind == "gateway" {
				excluded[a.UID] = true
				if tr != nil {
					tr.rotations++
				}
				continue
			}
			break
		}
		// 思考吃光额度：正文为空、finish=length。额度是上限不是花费，
		// 所以自动加到 8192 重试一次——用户要的是答案，不是一句「预算不够」。
		if upstream.ReasoningExhausted(obj) {
			if !escalated {
				escalated = true
				body = raiseMaxTokens(body, escalatedMaxTokens)
				lease.ReleaseSuccess(time.Since(started))
				if tr != nil {
					tr.rotations++
				}
				log.Printf("INFO: [chat] uid=%s model=%s 思考耗尽额度，提升到 %d 重试", a.UID, model, escalatedMaxTokens)
				continue
			}
			promptTokens, completionTokens, total := tokenCounts(obj["usage"])
			lease.ReleaseSuccess(time.Since(started))
			if tr != nil {
				tr.prompt, tr.completion, tr.total = promptTokens, completionTokens, total
				tr.errKind, tr.errMsg = "reasoning_exhausted", "思考耗尽输出额度"
			}
			h.recordUsage(tr, model, false, promptTokens, completionTokens, total, time.Since(started))
			writeOpenAIError(w, http.StatusBadGateway, "reasoning_exhausted",
				"模型把 max_tokens 全用于思考、正文为空（finish_reason=length）。"+
					"提高 max_tokens 后重试，或换一个非思考型模型（raccoon-* 系列）")
			return
		}
		promptTokens, completionTokens, total := tokenCounts(obj["usage"])
		lease.ReleaseSuccess(time.Since(started))
		if tr != nil {
			tr.prompt, tr.completion, tr.total = promptTokens, completionTokens, total
		}
		h.recordUsage(tr, model, false, promptTokens, completionTokens, total, time.Since(started))
		writeJSON(w, http.StatusOK, obj)
		return
	}

	// 全部尝试失败。
	if lastErr == nil {
		lastErr = pool.ErrNoAccount
	}
	kind := classifyErr(lastErr, 0, "")
	if tr != nil {
		tr.errKind = kind
		tr.errMsg = lastErr.Error()
	}
	status, code, msg := errorResponse(lastErr, kind, h.cfg.Pool.Counts())
	writeOpenAIError(w, status, code, msg)
}

// pickForRequest 选号：优先复用会话已绑定的账号（若仍可用）。
//
// 全部账号在途满时会最多等 QueueWait（默认 2s）——上游单号并发超过 10-12
// 就回 429，等一个正在返回的请求比撞限流划算。
func (h *Handler) pickForRequest(ctx context.Context, sessionKey string, excluded map[string]bool) (*pool.Lease, error) {
	if h.cfg.Session != nil && sessionKey != "" {
		if uid, ok := h.cfg.Session.Lookup(sessionKey); ok && !excluded[uid] {
			if lease, err := h.cfg.Pool.PickUID(uid); err == nil {
				return lease, nil
			}
			h.cfg.Session.Forget(sessionKey)
		}
	}
	return h.cfg.Pool.PickWait(ctx, excluded, 0, h.cfg.QueueWait)
}

// serveStream 把上游流写给客户端。
//
// 一旦写出第一个字节就不能换号了：HTTP 状态码已经发给客户端，重试只会让
// 客户端收到两段错乱的内容。所以流内错误统一在流里注入 error 帧 + [DONE] 收尾。
// 首帧错误（HTTP 200 + {"error":…}）已经在 ChatStream 里被 peek 拦下，不会走到这里。
func (h *Handler) serveStream(w http.ResponseWriter, r *http.Request, stream io.ReadCloser,
	clientModel, upstreamModel string, lease *pool.Lease, upstreamLatency time.Duration, tr *requestTrace) {
	started := time.Now()
	defer stream.Close()
	if tr != nil {
		tr.ttfbMS = upstreamLatency.Milliseconds()
	}
	obs := &responseObserver{ResponseWriter: w}
	onErr := func(se *upstream.StreamError) {
		if tr != nil {
			tr.errKind = se.Kind().String()
			tr.errMsg = se.Msg
		}
	}
	usageMap, err := upstream.StreamWithError(obs, stream, onErr, clientModel)
	if err != nil {
		kind := classifyErr(err, 0, err.Error())
		lease.Release(&kind, err.Error(), time.Since(started))
		if tr != nil {
			tr.errKind = kind
			tr.errMsg = err.Error()
		}
		// 还没写任何字节：可以给客户端一个明确的错误（已经写过就只能截流）。
		if !obs.wrote {
			status, code, msg := errorResponse(err, kind, h.cfg.Pool.Counts())
			writeOpenAIError(w, status, code, msg)
		}
		return
	}
	promptTokens, completionTokens, total := tokenCounts(usageMap)
	lease.ReleaseSuccess(time.Since(started))
	if tr != nil {
		tr.prompt, tr.completion, tr.total = promptTokens, completionTokens, total
	}
	h.recordUsage(tr, upstreamModel, true, promptTokens, completionTokens, total, time.Since(started))
}

// recordUsage 写用量（nil 安全）。
func (h *Handler) recordUsage(tr *requestTrace, model string, stream bool,
	promptTokens, completionTokens, total int64, dur time.Duration) {
	if h.cfg.Usage == nil {
		return
	}
	ev := usage.Event{
		Time: time.Now(), Model: model, Stream: stream,
		Prompt: promptTokens, Completion: completionTokens, Total: total,
		DurationMS: dur.Milliseconds(), Status: http.StatusOK,
	}
	if tr != nil {
		ev.UID, ev.Account, ev.Protocol = tr.uid, tr.account, tr.protocol
		ev.TTFBMS = tr.ttfbMS
		ev.Attempts = tr.attempts
	}
	h.cfg.Usage.Record(ev)
}

// tokenCounts 从 usage 里取 token（兼容 prompt_tokens/input_tokens 两套命名）。
func tokenCounts(v any) (prompt, completion, total int64) {
	m, ok := v.(map[string]any)
	if !ok {
		return 0, 0, 0
	}
	pick := func(keys ...string) int64 {
		for _, k := range keys {
			if n, ok := asInt64Any(m[k]); ok && n > 0 {
				return n
			}
		}
		return 0
	}
	prompt = pick("prompt_tokens", "input_tokens")
	completion = pick("completion_tokens", "output_tokens")
	total = pick("total_tokens")
	if total == 0 {
		total = prompt + completion
	}
	return
}

func asInt64Any(v any) (int64, bool) {
	switch typed := v.(type) {
	case float64:
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case json.Number:
		n, err := typed.Int64()
		return n, err == nil
	}
	return 0, false
}

// classifyErr 把任意错误归一成 pool 能理解的类别。
func classifyErr(err error, status int, body string) pool.ErrKind {
	var ue *upstream.Error
	if errors.As(err, &ue) {
		return ue.Kind.String()
	}
	var se *upstream.StreamError
	if errors.As(err, &se) {
		return se.Kind().String()
	}
	var be *upstream.BusinessError
	if errors.As(err, &be) {
		return upstream.Classify(status, body).String()
	}
	switch {
	case errors.Is(err, upstream.ErrEmptyStream):
		return upstream.ErrServer.String()
	case errors.Is(err, pool.ErrNoAccount):
		return "none"
	case errors.Is(err, context.Canceled):
		return "client"
	case errors.Is(err, context.DeadlineExceeded):
		return "gateway"
	}
	if status > 0 {
		return upstream.Classify(status, body).String()
	}
	return "none"
}

// shouldRotate 判断某类错误是否值得换号。
//
// 不换：请求本身被拒（模型名/参数/内容审核）、网关超时（换号也一样超时）、
// 客户端断开。换：额度/限流/登录态/5xx/排队/空流。
// chatRateRetryDelay 是对话遇到 429 后原地重试前的等待。
//
// 429 是上游的瞬时速率闸，不是账号故障：2026-10-30 双出口实测（同一账号
// 从 210.121.44.245 与 117.88.13.53 各发 6 个请求）全部 200，说明
// 「429 之后要等几十分钟」其实是网关自己的指数退避造成的幻觉。
// 罚号的代价远大于等待：一个客户端几秒内连撞几次，就能把全池按级别锁到
// 32 分钟~1.1 小时，此后 75% 的请求变成 503「没有可用账号」。
// 与生图（imageRateRetryDelay）同策：先原地等，别换号别罚号。
const chatRateRetryDelay = 2 * time.Second

func shouldRotate(kind pool.ErrKind) bool {
	switch kind {
	case "plan_limit", "soft_rate", "session_dead", "server", "busy", "not_found":
		return true
	}
	return false
}

// errorResponse 把错误映射成面向客户端的 (status, code, message)。
func errorResponse(err error, kind pool.ErrKind, counts pool.Counts) (int, string, string) {
	if errors.Is(err, pool.ErrNoAccount) {
		if counts.Total == 0 {
			return http.StatusServiceUnavailable, "no_account", "网关没有配置任何账号，请先在面板导入小浣熊账号"
		}
		return http.StatusServiceUnavailable, "no_account",
			fmt.Sprintf("没有可用账号（共 %d 个：%d 冷却中，%d 已禁用），请稍后重试或检查面板", counts.Total, counts.Cooling, counts.Disabled)
	}
	var ue *upstream.Error
	if errors.As(err, &ue) {
		switch ue.Kind {
		case upstream.ErrPlanLimit:
			return http.StatusTooManyRequests, "insufficient_quota", ue.Msg
		case upstream.ErrSoftRate:
			return http.StatusTooManyRequests, "rate_limit_exceeded", ue.Msg
		case upstream.ErrSessionDead:
			return http.StatusUnauthorized, "account_session_expired", ue.Msg
		case upstream.ErrGateway:
			return http.StatusGatewayTimeout, "upstream_timeout", ue.Msg
		case upstream.ErrBusy:
			return http.StatusServiceUnavailable, "upstream_busy", ue.Msg
		case upstream.ErrClient:
			status := ue.Status
			if status < 400 || status > 599 {
				status = http.StatusBadRequest
			}
			return status, "upstream_rejected", ue.Msg
		default:
			return http.StatusBadGateway, "upstream_error", ue.Msg
		}
	}
	var se *upstream.StreamError
	if errors.As(err, &se) {
		return http.StatusBadGateway, "upstream_error", se.Msg
	}
	if errors.Is(err, upstream.ErrEmptyStream) {
		return http.StatusBadGateway, "empty_stream", "上游返回空内容，请重试（多次出现请检查账号状态）"
	}
	switch kind {
	case "plan_limit":
		return http.StatusTooManyRequests, "insufficient_quota", err.Error()
	case "soft_rate":
		return http.StatusTooManyRequests, "rate_limit_exceeded", err.Error()
	case "session_dead":
		return http.StatusUnauthorized, "account_session_expired", err.Error()
	case "gateway":
		return http.StatusGatewayTimeout, "upstream_timeout", err.Error()
	}
	return http.StatusBadGateway, "proxy_error", err.Error()
}

// ---------------------------------------------------------------------------
// 辅助

// readBody 读取请求体（带上限）。
func readBody(r *http.Request, limit int64) ([]byte, error) {
	defer r.Body.Close()
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, fmt.Errorf("请求体超过 %d MiB 上限", limit>>20)
	}
	return buf.Bytes(), nil
}

func (h *Handler) promptMode() string {
	if h.cfg.Live != nil {
		if mode := h.cfg.Live.Load().PromptMode; mode != "" {
			return mode
		}
	}
	return h.cfg.PromptMode
}

func (h *Handler) promptText() string {
	if h.cfg.Live != nil {
		if text := h.cfg.Live.Load().PromptText; text != "" {
			return text
		}
	}
	return h.cfg.PromptText
}

func (h *Handler) requestLogClientInfo() bool {
	if h.cfg.RequestLog == nil {
		return false
	}
	if h.cfg.Live != nil {
		return h.cfg.Live.Load().RecordClientInfo
	}
	return h.cfg.RequestLog.ClientInfo()
}

// writeJSON 写 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeOpenAIError 写 OpenAI 形状的错误。
func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    openAIErrType(status),
			"code":    code,
			"param":   nil,
		},
	})
}

func openAIErrType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// sortedModelIDs 返回排序后的可见模型 id（测试与面板用）。
func (h *Handler) sortedModelIDs() []string {
	out := make([]string, 0, 16)
	for _, mi := range h.cfg.Upstream.Catalog() {
		if (!mi.Visible && h.cfg.ExcludeUpstreamHidden) || h.hidden(mi.ID) {
			continue
		}
		out = append(out, mi.ID)
	}
	sort.Strings(out)
	return out
}
