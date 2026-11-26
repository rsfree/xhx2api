// Package scheduler 跑账号的周期任务：每日签到、token 保活、余额刷新。
//
// 几条来自上游实测的结论，直接决定了这里的判定口径：
//
//  1. **每日 +300 由上游按天发放，但发放时点不固定**——2026-11-02 抓到的实际时刻
//     有 00:00 / 00:21 / 00:55 / 09:25 / 13:30 / 14:36，并不是「服务端 00:00 定时
//     任务」。也不存在「调接口就发分」的客户端开关：grant 接口恒回 granted:false，
//     调完 bills 里一条不涨（11 个号 × 5 种身份头组合全实测过），是纯幂等空操作。
//     所以「签到成功」只能以 bills 流水里当天有 daily_grant 为准，不看返回码。
//  2. **新号当天没有 daily_grant 是正常的**：发放要等次日首批。
//     报「签到失败」是谎报，要单独识别成「待发放」。
//  3. **有的号压根没有每日积分资格**，根因是上游没给它绑「可信设备」
//     （user_info.trusted_device.bound=false，reason=session_without_bound_device）。
//     2026-11-02 实测：11 个号里 9 个 unbound 的全部从来没过 daily_grant，
//     2 个 bound 的天天都有 300。这种号再怎么领也不会有，不该记成失败。
//     裸 token / 手机号导入的号都落在 unbound，得去官方客户端登录一次才可能绑上。
//  4. **保活只续期，不做任何业务动作**。上游对非官方客户端的识别有一部分来自
//     行为模式，保活越「像人」越好——只续 token 是最小足迹。
package scheduler

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
	"github.com/rsfree/xhx2api/internal/pool"
	"github.com/rsfree/xhx2api/internal/upstream"
)

// Config 是调度参数（面板可热改）。
type Config struct {
	CheckinHours     []int
	KeepaliveHours   []int
	CheckinEnabled   bool
	KeepaliveEnabled bool
	// BalanceRefreshEnabled 控制余额刷新循环。
	//
	// 单独一个开关而不是「把周期设 0」：0 在 normalize 里要兜底成默认 5
	// （配置缺省时就是 0），两个语义挤在一个字段上必然打架。
	BalanceRefreshEnabled bool
	// BalanceRefreshMinutes 余额刷新周期（分钟），<=0 时回落到 5。
	BalanceRefreshMinutes int
	// IncludeDisabled 让保号类任务对已禁用账号也执行。
	// 语义：面板「禁用」= 不参与选号，不等于「停止保号」——被禁用的号仍然需要
	// 签到与续期，否则解禁时额度已清零、token 已过期。
	IncludeDisabled bool
	// NewAccountGraceDays 新号静默期（天）：进池 N 天内的号只做保号动作。
	NewAccountGraceDays int
	// Concurrency 单轮任务并发账号数（默认 2，避免同一出口瞬间打多个账号）。
	Concurrency int
}

// Scheduler 是周期任务执行器。
type Scheduler struct {
	pool *pool.Pool
	up   *upstream.Client

	mu  sync.RWMutex
	cfg Config

	// lastRun 记录每个 job 最近一次执行的「日期+小时」键，避免同一小时重复跑。
	runMu   sync.Mutex
	lastRun map[string]string

	// mobileOnce 记录已尝试过手机端首登奖励的账号（上游幂等，进程内记忆即可）。
	mobileMu   sync.Mutex
	mobileDone map[string]bool

	// 最近一次执行结果（面板展示）。
	resultMu sync.RWMutex
	results  map[string]JobResult
}

// JobResult 是一次任务执行的汇总。
type JobResult struct {
	Job       string    `json:"job"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	OK        int       `json:"ok"`
	Failed    int       `json:"failed"`
	Skipped   int       `json:"skipped"`
	Details   []string  `json:"details,omitempty"`
}

// New 建调度器。
func New(p *pool.Pool, up *upstream.Client, cfg Config) *Scheduler {
	cfg = normalize(cfg)
	return &Scheduler{
		pool: p, up: up, cfg: cfg,
		lastRun:    map[string]string{},
		mobileDone: map[string]bool{},
		results:    map[string]JobResult{},
	}
}

func normalize(cfg Config) Config {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = allDayHours()
	}
	if cfg.BalanceRefreshMinutes <= 0 {
		cfg.BalanceRefreshMinutes = 5
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 2
	}
	return cfg
}

// Configure 热更新参数。
func (s *Scheduler) Configure(fn func(*Config)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.cfg)
	s.cfg = normalize(s.cfg)
}

// Config 返回当前参数。
func (s *Scheduler) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Run 起主循环（每分钟检查一次时点）。
func (s *Scheduler) Run(ctx context.Context) {
	s.runDueJobs(ctx, time.Now())
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.runDueJobs(ctx, now)
		}
	}
}

// StartBalanceRefresh 起独立的余额刷新循环（分钟级，不受「小时」约束）。
func (s *Scheduler) StartBalanceRefresh(ctx context.Context) {
	// 启动后先等 20 秒：让服务先开始接流量，避免启动瞬间打一堆上游请求。
	select {
	case <-ctx.Done():
		return
	case <-time.After(20 * time.Second):
	}
	s.RefreshBalances(ctx)
	for {
		cfg := s.Config()
		// 关闭（或周期非法）时不退出循环，而是挂起一分钟再看：
		// 面板改开关要能热生效，退出 goroutine 就再也起不来了。
		if !cfg.BalanceRefreshEnabled {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
				continue
			}
		}
		interval := time.Duration(cfg.BalanceRefreshMinutes) * time.Minute
		if interval <= 0 {
			interval = 5 * time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			s.RefreshBalances(ctx)
		}
	}
}

// allDayHours 返回 0-23 全部小时（保活的默认检查范围）。
func allDayHours() []int {
	hours := make([]int, 24)
	for i := range hours {
		hours[i] = i
	}
	return hours
}

// runDueJobs 检查当前时点该跑哪些任务。
func (s *Scheduler) runDueJobs(ctx context.Context, now time.Time) {
	cfg := s.Config()
	hourKey := now.In(shanghai()).Format("2006-01-02T15")
	hour := now.In(shanghai()).Hour()

	if cfg.CheckinEnabled && containsInt(cfg.CheckinHours, hour) && s.markIfNew("checkin", hourKey) {
		go s.checkinAll(ctx)
	}
	if cfg.KeepaliveEnabled && containsInt(cfg.KeepaliveHours, hour) && s.markIfNew("keepalive", hourKey) {
		go s.keepaliveAll(ctx)
	}
}

// markIfNew 同一次「小时键」只允许跑一次；返回 true 表示本次应当执行。
func (s *Scheduler) markIfNew(job, key string) bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.lastRun[job] == key {
		return false
	}
	s.lastRun[job] = key
	return true
}

// ---------------------------------------------------------------------------
// 每日签到

// CheckinOutcome 是单账号签到结果。
type CheckinOutcome struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Status   string `json:"status"` // ok / already / pending / unbound / ineligible / failed
	Credits  int64  `json:"credits"`
	Message  string `json:"message"`
	// DeviceBound 是上游可信设备绑定态：bound / unbound / ""（没取到）。
	DeviceBound string `json:"device_bound,omitempty"`
}

// CheckinAccount 对单个账号执行签到。
//
// 返回的 Status：
//
//	already    今天已有 daily_grant（最理想，不用做任何事）
//	ok         本次 claim 后确认到账
//	pending    上游今天还没发（新号 / 未到发放时点 / 设备状态没取到）
//	unbound    没绑可信设备 ⇒ 上游不会发每日积分，去官方客户端登录一次才可能绑上
//	ineligible 绑了设备却从未发过每日积分（极少见，保留兜底）
//	failed     真的失败了（网络 / 凭证问题）
func (s *Scheduler) CheckinAccount(ctx context.Context, a *auth.Auth) CheckinOutcome {
	out := CheckinOutcome{UID: a.UID, Nickname: a.Snapshot().Nickname}
	checked, credits, err := s.up.CheckinStatus(ctx, a)
	if err != nil {
		out.Status = "failed"
		out.Message = err.Error()
		return out
	}
	out.Credits = credits
	s.pool.NoteCredits(a.UID, credits)
	if checked {
		out.Status = "already"
		out.Message = "今日每日积分已到账"
		_ = a.SaveAtomic()
		return out
	}
	// 未到账：先补一次登录领取（幂等），再以流水复核。
	granted, claimErr := s.up.CheckinClaim(ctx, a)
	today, ever, billErr := s.up.DailyGrant(ctx, a)
	if billErr == nil && today {
		// 流水确认到账就记「今天已签」：granted=false 只说明本次领取是空操作，
		// 不影响「今日已发放」这个事实（漏记会让面板一直显示未签）。
		a.MarkCheckedIn()
		out.Status = "ok"
		if granted {
			out.Message = "本次领取成功，流水已确认到账"
		} else {
			out.Message = "上游今日已发放（本次领取为幂等空操作）"
		}
		_ = a.SaveAtomic()
		return out
	}
	if claimErr != nil && billErr != nil {
		out.Status = "failed"
		out.Message = fmt.Sprintf("领取失败：%v；流水查询也失败：%v", claimErr, billErr)
		return out
	}
	// 注册时间与设备绑定态一起拿（一次 user_info，带落盘缓存）。
	prof, known := s.profile(ctx, a)
	out.DeviceBound = prof.state()
	// 今天注册的号：bills 里必然一条 daily_grant 都没有，那是还没到发放时点。
	// 这条必须排在「从来没有」前面——否则刚注册几小时的号会被报成无资格
	// （实测踩过：手机号新号 17:07 注册、17:15 报「历史上从未获得」）。
	if reg := prof.createdAt; !reg.IsZero() &&
		reg.In(shanghai()).Format("2006-01-02") == time.Now().In(shanghai()).Format("2006-01-02") {
		out.Status = "pending"
		out.Message = "账号今天注册，每日积分次日首批发放"
		return out
	}
	if billErr == nil && !ever {
		if !known {
			// 设备态没取到就不能下「无资格」的结论：那是猜的。待定比谎报好。
			out.Status = "pending"
			out.Message = "历史上没有发放记录，且可信设备状态未取到，稍后重试"
			return out
		}
		if prof.DeviceState == "unbound" {
			// 实测根因：没绑可信设备上游就不发每日积分，claim 再多轮也不会到账。
			// 这不是失败，也不是「上游未开通」，得让用户看到真正该做的事。
			out.Status = "unbound"
			out.Message = "账号未绑定可信设备，上游不会发放每日积分；需在官方客户端登录一次完成设备绑定"
			return out
		}
		// 刚绑上设备的号要给它一个宽限期。
		//
		// 这条 ineligible 是在「绑定只能靠用户去官方客户端、代码侧补不了」的
		// 前提下写的：那时 bound=true 却从没发过分，基本就是上游真不给。
		// 自从补绑接口打通（/devices_current_bind），情况变了 —— 我们刚把号
		// 绑上，流水里当然一条 daily_grant 都没有。此时判 ineligible 等于
		// 「绑好了也永远不再检查」，白白把这批号废掉。
		// 绑定时刻未知（老凭证没有 device_bound_at）。
		//
		// 我们自己的补绑一定会写这个字段，所以「未知」= 绑定发生在系统之外
		// （用户在官方客户端绑的，或早于该字段引入）。这种情况沿用原判断：
		// 老号 + 从未发分 = 上游确实不给，判 ineligible。
		// 但必须留一条出路——万一用户刚在客户端绑过，点一次「补绑设备」
		// 就能把时刻补上，之后就进入宽限期正常参与签到。
		since, sinceKnown := boundRecently(a.DeviceBoundAt, time.Now())
		if sinceKnown && since < deviceBindGrace {
			out.Status = "pending"
			out.Message = fmt.Sprintf("刚补绑可信设备 %s，等上游首次发放（每日积分发放时点不固定）",
				since.Round(time.Minute))
			return out
		}
		out.Status = "ineligible"
		if sinceKnown {
			out.Message = "该账号已绑设备但历史上从未获得每日积分，无需签到"
		} else {
			out.Message = "该账号已绑设备但历史无发放记录（绑定时刻未知）；若刚在客户端绑过，" +
				"点「补绑设备」同步状态后即可参与签到"
		}
		return out
	}
	out.Status = "pending"
	out.Message = "上游今日尚未发放，稍后再试"
	return out
}

// deviceBindGrace 是「刚绑上设备」的宽限期。
//
// 每日 +300 的发放时点不固定（实测出现 00:00 / 00:21 / 09:25 / 13:30 等），
// 所以绑完当天没看到流水是正常的，不能马上判死。3 天足够覆盖周末式的延迟，
// 又不至于让真无资格的号一直占用签到请求。
const deviceBindGrace = 72 * time.Hour

// boundRecently 报告设备绑定距今多久。未记录绑定时刻时 ok=false：
// 那是「不知道」，不该被当成「很久以前」而立刻判 ineligible。
func boundRecently(boundAt string, now time.Time) (time.Duration, bool) {
	if strings.TrimSpace(boundAt) == "" {
		return 0, false
	}
	at, err := time.Parse(time.RFC3339, boundAt)
	if err != nil {
		return 0, false
	}
	return now.Sub(at), true
}

// deviceProfile 是账号上游资料的本地视图（含「这份资料新不新」）。
type deviceProfile struct {
	createdAt   time.Time
	DeviceBound bool
	DeviceState string
}

// state 归一成 bound / unbound / ""（未知），供面板直接展示。
func (p deviceProfile) state() string { return p.DeviceState }

// deviceCacheTTL 是设备绑定态的复用时长。
//
// 绑不绑设备是低频变化（用户去官方客户端登录一次才会变），但签到是每天跑的，
// 不缓存就等于每天给每个号多打一次 user_info。12 小时：一天最多两次。
const deviceCacheTTL = 12 * time.Hour

// profile 取账号的上游资料，带落盘缓存。
//
// 返回的 known=false 表示「确实不知道设备态」（查询失败且没有可用缓存），
// 调用方据此走「待定」而不是「无资格」——把网络抖动说成账号无资格是谎报。
func (s *Scheduler) profile(ctx context.Context, a *auth.Auth) (prof deviceProfile, known bool) {
	snap := a.Snapshot()
	prof.createdAt = a.RegisterTime()
	prof.DeviceBound = snap.DeviceBound == "bound"
	prof.DeviceState = snap.DeviceBound

	cached := snap.DeviceBound != "" && snap.DeviceCheckedAt != ""
	if cached {
		if at, err := time.Parse(time.RFC3339, snap.DeviceCheckedAt); err == nil {
			cached = time.Since(at) < deviceCacheTTL
		} else {
			cached = false
		}
	}
	if cached && !prof.createdAt.IsZero() {
		return prof, true
	}

	remote, err := s.up.UserProfile(ctx, a)
	if err != nil {
		// 查询失败：有旧缓存就继续用旧的（但 known 仍为 true，因为知道的是事实），
		// 没缓存就承认不知道。
		return prof, snap.DeviceBound != ""
	}
	if !remote.CreatedAt.IsZero() {
		prof.createdAt = remote.CreatedAt
	}
	prof.DeviceBound = remote.DeviceBound
	prof.DeviceState = remote.DeviceState
	if prof.DeviceState == "" {
		prof.DeviceState = map[bool]string{true: "bound", false: "unbound"}[remote.DeviceBound]
	}
	a.Apply(func(d *auth.Data) {
		if !remote.CreatedAt.IsZero() {
			d.RegisterAt = remote.CreatedAt.Format(time.RFC3339)
		}
		d.DeviceBound = prof.DeviceState
		d.DeviceCheckedAt = time.Now().Format(time.RFC3339)
	})
	_ = a.SaveAtomic()
	return prof, true
}

// checkinAll 对全池账号跑一轮签到。
func (s *Scheduler) checkinAll(ctx context.Context) JobResult {
	started := time.Now()
	result := JobResult{Job: "checkin", StartedAt: started}
	uids := s.pool.UIDs()
	cfg := s.Config()
	run := make(chan string, len(uids))
	for _, uid := range uids {
		run <- uid
	}
	close(run)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for uid := range run {
				if ctx.Err() != nil {
					return
				}
				entry, ok := s.pool.Get(uid)
				if !ok {
					continue
				}
				if entry.Disabled && !cfg.IncludeDisabled {
					mu.Lock()
					result.Skipped++
					mu.Unlock()
					continue
				}
				a := s.pool.AuthByUID(uid)
				if a == nil {
					continue
				}
				outcome := s.CheckinAccount(ctx, a)
				mu.Lock()
				switch outcome.Status {
				case "ok", "already":
					result.OK++
					s.pool.NoteCheckedIn(uid, time.Now())
				case "pending", "ineligible", "unbound":
					// unbound 归这里而不是 default：没绑设备是账号状态，不是本次
					// 执行的失败。算进 failed 会让面板每天都有一条红字，
					// 反而把真正的网络/凭证错误淹掉。
					result.Skipped++
				default:
					result.Failed++
				}
				result.Details = append(result.Details,
					fmt.Sprintf("%s(%s): %s — %s", displayName(outcome), uid, outcome.Status, outcome.Message))
				mu.Unlock()
				// 顺手领手机端首登奖励（每号只试一次）。
				s.tryMobileGrant(ctx, a)
			}
		}()
	}
	wg.Wait()
	result.EndedAt = time.Now()
	s.recordResult(result)
	log.Printf("[scheduler] 签到完成：成功 %d / 跳过 %d / 失败 %d（%s）",
		result.OK, result.Skipped, result.Failed, result.EndedAt.Sub(result.StartedAt).Round(time.Second))
	return result
}

// tryMobileGrant 领手机端首登奖励（与桌面端额度独立的一笔）。
func (s *Scheduler) tryMobileGrant(ctx context.Context, a *auth.Auth) {
	s.mobileMu.Lock()
	if s.mobileDone[a.UID] {
		s.mobileMu.Unlock()
		return
	}
	s.mobileDone[a.UID] = true
	s.mobileMu.Unlock()
	if _, err := s.up.ClaimMobileGrant(ctx, a); err != nil {
		log.Printf("[scheduler] 手机端首登奖励 %s: %v", a.UID, err)
	}
}

// CheckinAll 供面板手动触发。
func (s *Scheduler) CheckinAll(ctx context.Context) JobResult { return s.checkinAll(ctx) }

// CheckinOne 供面板手动触发单号签到。
func (s *Scheduler) CheckinOne(ctx context.Context, uid string) (CheckinOutcome, error) {
	a := s.pool.AuthByUID(uid)
	if a == nil {
		return CheckinOutcome{}, fmt.Errorf("账号 %s 不存在", uid)
	}
	out := s.CheckinAccount(ctx, a)
	if out.Status == "ok" || out.Status == "already" {
		s.pool.NoteCheckedIn(uid, time.Now())
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 保活

// keepaliveAll 给所有账号做一次「临期就续期」。
func (s *Scheduler) keepaliveAll(ctx context.Context) JobResult {
	started := time.Now()
	result := JobResult{Job: "keepalive", StartedAt: started}
	cfg := s.Config()
	for _, uid := range s.pool.UIDs() {
		if ctx.Err() != nil {
			break
		}
		entry, ok := s.pool.Get(uid)
		if !ok {
			continue
		}
		if entry.Disabled && !cfg.IncludeDisabled {
			result.Skipped++
			continue
		}
		a := s.pool.AuthByUID(uid)
		if a == nil || !a.CanRefresh() {
			result.Skipped++
			continue
		}
		// 窗口 6h，而调度按小时检查（见 runDueJobs）：窗口必须大于间隔，
		// 否则 token 会在两次检查之间过期。旧配置一天只查一次，等于 75% 漏判。
		refreshed, err := s.up.RefreshTokenIfNeeded(ctx, a, 6*time.Hour)
		if err != nil {
			result.Failed++
			result.Details = append(result.Details, fmt.Sprintf("%s: %v", uid, err))
			s.pool.Update(uid, func(st *pool.State) { st.LastError = "保活续期失败: " + err.Error() })
			continue
		}
		if refreshed {
			result.OK++
		} else {
			result.Skipped++
		}
	}
	result.EndedAt = time.Now()
	s.recordResult(result)
	if result.OK > 0 || result.Failed > 0 {
		log.Printf("[scheduler] 保活完成：续期 %d / 跳过 %d / 失败 %d", result.OK, result.Skipped, result.Failed)
	}
	return result
}

// KeepaliveAll 供面板手动触发。
func (s *Scheduler) KeepaliveAll(ctx context.Context) JobResult { return s.keepaliveAll(ctx) }

// ---------------------------------------------------------------------------
// 余额刷新

// RefreshBalances 刷新全池积分，并在「余额恢复」时解冻额度冷却。
func (s *Scheduler) RefreshBalances(ctx context.Context) JobResult {
	started := time.Now()
	result := JobResult{Job: "balance", StartedAt: started}
	cfg := s.Config()
	for _, uid := range s.pool.UIDs() {
		if ctx.Err() != nil {
			break
		}
		entry, ok := s.pool.Get(uid)
		if !ok {
			continue
		}
		if entry.Disabled && !cfg.IncludeDisabled {
			result.Skipped++
			continue
		}
		a := s.pool.AuthByUID(uid)
		if a == nil {
			continue
		}
		usage, err := s.up.UserEntUsageDetail(ctx, a)
		if err != nil {
			result.Failed++
			result.Details = append(result.Details, fmt.Sprintf("%s: %v", uid, err))
			continue
		}
		result.OK++
		s.pool.NoteCredits(uid, usage.Remain)
		s.pool.Update(uid, func(st *pool.State) {
			st.CreditAt = time.Now().Format(time.RFC3339)
		})
		_ = a.SaveAtomic()
		// 额度冷却 + 余额恢复 → 自动解冻（但不动人工禁用）。
		if usage.Remain > 0 && entry.CooldownKind == "plan_limit" && !entry.Disabled {
			s.pool.Revive(uid)
			log.Printf("[scheduler] %s 余额恢复为 %d，已解除额度冷却", uid, usage.Remain)
		}
	}
	result.EndedAt = time.Now()
	if result.Failed > 0 {
		log.Printf("[scheduler] 余额刷新：成功 %d / 失败 %d", result.OK, result.Failed)
	}
	s.recordResult(result)
	return result
}

// RefreshOne 刷新单号余额（面板调用）。
func (s *Scheduler) RefreshOne(ctx context.Context, uid string) (upstream.EntUsage, error) {
	a := s.pool.AuthByUID(uid)
	if a == nil {
		return upstream.EntUsage{}, fmt.Errorf("账号 %s 不存在", uid)
	}
	usage, err := s.up.UserEntUsageDetailWithExpiry(ctx, a)
	if err != nil {
		return usage, err
	}
	s.pool.NoteCredits(uid, usage.Remain)
	s.pool.Update(uid, func(st *pool.State) { st.CreditAt = time.Now().Format(time.RFC3339) })
	_ = a.SaveAtomic()
	return usage, nil
}

// ---------------------------------------------------------------------------
// 结果记录

func (s *Scheduler) recordResult(result JobResult) {
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	if s.results == nil {
		s.results = map[string]JobResult{}
	}
	s.results[result.Job] = result
}

// Results 返回最近结果。
func (s *Scheduler) Results() map[string]JobResult {
	s.resultMu.RLock()
	defer s.resultMu.RUnlock()
	out := make(map[string]JobResult, len(s.results))
	for k, v := range s.results {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------------------
// 辅助

func shanghai() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*3600)
}

func containsInt(list []int, v int) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func displayName(o CheckinOutcome) string {
	if o.Nickname != "" {
		return o.Nickname
	}
	return o.UID
}
