// Package pool 管理账号池：选号、在途上限、冷却、熔断、降权与状态持久化。
//
// 设计立场（与上游风控对齐）：
//
//   - **宁可漏判，不可误伤**。把参数错误判成额度不足，会让一个还能用的号白关
//     12 小时；把限流判成登录失效，连败几次就把号自动禁用了。所以「罚号」只在
//     证据明确时发生（见 upstream.Classify 的收窄判据）。
//   - **冷却不是禁用**。冷却只影响选号，账号仍然签到、续期、刷新余额；只有
//     人工禁用（Disabled）才真正退出池子。
//   - **状态要落盘**。重启后如果冷却/熔断状态全丢，一批刚被打满的号会立刻
//     重新投入使用，等于把风控窗口白送给上游。
package pool

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
)

// ErrNoAccount 池里没有可用账号（全部冷却/禁用/不可用）。
var ErrNoAccount = errors.New("没有可用账号")

// ErrNoCapacity 池里有账号，但全部达到了在途上限。
//
// 与 ErrNoAccount 分开是因为处置完全不同：前者要等（几百毫秒后就有位置），
// 后者等也没用（号在冷却/被禁用）。实测上游单账号并发超过 10-12 会回
// 429 limiter_request_error，所以宁可在这里排队，也别把请求打进上游换一个
// 60 秒冷却回来。
var ErrNoCapacity = errors.New("所有账号都在途已满")

// Strategy 是选号策略。
type Strategy string

const (
	// StrategyRoundRobin 在可用账号间轮转（默认）：均匀摊平额度消耗。
	StrategyRoundRobin Strategy = "round_robin"
	// StrategyPriority 按 priority 升序（数值越小越优先），同优先级内轮转。
	StrategyPriority Strategy = "priority"
	// StrategyWeighted 按权重随机：闲置越久权重越高（见 idle_weight_per_hour），
	// 冷却恢复的号会被优先补偿。
	StrategyWeighted Strategy = "weighted_random"
	// StrategyLeastInflight 优先选在途最少的号：慢请求场景下吞吐最好。
	StrategyLeastInflight Strategy = "least_inflight"
)

// ParseStrategy 校验策略名。
func ParseStrategy(raw string) (Strategy, error) {
	switch Strategy(strings.ToLower(strings.TrimSpace(raw))) {
	case "", StrategyRoundRobin:
		return StrategyRoundRobin, nil
	case StrategyPriority:
		return StrategyPriority, nil
	case StrategyWeighted, "weighted", "random":
		return StrategyWeighted, nil
	case StrategyLeastInflight, "least":
		return StrategyLeastInflight, nil
	}
	return StrategyRoundRobin, fmt.Errorf("未知选号策略 %q（可选 round_robin/priority/weighted_random/least_inflight）", raw)
}

// Config 是池的运行参数（main 从 config 注入，面板可热改一部分）。
type Config struct {
	Strategy      Strategy
	MaxInFlight   int           // 单账号在途上限，0 = 不限
	BreakerThresh int           // 连续失败达到该值触发熔断
	BreakerCool   time.Duration // 熔断初始冷却
	BreakerMax    time.Duration // 熔断冷却封顶（连续熔断指数放大）
	DegradeThresh int           // 连续失败达到该值开始降权
	DegradeCool   time.Duration // 降权持续时长
	SoftRateBase  time.Duration // 429 软冷却基数
	SoftRateMax   time.Duration // 软冷却封顶
	PlanCool      time.Duration // 额度不足冷却（0 = 到次日 04:00）
	IdleWeightPer time.Duration // 每闲置多久权重 +1
	IdleWeightMax float64       // 权重上限
	StateFile     string
}

// Entry 是对外（面板）的账号视图。
type Entry struct {
	UID           string  `json:"uid"`
	Nickname      string  `json:"nickname"`
	Disabled      bool    `json:"disabled"`
	Paused        bool    `json:"paused"`
	CooldownUntil int64   `json:"cooldown_until"` // unix 秒；0 = 不在冷却
	CooldownKind  string  `json:"cooldown_kind,omitempty"`
	CooldownNote  string  `json:"cooldown_note,omitempty"`
	BreakerUntil  int64   `json:"breaker_until"`
	DegradeUntil  int64   `json:"degrade_until"`
	FailCount     int     `json:"fail_count"`
	InFlight      int     `json:"in_flight"`
	Credits       int64   `json:"credits"`
	CreditAt      string  `json:"credit_at,omitempty"`
	CheckedIn     string  `json:"checked_in"`
	LastError     string  `json:"last_error,omitempty"`
	LastUsed      int64   `json:"last_used"`
	LastSuccess   int64   `json:"last_success"`
	Total         int64   `json:"total"`
	Success       int64   `json:"success"`
	Fail          int64   `json:"fail"`
	AvgLatencyMS  int64   `json:"avg_latency_ms"`
	SessionDead   int     `json:"session_dead"`
	Weight        float64 `json:"weight"`
	ExpiresAt     int64   `json:"expires_at"`
	ImportedAt    string  `json:"imported_at,omitempty"`
	FilePath      string  `json:"file,omitempty"`
	HasRefresh    bool    `json:"has_refresh"`
	Realm         string  `json:"realm"`
}

// Counts 是池的汇总计数（/status 与面板总览）。
type Counts struct {
	Total     int `json:"total"`
	Healthy   int `json:"healthy"`
	Cooling   int `json:"cooling"`
	Disabled  int `json:"disabled"`
	InFlight  int `json:"in_flight"`
	Available int `json:"available"`
}

// Pool 是并发安全的账号池。
type Pool struct {
	mu     sync.RWMutex
	cfg    Config
	auths  map[string]*auth.Auth
	order  []string // 稳定顺序（按导入顺序），决定轮转起点
	states map[string]*State
	cursor int

	dirtyMu sync.Mutex
	dirty   bool
}

// New 建池。
func New(cfg Config) *Pool {
	cfg = normalizeConfig(cfg)
	return &Pool{
		cfg:    cfg,
		auths:  map[string]*auth.Auth{},
		states: map[string]*State{},
	}
}

// normalizeConfig 补齐默认值（非正值一律回落，不报错——参数写错不该让服务起不来）。
func normalizeConfig(cfg Config) Config {
	if cfg.Strategy == "" {
		cfg.Strategy = StrategyRoundRobin
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = 3
	}
	if cfg.BreakerThresh <= 0 {
		cfg.BreakerThresh = 3
	}
	if cfg.BreakerCool <= 0 {
		cfg.BreakerCool = 10 * time.Minute
	}
	if cfg.BreakerMax <= 0 {
		cfg.BreakerMax = 2 * time.Hour
	}
	if cfg.DegradeThresh <= 0 {
		cfg.DegradeThresh = 5
	}
	if cfg.DegradeCool <= 0 {
		cfg.DegradeCool = 10 * time.Minute
	}
	// 429 的恢复是秒级的（2026-10-30 实测：被判「第 7 级退避、冷却 1.1h」的
	// 账号直接打上游，两个出口 IP 各 2 个请求全部 200）。基数定 15s 让一次
	// 瞬时过载只损失一个回合，封顶 5min 兜住「某个号真的被限」的情况——
	// 原来的 60s 起、2h 封顶会把一次抖动锁成半天的容量黑洞。
	if cfg.SoftRateBase <= 0 {
		cfg.SoftRateBase = 15 * time.Second
	}
	if cfg.SoftRateMax <= 0 {
		cfg.SoftRateMax = 5 * time.Minute
	}
	if cfg.IdleWeightPer <= 0 {
		cfg.IdleWeightPer = 30 * time.Minute
	}
	if cfg.IdleWeightMax <= 0 {
		cfg.IdleWeightMax = 5
	}
	return cfg
}

// Config 返回当前参数快照。
func (p *Pool) Config() Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

// Configure 热更新参数（面板保存配置后调用）。
func (p *Pool) Configure(fn func(*Config)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn(&p.cfg)
	p.cfg = normalizeConfig(p.cfg)
}

// SyncToDir 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）。
//
// 状态保留是刻意的：用户把账号文件挪走再挪回来，冷却/统计不该重置——
// 否则「摘出去避风头再放回」就成了绕过冷却的后门。
func (p *Pool) SyncToDir(auths []*auth.Auth) (added, removed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range auths {
		if a == nil || a.UID == "" {
			continue
		}
		seen[a.UID] = true
		if _, exists := p.auths[a.UID]; !exists {
			p.auths[a.UID] = a
			p.order = append(p.order, a.UID)
			added++
			if p.states[a.UID] == nil {
				p.states[a.UID] = newState(a)
			}
		} else {
			// 已存在：保留状态，换用最新的凭证对象（目录重载后 token 可能更新）。
			p.auths[a.UID] = a
			if st := p.states[a.UID]; st != nil {
				st.Nickname = a.Snapshot().Nickname
			}
		}
	}
	for uid := range p.auths {
		if !seen[uid] {
			delete(p.auths, uid)
			removed++
		}
	}
	if removed > 0 {
		kept := p.order[:0]
		for _, uid := range p.order {
			if _, ok := p.auths[uid]; ok {
				kept = append(kept, uid)
			}
		}
		p.order = kept
	}
	p.markDirtyLocked()
	return added, removed
}

// Add 加入一个账号（面板导入成功后调用）。
func (p *Pool) Add(a *auth.Auth) {
	if a == nil || a.UID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.auths[a.UID]; !exists {
		p.order = append(p.order, a.UID)
	}
	p.auths[a.UID] = a
	if p.states[a.UID] == nil {
		p.states[a.UID] = newState(a)
	} else {
		// 重新导入同一个号：清掉「登录态失效」标记（用户显然刚重新登录过），
		// 但保留额度冷却——重新登录不会让上游的额度恢复。
		st := p.states[a.UID]
		st.SessionDead = 0
		st.Disabled = false
		st.LastError = ""
		if st.CooldownKind == "session_dead" {
			st.CooldownUntil = 0
			st.CooldownKind = ""
		}
	}
	p.markDirtyLocked()
}

// Remove 从池里移除账号（不删文件）。
func (p *Pool) Remove(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.auths, uid)
	kept := p.order[:0]
	for _, id := range p.order {
		if id != uid {
			kept = append(kept, id)
		}
	}
	p.order = kept
	p.markDirtyLocked()
}

// AuthByUID 取凭证对象。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.auths[uid]
}

// FirstAvailable 取一个「不冷却、不禁用」的账号（不占用在途），用于拉目录/查余额
// 这类一次性动作。没有可用号时返回 nil。
func (p *Pool) FirstAvailable() *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, uid := range p.order {
		if p.availableLocked(uid, now) {
			return p.auths[uid]
		}
	}
	return nil
}

// availableLocked 判断账号当前是否可承接新请求（不看在途）。
func (p *Pool) availableLocked(uid string, now time.Time) bool {
	a := p.auths[uid]
	st := p.states[uid]
	if a == nil || st == nil {
		return false
	}
	if st.Disabled || st.Paused {
		return false
	}
	ts := now.Unix()
	if st.CooldownUntil > ts || st.BreakerUntil > ts {
		return false
	}
	return true
}

// Lease 是一次选号的租约：调用方必须在请求结束时 Release，否则在途计数会泄漏，
// 该账号会在 max_in_flight 次之后永久不可用。
type Lease struct {
	UID     string
	Started time.Time
	pool    *Pool
	once    sync.Once
}

// Auth 返回租约对应的凭证。
func (l *Lease) Auth() *auth.Auth {
	if l == nil || l.pool == nil {
		return nil
	}
	return l.pool.AuthByUID(l.UID)
}

// PickUID 选指定的账号（会话粘性用）。账号不可用时返回 ErrNoAccount，
// 由调用方决定是回退到普通 Pick 还是换号。
func (p *Pool) PickUID(uid string) (*Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if !p.availableLocked(uid, now) {
		return nil, ErrNoAccount
	}
	st := p.states[uid]
	st.InFlight++
	st.LastUsed = now.Unix()
	return &Lease{UID: uid, Started: now, pool: p}, nil
}

// Pick 选一个可用账号并占用一个在途名额。
//
// exclude 是本次请求已经失败过的 uid（换号重试时排除，避免在同一个号上打转）。
// minCredits > 0 时跳过已知余额不足的号（额度冷却还没生效时的兜底）。
func (p *Pool) Pick(exclude map[string]bool, minCredits int64) (*Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pickLocked(exclude, minCredits, false)
}

// PickWait 与 Pick 相同，但「全部账号在途已满」时最多等 wait 再软回退。
//
// 这段时间不是空转：等的是别的请求结束、把在途名额还回来。
// ctx 取消立刻返回（客户端断开时不要继续占位）。
func (p *Pool) PickWait(ctx context.Context, exclude map[string]bool, minCredits int64, wait time.Duration) (*Lease, error) {
	if wait <= 0 {
		return p.Pick(exclude, minCredits)
	}
	deadline := time.Now().Add(wait)
	for {
		p.mu.Lock()
		lease, err := p.pickLocked(exclude, minCredits, true)
		p.mu.Unlock()
		if err == nil {
			return lease, nil
		}
		if !errors.Is(err, ErrNoCapacity) {
			return nil, err
		}
		if time.Now().After(deadline) {
			// 等不到了：按老行为软回退（多排一个），宁可慢也别拒。
			return p.Pick(exclude, minCredits)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(40 * time.Millisecond):
		}
	}
}

// pickLocked 是选号的真实实现（调用方持锁）。
//
// requireUnder=true 时，所有账号都在途满会返回 ErrNoCapacity，由 PickWait 决定等不等。
func (p *Pool) pickLocked(exclude map[string]bool, minCredits int64, requireUnder bool) (*Lease, error) {
	now := time.Now()
	candidates := make([]string, 0, len(p.order))
	inflight := map[string]int{}
	for _, uid := range p.order {
		if exclude != nil && exclude[uid] {
			continue
		}
		if !p.availableLocked(uid, now) {
			continue
		}
		st := p.states[uid]
		if minCredits > 0 && st.Credits > 0 && st.Credits < minCredits {
			continue
		}
		inflight[uid] = st.InFlight
		candidates = append(candidates, uid)
	}
	if len(candidates) == 0 {
		return nil, ErrNoAccount
	}
	// 在途上限：先按上限过滤。
	//
	// 这里是**软**上限：全都满了还是挑一个（多排一个总比直接拒好）。
	// 硬性等待在 PickWait 里做——因为上游确实有并发限制：2026-11-01 三账号
	// 两个模型实测，单账号**第 6 个**在途请求起就 429（0.1s 内、不排队），
	// 且额度按账号算、跨模型共享。超发不会「多挤一个」，只会把号打进去。
	under := candidates[:0]
	for _, uid := range candidates {
		if inflight[uid] < p.cfg.MaxInFlight {
			under = append(under, uid)
		}
	}
	if len(under) > 0 {
		candidates = under
	} else if requireUnder {
		return nil, ErrNoCapacity
	}

	var chosen string
	switch p.cfg.Strategy {
	case StrategyPriority:
		chosen = p.pickPriority(candidates)
	case StrategyWeighted:
		chosen = p.pickWeighted(candidates, now)
	case StrategyLeastInflight:
		chosen = p.pickLeastInflight(candidates, inflight)
	default:
		chosen = p.pickRoundRobin(candidates)
	}
	st := p.states[chosen]
	st.InFlight++
	st.LastUsed = now.Unix()
	return &Lease{UID: chosen, Started: now, pool: p}, nil
}

// pickRoundRobin 从游标位置起找第一个候选，保证「每个号轮流用」而不是随机。
func (p *Pool) pickRoundRobin(candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	// 以 order 里的位置做游标扫描；候选是 order 的子集，所以扫一遍一定能命中。
	for i := 0; i < len(p.order); i++ {
		idx := (p.cursor + i) % len(p.order)
		uid := p.order[idx]
		for _, c := range candidates {
			if c == uid {
				p.cursor = (idx + 1) % len(p.order)
				return uid
			}
		}
	}
	return candidates[0]
}

// pickPriority 按 priority（存在 State.Priority）升序，同优先级内轮转。
func (p *Pool) pickPriority(candidates []string) string {
	best := candidates[0]
	for _, uid := range candidates[1:] {
		if p.states[uid].Priority < p.states[best].Priority {
			best = uid
		}
	}
	// 同优先级的多个号之间仍走轮转，避免总是打死第一个。
	same := candidates[:0]
	for _, uid := range candidates {
		if p.states[uid].Priority == p.states[best].Priority {
			same = append(same, uid)
		}
	}
	if len(same) > 1 {
		return p.pickRoundRobin(same)
	}
	return best
}

// pickWeighted 按「闲置补偿」权重随机。
func (p *Pool) pickWeighted(candidates []string, now time.Time) string {
	weights := make([]float64, len(candidates))
	var total float64
	for i, uid := range candidates {
		w := p.weightOf(p.states[uid], now)
		weights[i] = w
		total += w
	}
	if total <= 0 {
		return p.pickRoundRobin(candidates)
	}
	r := rand.Float64() * total
	for i, w := range weights {
		r -= w
		if r <= 0 {
			return candidates[i]
		}
	}
	return candidates[len(candidates)-1]
}

// pickLeastInflight 取在途最少的号（相同则轮转）。
func (p *Pool) pickLeastInflight(candidates []string, inflight map[string]int) string {
	least := inflight[candidates[0]]
	for _, uid := range candidates[1:] {
		if inflight[uid] < least {
			least = inflight[uid]
		}
	}
	tied := candidates[:0]
	for _, uid := range candidates {
		if inflight[uid] == least {
			tied = append(tied, uid)
		}
	}
	return p.pickRoundRobin(tied)
}

// weightOf 计算权重：基础 1.0 + 闲置时长补偿，上限 IdleWeightMax。
// 刚被冷却/熔断过的号闲置最久，权重最高，恢复后优先补偿。
func (p *Pool) weightOf(st *State, now time.Time) float64 {
	w := 1.0
	if st.LastUsed > 0 {
		idle := now.Sub(time.Unix(st.LastUsed, 0))
		if idle > 0 {
			w += float64(idle) / float64(p.cfg.IdleWeightPer)
		}
	} else {
		w += p.cfg.IdleWeightMax // 从未用过的号直接给满权重，尽快试水
	}
	if now.Unix() < st.DegradeUntil {
		w *= 0.2 // 降权期：仍可用，但明显靠后
	}
	if w > p.cfg.IdleWeightMax {
		w = p.cfg.IdleWeightMax
	}
	if w < 0.05 {
		w = 0.05
	}
	return w
}

// Release 结算一次请求。
//
// kind 是错误分类；nil 表示成功。latency 只用于面板统计。
// 这是**唯一**的埋点汇聚处：流式/非流式、成功/失败都走这里，两条口径不会漂移。
func (l *Lease) Release(kind *ErrKind, message string, latency time.Duration) {
	if l == nil || l.pool == nil {
		return
	}
	l.once.Do(func() {
		l.pool.release(l.UID, kind, message, latency, l.Started)
	})
}

// ReleaseSuccess 是 Release(nil) 的语义糖。
func (l *Lease) ReleaseSuccess(latency time.Duration) { l.Release(nil, "", latency) }

// ErrKind 是 pool 对 upstream.ErrKind 的最小依赖（避免循环 import）。
//
// upstream 不依赖 pool，pool 也不 import upstream；两者用这个别名在 main 里桥接。
type ErrKind = string

func (p *Pool) release(uid string, kind *ErrKind, message string, latency time.Duration, started time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[uid]
	if st == nil {
		return
	}
	if st.InFlight > 0 {
		st.InFlight--
	}
	now := time.Now()
	// client / none 不是账号的问题：客户端传错参数（400）或无法归类的错误，
	// 只留 last_error 供排障，**整条不进账号统计**——不计 Total/Fail/FailCount、
	// 不参与平均延迟、不触发降权。否则下游一个错参数就能把好号喂到
	// DegradeThresh，账号页的成功率也失去意义（2026-11-02 线上：16 条
	// temperature 400 给 5 个号各记了失败，成功率被拖到 21%-35%）。
	if kind != nil && (*kind == "client" || *kind == "none") {
		st.LastError = truncateText(message, 200)
		p.markDirtyLocked()
		return
	}
	st.Total++
	// 滑动平均延迟（面板看趋势用；首次直接取样本）。
	if latency > 0 {
		ms := latency.Milliseconds()
		if st.AvgLatencyMS == 0 {
			st.AvgLatencyMS = ms
		} else {
			st.AvgLatencyMS = (st.AvgLatencyMS*4 + ms) / 5
		}
	}
	if kind == nil {
		st.Success++
		st.FailCount = 0
		st.SessionDead = 0
		st.LastSuccess = now.Unix()
		st.LastError = ""
		st.SoftRateLevel = 0
		st.CooldownUntil = 0
		st.CooldownKind = ""
		st.CooldownNote = ""
		st.BreakerUntil = 0
		st.DegradeUntil = 0
		p.markDirtyLocked()
		return
	}
	st.Fail++
	st.FailCount++
	st.LastError = truncateText(message, 200)
	p.applyPenaltyLocked(st, *kind, now)
	p.markDirtyLocked()
}

// applyPenaltyLocked 按错误类别决定惩罚。这里是「不误伤」原则的落点：
// 只有 plan_limit / soft_rate / session_dead(累计) / server(熔断) 会真正影响选号。
// cooldownUntil 把时长换算成 unix 秒的截止时刻。
//
// 向上取整并保证至少 1 秒：State 里存的是秒级时间戳，500ms 的冷却直接取
// Unix() 会得到「现在就到期」，等于没冷却（实测踩过：15s 以下都正常，但测试里
// 用毫秒级值会静默失效）。
func cooldownUntil(now time.Time, d time.Duration) int64 {
	secs := int64(d / time.Second)
	if d%time.Second != 0 {
		secs++
	}
	if secs < 1 {
		secs = 1
	}
	return now.Unix() + secs
}

func (p *Pool) applyPenaltyLocked(st *State, kind ErrKind, now time.Time) {
	switch ErrKind(kind) {
	case "plan_limit":
		cool := p.cfg.PlanCool
		if cool <= 0 {
			st.CooldownUntil = NextPlanReset(now).Unix()
			st.CooldownKind = "plan_limit"
			st.CooldownNote = "额度不足（冷却至次日 04:00）"
			return
		}
		st.CooldownUntil = cooldownUntil(now, cool)
		st.CooldownKind = "plan_limit"
		st.CooldownNote = "额度不足"
	case "soft_rate":
		// 指数退避：60s → 120s → 240s …，封顶 soft_rate_max。
		st.SoftRateLevel++
		cool := p.cfg.SoftRateBase << minInt(st.SoftRateLevel-1, 10)
		if cool > p.cfg.SoftRateMax || cool <= 0 {
			cool = p.cfg.SoftRateMax
		}
		st.CooldownUntil = cooldownUntil(now, cool)
		st.CooldownKind = "soft_rate"
		st.CooldownNote = fmt.Sprintf("限流，冷却 %s（第 %d 级退避）", shortDur(cool), st.SoftRateLevel)
	case "session_dead":
		st.SessionDead++
		// 单次 401 可能只是 token 恰好过期（续期还能救）。连续 3 次才认为真失效。
		if st.SessionDead >= 3 {
			st.Disabled = true
			st.CooldownNote = "登录态连续失效，已自动禁用"
		} else {
			st.CooldownUntil = cooldownUntil(now, 2*time.Minute)
			st.CooldownKind = "session_dead"
			st.CooldownNote = fmt.Sprintf("登录态失效（第 %d 次），等待续期或重新登录", st.SessionDead)
		}
	case "server":
		if st.FailCount >= p.cfg.BreakerThresh {
			// 连续熔断：每次翻倍，封顶 BreakerMax。
			level := st.FailCount - p.cfg.BreakerThresh
			cool := p.cfg.BreakerCool << minInt(level, 8)
			if cool > p.cfg.BreakerMax || cool <= 0 {
				cool = p.cfg.BreakerMax
			}
			st.BreakerUntil = cooldownUntil(now, cool)
			st.CooldownKind = "server"
			st.CooldownNote = fmt.Sprintf("连续上游异常，熔断 %s", shortDur(cool))
		}
	case "gateway":
		// 网关超时/502：换号，但不罚号（这是链路问题，不是账号问题）。
		st.CooldownUntil = cooldownUntil(now, 15*time.Second)
		st.CooldownKind = "gateway"
		st.CooldownNote = "上游网关超时，短暂回避"
	case "busy":
		// 排队：换号，不罚号。
		st.CooldownUntil = cooldownUntil(now, 45*time.Second)
		st.CooldownKind = "busy"
		st.CooldownNote = "上游模型排队，短暂回避"
	case "not_found":
		st.CooldownUntil = cooldownUntil(now, time.Minute)
		st.CooldownKind = "not_found"
		st.CooldownNote = "上游 404，短暂回避"
	default:
		// client / none / reasoning_loop：不罚号，只累计降权观察。
		if st.FailCount >= p.cfg.DegradeThresh {
			st.DegradeUntil = cooldownUntil(now, p.cfg.DegradeCool)
		}
	}
}

// List 返回全部账号视图（按池内顺序）。
func (p *Pool) List() []Entry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	out := make([]Entry, 0, len(p.order))
	for _, uid := range p.order {
		a := p.auths[uid]
		st := p.states[uid]
		if a == nil || st == nil {
			continue
		}
		snap := a.Snapshot()
		entry := Entry{
			UID: uid, Nickname: firstNonEmpty(snap.Nickname, uid),
			Disabled: st.Disabled, Paused: st.Paused,
			CooldownUntil: st.CooldownUntil, CooldownKind: st.CooldownKind, CooldownNote: st.CooldownNote,
			BreakerUntil: st.BreakerUntil, DegradeUntil: st.DegradeUntil,
			FailCount: st.FailCount, InFlight: st.InFlight,
			Credits: firstNonZero(st.Credits, snap.Credit), CreditAt: snap.CreditAt,
			CheckedIn: snap.CheckedIn, LastError: st.LastError,
			LastUsed: st.LastUsed, LastSuccess: st.LastSuccess,
			Total: st.Total, Success: st.Success, Fail: st.Fail,
			AvgLatencyMS: st.AvgLatencyMS, SessionDead: st.SessionDead,
			Weight:    round2(p.weightOf(st, now)),
			ExpiresAt: snap.ExpiresAt, ImportedAt: snap.ImportedAt,
			FilePath: a.FilePath, HasRefresh: snap.RefreshToken != "",
			Realm: auth.RealmCN,
		}
		out = append(out, entry)
	}
	return out
}

// Counts 汇总池状态。
func (p *Pool) Counts() Counts {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var c Counts
	c.Total = len(p.order)
	for _, uid := range p.order {
		st := p.states[uid]
		if st == nil {
			continue
		}
		c.InFlight += st.InFlight
		if st.Disabled {
			c.Disabled++
			continue
		}
		if st.CooldownUntil > now.Unix() || st.BreakerUntil > now.Unix() || st.Paused {
			c.Cooling++
			continue
		}
		c.Healthy++
	}
	c.Available = c.Healthy
	return c
}

// UIDs 返回池内全部 uid（快照）。
func (p *Pool) UIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, len(p.order))
	copy(out, p.order)
	return out
}

// SetDisabled 人工禁用/解禁（禁用 = 完全退出选号，但保号任务仍会执行）。
func (p *Pool) SetDisabled(uid string, disabled bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[uid]
	if st == nil {
		return false
	}
	st.Disabled = disabled
	if !disabled {
		st.CooldownUntil = 0
		st.CooldownKind = ""
		st.CooldownNote = ""
		st.BreakerUntil = 0
		st.SessionDead = 0
		st.LastError = ""
	}
	p.markDirtyLocked()
	return true
}

// SetPaused 人工暂停（临时摘流量：仍在池里，签到/续期照常）。
func (p *Pool) SetPaused(uid string, paused bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[uid]
	if st == nil {
		return false
	}
	st.Paused = paused
	p.markDirtyLocked()
	return true
}

// Revive 解冻：清掉所有冷却/熔断/降权/失效计数。
func (p *Pool) Revive(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[uid]
	if st == nil {
		return false
	}
	st.Disabled = false
	st.Paused = false
	st.CooldownUntil = 0
	st.CooldownKind = ""
	st.CooldownNote = ""
	st.BreakerUntil = 0
	st.DegradeUntil = 0
	st.FailCount = 0
	st.SessionDead = 0
	st.LastError = ""
	p.markDirtyLocked()
	return true
}

// NoteCredits 更新已知积分（余额刷新/拒绝报文里拿到的值）。
func (p *Pool) NoteCredits(uid string, credits int64) {
	if credits < 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if st := p.states[uid]; st != nil {
		st.Credits = credits
		p.markDirtyLocked()
	}
}

// NoteRegistered 记录「上游确认该号今天有每日发放」等外部事实。
func (p *Pool) NoteCheckedIn(uid string, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st := p.states[uid]; st != nil {
		st.CheckedInAt = at.Format("2006-01-02")
		p.markDirtyLocked()
	}
}

// AvailableUIDs 返回当前可承接请求的 uid 列表（按池内顺序）。
func (p *Pool) AvailableUIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	out := make([]string, 0, len(p.order))
	for _, uid := range p.order {
		if p.availableLocked(uid, now) {
			out = append(out, uid)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 辅助

func newState(a *auth.Auth) *State {
	snap := a.Snapshot()
	return &State{
		UID: snap.UID, Nickname: snap.Nickname,
		Credits: snap.Credit, CreditAt: snap.CreditAt, CheckedInAt: snap.CheckedIn,
		Priority: 100,
	}
}

// NextPlanReset 返回下一个额度重置时刻（次日 04:00，东八区）。
//
// 为什么是 04:00 而不是 00:00：上游每日积分在 00:00 发放，但账单/限流状态
// 在零点前后仍有几分钟的抖动窗口。把解冻推迟到 04:00，避免刚过零点就
// 拿一个还没恢复额度的号去撞墙（撞一次就是再关 12 小时）。
func NextPlanReset(now time.Time) time.Time {
	loc := shanghai()
	local := now.In(loc)
	reset := time.Date(local.Year(), local.Month(), local.Day(), 4, 0, 0, 0, loc)
	if !reset.After(local) {
		reset = reset.Add(24 * time.Hour)
	}
	return reset
}

func shanghai() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*3600)
}

func shortDur(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	if d >= time.Minute {
		return fmt.Sprintf("%.0fm", d.Minutes())
	}
	return fmt.Sprintf("%.0fs", d.Seconds())
}

func truncateText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(values ...int64) int64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// hashUID 把 uid 稳定映射到 [0,n)，供分片/分组用（egress、A/B）。
func hashUID(uid string, n int) int {
	if n <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(uid))
	return int(h.Sum32() % uint32(n))
}

// SortedUIDs 返回排序后的 uid（面板稳定展示）。
func (p *Pool) SortedUIDs() []string {
	uids := p.UIDs()
	sort.Strings(uids)
	return uids
}
