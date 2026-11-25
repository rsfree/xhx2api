// Package reqlog 记录请求指标与脱敏归档。
//
// 两层：
//
//   - 内存环形缓冲（最近 N 条）：面板「最近请求」直接读，重启即清；
//   - JSONL 归档（按日文件）：排障时能回看一周前的请求，按总量上限自动清理。
//
// 记录的内容**不含请求正文**：正文里有用户的代码与对话，落盘等于把隐私写进
// 运维目录。只留元数据（模型、账号、状态、耗时、token、来源）足够定位问题。
package reqlog

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Event 是一条请求元数据。
type Event struct {
	ID         string    `json:"id"`
	Time       time.Time `json:"time"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Protocol   string    `json:"protocol,omitempty"`
	Model      string    `json:"model,omitempty"`
	UID        string    `json:"uid,omitempty"`
	Account    string    `json:"account,omitempty"`
	Status     int       `json:"status"`
	ErrorKind  string    `json:"error_kind,omitempty"`
	Error      string    `json:"error,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	TTFBMS     int64     `json:"ttfb_ms,omitempty"`
	Prompt     int64     `json:"prompt_tokens,omitempty"`
	Completion int64     `json:"completion_tokens,omitempty"`
	Total      int64     `json:"total_tokens,omitempty"`
	// UsageMissing 表示上游没回 usage：token 全 0 **不等于**零消耗。
	// 面板要显示「?」而不是 0，否则这些记录会把 tokens 总量与均值一起拉低，
	// 看起来像「用了很多次但几乎没花 token」。
	UsageMissing bool   `json:"usage_missing,omitempty"`
	Stream       bool   `json:"stream,omitempty"`
	Attempts     int    `json:"attempts,omitempty"`
	Rotations    int    `json:"rotations,omitempty"`
	ClientIP     string `json:"client_ip,omitempty"`
	UserAgent    string `json:"user_agent,omitempty"`
	// C2PASigner 是生图响应里 C2PA 清单的签名者（如 "OpenAI Media Service"）。
	//
	// 同一个模型名在上游可能落到不同厂商的后端——`raccoon-image-gen` 是
	// LiteLLM 的 model group，实测同一分钟内就能在 Azure OpenAI 与 OpenAI
	// 官方之间切换。不记这一列，就只能靠事后扫图猜分布；空串表示这次
	// 响应没有任何溯源凭证，"未知"与"没有"必须分开看（沿用 UsageMissing 的教训）。
	C2PASigner string `json:"c2pa_signer,omitempty"`
}

// ModelMetric 是按模型聚合。
type ModelMetric struct {
	Requests int64 `json:"requests"`
	Failures int64 `json:"failures"`
	Prompt   int64 `json:"prompt_tokens"`
	Complete int64 `json:"completion_tokens"`
	Total    int64 `json:"total_tokens"`
	SumMS    int64 `json:"sum_ms"`
}

// Metrics 是聚合指标。
type Metrics struct {
	Since       time.Time               `json:"since"`
	Total       int64                   `json:"total"`
	Success     int64                   `json:"success"`
	Failure     int64                   `json:"failure"`
	InFlight    int64                   `json:"in_flight"`
	ByStatus    map[int]int64           `json:"by_status"`
	ByProtocol  map[string]int64        `json:"by_protocol"`
	ByModel     map[string]*ModelMetric `json:"by_model"`
	LatencyP50  int64                   `json:"latency_p50_ms"`
	LatencyP95  int64                   `json:"latency_p95_ms"`
	LatencyAvg  int64                   `json:"latency_avg_ms"`
	RPM         float64                 `json:"rpm"`
	TokensTotal int64                   `json:"tokens_total"`
	// Unmetered 是「上游没回 usage」的条数：这些请求的 token 记 0，但不代表
	// 没消耗。看 Token 总量时必须连同这个数一起看，否则「没记上」会被读成「省」。
	Unmetered int64 `json:"unmetered"`
}

// Config 是归档参数。
type Config struct {
	RingSize      int    // 内存环形缓冲条数，<=0 回落 500
	ArchiveDir    string // 空 = 不归档
	RetentionDays int    // 归档保留天数，<=0 回落 7
	MaxMB         int    // 归档总上限（MiB），<=0 回落 100
	ClientInfo    bool   // 是否记录来源 IP/UA
}

// Recorder 是并发安全的记录器。
type Recorder struct {
	cfg Config

	mu        sync.Mutex
	ring      []Event
	next      int
	filled    bool
	metrics   Metrics
	latency   []int64 // 最近 512 次延迟，算分位
	inFlight  int64
	dayLock   sync.Mutex
	dayFile   string
	dayHandle *os.File
	writeMu   sync.Mutex
}

// New 建记录器。
func New(cfg Config) *Recorder {
	if cfg.RingSize <= 0 {
		cfg.RingSize = 500
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 7
	}
	if cfg.MaxMB <= 0 {
		cfg.MaxMB = 100
	}
	return &Recorder{
		cfg:     cfg,
		ring:    make([]Event, cfg.RingSize),
		metrics: Metrics{Since: time.Now(), ByStatus: map[int]int64{}, ByProtocol: map[string]int64{}, ByModel: map[string]*ModelMetric{}},
	}
}

// Begin 标记一个请求进入处理（在途 +1）。
func (r *Recorder) Begin() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.inFlight++
	r.mu.Unlock()
}

// Record 记一条并归档。
func (r *Recorder) Record(ev Event) {
	if r == nil {
		return
	}
	if ev.ID == "" {
		ev.ID = NewRequestID()
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	r.mu.Lock()
	if r.inFlight > 0 {
		r.inFlight--
	}
	r.metrics.Total++
	r.metrics.InFlight = r.inFlight
	if ev.Status >= 400 || ev.ErrorKind != "" {
		r.metrics.Failure++
	} else {
		r.metrics.Success++
	}
	r.metrics.ByStatus[ev.Status]++
	if ev.Protocol != "" {
		r.metrics.ByProtocol[ev.Protocol]++
	}
	if ev.Model != "" {
		mm := r.metrics.ByModel[ev.Model]
		if mm == nil {
			mm = &ModelMetric{}
			r.metrics.ByModel[ev.Model] = mm
		}
		mm.Requests++
		if ev.Status >= 400 || ev.ErrorKind != "" {
			mm.Failures++
		}
		mm.Prompt += ev.Prompt
		mm.Complete += ev.Completion
		mm.Total += ev.Total
		mm.SumMS += ev.DurationMS
	}
	r.metrics.TokensTotal += ev.Total
	if ev.UsageMissing {
		r.metrics.Unmetered++
	}
	if ev.DurationMS > 0 {
		r.latency = append(r.latency, ev.DurationMS)
		if len(r.latency) > 512 {
			r.latency = r.latency[len(r.latency)-512:]
		}
	}
	r.ring[r.next] = ev
	r.next = (r.next + 1) % len(r.ring)
	if r.next == 0 {
		r.filled = true
	}
	r.computeLatencyLocked()
	r.mu.Unlock()
	r.archive(ev)
}

// computeLatencyLocked 重算分位（512 个样本排序开销可忽略；不必做流式分位）。
func (r *Recorder) computeLatencyLocked() {
	n := len(r.latency)
	if n == 0 {
		return
	}
	sorted := make([]int64, n)
	copy(sorted, r.latency)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum int64
	for _, v := range sorted {
		sum += v
	}
	r.metrics.LatencyAvg = sum / int64(n)
	r.metrics.LatencyP50 = sorted[n/2]
	idx := int(float64(n) * 0.95)
	if idx >= n {
		idx = n - 1
	}
	r.metrics.LatencyP95 = sorted[idx]
	if elapsed := time.Since(r.metrics.Since); elapsed > 0 {
		r.metrics.RPM = float64(r.metrics.Total) / elapsed.Minutes()
	}
}

// Recent 返回最近 n 条（新的在前）。
func (r *Recorder) Recent(n int) []Event {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	size := r.next
	if r.filled {
		size = len(r.ring)
	}
	if n <= 0 || n > size {
		n = size
	}
	out := make([]Event, 0, n)
	// 从最新往回取。
	for i := 0; i < n; i++ {
		idx := (r.next - 1 - i + len(r.ring)*2) % len(r.ring)
		if !r.filled && idx >= size {
			break
		}
		out = append(out, r.ring[idx])
	}
	return out
}

// Metrics 返回指标快照。
func (r *Recorder) Metrics() Metrics {
	if r == nil {
		return Metrics{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.metrics
	out.InFlight = r.inFlight
	out.ByStatus = copyMapInt(r.metrics.ByStatus)
	out.ByProtocol = copyMapStr(r.metrics.ByProtocol)
	out.ByModel = make(map[string]*ModelMetric, len(r.metrics.ByModel))
	for k, v := range r.metrics.ByModel {
		cp := *v
		out.ByModel[k] = &cp
	}
	if elapsed := time.Since(out.Since); elapsed > 0 {
		out.RPM = float64(out.Total) / elapsed.Minutes()
	}
	return out
}

// Reset 清零指标（不清归档）。
func (r *Recorder) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.metrics = Metrics{Since: time.Now(), ByStatus: map[int]int64{}, ByProtocol: map[string]int64{}, ByModel: map[string]*ModelMetric{}}
	r.latency = nil
	r.ring = make([]Event, r.cfg.RingSize)
	r.next, r.filled = 0, false
	r.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 归档

// archive 把事件追加到当天的 JSONL。
//
// 失败静默：归档是辅助能力，写不进去（磁盘满/只读挂载）不该影响请求本身。
func (r *Recorder) archive(ev Event) {
	if r == nil || r.cfg.ArchiveDir == "" {
		return
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	day := ev.Time.Format("2006-01-02")
	if r.dayFile != day {
		if r.dayHandle != nil {
			_ = r.dayHandle.Close()
			r.dayHandle = nil
		}
		if err := os.MkdirAll(r.cfg.ArchiveDir, 0o700); err != nil {
			return
		}
		path := filepath.Join(r.cfg.ArchiveDir, fmt.Sprintf("requests-%s.jsonl", day))
		f, ferr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if ferr != nil {
			return
		}
		r.dayHandle, r.dayFile = f, day
		r.pruneLocked()
	}
	if r.dayHandle != nil {
		_, _ = r.dayHandle.Write(append(raw, '\n'))
	}
}

// pruneLocked 清理过期与超量归档（调用方已持 writeMu）。
func (r *Recorder) pruneLocked() {
	entries, err := os.ReadDir(r.cfg.ArchiveDir)
	if err != nil {
		return
	}
	type fileInfo struct {
		path string
		mod  time.Time
		size int64
	}
	files := make([]fileInfo, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") {
			continue
		}
		info, ierr := entry.Info()
		if ierr != nil {
			continue
		}
		files = append(files, fileInfo{filepath.Join(r.cfg.ArchiveDir, entry.Name()), info.ModTime(), info.Size()})
		total += info.Size()
	}
	cutoff := time.Now().Add(-time.Duration(r.cfg.RetentionDays) * 24 * time.Hour)
	for _, f := range files {
		if f.mod.Before(cutoff) {
			if err := os.Remove(f.path); err == nil {
				total -= f.size
			}
		}
	}
	maxBytes := int64(r.cfg.MaxMB) << 20
	if total <= maxBytes {
		return
	}
	// 超量：从最旧的开始删。
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= maxBytes {
			break
		}
		if err := os.Remove(f.path); err == nil {
			total -= f.size
		}
	}
}

// Close 关闭归档文件句柄。
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if r.dayHandle != nil {
		_ = r.dayHandle.Close()
		r.dayHandle = nil
	}
}

// ---------------------------------------------------------------------------
// 辅助

// NewRequestID 生成 req_<12 hex>。
func NewRequestID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req_%d", time.Now().UnixNano()%1e12)
	}
	return "req_" + hex.EncodeToString(b[:])
}

func copyMapInt(in map[int]int64) map[int]int64 {
	out := make(map[int]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyMapStr(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// ClientInfo 报告是否记录来源（配置开关）。
func (r *Recorder) ClientInfo() bool {
	if r == nil {
		return false
	}
	return r.cfg.ClientInfo
}
