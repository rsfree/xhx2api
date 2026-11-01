// Package upstream 封装小浣熊（xiaohuanxiong.com）Web/桌面端所依赖的网关。
//
// 协议事实（抓包 + 实测，2026-10）：
//
//	对话    POST {llm}/chat/completions        OpenAI 形状；SSE 是标准 data: 帧
//	目录    GET  {llm}/model_catalog          data.categories[].models[]
//	续期    POST {auth}/refresh               {"refresh_token":…} → 新 access/refresh
//	用户    GET  {auth}/user_info             data.id / name
//	额度    GET  {points}/balance             data.available_points + 四个钱包桶
//	流水    GET  {points}/bills               逐笔发放/消费（含 expire_at）
//	桌面签到 POST {origin}/api/web/desktop/v1/login/points/grant  幂等
//	手机签到 POST {origin}/api/web/mobile/v1/login/points/grant   独立一笔
//	换码    POST {auth}/login_with_authorization_code
//	扫码    POST {auth}/login_with_qrcode_code  轮询 status=pending/logging/success
//
// 网关只做四件事：选号与续期、把响应帧里的 model 改回客户端请求的名字、
// 把「请求本身被拒」与「账号故障」分开、把 chat 的 SSE 翻译成 Anthropic /
// Responses 的线上形状。出站 chat 请求体除 model 外原样透传。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
)

// 上游地址常量（可被 config.upstream 覆盖）。
const (
	LLMBase    = "https://xiaohuanxiong.com/api/web/llm/v2"
	AuthBase   = "https://xiaohuanxiong.com/api/web/auth/v1"
	PointsBase = "https://xiaohuanxiong.com/api/web/points/v1"
	Origin     = "https://xiaohuanxiong.com"

	ChatPath        = "/chat/completions"
	CatalogPath     = "/model_catalog"
	RefreshPath     = "/refresh"
	LoginCodePath   = "/login_with_authorization_code"
	QrCodePath      = "/login_with_qrcode_code"
	BalancePath     = "/balance"
	BillsPath       = "/bills"
	UserInfoPath    = "/user_info"
	MobileGrantPath = "/login/points/grant"
	DesktopGrantURL = Origin + "/api/web/desktop/v1/login/points/grant"

	// DefaultModel 是目录拉不到时的兜底模型名（v1.1.0 客户端 model_catalog
	// 的 default_model 实测值）。raccoon-chat-ml-5-5 虽不在目录里，上游仍接受，
	// 客户端写死 gpt-4o 一类名字时由 server 层的别名表映射到它。
	DefaultModel = "raccoon-8c4485"

	// AliasModel 是「客户端写了通用模型名」时的落点。
	AliasModel = "raccoon-chat-ml-5-5"

	// DefaultClientVersion 是出站 x-client-version 的内置值（v1.1.0 抓包）。
	DefaultClientVersion = "v1.1.0"

	// BillsQuery 是流水查询串。limit 必须给足：流水按时间倒序，重度使用一天几十条
	// 消费记录会把今日 +300 的 daily_grant 挤出前 10 条，签到状态就会误判。
	BillsQuery = "paging.limit=100&type=all&paging.offset=0"
)

// 业务码。
const (
	// CodeOK 是上游成功码。
	CodeOK = 0
	// CodeAuthExpired 是授权码/登录态失效的业务码（实测 200035）。
	CodeAuthExpired = 200035
)

// ErrKind 错误分类，pool 据此决定冷却时长。
//
// 分类的粒度决定了「会不会误伤好号」：把参数错误记成额度不足，会让一个还能用的
// 账号白关 12 小时；把限流记成登录失效，连败几次就自动禁用。所以每类判据都
// 收窄到「文案确实在说这件事」。
type ErrKind int

const (
	ErrNone          ErrKind = iota // 与账号无关（模型名/参数/业务码）
	ErrPlanLimit                    // 额度不足 → 硬冷却（次日或长时）
	ErrSoftRate                     // 限流 → 软冷却（指数退避）
	ErrSessionDead                  // 登录态失效 → 连续达阈值才禁用
	ErrNotFound                     // 404 → 短冷却
	ErrServer                       // 5xx → 熔断计数
	ErrClient                       // 其余 4xx → 不罚号
	ErrBusy                         // 上游排队 → 换号不罚号
	ErrReasoningLoop                // 思考链复读 → 中断且不罚号
	ErrGateway                      // 502/504 → 不罚号不换号
)

// String 供日志与面板展示。
func (k ErrKind) String() string {
	switch k {
	case ErrPlanLimit:
		return "plan_limit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	case ErrBusy:
		return "busy"
	case ErrReasoningLoop:
		return "reasoning_loop"
	case ErrGateway:
		return "gateway"
	default:
		return "none"
	}
}

// Minor 报告这类错误是否「轻」：换号重试有意义，但不该惩罚账号。
func (k ErrKind) Minor() bool {
	switch k {
	case ErrBusy, ErrReasoningLoop, ErrGateway, ErrClient, ErrNone:
		return true
	}
	return false
}

// Error 是带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
	// Busy 表示上游排队：可以原地重试，不是账号问题。
	Busy bool
	// RetryAfterSecs 是上游建议的重试间隔（排队态才有）。
	RetryAfterSecs int64
	// ServiceUnavailable = 排队体里 serviceAvailable:false：等也没用，立刻回错。
	ServiceUnavailable bool
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return e.Msg
	}
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// BusinessError 是 HTTP 成功但上游业务拒绝（code != 0）。
type BusinessError struct {
	Code    int
	Message string
}

func (e *BusinessError) Error() string { return e.Message }

// 若干哨兵错误：调用方按「可重试 / 不可重试 / 已结算」区分。
var (
	// ErrEmptyStream 上游一个字都没吐：可换号重试，但不罚号。
	ErrEmptyStream = errors.New("上游返回空流")
	// ErrCodeExpired 授权码已失效：提示重新发起，不要重试。
	ErrCodeExpired = errors.New("授权码已失效，请重新发起登录")
	// ErrQrCanceled 扫码被取消或二维码过期。
	ErrQrCanceled = errors.New("二维码已失效或已取消，请刷新后重新扫码")
	// ErrNoAccount 池里没有可用账号。
	ErrNoAccount = errors.New("没有可用账号")
)

// ---------------------------------------------------------------------------
// 错误分类判据

// quotaSignal 文案里是否**确实**在说额度/余额不足。
//
// 不能只匹配 "insufficient"：上下文超限的报文里也有这个词，会把好号判成欠费。
func quotaSignal(lower string) bool {
	if strings.Contains(lower, "14018") || strings.Contains(lower, "credits exhausted") ||
		strings.Contains(lower, "point_exhausted") {
		return true
	}
	if strings.Contains(lower, "insufficient") {
		for _, w := range []string{"point", "credit", "balance", "quota", "余额", "积分", "额度"} {
			if strings.Contains(lower, w) {
				return true
			}
		}
	}
	for _, pair := range [][2]string{
		{"积分", "不足"}, {"积分", "耗尽"}, {"额度", "不足"}, {"额度", "耗尽"},
		{"余额", "不足"}, {"余额", "耗尽"}, {"quota", "exceeded"}, {"quota", "exhausted"},
	} {
		if strings.Contains(lower, pair[0]) && strings.Contains(lower, pair[1]) {
			return true
		}
	}
	return false
}

// sessionDeadSignal 文案里是否**确实**在说登录态失效（不含泛化的 "token"）。
func sessionDeadSignal(lower string) bool {
	for _, s := range []string{
		"unauthorized", "invalid token", "token expired", "token is expired",
		"token_expired", "authorization_verify_error", "invalid_grant",
		"登录态已失效", "凭证已失效", "请重新登录",
	} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// paramErrorSignal 文案里是否在说「请求体里的参数不被接受」。
//
// 上游对未知字段是严格校验的（真号实测 raccoon-8c4485：body 多一个 title 就
// HTTP 500 "Unexpected keyword argument 'title'"）。这类 5xx 是请求本身的问题，
// 换个号也会同样失败——归 client 只换号不罚号，否则一次参数错误就把好号熔断。
func paramErrorSignal(lower string) bool {
	for _, s := range []string{
		"unexpected keyword argument", "unexpected keyword", "invalid parameter",
		"unsupported parameter", "extra fields not permitted", "invalid request body",
	} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// Classify 按 HTTP 状态与正文判定错误类别。
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	// 参数类错误必须先判：上游对未知字段严格校验，常常用 5xx 返回
	// （实测 "Unexpected keyword argument 'title'" → HTTP 500）。先判 5xx
	// 就会把它记成 server，一次参数错误就把好号熔断。
	if status >= 400 && paramErrorSignal(lower) {
		return ErrClient
	}
	switch {
	// 认证类先判：403 也可能只是权限问题，落到下面的额度文案会白白封号。
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ErrSessionDead
	case status == http.StatusTooManyRequests:
		// 429 默认是限流，但文案明确在说额度/积分耗尽时按欠费处理：
		// 两者的冷却语义差一个数量级（限流 60s 退避 vs 欠费 12h），判错会让
		// 一个已经打空的号每 60 秒被重新捞出来撞墙。
		if quotaSignal(lower) {
			return ErrPlanLimit
		}
		return ErrSoftRate
	case status == http.StatusGatewayTimeout, status == http.StatusBadGateway:
		return ErrGateway
	case status == http.StatusRequestTimeout || status == 499:
		// 499（nginx 的 client closed request）/408：调用方提前断开，多为请求体过大
		// 或客户端超时。换号重发只会照样失败，还把每个号都记一次错。
		return ErrClient
	case status >= 500:
		return ErrServer
	case status == http.StatusNotFound:
		return ErrNotFound
	}
	if queueUnavailable(body) {
		return ErrBusy
	}
	if quotaSignal(lower) || status == http.StatusPaymentRequired {
		return ErrPlanLimit
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

var (
	retryAfterRE   = regexp.MustCompile(`"retryAfterSeconds"\s*:\s*(\d+)`)
	waitTimeRE     = regexp.MustCompile(`"waitTime"\s*:\s*(\d+)`)
	serviceAvailRE = regexp.MustCompile(`"serviceAvailable"\s*:\s*false`)
)

// queueUnavailable 看排队体里的 serviceAvailable:false——上游自己说此刻不可服务。
func queueUnavailable(raw string) bool {
	return serviceAvailRE.MatchString(strings.ReplaceAll(raw, `\`, ""))
}

// retryAfterOf 抓上游建议的重试间隔（秒）。
func retryAfterOf(raw string) int64 {
	flat := strings.ReplaceAll(raw, `\`, "")
	for _, re := range []*regexp.Regexp{retryAfterRE, waitTimeRE} {
		if m := re.FindStringSubmatch(flat); len(m) == 2 {
			if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// Client

// Client 访问小浣熊上游。四个基址都可由配置/测试覆盖。
type Client struct {
	// HTTP 用于短 JSON 请求（续期/模型/签到/额度），有总超时兜底。
	HTTP *http.Client
	// StreamHTTP 用于 SSE 流式对话：不设总超时，只用 ResponseHeaderTimeout
	// 兜住「上游一直不返首字节」的悬挂。
	StreamHTTP *http.Client
	// ImageHTTP 用于生图：独立 transport（无响应头超时），耗时由 ctx 控制。
	// 实测生图 20-200s，共用 120s 的 ResponseHeaderTimeout 会卡在边上。
	ImageHTTP *http.Client
	imageOnce sync.Once

	timeoutMu     sync.RWMutex
	idleTimeout   time.Duration
	headerTimeout time.Duration

	hostMu sync.RWMutex
	llm    string
	auth   string
	points string
	origin string

	identMu       sync.RWMutex
	userAgent     string
	clientVersion string

	catMu       sync.RWMutex
	catalog     []ModelInfo
	catAt       time.Time
	catalogFile string // 目录持久化缓存路径（空 = 不落盘）

	// 续期单飞：同一份 refresh_token 并发只打一次上游。上游每次续期都轮换，
	// 两个并发请求带着同一张旧票去换，后到的那次会把先到的票顶掉。
	flightMu sync.Mutex
	flights  map[string]*refreshFlight

	// persist 是续期成功后的落盘钩子。新票只在内存里 = 重启后账号掉线，
	// 所以这一步不能省；钩子失败只记日志（内存里已生效），由调用方决定是否重试。
	persistMu sync.RWMutex
	persist   func(*auth.Auth) error

	// 出站代理（egress）：nil = 直连。
	transportMu sync.RWMutex
	dialer      func(ctx context.Context, network, addr string) (net.Conn, error)
}

type refreshFlight struct {
	done  chan struct{}
	token string
	exp   int64
	uid   string
	err   error
}

// New 生产默认值：连接池共享，短请求 120s 兜底、长流只看首字节超时。
func New() *Client {
	tr := newTransport(120 * time.Second)
	return &Client{
		HTTP:          &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP:    &http.Client{Transport: tr},
		headerTimeout: 120 * time.Second,
		llm:           LLMBase,
		auth:          AuthBase,
		points:        PointsBase,
		origin:        Origin,
		flights:       map[string]*refreshFlight{},
	}
}

// newTransport 建连接的公共参数。
//
// DisableCompression：SSE 走代理时有些中间层会把 gzip 缓冲到流结束才吐，
// 表现为「客户端一直等、上游早就答完」。我们自己带 Accept-Encoding: identity，
// 并关掉 Transport 的自动解压，避免二次压缩。
func newTransport(headerTimeout time.Duration) *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   40,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		DisableCompression:    true,
		ForceAttemptHTTP2:     true,
	}
}

// SetTimeouts 热改超时：short 是短请求总超时，header 是首字节超时，idle 是流内空闲上限。
func (c *Client) SetTimeouts(short, header, idle time.Duration) {
	c.timeoutMu.Lock()
	defer c.timeoutMu.Unlock()
	if short > 0 && c.HTTP != nil {
		c.HTTP.Timeout = short
	}
	if header > 0 {
		for _, hc := range []*http.Client{c.HTTP, c.StreamHTTP} {
			if hc == nil {
				continue
			}
			if tr, ok := hc.Transport.(*http.Transport); ok {
				tr.ResponseHeaderTimeout = header
			}
		}
	}
	c.headerTimeout = header
	c.idleTimeout = idle
}

// IdleTimeout 返回流内空闲上限（0 = 不限制）。
func (c *Client) IdleTimeout() time.Duration {
	c.timeoutMu.RLock()
	defer c.timeoutMu.RUnlock()
	return c.idleTimeout
}

// HeaderTimeout 返回等响应头的上限（0 = 未设置）。
func (c *Client) HeaderTimeout() time.Duration {
	c.timeoutMu.RLock()
	defer c.timeoutMu.RUnlock()
	return c.headerTimeout
}

// SetIdentity 覆盖出站 User-Agent 与客户端版本头（空 = 用内置默认）。
func (c *Client) SetIdentity(userAgent, clientVersion string) {
	c.identMu.Lock()
	defer c.identMu.Unlock()
	c.userAgent = strings.TrimSpace(userAgent)
	v := strings.TrimSpace(clientVersion)
	if v != "" && !strings.HasPrefix(v, "v") {
		// 配置里写 1.1.0 和写 v1.1.0 都行，头里统一带 v（照客户端实测）。
		v = "v" + v
	}
	c.clientVersion = v
}

// currentVersion 返回 x-client-version 头取值。
func (c *Client) currentVersion() string {
	c.identMu.RLock()
	defer c.identMu.RUnlock()
	if c.clientVersion != "" {
		return c.clientVersion
	}
	return DefaultClientVersion
}

// currentUA 返回出站 User-Agent。
func (c *Client) currentUA() string {
	c.identMu.RLock()
	defer c.identMu.RUnlock()
	if c.userAgent != "" {
		return c.userAgent
	}
	return "xhx2api/1.0"
}

// SetAgentHosts 覆盖推理网关与鉴权基址。
func (c *Client) SetAgentHosts(llm, authBase string) {
	c.hostMu.Lock()
	defer c.hostMu.Unlock()
	c.llm = orDefault(strings.TrimRight(strings.TrimSpace(llm), "/"), LLMBase)
	c.auth = orDefault(strings.TrimRight(strings.TrimSpace(authBase), "/"), AuthBase)
}

// SetAPIHosts 覆盖积分与主站基址。
func (c *Client) SetAPIHosts(points, origin string) {
	c.hostMu.Lock()
	defer c.hostMu.Unlock()
	c.points = orDefault(strings.TrimRight(strings.TrimSpace(points), "/"), PointsBase)
	c.origin = orDefault(strings.TrimRight(strings.TrimSpace(origin), "/"), Origin)
}

// SetPersistHook 注入续期后的落盘动作。
func (c *Client) SetPersistHook(fn func(*auth.Auth) error) {
	c.persistMu.Lock()
	c.persist = fn
	c.persistMu.Unlock()
}

// persistAuth 调用落盘钩子（未配置时直接调 auth 自带实现）。
func (c *Client) persistAuth(a *auth.Auth) error {
	c.persistMu.RLock()
	fn := c.persist
	c.persistMu.RUnlock()
	if fn != nil {
		return fn(a)
	}
	return a.SaveAtomic()
}

// SetDialer 注入自定义拨号（出口隔离/代理）。
func (c *Client) SetDialer(fn func(ctx context.Context, network, addr string) (net.Conn, error)) {
	c.transportMu.Lock()
	defer c.transportMu.Unlock()
	c.dialer = fn
	for _, hc := range []*http.Client{c.HTTP, c.StreamHTTP} {
		if hc == nil {
			continue
		}
		tr, ok := hc.Transport.(*http.Transport)
		if !ok {
			continue
		}
		if fn == nil {
			tr.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		} else {
			tr.DialContext = fn
		}
		tr.CloseIdleConnections()
	}
}

// AgentHosts 返回当前网关基址（面板展示「生效地址」用）。
func (c *Client) AgentHosts() (llm, authBase string) {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.llm, c.auth
}

// Origins 返回积分与主站基址。
func (c *Client) Origins() (points, origin string) {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.points, c.origin
}

func (c *Client) llmBase() string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.llm
}

func (c *Client) authBase() string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.auth
}

func (c *Client) pointsBase() string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.points
}

func (c *Client) originBase() string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.origin
}

// desktopBase 桌面端专属域（登录领取积分等）。
func (c *Client) desktopBase() string { return c.originBase() + "/api/web/desktop/v1" }

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) streamClient() *http.Client {
	if c.StreamHTTP != nil {
		return c.StreamHTTP
	}
	return c.httpClient()
}

// apiResponse 是一次上游响应的原始形状。
type apiResponse struct {
	Status int
	Body   []byte
}

// do 发一次请求并读回响应体（上限 4MiB）。
func (c *Client) do(req *http.Request) (*apiResponse, error) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.currentUA())
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "无法连接小浣熊，请检查网络或代理设置"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "读取小浣熊响应失败"}
	}
	return &apiResponse{Status: resp.StatusCode, Body: raw}, nil
}

// doJSON 发请求：body 非 nil 时按 JSON 序列化，headers 原样写入。
func (c *Client) doJSON(ctx context.Context, method, url string, body any, headers map[string]string) (*apiResponse, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return c.do(req)
}

// payloadOf 校验 2xx 并拆掉 {code,message,data} 信封，返回 data 节点。
//
// 上游把「业务失败」也放在 HTTP 200 里（code != 0），只看状态码会把
// 限流/欠费当成功，账号也就没法换——所以这里两层都判。
func payloadOf(resp *apiResponse, action string) (map[string]any, error) {
	if resp.Status < 200 || resp.Status >= 300 {
		hint := ""
		switch resp.Status {
		case 401, 403:
			hint = "，登录态可能已失效"
		case 429:
			hint = "，请稍后重试"
		}
		return nil, &Error{
			Kind:   Classify(resp.Status, string(resp.Body)),
			Status: resp.Status,
			Msg:    fmt.Sprintf("小浣熊 %s失败（HTTP %d）%s: %s", action, resp.Status, hint, truncate(errorText(resp.Body), 200)),
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(resp.Body))
	decoder.UseNumber()
	var envelope map[string]any
	if err := decoder.Decode(&envelope); err != nil {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: fmt.Sprintf("小浣熊 %s未返回有效 JSON 对象", action)}
	}
	if code, ok := intOf(envelope["code"]); ok && code != 0 {
		return nil, &BusinessError{
			Code:    code,
			Message: orDefault(text(envelope, "message", "msg"), fmt.Sprintf("小浣熊 %s被拒绝（code=%d）", action, code)),
		}
	}
	data, _ := envelope["data"].(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	return data, nil
}

// errorText 从错误体里取人类可读文案。
func errorText(body []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if decoder.Decode(&payload) != nil {
		return strings.TrimSpace(string(body))
	}
	if msg := text(payload, "message", "msg", "detail", "error_description"); msg != "" {
		return msg
	}
	if inner, ok := payload["error"].(map[string]any); ok {
		if msg := text(inner, "message", "detail"); msg != "" {
			return msg
		}
	}
	return strings.TrimSpace(string(body))
}

// ---------------------------------------------------------------------------
// 续期

// RefreshToken 无条件续期一次。
func (c *Client) RefreshToken(ctx context.Context, a *auth.Auth) error {
	_, err := c.refresh(ctx, a)
	return err
}

// RefreshTokenIfNeeded 仅在 skew 内将过期时续期；返回是否真的续了。
func (c *Client) RefreshTokenIfNeeded(ctx context.Context, a *auth.Auth, skew time.Duration) (bool, error) {
	if !a.NeedsRefresh(skew) {
		return false, nil
	}
	return c.refresh(ctx, a)
}

// refresh 单飞续期。
func (c *Client) refresh(ctx context.Context, a *auth.Auth) (bool, error) {
	refreshToken := a.RefreshTokenValue()
	if refreshToken == "" {
		return false, &Error{Kind: ErrClient, Status: 400, Msg: "该账号没有 refresh_token，无法续期，请重新登录"}
	}
	key := a.UID + "|" + refreshToken
	c.flightMu.Lock()
	if c.flights == nil {
		c.flights = map[string]*refreshFlight{}
	}
	if flight, ok := c.flights[key]; ok {
		c.flightMu.Unlock()
		select {
		case <-flight.done:
		case <-ctx.Done():
			return false, ctx.Err()
		}
		if flight.err != nil {
			return false, flight.err
		}
		a.SetTokens(flight.token, "", flight.exp)
		applyRefreshedUID(a, flight.uid)
		return true, nil
	}
	flight := &refreshFlight{done: make(chan struct{})}
	c.flights[key] = flight
	c.flightMu.Unlock()

	token, refresh, exp, uid, err := c.refreshOnce(ctx, a)
	flight.token, flight.exp, flight.uid, flight.err = token, exp, uid, err
	close(flight.done)
	c.flightMu.Lock()
	delete(c.flights, key)
	c.flightMu.Unlock()
	if err != nil {
		return false, err
	}
	a.SetTokens(token, refresh, exp)
	applyRefreshedUID(a, uid)
	// 落盘失败不算续期失败：新票已在内存里，当前进程可用；但下一次重启就会掉线，
	// 所以错误要如实返回给调用方记日志（由 handler/调度器打 WARN）。
	if perr := c.persistAuth(a); perr != nil {
		return true, fmt.Errorf("续期成功但落盘失败（重启后需重新登录）: %w", perr)
	}
	return true, nil
}

// applyRefreshedUID 用续期响应补 uid：access token 未必是 JWT（也可能只是不透明串），
// uid 只有上游自己知道，缺了就没法落盘成账号。
func applyRefreshedUID(a *auth.Auth, uid string) {
	if uid == "" || a.UID != "" {
		return
	}
	a.Apply(func(d *auth.Data) {
		if d.UID == "" {
			d.UID = uid
		}
		if d.Nickname == "" {
			d.Nickname = uid
		}
	})
	// UID 变了要跟着改文件名，否则重启后按新 uid 找不到旧文件、按旧文件名
	// 读出来的账号又没有 uid，池里会出现两条。
	if a.FilePath != "" {
		_ = a.SaveAtomic()
	}
}

// refreshOnce 真的打一次续期接口并返回新票（不写回账号）。
func (c *Client) refreshOnce(ctx context.Context, a *auth.Auth) (token, refresh string, exp int64, uid string, err error) {
	resp, err := c.doJSON(ctx, http.MethodPost, c.authBase()+RefreshPath,
		map[string]string{"refresh_token": a.RefreshTokenValue()}, c.clientIDHeaders())
	if err != nil {
		return "", "", 0, "", err
	}
	if resp.Status == 401 || resp.Status == 403 {
		return "", "", 0, "", &Error{Kind: ErrSessionDead, Status: 401, Msg: "小浣熊刷新令牌已失效，请重新登录该账号"}
	}
	data, err := payloadOf(resp, "凭证续期")
	if err != nil {
		return "", "", 0, "", err
	}
	token = text(data, "access_token", "accessToken", "token")
	if token == "" {
		return "", "", 0, "", &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊续期响应缺少令牌，旧凭证未被覆盖"}
	}
	uid = text(data, "user_id", "uid", "userId")
	if uid != "" && a.UID != "" && uid != a.UID {
		return "", "", 0, "", &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊续期返回了不同账号，旧凭证未被覆盖"}
	}
	// 上游可能不轮换 refresh_token：没有新值就沿用旧的（SetTokens 收到空串即不动）。
	refresh = text(data, "refresh_token", "refreshToken")
	for _, key := range []string{"expires_at", "expiresAt", "expire_time", "expireTime"} {
		if ms := millisOf(data[key]); ms > 0 {
			exp = ms
			break
		}
	}
	return token, refresh, exp, uid, nil
}

// GetUserInfo 用上游 GET {auth_base}/user_info 补齐 uid/昵称。
//
// 接口不可用时退回 JWT claims——这条路径保证「哪怕 user_info 被上游下线，
// 导入 token 仍然能入池」，只是昵称可能缺失。
func (c *Client) GetUserInfo(ctx context.Context, a *auth.Auth) (uid, nickname, org string, err error) {
	token := a.JWT()
	if token != "" {
		// 取一次资料；拿不到时若报的是「IAM 票未确认」，补一次确认再取。
		// 少了这一步，未确认的票会静默回落到 JWT 里的 pu-… 主体（uid 看着有值、
		// 实际是 IAM 主体），账号入池后每个请求都是 400 iam_confirm_required。
		fetch := func() (bool, error) {
			resp, uerr := c.doJSON(ctx, http.MethodGet, c.authBase()+UserInfoPath, nil,
				map[string]string{"Accept": "application/json", "Authorization": "Bearer " + token})
			if uerr != nil {
				return false, uerr
			}
			data, perr := payloadOf(resp, "用户资料")
			if perr != nil {
				return false, perr
			}
			uid = text(data, "id", "user_id", "uid")
			nickname = text(data, "name", "nickname")
			org = text(data, "office_org_name", "org_code")
			return uid != "", nil
		}
		ok, ferr := fetch()
		if !ok && isIAMConfirmRequired(ferr) && c.ConfirmIAM(ctx, token) == nil {
			ok, _ = fetch()
		}
		if ok {
			return uid, nickname, org, nil
		}
	}
	uid = auth.JWTUserID(token)
	if uid == "" && a.UID != "" {
		return a.UID, a.Snapshot().Nickname, "", nil
	}
	if uid == "" {
		return "", "", "", &Error{Kind: ErrSessionDead, Status: 401, Msg: "无法从 token 解析用户标识，请重新登录"}
	}
	return uid, a.Snapshot().Nickname, "", nil
}

// ---------------------------------------------------------------------------
// 推理

// ChatStream 把 OpenAI 请求体发给小浣熊的 chat/completions。
//
// 上游就是 OpenAI 形状：body 原样转发（model 已由 handler 定成规范 id），
// 返回未读的 SSE 响应体供 Stream/Aggregate 消费；非 2xx 时返回分类后的 *Error。
//
// turnTitle 非空时作为 X-RACCOON-Title 发出去。这不是装饰：上游会把标题写进
// 积分流水的 event_name（「任务消耗 · <title>」），于是每一笔扣费都能反查到是
// 哪一次请求——积分对账完全靠它。
func (c *Client) ChatStream(ctx context.Context, a *auth.Auth, body []byte, turnTitle string) (io.ReadCloser, int, []byte, error) {
	url := c.llmBase() + ChatPath
	// 派生一个可取消的 ctx：流内空闲看门狗靠它把卡死的流掐断。
	reqCtx, cancel := context.WithCancel(ctx)
	handedOff := false
	defer func() {
		// 没把流交出去（早退/报错）时立刻回收，避免计时器和连接悬着。
		if !handedOff {
			cancel()
		}
	}()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 500, nil, &Error{Kind: ErrServer, Status: 500, Msg: "构造小浣熊上游请求失败"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Accept-Encoding", "identity")
	for key, value := range c.clientIDHeaders() {
		req.Header.Set(key, value)
	}
	req.Header.Set("User-Agent", c.currentUA())
	if title := strings.TrimSpace(turnTitle); title != "" {
		req.Header.Set("X-RACCOON-Title", title)
	}
	if token := a.JWT(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.streamClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 499, nil, &Error{Kind: ErrClient, Status: 499, Msg: "客户端已断开"}
		}
		// 等响应头超时**不是**连不上：以前这里一律记成 server，handler 就按
		// 「值得换号」处理，一个慢上游被 3 个账号各等 120s 才收场（线上实测
		// 单请求 360s），还给这 3 个无辜账号各记一次失败、可能触发熔断。
		// 超时换号没有意义（换个号一样慢），所以归到 gateway：快速失败、不罚号。
		if isTimeoutErr(err) {
			return nil, 504, nil, &Error{Kind: ErrGateway, Status: 504, Msg: c.timeoutMessage()}
		}
		return nil, 502, nil, &Error{Kind: ErrServer, Status: 502, Msg: "无法连接小浣熊，请检查网络或代理设置"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		msg := truncate(errorText(raw), 300)
		if msg == "" {
			msg = fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode)
		}
		return nil, resp.StatusCode, raw, &Error{
			Kind: kind, Status: resp.StatusCode, Msg: msg,
			RetryAfterSecs: retryAfterOf(string(raw)), ServiceUnavailable: queueUnavailable(string(raw)),
		}
	}

	// stream:false 时上游回的是普通 JSON（不是 SSE）：包成一帧 SSE 交给同一套
	// 管道，下游就只用认一种线上形状。
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, 502, nil, &Error{Kind: ErrServer, Status: 502, Msg: "读取上游响应失败"}
		}
		if event := parseSSELine(string(raw)); event.Kind == sseError {
			return nil, event.Status, raw, &Error{
				Kind: event.Class, Status: event.Status, Msg: orDefault(event.Message, event.Raw),
				RetryAfterSecs: event.RetryAfterSecs, ServiceUnavailable: event.ServiceUnavailable,
			}
		}
		frame := deltaFrameOf(raw)
		stream := "data: " + string(frame) + "\n\ndata: [DONE]\n\n"
		return io.NopCloser(strings.NewReader(stream)), resp.StatusCode, nil, nil
	}

	// HTTP 200 也可能是错误：上游会用一帧 {"error":{...}} 表达限流/额度。
	// 只看状态码会把它当成功，账号也就没法换——所以先窥一帧再交出去。
	br := newPeekReader(newIdleBody(resp.Body, c.IdleTimeout(), cancel))
	first, err := br.peekFrame()
	if err != nil {
		_ = resp.Body.Close()
		var ue *Error
		if errors.As(err, &ue) {
			return nil, ue.Status, nil, ue
		}
		return nil, 502, nil, &Error{Kind: ErrServer, Status: 502, Msg: "读取上游首帧失败"}
	}
	if first.Kind == sseError {
		_ = resp.Body.Close()
		return nil, first.Status, []byte(first.Raw), &Error{
			Kind: first.Class, Status: first.Status, Msg: orDefault(first.Message, first.Raw),
			RetryAfterSecs: first.RetryAfterSecs, ServiceUnavailable: first.ServiceUnavailable,
		}
	}
	handedOff = true
	return br.stream(), resp.StatusCode, nil, nil
}

// isTimeoutErr 判断传输层错误是「等超时」还是「连不上」。
//
// net/http 的 ResponseHeaderTimeout / Client.Timeout / 读超时都实现 Timeout() bool；
// 连接被拒、DNS 失败、TLS 握手失败则不然。两者的处理恰好相反。
func isTimeoutErr(err error) bool {
	var t interface{ Timeout() bool }
	if errors.As(err, &t) && t.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// timeoutMessage 说明等的是哪个窗口，避免把「上游慢」读成「网络不通」。
func (c *Client) timeoutMessage() string {
	if d := c.HeaderTimeout(); d > 0 {
		return fmt.Sprintf("上游响应超时（等响应头超过 %s），请稍后重试", d)
	}
	return "上游响应超时，请稍后重试"
}

// idleBody 给流式响应加「流内空闲上限」。
//
// http.Client 没有「两次 Read 之间最多等多久」这种能力：ResponseHeaderTimeout
// 只管响应头，服务端 ReadTimeout 只管读请求体。于是一条已经回了 200、但上游
// 卡住不吐字节的流会一直挂着——2026-10-31 线上实测出现过 877s 才结束的请求，
// 期间它一直占着连接和一个在途名额。
//
// 这里在每次读到字节后重置计时，超时就 cancel 掉这次上游请求：读端立刻拿到
// 错误，在途名额随之释放。错误必须包成 *Error，分类器才能判成 gateway
// （超时是上游的事，不该罚账号）。
type idleBody struct {
	rc     io.ReadCloser
	idle   time.Duration
	cancel context.CancelFunc
	timer  *time.Timer

	mu    sync.Mutex
	fired bool
}

func newIdleBody(rc io.ReadCloser, idle time.Duration, cancel context.CancelFunc) io.ReadCloser {
	b := &idleBody{rc: rc, idle: idle, cancel: cancel}
	if idle > 0 {
		b.timer = time.AfterFunc(idle, func() {
			b.mu.Lock()
			b.fired = true
			b.mu.Unlock()
			cancel()
		})
	}
	return b
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 && b.timer != nil {
		b.timer.Reset(b.idle)
	}
	if err != nil {
		b.mu.Lock()
		fired := b.fired
		b.mu.Unlock()
		if fired {
			return n, &Error{Kind: ErrGateway, Status: 504,
				Msg: fmt.Sprintf("上游流空闲超过 %s 没有新内容，已断开", b.idle)}
		}
	}
	return n, err
}

func (b *idleBody) Close() error {
	if b.timer != nil {
		b.timer.Stop()
	}
	b.cancel()
	return b.rc.Close()
}

// PrepareBody 是入站体的轻量规整钩子：小浣熊原样透传，这里不改任何字段。
//
// 保留成函数而不是直接透传，是为了给「上游升级后要求某个字段必须存在」留
// 一个统一落点，不必在每个 handler 里各插一段。
func PrepareBody(src []byte) []byte { return src }

// clientIDHeaders 复刻 v1.1.0 桌面客户端的识别头（抓包实测）。
// 上游网关按客户端渠道做风控，缺了这些头虽能过，但带上最贴近真实流量。
func (c *Client) clientIDHeaders() map[string]string {
	return map[string]string{
		"x-client-name":     "@raccoon/office-desktop",
		"x-client-platform": "desktop-windows-x64",
		"x-client-version":  c.currentVersion(),
		"x-client-channel":  "official",
	}
}

// authHeader 是带登录态的通用请求头。
func (c *Client) authHeader(a *auth.Auth) map[string]string {
	h := c.clientIDHeaders()
	h["Accept"] = "application/json"
	if a != nil {
		h["Authorization"] = "Bearer " + a.JWT()
	}
	return h
}

// millisOf 把数字/数字字符串归一成毫秒时间戳（0 = 没有）。
func millisOf(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return normalizeMillis(int64(typed))
	case int64:
		return normalizeMillis(typed)
	case json.Number:
		if n, err := typed.Int64(); err == nil {
			return normalizeMillis(n)
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return normalizeMillis(n)
		}
		if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
			return parsed.UnixMilli()
		}
	}
	return 0
}

func normalizeMillis(value int64) int64 {
	if value <= 0 {
		return 0
	}
	if value < 100_000_000_000 {
		return value * 1000
	}
	return value
}

// truncate 截断到 n 个 rune（不切坏多字节字符）。
func truncate(value string, n int) string {
	runes := []rune(value)
	if len(runes) <= n {
		return value
	}
	return string(runes[:n])
}

// text 取第一个非空的字符串/数字字段。
func text(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		switch value := payload[key].(type) {
		case string:
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		case json.Number:
			return value.String()
		case float64:
			return strconv.FormatFloat(value, 'f', -1, 64)
		case int64:
			return strconv.FormatInt(value, 10)
		}
	}
	return ""
}

// intOf 把 JSON 数字/数字字符串归一成 int。
func intOf(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case json.Number:
		if n, err := typed.Int64(); err == nil {
			return int(n), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return n, true
		}
	}
	return 0, false
}

// deltaFrameOf 把非流式 chat.completion 摊成同形状的一帧 chunk（message → delta）。
func deltaFrameOf(raw []byte) []byte {
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		return raw
	}
	choices, _ := frame["choices"].([]any)
	if len(choices) == 0 {
		return raw
	}
	if choice, ok := choices[0].(map[string]any); ok {
		if message, ok := choice["message"].(map[string]any); ok {
			choice["delta"] = message
			delete(choice, "message")
			if _, has := choice["index"]; !has {
				choice["index"] = 0
			}
		}
	}
	frame["object"] = "chat.completion.chunk"
	out, err := json.Marshal(frame)
	if err != nil {
		return raw
	}
	return out
}
