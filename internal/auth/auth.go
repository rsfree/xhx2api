// Package auth 管理小浣熊账号凭证：磁盘形态、原子写回、目录扫描与 JWT 解析。
//
// 一个账号一个 JSON 文件（auths/<安全化后的 uid>.json，0600）：
//
//	{"uid":"4465567","nickname":"小浣熊-1",
//	 "access_token":"eyJ…","refresh_token":"eyJ…","expires_at":1792369369000,
//	 "credit":1234,"credit_at":"2026-10-26T09:00:00+08:00","checked_in":"2026-10-26"}
//
// 为什么要一个账号一个文件而不是一个大 JSON：
//   - 上游每次续期都会**轮换 refresh_token**，新票必须立刻落盘才算真到手；
//     单文件在大池子里每次续期都要重写全量，写失败会连累别的账号；
//   - 账号增删是文件级操作，面板「移除账号」= 删文件，重启后与目录天然对齐，
//     不需要额外的软删除标记；
//   - 运维可以手工 scp 一个 .json 进来就完成导入。
//
// 所有写回都是「临时文件 + fsync + rename」的原子替换：断电/崩溃只会留下旧版本
// 或新版本，不会出现半截 JSON 把账号弄坏（那种损坏只能重新登录）。
package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// realm 常量：小浣熊只有国内一个站点。保留该字段是因为池、面板与日志都按
// realm 分桶展示；将来若出国际版，只需在这里加一个值，不必改调用方形状。
const (
	RealmCN     = "cn"
	RealmGlobal = "cn"
)

// MaxTokenLength 是 token 字段的长度上限。正常 JWT 约 500-1500 字符，8K 足够
// 覆盖上游换算法；超过这条线基本是粘贴了别的东西（比如整段 HTML）。
const MaxTokenLength = 8192

// tmpSeq 让同一账号的临时文件名在进程内唯一：同一毫秒内两次续期不会撞名
// （撞名会让 rename 抢同一个 inode，理论上前一次的 fsync 可能写到别人的文件上）。
var tmpSeq atomic.Uint64

// ErrNoIdentity 表示既没有 uid 也没有可解析 token 的账号无法入池。
var ErrNoIdentity = errors.New("账号缺少 uid，且无法从 access_token 解析出用户标识")

// Data 是可序列化字段。单独拆出来是为了 Snapshot 能拷值而不拷锁。
type Data struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname,omitempty"`
	Email        string `json:"email,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpiresAt 是 access_token 到期时刻（Unix 毫秒）；0 = 未知。
	// 未知时不主动刷新，等上游 401 再刷（少打一次没必要的续期请求）。
	ExpiresAt int64 `json:"expires_at,omitempty"`

	// 以下是最近一次查询的快照，随账号文件一起持久化，重启后面板不空表。
	Credit    int64  `json:"credit"`
	CreditAt  string `json:"credit_at,omitempty"`
	CheckedIn string `json:"checked_in,omitempty"` // 东八区日期 YYYY-MM-DD

	// ImportedAt 是**导入动作**写入的进池时刻（RFC3339）。
	// 调度器用它判断「新号静默期」：刚导入的号不立刻跑重任务，先养几天。
	// 不用文件 mtime 推断——搬机器/解压会让 mtime 集体漂移。
	ImportedAt string `json:"imported_at,omitempty"`
	// RegisterAt 是上游账号的注册时间（能查到才有），签到时用来区分
	// 「上游今天还没发每日积分」与「签到了但没落账」。
	RegisterAt string `json:"register_at,omitempty"`

	// DeviceBound 是上游「可信设备绑定」状态：bound / unbound；空 = 还没查过。
	//
	// 它决定这个号到底有没有每日积分资格。2026-11-02 实测 11 个号：9 个
	// unbound（reason=session_without_bound_device）的**从来没拿到过一次**
	// daily_grant，2 个 bound 的每天都有 300。裸 token / 手机号导入的号都不带
	// 绑定态，所以面板以前报的「上游未开通」是误导——真正的原因是没绑设备。
	DeviceBound string `json:"device_bound,omitempty"`
	// ClientDeviceID 是「这台设备」的稳定标识（UUIDv4 形态）。
	//
	// 绑可信设备时必须带上它，而且**必须持久化后复用**：每次绑都换一个 UUID
	// 等于同一账号不断声明新设备，正是风控最敏感的模式。首次绑定时生成，
	// 之后所有绑定/心跳都用同一个值。
	ClientDeviceID string `json:"client_device_id,omitempty"`
	// DeviceBoundAt 是「设备被绑上」的时刻（RFC3339），只在绑定成功时写。
	//
	// 与 DeviceCheckedAt 分开记，因为后者每次查 user_info 都会刷新，
	// 而判断「是不是刚绑的、上游可能还没来得及发分」需要的是绑定时刻。
	DeviceBoundAt string `json:"device_bound_at,omitempty"`
	// DeviceCheckedAt 是上一次查 DeviceBound 的时刻（RFC3339），用于 TTL 复用，
	// 免得每轮签到都对每个号打一次 user_info。
	DeviceCheckedAt string `json:"device_checked_at,omitempty"`

	// SessionCapped 标记「上游 IAM 会话已到绝对上限」。
	//
	// 为什么要单独记：到期前续期**不会报错**，只会续出一张只活 1~2 分钟的票
	// （exp 被钳在会话上限）。请求路径按 RefreshSkew 判断临期，于是一个本该
	// 「停机」的账号会变成「每个请求都续一次、永远续不好」的续期风暴。
	// 实测 2026-11-26：7772869 在 09:48 续出 09:49 到期的票，7774611 在
	// 12:25 续出 12:27 到期的票——两张票的 exp 正好是各自 30 天会话的终点。
	// 打上这个标记后不再尝试续期，直接按登录态失效处理，等重新登录清掉。
	SessionCapped bool `json:"session_capped,omitempty"`
}

// Auth 是归一化后的账号凭证。
//
// 并发模型：mu 保护可变字段；写路径（续期回写、余额刷新、签到标记）持写锁
// 只改内存，随后调 SaveAtomic 落盘；读路径持读锁取快照。刻意不让「持锁写盘」：
// 磁盘 IO 慢，持写锁会把整池选号卡住。
type Auth struct {
	Data
	mu       sync.RWMutex
	FilePath string `json:"-"`
}

// Lock/Unlock/RLock/RUnlock 暴露给需要「读-改-写」原子性的调用方（如续期）。
func (a *Auth) Lock()    { a.mu.Lock() }
func (a *Auth) Unlock()  { a.mu.Unlock() }
func (a *Auth) RLock()   { a.mu.RLock() }
func (a *Auth) RUnlock() { a.mu.RUnlock() }

// JWT 返回当前 access token 的读锁快照。
func (a *Auth) JWT() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

// RefreshTokenValue 返回当前 refresh token 的读锁快照。
func (a *Auth) RefreshTokenValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.RefreshToken
}

// ExpiresAtValue 返回 access token 到期时刻（Unix 毫秒）。
func (a *Auth) ExpiresAtValue() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ExpiresAt
}

// Realm 返回地区；小浣熊恒 cn。
func (a *Auth) Realm() string { return RealmCN }

// CanRefresh 报告是否有刷新凭证。
func (a *Auth) CanRefresh() bool { return a.RefreshTokenValue() != "" }

// SessionCappedValue 报告会话是否已到上游的绝对上限。
func (a *Auth) SessionCappedValue() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.SessionCapped
}

// MarkSessionCapped 打上「会话到顶」标记：此后不再续期，直到换到一张健康的新票。
func (a *Auth) MarkSessionCapped() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.SessionCapped = true
}

// sessionCapRecover 是「新票够健康、可以解除到顶标记」的门槛。
//
// 取 10 分钟而非「大于 0」：续期在到顶那一刻返回的票正好是 1~2 分钟的，
// 用它当判据会把到顶状态的解除条件设在「刚好续了一次」上，标记就白打了。
const sessionCapRecover = 10 * time.Minute

// NeedsRefresh 报告 token 是否将在 within 内过期。
//
// 无 expiry（=0）时返回 false：上游没告诉我们到期时间就等 401 兜底，
// 否则每个请求前都会白打一次续期。
//
// 会话到顶时恒为 false：那种状态下的续期换回来的票永远比 skew 短，
// 判定「临期」只会让每个请求都去续一次，而结果不会变好。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.needsRefreshLocked(within)
}

func (a *Auth) needsRefreshLocked(within time.Duration) bool {
	if a.SessionCapped {
		return false
	}
	exp := a.ExpiresAt
	if exp <= 0 {
		return false
	}
	return time.Now().Add(within).UnixMilli() >= exp
}

// NeedsRefreshLocked 在已持写锁时判断（避免续期路径自己再 RLock 造成重入死锁——
// Go 的 RWMutex 不可重入，读锁套写锁会直接卡死）。
func (a *Auth) NeedsRefreshLocked(within time.Duration) bool {
	return a.needsRefreshLocked(within)
}

// SetTokens 写入续期得到的新票。
//
// 三个约定：
//   - refresh 为空 = 上游没有轮换，保留旧值（宁可留着旧票也别把唯一的续期凭证抹掉）；
//   - expiresAtMs<=0 = 上游没给，用新 access token 的 JWT exp 兜底；
//   - 新票明显长于「到顶时那种一两分钟的残票」时解除 SessionCapped：
//     换到健康的新票（重新登录/导入）就是到顶状态唯一的出路。
func (a *Auth) SetTokens(access, refresh string, expiresAtMs int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if access != "" {
		a.AccessToken = access
	}
	if refresh != "" {
		a.RefreshToken = refresh
	}
	if expiresAtMs > 0 {
		a.ExpiresAt = normalizeMillis(expiresAtMs)
	} else if exp := JWTExpiry(a.AccessToken); exp > 0 {
		a.ExpiresAt = exp
	}
	if a.SessionCapped && a.ExpiresAt > time.Now().Add(sessionCapRecover).UnixMilli() {
		a.SessionCapped = false
	}
}

// SetCredit 更新积分快照与查询时间。
func (a *Auth) SetCredit(credit int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Credit = credit
	a.CreditAt = time.Now().Format(time.RFC3339)
}

// MarkCheckedIn 记下「今天已确认领到每日积分」（东八区日期）。
func (a *Auth) MarkCheckedIn() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.CheckedIn = time.Now().In(shanghai()).Format("2006-01-02")
}

// CheckedInToday 报告今天是否已确认签到。
func (a *Auth) CheckedInToday() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CheckedIn == time.Now().In(shanghai()).Format("2006-01-02")
}

// SetRegisterAt 记录上游注册时间。
func (a *Auth) SetRegisterAt(t time.Time) {
	if t.IsZero() {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.RegisterAt = t.In(shanghai()).Format(time.RFC3339)
}

// RegisterTime 解析注册时间；缺失/非法返回零值。
func (a *Auth) RegisterTime() time.Time {
	a.mu.RLock()
	raw := a.RegisterAt
	a.mu.RUnlock()
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ImportedTime 解析进池时间；缺失/非法返回零值。
func (a *Auth) ImportedTime() time.Time {
	a.mu.RLock()
	raw := a.ImportedAt
	a.mu.RUnlock()
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

// shanghai 返回东八区时区（无 tzdata 的环境回落到固定 +8 偏移）。
//
// 容器镜像用 alpine 时不装 tzdata 是很常见的——那时 LoadLocation 报错，
// 退化成 FixedZone 才能保证「今天」的判断与上游（东八区）一致。
func shanghai() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*3600)
}

// safeNamePattern 是落盘文件名的白名单：字母数字与 -_.。
// uid 来自上游，但面板也允许手工填 uid；不白名单化就会出现 "../" 把文件
// 写到目录外面去（路径穿越）。
var safeNamePattern = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// FileNameFor 把 uid 变成安全的文件名。
//
// 全非法字符时用 uid 的 sha256 前 12 位兜底——两个中文昵称当 uid 的账号
// 不会互相覆盖。
func FileNameFor(uid string) string {
	name := safeNamePattern.ReplaceAllString(strings.TrimSpace(uid), "_")
	// 连续的点折叠：白名单允许 "."，但 ".." 在部分路径拼接实现里仍有特殊含义
	// （Windows 尤其明显），落盘名里不该出现。
	for strings.Contains(name, "..") {
		name = strings.ReplaceAll(name, "..", "_")
	}
	name = strings.Trim(name, ".")
	if name == "" {
		sum := sha256.Sum256([]byte(uid))
		name = "acct-" + hex.EncodeToString(sum[:6])
	}
	// 文件名过长会让某些文件系统报 ENAMETOOLONG（255 字节上限）。
	if len(name) > 120 {
		sum := sha256.Sum256([]byte(uid))
		name = name[:100] + "-" + hex.EncodeToString(sum[:6])
	}
	return name + ".json"
}

// SaveAtomic 把当前凭证原子写回磁盘（0600）。
//
// 顺序：写 tmp → fsync 文件 → rename → fsync 目录。少了最后的目录 fsync，
// 断电后 rename 可能只落在 journal 里，重启看到的是旧文件——而旧 refresh_token
// 已被上游轮换作废，等于账号掉线。
func (a *Auth) SaveAtomic() error {
	if a.FilePath == "" {
		return errors.New("auth: 未设置 FilePath，无法保存")
	}
	a.mu.RLock()
	data := a.Data
	a.mu.RUnlock()
	return saveData(a.FilePath, data)
}

// saveData 是 SaveAtomic 的实际实现（拿到值拷贝后调用，避免持锁做 IO）。
func saveData(path string, data Data) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), tmpSeq.Add(1))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(raw)
	if werr == nil {
		werr = f.Sync()
	}
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return werr
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// 目录 fsync：保证 rename 本身持久化。失败不视为致命（某些文件系统不支持
	// 目录 fsync，比如部分网络盘），只影响掉电语义。
	if dir, derr := os.Open(filepath.Dir(path)); derr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// Snapshot 返回副本（不持锁读取）。
func (a *Auth) Snapshot() Data {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Data
}

// Apply 把外部修改合并进账号（面板编辑昵称/积分等）。
func (a *Auth) Apply(fn func(*Data)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	fn(&a.Data)
}

// Parse 解析一段账号 JSON（宽松口径）。
//
// 支持的形态（面板「导入账号」与命令行都走这里）：
//   - 官方桌面端 ~/.box-agent/config/auth.json 里的对象（可能嵌在 data/account 下）
//   - {"access_token":…,"refresh_token":…,"uid":…,"nickname":…}
//   - {"token":…} / {"accessToken":…} 等驼峰别名
//   - 只有一串 JWT（access token），uid 从 claims 里取
func Parse(raw []byte) (*Auth, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil, errors.New("内容为空")
	}
	// 裸 token（用户可能直接粘了一串 eyJ…）
	if !strings.HasPrefix(text, "{") && !strings.HasPrefix(text, "[") {
		token := stripBearer(text)
		if token == "" || !looksLikeToken(token) {
			return nil, errors.New("无法识别的内容：请粘贴账号 JSON 或 access_token")
		}
		return buildAuth(map[string]any{"access_token": token})
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	// 官方配置文件可能把凭证放在 data / account / auth 下，逐层下钻一次。
	for _, key := range []string{"data", "account", "auth", "credentials"} {
		if inner, ok := payload[key].(map[string]any); ok && inner != nil {
			// 把外层没覆盖的键补进来（uid 可能在外层）。
			for k, v := range payload {
				if _, exists := inner[k]; !exists && k != key {
					if _, isMap := v.(map[string]any); !isMap {
						inner[k] = v
					}
				}
			}
			payload = inner
			break
		}
	}
	return buildAuth(payload)
}

// buildAuth 从通用 map 提取凭证。
func buildAuth(payload map[string]any) (*Auth, error) {
	access := stripBearer(text(payload, "access_token", "accessToken", "token", "jwt", "id_token"))
	refresh := stripBearer(text(payload, "refresh_token", "refreshToken", "refresh"))
	uid := text(payload, "uid", "user_id", "userId", "userID", "id")
	// 有些导出形态 uid 是数字，text() 已处理。
	if uid == "0" {
		uid = ""
	}
	nickname := text(payload, "nickname", "name", "user_name", "display_name", "office_identity")
	if access == "" && refresh == "" {
		return nil, errors.New("缺少 access_token / refresh_token")
	}
	if len(access) > MaxTokenLength || len(refresh) > MaxTokenLength {
		return nil, errors.New("token 长度异常，请确认粘贴的是凭证而不是整段页面内容")
	}
	a := &Auth{Data: Data{
		UID:          uid,
		Nickname:     nickname,
		Email:        text(payload, "email", "mail"),
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    millisOf(payload["expires_at"]),
		Credit:       int64Of(payload["credit"], payload["credits"], payload["available_points"]),
		RegisterAt:   text(payload, "register_at", "created_at", "register_time"),
		// 这三个是「程序自己写回去的状态」，读回来时也必须还原：
		// 漏了 checked_in 会让面板在每次重启后显示「未签」（签到其实成功），
		// 漏了 imported_at 会让「新号静默期」在重启后失效。
		CreditAt:   text(payload, "credit_at"),
		CheckedIn:  text(payload, "checked_in"),
		ImportedAt: text(payload, "imported_at"),
		// 设备绑定态同理：不还原就得每轮签到重新问一次上游，
		// 而且重启后面板会把「已知 unbound 的号」重新显示成「待定」。
		DeviceBound:     text(payload, "device_bound"),
		DeviceCheckedAt: text(payload, "device_checked_at"),
		// client_device_id 也必须还原：换了它等于换了台机器，
		// 上游会把「同一账号不断声明新设备」当成异常。
		ClientDeviceID: text(payload, "client_device_id"),
		DeviceBoundAt:  text(payload, "device_bound_at"),
	}}
	if a.ExpiresAt == 0 {
		a.ExpiresAt = JWTExpiry(access)
	}
	if a.UID == "" {
		a.UID = JWTUserID(access)
	}
	if a.UID == "" {
		return nil, ErrNoIdentity
	}
	if a.Nickname == "" {
		a.Nickname = a.UID
	}
	return a, nil
}

// LoadDir 扫描目录下全部 *.json 并建号。
//
// 容错口径：单个文件坏掉只跳过并回报错误（错误里带文件名），不影响其它账号
// 启动——一个手抖改坏的 JSON 不该让整池服务起不来。
func LoadDir(dir string) ([]*Auth, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]*Auth, 0, len(entries))
	var problems []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		// 跳过原子写的临时文件（进程被杀时可能残留）。
		if strings.Contains(entry.Name(), ".tmp.") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", entry.Name(), rerr))
			continue
		}
		a, perr := Parse(raw)
		if perr != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", entry.Name(), perr))
			continue
		}
		a.FilePath = path
		out = append(out, a)
	}
	if len(problems) > 0 {
		return out, fmt.Errorf("部分账号文件无法解析: %s", strings.Join(problems, "；"))
	}
	return out, nil
}

// WriteNew 把新账号写进目录（已存在则覆盖），返回落盘路径。
func WriteNew(dir string, a *Auth) error {
	path := filepath.Join(dir, FileNameFor(a.UID))
	a.mu.Lock()
	if a.ImportedAt == "" {
		a.ImportedAt = time.Now().Format(time.RFC3339)
	}
	data := a.Data
	a.FilePath = path
	a.mu.Unlock()
	return saveData(path, data)
}

// Save 落盘（FilePath 为空时按 dir 生成）。
//
// 首次落盘（文件还不存在）时补 ImportedAt：调度器的「新号静默期」靠它判断
// 进池时间，漏了就等于静默期失效——新号会被立刻拿去做最重的行为链。
// 只在文件不存在时补，避免「老账号被 Save 一次就当成新号」。
func (a *Auth) Save(dir string) error {
	a.mu.Lock()
	if a.FilePath == "" {
		if dir == "" {
			a.mu.Unlock()
			return errors.New("auth: 未指定保存目录")
		}
		a.FilePath = filepath.Join(dir, FileNameFor(a.UID))
	}
	path := a.FilePath
	needImported := a.ImportedAt == ""
	a.mu.Unlock()

	if needImported {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			a.mu.Lock()
			if a.ImportedAt == "" {
				a.ImportedAt = time.Now().Format(time.RFC3339)
			}
			a.mu.Unlock()
		}
	}
	return a.SaveAtomic()
}

// Remove 删除账号文件。
func (a *Auth) Remove() error {
	if a.FilePath == "" {
		return nil
	}
	err := os.Remove(a.FilePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// MaskToken 把 token 脱敏成「前 6 … 后 4」形状，日志与面板用。
func MaskToken(token string) string {
	runes := []rune(token)
	if len(runes) <= 12 {
		return "***"
	}
	return string(runes[:6]) + "…" + string(runes[len(runes)-4:])
}

// JWTExpiry 解码 JWT payload 的 exp（秒）并返回毫秒；解析失败返回 0。
//
// 不验签——签名是上游的事，我们只想知道什么时候该续期。验签需要公钥，
// 而公钥随上游轮换，跟着验会让网关比上游更早失效。
func JWTExpiry(token string) int64 {
	claims := jwtClaims(token)
	if claims == nil {
		return 0
	}
	raw, ok := claims["exp"]
	if !ok {
		return 0
	}
	seconds, ok := floatOf(raw)
	if !ok || seconds <= 0 {
		return 0
	}
	return int64(seconds * 1000)
}

// JWTUserID 解码 JWT 里的用户标识（依次尝试常见键）。
//
// 小浣熊的 access token 是 JWT 时 claims 里带 user_id；不是 JWT（不透明串）
// 时返回空，由上游 /user_info 兜底。
func JWTUserID(token string) string {
	claims := jwtClaims(token)
	if claims == nil {
		return ""
	}
	for _, key := range []string{"user_id", "userId", "uid", "userID", "sub"} {
		raw, ok := claims[key]
		if !ok {
			continue
		}
		if s := scalarText(raw); s != "" && s != "0" {
			return s
		}
	}
	return ""
}

// jwtClaims 解出 payload 段（不验签）；格式不对返回 nil。
func jwtClaims(token string) map[string]json.RawMessage {
	token = stripBearer(token)
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return nil
		}
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return claims
}

// looksLikeToken 粗判一段字符串是否像 token（JWT 有 2 个点，不透明串通常较长）。
func looksLikeToken(token string) bool {
	if len(token) < 20 {
		return false
	}
	for _, r := range token {
		if r <= 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

// stripBearer 去 "Bearer " 前缀与空白。
func stripBearer(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return v
}

// text 取第一个非空的字符串/数字字段。
func text(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := payload[key]; ok {
			if s := scalarText(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// scalarText 把 JSON 标量读成字符串。
func scalarText(v any) string {
	switch typed := v.(type) {
	case json.RawMessage:
		// jwtClaims 把每个 claim 存成 RawMessage；先解成通用值再递归。
		var inner any
		if err := json.Unmarshal(typed, &inner); err != nil {
			return ""
		}
		return scalarText(inner)
	case string:
		return strings.TrimSpace(typed)
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return typed.String()
	case bool:
		return strconv.FormatBool(typed)
	}
	return ""
}

// millisOf 把秒/毫秒/数字串/RFC3339 统一成毫秒时间戳。
func millisOf(v any) int64 {
	if v == nil {
		return 0
	}
	switch typed := v.(type) {
	case float64:
		return normalizeMillis(int64(typed))
	case json.Number:
		if n, err := typed.Int64(); err == nil {
			return normalizeMillis(n)
		}
	case string:
		s := strings.TrimSpace(typed)
		if s == "" {
			return 0
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return normalizeMillis(n)
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UnixMilli()
			}
		}
	}
	return 0
}

// normalizeMillis 把 10 位秒级时间戳升到毫秒（< 1e11 视为秒）。
func normalizeMillis(v int64) int64 {
	if v <= 0 {
		return 0
	}
	if v < 100_000_000_000 {
		return v * 1000
	}
	return v
}

// int64Of 取第一个能解析成整数的字段（0 = 没有）。
func int64Of(values ...any) int64 {
	for _, v := range values {
		switch typed := v.(type) {
		case float64:
			return int64(typed)
		case json.Number:
			if n, err := typed.Int64(); err == nil {
				return n
			}
		case string:
			if n, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64); err == nil {
				return n
			}
		}
	}
	return 0
}

// floatOf 把 JSON 数字/数字串读成 float64。
func floatOf(v any) (float64, bool) {
	switch typed := v.(type) {
	case json.RawMessage:
		var inner any
		if err := json.Unmarshal(typed, &inner); err != nil {
			return 0, false
		}
		return floatOf(inner)
	case float64:
		return typed, true
	case json.Number:
		f, err := typed.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return f, err == nil
	}
	return 0, false
}
