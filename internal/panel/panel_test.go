package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
	"github.com/rsfree/xhx2api/internal/reqlog"
	"github.com/rsfree/xhx2api/internal/upstream"
)

// TestMergeExistingKeepsRefresh 守卫「覆盖导入不降级」。
//
// 真实场景：同一个号先用浏览器授权导入（有 refresh_token，能续期），
// 后来又从客户端目录扫了一次（IAM 票，不带 refresh_token）。直接覆盖会让这个
// 号从「能续期」退化成「30 天后失效」，而用户只是想刷新票。
func TestMergeExistingKeepsRefresh(t *testing.T) {
	existing := &auth.Auth{FilePath: "/data/auths/7772869.json", Data: auth.Data{
		UID: "7772869", Nickname: "RaccoonAva",
		AccessToken: "old", RefreshToken: "rt-old",
	}}
	incoming := &auth.Auth{Data: auth.Data{UID: "7772869", AccessToken: "new-iam"}}

	mergeExisting(incoming, existing)
	if incoming.RefreshToken != "rt-old" {
		t.Fatalf("refresh_token 丢了: %q", incoming.RefreshToken)
	}
	if incoming.FilePath != existing.FilePath {
		t.Fatalf("文件路径没沿用: %q", incoming.FilePath)
	}
	if incoming.Nickname != "RaccoonAva" {
		t.Fatalf("昵称为空时应沿用旧值: %q", incoming.Nickname)
	}
	if incoming.AccessToken != "new-iam" {
		t.Fatalf("新票应当保留，得到 %q", incoming.AccessToken)
	}
}

// TestMergeExistingKeepsSnapshot 守卫「覆盖导入不抹掉查询结果」。
//
// 实测踩过：从客户端目录覆盖导入同一个号后，面板显示 0 分、未签到——
// 因为新构造的 Auth 只有凭证字段，Save 把 credit/checked_in 全写成零值。
func TestMergeExistingKeepsSnapshot(t *testing.T) {
	existing := &auth.Auth{Data: auth.Data{
		UID: "7772869", Credit: 5848, CreditAt: "2026-10-26T11:06:39+08:00",
		CheckedIn: "2026-10-26", ImportedAt: "2026-10-26T09:45:00+08:00",
		RegisterAt: "2026-10-26T09:42:45+08:00", RefreshToken: "rt",
	}}
	incoming := &auth.Auth{Data: auth.Data{UID: "7772869", AccessToken: "new"}}
	mergeExisting(incoming, existing)

	if incoming.Credit != 5848 {
		t.Fatalf("余额被抹掉了: %d", incoming.Credit)
	}
	if incoming.CheckedIn != "2026-10-26" {
		t.Fatalf("签到状态被抹掉了: %q", incoming.CheckedIn)
	}
	if incoming.ImportedAt == "" {
		t.Fatal("进池时间被抹掉了（会让新号静默期失效）")
	}
	if incoming.CreditAt == "" || incoming.RegisterAt == "" {
		t.Fatal("credit_at / register_at 被抹掉了")
	}
}

// TestMergeExistingPrefersIncoming 新凭证自带 refresh_token 时以新的为准
// （重新登录拿到的 refresh 更可信）。
func TestMergeExistingPrefersIncoming(t *testing.T) {
	existing := &auth.Auth{Data: auth.Data{UID: "1", RefreshToken: "rt-old", Nickname: "老名字"}}
	incoming := &auth.Auth{Data: auth.Data{UID: "1", RefreshToken: "rt-new", Nickname: "新名字"}}

	mergeExisting(incoming, existing)
	if incoming.RefreshToken != "rt-new" {
		t.Fatalf("应当以新 refresh_token 为准，得到 %q", incoming.RefreshToken)
	}
	if incoming.Nickname != "新名字" {
		t.Fatalf("非空昵称不该被覆盖: %q", incoming.Nickname)
	}
}

func TestMergeExistingNilExisting(t *testing.T) {
	incoming := &auth.Auth{Data: auth.Data{UID: "1", AccessToken: "t"}}
	mergeExisting(incoming, nil)
	if incoming.AccessToken != "t" {
		t.Fatal("nil existing 不该改动入参")
	}
}

func TestMaskPhone(t *testing.T) {
	if got := maskPhone("18550288233"); got != "*******8233" {
		t.Fatalf("maskPhone = %q", got)
	}
	if got := maskPhone("123"); got != "123" {
		t.Fatalf("短号不该掩码: %q", got)
	}
}

// TestModelsPayloadMarksAliasesAndOrphans 守卫模型页的三件事：
//  1. 别名要挂在目标模型上（用户才知道「我该填哪个名字」）；
//  2. 默认模型要标出来；
//  3. 别名指向目录里没有的模型时要单独列出来（实测这类目标仍可调用，
//     如 raccoon-chat-ml-5-5，但用户在列表里看不到它）。
func TestModelsPayloadMarksAliasesAndOrphans(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		switch r.URL.Path {
		case "/api/web/llm/v2/model_catalog":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"categories":[{"type":"对话","models":[
				{"name":"model-a","description":"主模型","visible":true,"context_window":1000000,"max_tokens":100000,"billing_multiplier":1},
				{"name":"model-b","description":"上游未列出","visible":false,"context_window":100000,"max_tokens":8000,"billing_multiplier":0}
			]}]}}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	defer srv.Close()

	up := upstream.New()
	up.SetAgentHosts(srv.URL+"/api/web/llm/v2", srv.URL+"/api/web/auth/v1")
	up.SetAPIHosts(srv.URL+"/api/web/points/v1", srv.URL)
	a := &auth.Auth{Data: auth.Data{UID: "1", AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}}
	if _, err := up.RefreshModels(context.Background(), a, true); err != nil {
		t.Fatalf("刷新目录: %v （请求路径: %v）", err, seen)
	}

	p := New(Config{
		Upstream: up, DefaultModel: "model-a",
		Aliases: map[string]string{
			"gpt-4o": "model-a", "o3": "model-a",
			"claude-x": "ghost-model", // 目录里没有，但实测这类目标仍可调用
		},
	})
	rec := httptest.NewRecorder()
	p.models(rec, httptest.NewRequest(http.MethodGet, "/panel/api/models", nil))
	if rec.Code != 200 {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Models []struct {
			ID       string   `json:"id"`
			Aliases  []string `json:"aliases"`
			Default  bool     `json:"default"`
			Unlisted bool     `json:"unlisted"`
			Hidden   bool     `json:"hidden"`
		} `json:"models"`
		DefaultModel string `json:"default_model"`
		Orphans      []struct {
			Alias  string `json:"alias"`
			Target string `json:"target"`
		} `json:"orphans"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析响应: %v (%s)", err, rec.Body.String())
	}
	if payload.DefaultModel != "model-a" {
		t.Fatalf("default_model = %q", payload.DefaultModel)
	}
	if len(payload.Models) != 2 {
		t.Fatalf("应当返回 2 个模型，得到 %d", len(payload.Models))
	}
	a0, b0 := payload.Models[0], payload.Models[1]
	if !a0.Default || a0.ID != "model-a" {
		t.Fatalf("model-a 应当是默认: %+v", a0)
	}
	if len(a0.Aliases) != 2 || a0.Aliases[0] != "gpt-4o" || a0.Aliases[1] != "o3" {
		t.Fatalf("别名应当排序挂在 model-a 上: %+v", a0.Aliases)
	}
	if !b0.Unlisted {
		t.Fatalf("visible=false 的模型应当标 unlisted: %+v", b0)
	}
	if len(payload.Orphans) != 1 || payload.Orphans[0].Alias != "claude-x" || payload.Orphans[0].Target != "ghost-model" {
		t.Fatalf("目录外的别名目标应当单独列出: %+v", payload.Orphans)
	}
}

// ---------------------------------------------------------------------------
// 面板静态结构（无浏览器也能守的回归）

// TestLoginModalOutsideApp 守「未登录时不能白屏」。
//
// 2026-10-26 线上实测：`#modal-login` 曾写在 `#app` 内部，而 showLogin() 的
// 第一行就是 `$("#app").hidden = true` —— 登录框跟着容器一起被隐藏，无 key
// 首次访问时整页纯白。本地一直用 `?key=` 直达，所以这条路径长期没被覆盖。
//
// 这里用标签配平算出 #app 的真实闭合位置，断言登录框在它之后 —— 纯字符串
// 操作，不依赖浏览器，CI 里几毫秒就能拦住同类改动。
func TestLoginModalOutsideApp(t *testing.T) {
	raw, err := panelFS.ReadFile("index.html")
	if err != nil {
		t.Fatalf("读取 index.html: %v", err)
	}
	s := string(raw)
	// 必须从 `<div` 起算：从 `id="app"` 起算会把「第一个内层 div」当容器，
	// 配平会提前结束（实测返回 240 字符处），测试就成了永远通过的空转。
	start := strings.Index(s, `<div id="app"`)
	if start < 0 {
		t.Fatal("index.html 里找不到 #app")
	}
	close := divCloseIndex(s, start)
	if close < 0 {
		t.Fatal("#app 的 <div> 没有配平（结构损坏）")
	}
	login := strings.Index(s, `id="modal-login"`)
	if login < 0 {
		t.Fatal("index.html 里找不到 #modal-login")
	}
	if login < close {
		t.Fatalf("#modal-login 在 #app 内部（app 开标签 %d、闭合 %d、登录框 %d）——"+
			"showLogin() 会先隐藏 #app，登录框跟着消失，未登录时页面纯白", start, close, login)
	}
	// 登录框自身初始必须是 hidden：带着遮罩渲染会在已登录场景闪一下。
	if !strings.Contains(s[login-40:login+40], "hidden") {
		t.Errorf("#modal-login 初始应带 hidden 属性")
	}
}

// divCloseIndex 从 start 处的 <div 开始做标签配平，返回配平后的位置（含 </div> 末尾）。
func divCloseIndex(s string, start int) int {
	depth := 0
	for i := start; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "<div"):
			depth++
			i += 4
		case strings.HasPrefix(s[i:], "</div>"):
			depth--
			i += 6
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return -1
}

// TestPanelHasFavicon 守「控制台不刷 404」：favicon 用内联 data URI，不额外发请求。
func TestPanelHasFavicon(t *testing.T) {
	raw, err := panelFS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `rel="icon"`) {
		t.Fatal("index.html 缺少 favicon，浏览器会自动请求 /favicon.ico 并得到 404")
	}
}

// TestRequestsSummaryIncludesUnmetered 守「新增指标不能漏进 overview」。
//
// 2026-11-21 真踩过：reqlog 加了 usage_missing/unmetered，面板 overview 的
// payload 是手写 map，没带 unmetered —— 前端「N 条未记账」永远不显示，
// 看起来像这个池子特别省 token。
func TestRequestsSummaryIncludesUnmetered(t *testing.T) {
	got := requestsSummary(reqlog.Metrics{Total: 3, TokensTotal: 120, Unmetered: 2})
	if got["unmetered"] != int64(2) {
		t.Fatalf("unmetered = %v，期望 2", got["unmetered"])
	}
	if got["tokens_total"] != int64(120) || got["total"] != int64(3) {
		t.Fatalf("其它指标被改坏了: %+v", got)
	}
}
