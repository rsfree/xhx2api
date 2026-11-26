package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestStripJSONC(t *testing.T) {
	src := []byte(`{
	  // 行注释
	  "listen": ":7867", /* 块注释 */
	  "upstream": { "user_agent": "http://example.com/a" }, // 字符串里的 // 不能被当注释
	  "models": { "hidden": ["a", "b",], },
	}`)
	out := string(stripJSONC(src))
	if strings.Contains(out, "行注释") || strings.Contains(out, "块注释") {
		t.Fatalf("注释没被去掉:\n%s", out)
	}
	if !strings.Contains(out, "http://example.com/a") {
		t.Fatalf("字符串里的 // 被误删:\n%s", out)
	}
	if strings.Contains(out, `"b",`) || strings.Contains(out, "}, }") {
		t.Fatalf("尾逗号没被去掉:\n%s", out)
	}
	// 去注释后应当能解析。
	cfg, err := ParseConfig(src)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Listen != ":7867" {
		t.Fatalf("listen = %q", cfg.Listen)
	}
}

func TestParseConfigRejectsUnknownKey(t *testing.T) {
	_, err := ParseConfig([]byte(`{"listne": ":7867"}`))
	if err == nil {
		t.Fatal("未知键应当报错（写错键名静默忽略会让人排查半天）")
	}
	if !strings.Contains(err.Error(), "listne") {
		t.Fatalf("错误信息应点出键名: %v", err)
	}
}

func TestParseConfigAllowsCommentKeys(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"_comment":"说明","listen":":9000"}`))
	if err != nil {
		t.Fatalf("_comment 应当被允许: %v", err)
	}
	if cfg.Listen != ":9000" {
		t.Fatalf("listen = %q", cfg.Listen)
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "0.0.0.0:7867" {
		t.Fatalf("默认监听 = %q", cfg.Listen)
	}
	if cfg.DefaultModel != "raccoon-8c4485" {
		t.Fatalf("默认模型 = %q", cfg.DefaultModel)
	}
	if len(cfg.Schedule.CheckinHours) != 1 || cfg.Schedule.CheckinHours[0] != 9 {
		t.Fatalf("默认签到小时 = %v", cfg.Schedule.CheckinHours)
	}
	if cfg.upstreamShort != 120*time.Second {
		t.Fatalf("upstream short = %v", cfg.upstreamShort)
	}
	// 保活必须每小时检查：窗口 6h，一天查一次会漏掉大部分临期 token。
	if len(cfg.Schedule.KeepaliveHours) != 24 {
		t.Fatalf("默认保活小时应为 0-23，得到 %v", cfg.Schedule.KeepaliveHours)
	}
	// 请求路径的续期窗口与保活同口径。
	if cfg.Server.RefreshSkewSeconds != 21600 {
		t.Fatalf("默认续期窗口 = %d 秒", cfg.Server.RefreshSkewSeconds)
	}
	// 别名表要能让 gpt-4o 落到上游模型。
	if cfg.Aliases["gpt-4o"] != "raccoon-chat-ml-5-5" {
		t.Fatalf("别名表不对: %v", cfg.Aliases)
	}
}

// TestParseConfigMigratesLegacyKeepaliveHours 老配置里写死的 [3] 必须自动升级。
//
// [3] 是历史默认值（每天只检查一次），配上 6h 续期窗口必然漏判；升级后
// 老实例不改配置也能恢复覆盖。用户自己填的组合（如 [3,15]）不能被吞掉。
func TestParseConfigMigratesLegacyKeepaliveHours(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"schedule":{"keepalive_hours":[3]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Schedule.KeepaliveHours) != 24 {
		t.Fatalf("旧默认 [3] 应迁移成每小时检查，得到 %v", cfg.Schedule.KeepaliveHours)
	}

	cfg, err = ParseConfig([]byte(`{"schedule":{"keepalive_hours":[3,15]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Schedule.KeepaliveHours) != 2 || cfg.Schedule.KeepaliveHours[0] != 3 || cfg.Schedule.KeepaliveHours[1] != 15 {
		t.Fatalf("自定义小时被改写: %v", cfg.Schedule.KeepaliveHours)
	}
}

func TestParseConfigRejectsBadDuration(t *testing.T) {
	if _, err := ParseConfig([]byte(`{"cooldown":{"soft_rate":"一分钟"}}`)); err == nil {
		t.Fatal("非法时长应当报错")
	}
}

func TestParseConfigRejectsBadHour(t *testing.T) {
	if _, err := ParseConfig([]byte(`{"schedule":{"checkin_hours":[25]}}`)); err == nil {
		t.Fatal("非法小时应当报错")
	}
}

func TestSaveConfigMergesUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	// 原文件带一个未来版本才有的键与一个注释键。
	if err := os.WriteFile(path, []byte(`{"listen":":7867","_comment":"手写说明","future_key":{"a":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 面板只提交它管理的字段。
	next, restart, err := SaveConfig(path, []byte(`{"api_key":"sk-test","listen":":9999","models":{"include_upstream_hidden":false}}`))
	if err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if next.APIKey != "sk-test" {
		t.Fatalf("api_key = %q", next.APIKey)
	}
	// 只列真正变化的需重启字段：listen 变了要提示，api_key 是热改不该出现。
	var hasListen, hasAPIKey, hasUnlisted bool
	for _, field := range restart {
		switch field {
		case "listen":
			hasListen = true
		case "api_key":
			hasAPIKey = true
		case "models.include_upstream_hidden":
			hasUnlisted = true
		}
	}
	if !hasListen {
		t.Fatalf("listen 改动应当提示重启: %v", restart)
	}
	if hasAPIKey {
		t.Fatalf("api_key 是热改字段，不该出现在重启清单: %v", restart)
	}
	// 这个开关是启动时注入 handler 的，改了必须重启——不提示就是在骗用户。
	if !hasUnlisted {
		t.Fatalf("models.include_upstream_hidden 改动应当提示重启: %v", restart)
	}
	if next.Models.IncludeUpstreamHidden == nil || *next.Models.IncludeUpstreamHidden {
		t.Fatalf("新值没生效: %+v", next.Models.IncludeUpstreamHidden)
	}
	rawBytes, _ := os.ReadFile(path)
	raw := string(rawBytes)
	if !strings.Contains(raw, "future_key") || !strings.Contains(raw, "_comment") {
		t.Fatalf("未知键被洗掉了:\n%s", raw)
	}
	if !strings.Contains(raw, "sk-test") {
		t.Fatalf("新值没写进去:\n%s", raw)
	}
	// 落盘权限：配置里有密钥。
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("配置权限 = %o, want 600", mode)
	}
}

func TestMergeMapsDeep(t *testing.T) {
	cur := map[string]any{"a": map[string]any{"x": 1, "y": 2}}
	in := map[string]any{"a": map[string]any{"y": 3}}
	out := mergeMaps(cur, in)
	nested := out["a"].(map[string]any)
	if nested["x"] != 1 || nested["y"] != 3 {
		t.Fatalf("深合并结果不对: %v", nested)
	}
}

// 别名表是「集合型」段：面板里删掉一行，保存后必须消失。
// 以前走深合并，删掉的行会被磁盘旧值补回来（「只能加不能删」）。
func TestSaveConfigDeletesAliasEntry(t *testing.T) {
	path := t.TempDir() + "/config.json"
	if err := os.WriteFile(path, []byte(`{"api_key":"sk-test","aliases":{"gpt-4o":"raccoon-a","claude-3":"raccoon-b"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	next, _, err := SaveConfig(path, []byte(`{"api_key":"sk-test","aliases":{"gpt-4o":"raccoon-a"}}`))
	if err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if _, ok := next.Aliases["claude-3"]; ok {
		t.Fatalf("删掉的别名又回来了: %v", next.Aliases)
	}
	if next.Aliases["gpt-4o"] != "raccoon-a" {
		t.Fatalf("保留的别名不对: %v", next.Aliases)
	}
}

// JSON 编辑器走 replace 语义：整份提交，没提到的已知键就是删除。
func TestReplaceConfigDropsOmittedKeys(t *testing.T) {
	path := t.TempDir() + "/config.json"
	cur := `{"api_key":"sk-test","listen":":7867","cooldown":{"plan_credit":"36h"},` +
		`"_comment":"手写说明","upstream":{"user_agent":"UA-X","_comment":"段内说明"}}`
	if err := os.WriteFile(path, []byte(cur), 0o600); err != nil {
		t.Fatal(err)
	}
	// 提交里故意不带 cooldown 与 upstream.user_agent。
	next, _, err := ReplaceConfig(path, []byte(`{"api_key":"sk-test","listen":":7867","upstream":{"timeout_seconds":30}}`))
	if err != nil {
		t.Fatalf("ReplaceConfig: %v", err)
	}
	// 删掉的段回到默认值（plan_credit 默认 12h），不是保留提交里已被删掉的 36h。
	if next.Cooldown.PlanCredit == "36h" {
		t.Fatalf("提交里没有的段被旧值补回来了: %q", next.Cooldown.PlanCredit)
	}
	if next.Upstream.UserAgent != "" {
		t.Fatalf("提交里没有的字段应当删掉，实得 %q", next.Upstream.UserAgent)
	}
	rawBytes, _ := os.ReadFile(path)
	raw := string(rawBytes)
	// 未知键（注释）连同一层子键都要保住，否则用户的说明会被吃掉。
	if !strings.Contains(raw, "手写说明") || !strings.Contains(raw, "段内说明") {
		t.Fatalf("注释键被洗掉了:\n%s", raw)
	}
}

// 手滑把 api_key 删掉不该导致全客户端 401：沿用磁盘旧值。
func TestReplaceConfigKeepsAPIKeyWhenOmitted(t *testing.T) {
	path := t.TempDir() + "/config.json"
	if err := os.WriteFile(path, []byte(`{"api_key":"sk-keep-me","listen":":7867"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	next, _, err := ReplaceConfig(path, []byte(`{"listen":":7867"}`))
	if err != nil {
		t.Fatalf("ReplaceConfig: %v", err)
	}
	if next.APIKey != "sk-keep-me" {
		t.Fatalf("api_key 被换掉了: %q", next.APIKey)
	}
	if _, _, err := ReplaceConfig(path, []byte(`{}`)); err == nil {
		t.Fatal("空提交在整份覆盖模式下应当被拒")
	}
}

func TestCallbackBase(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0:7867":     "http://127.0.0.1:7867",
		":7867":            "http://127.0.0.1:7867",
		"127.0.0.1:9000":   "http://127.0.0.1:9000",
		"192.168.1.5:7867": "",
	}
	for listen, want := range cases {
		if got := callbackBase(listen); got != want {
			t.Fatalf("callbackBase(%q) = %q, want %q", listen, got, want)
		}
	}
}

func TestResolveCallbackBase(t *testing.T) {
	cases := []struct {
		name   string
		listen string
		config string
		want   string
	}{
		// 配置优先：线上部署填域名，忽略监听地址。
		{"配置域名", "0.0.0.0:7867", "https://xhx2api.1task.cn", "https://xhx2api.1task.cn"},
		{"配置带尾斜杠", "0.0.0.0:7867", "https://xhx2api.1task.cn/", "https://xhx2api.1task.cn"},
		// 未配置：按 listen 推导。
		{"本机通配", "0.0.0.0:7867", "", "http://127.0.0.1:7867"},
		{"本机回环", "127.0.0.1:9000", "", "http://127.0.0.1:9000"},
		// 监听在具体非环回 IP 且没配域名：推导为空，面板会提示改用粘贴。
		{"局域网 IP", "192.168.1.5:7867", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaults()
			cfg.Listen = tc.listen
			cfg.Login.CallbackBase = tc.config
			if got := resolveCallbackBase(cfg); got != tc.want {
				t.Fatalf("resolveCallbackBase = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMaskKey(t *testing.T) {
	if maskKey("") != "(空：不鉴权)" {
		t.Fatal("空密钥提示不对")
	}
	if got := maskKey("sk-xhx-1234567890abcdef"); strings.Contains(got, "567890abcdef") {
		t.Fatalf("密钥没脱敏: %q", got)
	}
}

// ---------------------------------------------------------------------------
// 生图默认返回 url

// TestImageDefaultsToURL 守「默认就回 url」这条产品口径。
//
// 上游只回 base64，一张 4K 图 8MB+，直接丢给客户端/工作流很难用。
// 所以 return_url 默认开、默认形态 url。
func TestImageDefaultsToURL(t *testing.T) {
	c, err := ParseConfig([]byte(`{"listen":"127.0.0.1:1"}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if c.Images.ReturnURL == nil || !*c.Images.ReturnURL {
		t.Fatalf("return_url 默认应为 true，得到 %v", c.Images.ReturnURL)
	}
	if !c.ImageURLEnabled() {
		t.Fatal("默认应当具备 url 能力")
	}
	if got := c.ImageDefaultFormat(); got != "url" {
		t.Fatalf("默认形态应当是 url，得到 %q", got)
	}
	if got := c.ImageMaxTotalMB(); got != 2048 {
		t.Fatalf("落盘上限默认 2048MB，得到 %d", got)
	}
}

// TestImageExplicitOptOut 守「显式关掉要真的关掉」：*bool 三态不能被零值吞掉。
func TestImageExplicitOptOut(t *testing.T) {
	c, err := ParseConfig([]byte(`{"listen":"127.0.0.1:1","images":{"return_url":false}}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if c.ImageURLEnabled() {
		t.Fatal("显式 return_url=false 之后不该再有 url 能力")
	}
	if got := c.ImageDefaultFormat(); got != "b64_json" {
		t.Fatalf("没有 url 能力时必须回落 b64_json，得到 %q", got)
	}

	// response_format=url 但没有 url 能力：不该假装能给，同样回落 b64。
	c2, err := ParseConfig([]byte(`{"listen":"127.0.0.1:1","images":{"return_url":false,"response_format":"url"}}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := c2.ImageDefaultFormat(); got != "b64_json" {
		t.Fatalf("没有 url 能力时 response_format=url 也应回落，得到 %q", got)
	}
}

func TestImageResponseFormatValues(t *testing.T) {
	cases := map[string]string{
		`"url"`:      "url",
		`"b64_json"`: "b64_json",
		`"both"`:     "both",
		`"URL"`:      "url", // 大小写不敏感
		`"  both "`:  "both",
	}
	for raw, want := range cases {
		c, err := ParseConfig([]byte(`{"listen":"127.0.0.1:1","images":{"response_format":` + raw + `}}`))
		if err != nil {
			t.Fatalf("%s 解析失败: %v", raw, err)
		}
		if got := c.ImageDefaultFormat(); got != want {
			t.Fatalf("response_format=%s 得到 %q，期望 %q", raw, got, want)
		}
	}

	// 写错值必须报错：静默回落会让「配了 both 却只拿到 url」无从排查。
	if _, err := ParseConfig([]byte(`{"listen":"127.0.0.1:1","images":{"response_format":"png"}}`)); err == nil {
		t.Fatal("非法的 response_format 应当被拒绝")
	}
	// 负数上限同理（0 才是「不限制」）。
	if _, err := ParseConfig([]byte(`{"listen":"127.0.0.1:1","images":{"max_total_mb":-1}}`)); err == nil {
		t.Fatal("负的 max_total_mb 应当被拒绝")
	}
}

// TestImageRestartFields 守「改了要提示重启」：这些值都是启动时注入 handler 的。
func TestImageRestartFields(t *testing.T) {
	a, _ := ParseConfig([]byte(`{"listen":"127.0.0.1:1"}`))
	b, _ := ParseConfig([]byte(`{"listen":"127.0.0.1:1","images":{"response_format":"both","return_url":false,"max_total_mb":10}}`))
	got := map[string]bool{}
	for _, f := range changedRestartFields(a, b) {
		got[f] = true
	}
	for _, want := range []string{"images.return_url", "images.response_format", "images.max_total_mb"} {
		if !got[want] {
			t.Fatalf("改 %s 应当提示重启，实际 %v", want, got)
		}
	}
}

// TestParseConfigAcceptsDeprecatedImagesModel 守「删掉的键不会把老实例拦在启动前」。
//
// images.model 从来没被读过（上游生图只有一条通道、model 被完全忽略），删掉它是
// 清死配置；但 ParseConfig 是严格解析（DisallowUnknownFields），删字段还让老配置
// 报「配置项错误」，就等于升级即停机。所以这个键要在严格解析之前被摘掉，且同段的
// 其它键照旧生效。
func TestParseConfigAcceptsDeprecatedImagesModel(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"api_key":"sk-x","images":{"enabled":true,"model":"raccoon-4eb26a","timeout_seconds":321}}`))
	if err != nil {
		t.Fatalf("老配置里的 images.model 不该报错: %v", err)
	}
	if cfg.Images.TimeoutSeconds != 321 {
		t.Fatalf("同段其它键应当照常解析，timeout_seconds = %d", cfg.Images.TimeoutSeconds)
	}
	if cfg.Images.Enabled == nil || !*cfg.Images.Enabled {
		t.Fatalf("enabled 丢了: %+v", cfg.Images.Enabled)
	}
}

// TestParseConfigKeepsRejectingUnknownKeys 是上面那条的对照：摘废弃键不能把严格
// 校验一起放开——写错键名（modle）仍然要报错并点出键名。
func TestParseConfigKeepsRejectingUnknownKeys(t *testing.T) {
	_, err := ParseConfig([]byte(`{"images":{"enabled":true,"modle":"x"}}`))
	if err == nil || !strings.Contains(err.Error(), "modle") {
		t.Fatalf("未知键仍应报错并点出键名: %v", err)
	}
}

// TestParseConfigDeprecatedKeyWrongParentType：images 写成字符串是真正的配置错误，
// 不能被「摘废弃键」顺手吞掉。
func TestParseConfigDeprecatedKeyWrongParentType(t *testing.T) {
	if _, err := ParseConfig([]byte(`{"images":"oops"}`)); err == nil {
		t.Fatal("images 不是对象时应当报错")
	}
}

// TestSaveConfigDropsDeprecatedImagesModel：面板保存一次就把死键从磁盘上抹掉。
//
// 合并语义（SaveConfig）会从旧文件里把它原样带回来，不显式摘掉的话「删掉死配置」
// 对老实例永远不生效：表单早就不提交它了，文件里却一直留着。
func TestSaveConfigDropsDeprecatedImagesModel(t *testing.T) {
	path := t.TempDir() + "/config.json"
	old := `{"api_key":"sk-old","images":{"enabled":true,"model":"raccoon-4eb26a","timeout_seconds":600}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SaveConfig(path, []byte(`{"images":{"timeout_seconds":600}}`)); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	images, _ := got["images"].(map[string]any)
	if images == nil {
		t.Fatalf("images 段丢了:\n%s", raw)
	}
	if _, ok := images["model"]; ok {
		t.Fatalf("废弃键应当从磁盘上抹掉:\n%s", raw)
	}
	if _, ok := images["enabled"]; !ok {
		t.Fatalf("同段其它键被洗掉了（合并语义不能吃掉整段）:\n%s", raw)
	}
	if got["api_key"] == nil {
		t.Fatalf("api_key 丢了:\n%s", raw)
	}
}
