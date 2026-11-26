package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{401, `{}`, ErrSessionDead},
		{403, `{"message":"forbidden"}`, ErrSessionDead},
		{429, `{}`, ErrSoftRate},
		{504, `{}`, ErrGateway},
		{502, `{}`, ErrGateway},
		{500, `{}`, ErrServer},
		{503, `{}`, ErrServer},
		{404, `{}`, ErrNotFound},
		{400, `{"message":"invalid model"}`, ErrClient},
		// 额度文案：必须同时出现「额度类词 + 不足类词」才判欠费。
		{400, `{"message":"积分不足，请充值"}`, ErrPlanLimit},
		{429, `{"message":"quota exceeded"}`, ErrPlanLimit},
		{402, `{}`, ErrPlanLimit},
		// 关键反例：上下文超限不该判欠费。
		{400, `{"message":"context window is insufficient for this model"}`, ErrClient},
		// 关键反例：max_tokens 不该判登录态失效。
		{400, `{"message":"max_tokens exceeded"}`, ErrClient},
		// 排队：serviceAvailable:false。
		{200, `{"error":{"message":"busy","serviceAvailable":false}}`, ErrBusy},
		// 参数错误以 500 返回：必须归 client（不罚号），不能归 server（熔断）。
		{500, `{"error":{"message":"Unexpected keyword argument 'title'."}}`, ErrClient},
		{500, `{"message":"invalid parameter: max_tokens"}`, ErrClient},
	}
	for _, tc := range cases {
		got := Classify(tc.status, tc.body)
		if got != tc.want {
			t.Fatalf("Classify(%d, %q) = %s, want %s", tc.status, tc.body, got, tc.want)
		}
	}
}

func TestClassifyDoesNotBlameAccountForParams(t *testing.T) {
	// 这些报文历史上都曾把好号误伤成 12 小时冷却或自动禁用。
	for _, body := range []string{
		`{"message":"context length exceeded maximum"}`,
		`{"message":"insufficient context window"}`,
		`{"message":"max_tokens is too large"}`,
		`{"message":"invalid token in request body"}`,
	} {
		if kind := Classify(400, body); kind != ErrClient {
			t.Fatalf("%q 被判成 %s，会误伤账号", body, kind)
		}
	}
}

func TestParseSSELine(t *testing.T) {
	if ev := parseSSELine(""); ev.Kind != sseSkip {
		t.Fatalf("空行 = %v", ev.Kind)
	}
	if ev := parseSSELine("[DONE]"); ev.Kind != sseDone {
		t.Fatalf("[DONE] = %v", ev.Kind)
	}
	ev := parseSSELine(`{"id":"x","choices":[{"delta":{"content":"hi"}}]}`)
	if ev.Kind != sseChunk || ev.Chunk == nil {
		t.Fatalf("正常帧 = %+v", ev)
	}
	ev = parseSSELine(`{"error":{"message":"额度不足，积分耗尽","status":429}}`)
	if ev.Kind != sseError || ev.Class != ErrPlanLimit || ev.Status != 429 {
		t.Fatalf("错误帧 = %+v", ev)
	}
	// 业务码形式。
	ev = parseSSELine(`{"code":500,"message":"服务器开小差了"}`)
	if ev.Kind != sseError || ev.Message == "" {
		t.Fatalf("业务码帧 = %+v", ev)
	}
}

func TestAggregateCollectsTextReasoningAndTools(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"reasoning_content":"想一想"}}]}`,
		``,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"答案"}}]}`,
		``,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{\"a\":1}"}}]}}]}`,
		``,
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	obj, err := Aggregate(strings.NewReader(stream), "client-model")
	if err != nil {
		t.Fatal(err)
	}
	if obj["model"] != "client-model" {
		t.Fatalf("model 没被改写: %v", obj["model"])
	}
	choice := obj["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "答案" {
		t.Fatalf("content = %v", msg["content"])
	}
	if msg["reasoning_content"] != "想一想" {
		t.Fatalf("reasoning = %v", msg["reasoning_content"])
	}
	calls := msg["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %v", calls)
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "f" || fn["arguments"] != `{"a":1}` {
		t.Fatalf("function = %v", fn)
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	usage := obj["usage"].(map[string]any)
	if usage["total_tokens"].(float64) != 3 {
		t.Fatalf("usage = %v", usage)
	}
}

func TestAggregateEmptyStream(t *testing.T) {
	if _, err := Aggregate(strings.NewReader("data: [DONE]\n\n"), "m"); !errors.Is(err, ErrEmptyStream) {
		t.Fatalf("空流应当报 ErrEmptyStream, got %v", err)
	}
}

func TestParseCatalog(t *testing.T) {
	body := []byte(`{"code":0,"data":{"categories":[
	  {"type":"对话","models":[
	    {"name":"a","description":"A","visible":true,"context_window":"180000","max_tokens":"80000","tags":["thinking"],"billing_multiplier":1},
	    {"name":"b","visible":false,"tags":["vision"]},
	    {"name":"a","description":"重复"}
	  ]},
	  {"type":"图像","models":[{"name":"c","params":{"context_window":8000},"billing_status":"limited_free","billing_effective_multiplier":0,"billing_multiplier":2}]}
	]}}`)
	models, err := parseCatalog(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("模型数 = %d（去重/去空失败）: %+v", len(models), models)
	}
	a := models[0]
	if a.ID != "a" || a.ContextWindow != 180000 || a.MaxOutput != 80000 || !a.Reasoning {
		t.Fatalf("a = %+v", a)
	}
	if models[1].Visible {
		t.Fatal("visible:false 应当保留但标记不可见")
	}
	if !models[1].VL {
		t.Fatal("vision 标签没识别")
	}
	c := models[2]
	if c.ContextWindow != 8000 {
		t.Fatalf("params.context_window 没读到: %+v", c)
	}
	if c.Credits != "x0" || c.BaseCredits != "x2" {
		t.Fatalf("限免标记不对: %+v", c)
	}
}

func TestParseCatalogEmpty(t *testing.T) {
	if _, err := parseCatalog([]byte(`{"code":0,"data":{"categories":[]}}`)); err == nil {
		t.Fatal("空目录应当报错")
	}
}

// fakeUpstream 起一个假上游，path → handler。
func fakeUpstream(t *testing.T, handlers map[string]http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	for path, h := range handlers {
		mux.HandleFunc(path, h)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := New()
	c.SetAgentHosts(srv.URL+"/llm", srv.URL+"/auth")
	c.SetAPIHosts(srv.URL+"/points", srv.URL)
	c.SetPersistHook(func(a *auth.Auth) error { return nil })
	return c, srv
}

func TestUserEntUsageDetail(t *testing.T) {
	c, _ := fakeUpstream(t, map[string]http.HandlerFunc{
		"/points/balance": func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				t.Error("缺少鉴权头")
			}
			fmt.Fprint(w, `{"code":0,"data":{"available_points":1500,"daily_points":300,"monthly_points":1200,"reward_points":0,"topup_points":0}}`)
		},
	})
	a := &auth.Auth{Data: auth.Data{UID: "u", AccessToken: "tok"}}
	usage, err := c.UserEntUsageDetail(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Remain != 1500 || usage.Daily != 300 || usage.Monthly != 1200 {
		t.Fatalf("usage = %+v", usage)
	}
	if a.Snapshot().Credit != 1500 {
		t.Fatal("余额没写回账号")
	}
}

func TestBalancePackagesSkipsEmptyBuckets(t *testing.T) {
	c, _ := fakeUpstream(t, map[string]http.HandlerFunc{
		"/points/balance": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"code":0,"data":{"available_points":500,"daily_points":500,"monthly_points":0,"reward_points":0,"topup_points":0}}`)
		},
		"/points/bills": func(w http.ResponseWriter, r *http.Request) {
			future := time.Now().Add(48 * time.Hour).Format(time.RFC3339)
			fmt.Fprintf(w, `{"code":0,"data":{"items":[{"biz_type":"daily_grant","points":300,"expire_at":"%s"}]}}`, future)
		},
	})
	a := &auth.Auth{Data: auth.Data{UID: "u", AccessToken: "tok"}}
	packs, usage, err := c.BalancePackages(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) != 1 {
		t.Fatalf("全 0 桶不该输出: %+v", packs)
	}
	if packs[0].Name != "每日积分" || packs[0].Expire == 0 {
		t.Fatalf("批次到期没取到: %+v", packs[0])
	}
	if usage.Remain != 500 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestDailyGrantToday(t *testing.T) {
	today := time.Now().In(shanghaiTZ).Format("2006-01-02")
	c, _ := fakeUpstream(t, map[string]http.HandlerFunc{
		"/points/bills": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"code":0,"data":{"items":[{"biz_type":"daily_grant","points":300,"grant_at":"%sT00:00:01+08:00"}]}}`, today)
		},
	})
	a := &auth.Auth{Data: auth.Data{UID: "u", AccessToken: "tok"}}
	got, ever, err := c.DailyGrant(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if !got || !ever {
		t.Fatalf("今天有 daily_grant 应当 today=true ever=true, got %v/%v", got, ever)
	}
}

func TestDailyGrantIneligible(t *testing.T) {
	// 只有 reward_grant：这个号没有每日积分资格。
	c, _ := fakeUpstream(t, map[string]http.HandlerFunc{
		"/points/bills": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"code":0,"data":{"items":[{"biz_type":"reward_grant","points":100}]}}`)
		},
	})
	a := &auth.Auth{Data: auth.Data{UID: "u", AccessToken: "tok"}}
	got, ever, err := c.DailyGrant(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if got || ever {
		t.Fatalf("应当 today=false ever=false, got %v/%v", got, ever)
	}
}

// 签到判定必须走 type=income。
//
// 回归的是 2026-11-02 线上实测的一个谎报：上游 paging.offset 是死参数，
// type=all 只会返回最近 100 条；重度账号一天几十上百条消费，把 100 条窗口
// 压到只剩 1.5 天，于是 10-29~10-31 连续三天拿到 daily_grant 的号被报成
// 「历史上从未获得每日积分」，直接按无资格跳过、永远不再签。
func TestDailyGrantUsesIncomeQuery(t *testing.T) {
	var gotQuery string
	c, _ := fakeUpstream(t, map[string]http.HandlerFunc{
		"/points/bills": func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			fmt.Fprint(w, `{"code":0,"data":{"items":[]}}`)
		},
	})
	a := &auth.Auth{Data: auth.Data{UID: "u", AccessToken: "tok"}}
	if _, _, err := c.DailyGrant(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "type=income") {
		t.Fatalf("判签到必须查 type=income（不受消费流水截断），实际 query = %q", gotQuery)
	}
	if strings.Contains(gotQuery, "type=all") {
		t.Fatalf("type=all 的 100 条窗口会把历史 daily_grant 挤掉，实际 query = %q", gotQuery)
	}
}

func TestRefreshSingleFlight(t *testing.T) {
	var hits atomic.Int64
	c, _ := fakeUpstream(t, map[string]http.HandlerFunc{
		"/auth/refresh": func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			time.Sleep(50 * time.Millisecond)
			fmt.Fprint(w, `{"code":0,"data":{"access_token":"new-token","refresh_token":"new-refresh","expires_at":9999999999999}}`)
		},
	})
	a := &auth.Auth{Data: auth.Data{UID: "u", AccessToken: "old", RefreshToken: "ref"}}
	done := make(chan struct{}, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_ = c.RefreshToken(context.Background(), a)
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if hits.Load() != 1 {
		t.Fatalf("并发续期应当只打一次上游，实际 %d 次", hits.Load())
	}
	if a.JWT() != "new-token" {
		t.Fatalf("access = %q", a.JWT())
	}
	if a.RefreshTokenValue() != "new-refresh" {
		t.Fatalf("refresh 没轮换: %q", a.RefreshTokenValue())
	}
}

func TestRefreshKeepsOldRefreshWhenUpstreamOmit(t *testing.T) {
	c, _ := fakeUpstream(t, map[string]http.HandlerFunc{
		"/auth/refresh": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"code":0,"data":{"access_token":"new-token"}}`)
		},
	})
	a := &auth.Auth{Data: auth.Data{UID: "u", RefreshToken: "keep-me"}}
	if err := c.RefreshToken(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if a.RefreshTokenValue() != "keep-me" {
		t.Fatalf("上游没给新 refresh，旧值被覆盖了: %q", a.RefreshTokenValue())
	}
}

func TestPrepareAccountFromToken(t *testing.T) {
	c, _ := fakeUpstream(t, map[string]http.HandlerFunc{
		"/auth/user_info": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"code":0,"data":{"id":"4465567","name":"小浣熊"}}`)
		},
	})
	account, err := c.PrepareAccount(context.Background(), map[string]any{
		"access_token": "tok", "refresh_token": "ref",
	})
	if err != nil {
		t.Fatal(err)
	}
	if account.UID != "4465567" || account.Nickname != "小浣熊" {
		t.Fatalf("account = %+v", account)
	}
}

func TestPrepareAccountExchangeCode(t *testing.T) {
	c, _ := fakeUpstream(t, map[string]http.HandlerFunc{
		"/auth/login_with_authorization_code": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["authorization_code"] != "ac_123" {
				t.Errorf("authorization_code = %q", body["authorization_code"])
			}
			fmt.Fprint(w, `{"code":0,"data":{"access_token":"tok","refresh_token":"ref"}}`)
		},
		"/auth/user_info": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"code":0,"data":{"id":"99","name":"换码号"}}`)
		},
	})
	account, err := c.PrepareAccount(context.Background(), map[string]any{
		"callback": "office-raccoon://auth/callback?code=ac_123&state=abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if account.UID != "99" || account.Nickname != "换码号" {
		t.Fatalf("account = %+v", account)
	}
}

func TestPrepareAccountExpiredCode(t *testing.T) {
	c, _ := fakeUpstream(t, map[string]http.HandlerFunc{
		"/auth/login_with_authorization_code": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"code":200035,"message":"authorization code expired"}`)
		},
	})
	_, err := c.PrepareAccount(context.Background(), map[string]any{"code": "ac_old"})
	if !errors.Is(err, ErrCodeExpired) {
		t.Fatalf("应当返回 ErrCodeExpired, got %v", err)
	}
}

// TestValidCallback 覆盖「本地环回 + 线上域名」两种部署形态。
// 域名回调是 2026-10-26 实测确认的：上游 authorize 页原样透传域名 redirect。
func TestValidCallback(t *testing.T) {
	ok := []string{
		"http://127.0.0.1:7867/auth/callback",
		"http://127.0.0.1:1/auth/callback",
		"https://xhx2api.1task.cn/auth/callback",
		"http://10.0.0.5:7867/auth/callback",
	}
	for _, raw := range ok {
		if !validCallback(raw) {
			t.Fatalf("%q 应当合法", raw)
		}
	}
	bad := []string{
		"ftp://127.0.0.1:7867/auth/callback",
		"https://xhx2api.1task.cn/other",
		"https://xhx2api.1task.cn",
		"not a url",
		"/auth/callback",
	}
	for _, raw := range bad {
		if validCallback(raw) {
			t.Fatalf("%q 不该通过", raw)
		}
	}
}

func TestParseCallbackCode(t *testing.T) {
	code, state, ok := ParseCallbackCode("office-raccoon://auth/callback?code=ac_1&state=xyz")
	if !ok || code != "ac_1" || state != "xyz" {
		t.Fatalf("got %q %q %v", code, state, ok)
	}
	// 环回回调。
	code, state, ok = ParseCallbackCode("http://127.0.0.1:7867/auth/callback?code=ac_2&state=s2")
	if !ok || code != "ac_2" || state != "s2" {
		t.Fatalf("环回 got %q %q %v", code, state, ok)
	}
	// 裸授权码。
	if code, _, ok := ParseCallbackCode("ac_3"); !ok || code != "ac_3" {
		t.Fatal("裸授权码应当被认")
	}
	// 别的链接不该被当回调。
	if _, _, ok := ParseCallbackCode("https://example.com/other?code=x"); ok {
		t.Fatal("非回调路径不该通过")
	}
}

func TestPeekReaderReplaysBytes(t *testing.T) {
	body := "data: {\"a\":1}\n\ndata: [DONE]\n\n"
	src := &nopCloser{strings.NewReader(body)}
	pr := newPeekReader(src)
	ev, err := pr.peekFrame()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != sseChunk {
		t.Fatalf("首帧 = %+v", ev)
	}
	// 重放后应当一个字节都不少。
	replayed, err := readAllString(pr.stream())
	if err != nil {
		t.Fatal(err)
	}
	if replayed != body {
		t.Fatalf("重放内容不完整:\n got %q\nwant %q", replayed, body)
	}
}

func TestChatStreamNonStreamJSONWrapped(t *testing.T) {
	c, _ := fakeUpstream(t, map[string]http.HandlerFunc{
		"/llm/chat/completions": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
		},
	})
	a := &auth.Auth{Data: auth.Data{UID: "u", AccessToken: "tok"}}
	stream, status, _, err := c.ChatStream(context.Background(), a, []byte(`{"model":"m"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	obj, err := Aggregate(stream, "client")
	if err != nil {
		t.Fatal(err)
	}
	msg := obj["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "hi" {
		t.Fatalf("content = %v", msg["content"])
	}
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
}

func TestChatStreamFirstFrameError(t *testing.T) {
	c, _ := fakeUpstream(t, map[string]http.HandlerFunc{
		"/llm/chat/completions": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `data: {"error":{"message":"积分不足","status":429}}`+"\n\n")
		},
	})
	a := &auth.Auth{Data: auth.Data{UID: "u", AccessToken: "tok"}}
	_, _, _, err := c.ChatStream(context.Background(), a, []byte(`{"model":"m"}`), "")
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("应当返回 *Error, got %T %v", err, err)
	}
	if ue.Kind != ErrPlanLimit {
		t.Fatalf("kind = %s", ue.Kind)
	}
}

// ---------------------------------------------------------------------------
// 测试辅助

type nopCloser struct{ *strings.Reader }

func (n *nopCloser) Close() error { return nil }

func readAllString(r interface{ Read([]byte) (int, error) }) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			if err.Error() == "EOF" {
				return sb.String(), nil
			}
			return sb.String(), err
		}
	}
}

// ---------------------------------------------------------------------------
// 生图别名（2026-10-26 实测：上游 /images/gen 忽略 model）

func TestEffectiveImageAliases(t *testing.T) {
	// nil = 没配置 → 默认表（gpt-image-* / dall-e-* 全在）。
	def := EffectiveImageAliases(nil)
	if len(def) != len(ImageAliases) {
		t.Fatalf("nil 应当回默认表，得到 %v", def)
	}
	for _, want := range []string{"gpt-image-1", "gpt-image-1-mini", "gpt-image-2", "gpt-image-2.5", "dall-e-2", "dall-e-3"} {
		if !containsFold(def, want) {
			t.Fatalf("默认表里缺 %s: %v", want, def)
		}
	}

	// 显式空数组 = 一个别名都不要（必须能与 nil 区分开）。
	if got := EffectiveImageAliases([]string{}); len(got) != 0 {
		t.Fatalf("[] 应当得到空表，得到 %v", got)
	}

	// 自定义：去空白、去重（大小写不敏感）、并剔除与合成 id 撞名的。
	got := EffectiveImageAliases([]string{" my-image ", "MY-IMAGE", "", "raccoon-image-gen"})
	if len(got) != 1 || got[0] != "my-image" {
		t.Fatalf("去重/去空白不对: %v", got)
	}
}

func TestImageModelIDsAndIsImageModel(t *testing.T) {
	ids := ImageModelIDs(nil)
	if len(ids) == 0 || ids[0] != ImageModelID {
		t.Fatalf("合成 id 必须排第一，得到 %v", ids)
	}
	if ImageModelIDs([]string{})[0] != ImageModelID {
		t.Fatal("关掉别名时也要保留合成 id")
	}

	if !IsImageModel("raccoon-image-gen", nil) {
		t.Fatal("合成 id 应当算生图模型")
	}
	if !IsImageModel("GPT-Image-2", nil) {
		t.Fatal("别名判定应当大小写不敏感")
	}
	if IsImageModel("gpt-image-2", []string{}) {
		t.Fatal("aliases=[] 后别名不该再算生图模型")
	}
	if IsImageModel("raccoon-8c4485", nil) {
		t.Fatal("对话模型不能被误判成生图模型")
	}
	if IsImageModel("  ", nil) {
		t.Fatal("空名不算生图模型")
	}
}

func containsFold(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

// TestClampImageSize 守「超上限尺寸自动等比压缩」。
//
// 实测 4096x4096 上游不是立刻拒绝：先跑 106 秒再回 400
// "The longest edge must be less than or equal to 3840"。所以网关必须提前压。
func TestClampImageSize(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		changed bool
	}{
		{"1024x1024", "1024x1024", false},
		{"3840x2160", "3840x2160", false}, // 正好卡在像素预算上
		{"2160x3840", "2160x3840", false}, // 竖版同理
		// 边长合法但像素超预算：上游会静默降级成 2880x2880，网关自己先算出来。
		{"3840x3840", "2880x2880", true},
		{"4096x4096", "2880x2880", true},
		{"5120x2880", "3840x2160", true},
		{"7680x4320", "3840x2160", true},
		{"4096x2048", "3840x1920", true}, // 边长超、像素没超
		{"4096X4096", "2880x2880", true}, // 大写 X
		{"4096*4096", "2880x2880", true}, // 星号
		{"auto", "auto", false},
		{"", "", false},
		{"abc", "abc", false},
		{"0x100", "0x100", false},
		{"-1x100", "-1x100", false},
	}
	for _, tc := range cases {
		got, changed := ClampImageSize(tc.in)
		if got != tc.want || changed != tc.changed {
			t.Errorf("ClampImageSize(%q) = (%q, %v)，期望 (%q, %v)",
				tc.in, got, changed, tc.want, tc.changed)
		}
	}
}

// TestClampImageSizeNeverExceedsEdge 兜住「压缩后仍越界」这类回归。
func TestClampImageSizeNeverExceedsEdge(t *testing.T) {
	for _, in := range []string{"4096x4096", "5000x4999", "9999x1", "3841x3841", "10000x9999", "3840x3840"} {
		got, _ := ClampImageSize(in)
		w, h, ok := parseImageSize(got)
		if !ok {
			t.Fatalf("%q -> %q 解析失败", in, got)
		}
		if w > MaxImageEdge || h > MaxImageEdge {
			t.Fatalf("%q -> %q 仍超过边长上限 %d", in, got, MaxImageEdge)
		}
		if w*h > MaxImagePixels {
			t.Fatalf("%q -> %q 仍超过像素预算 %d", in, got, MaxImagePixels)
		}
	}
}

// TestRefreshMarksSessionCappedWhenTokenBarelyExtends 锁定「会话到顶」的识别。
//
// 上游到期前续期**不报错**，只回一张活一两分钟的票（exp 被钳在 IAM 会话上限）。
// 不识别它，请求路径就会按 RefreshSkew（默认 10 分钟）把这张残票判成「临期」，
// 于是每个请求都去续一次、永远续不好。2026-11-26 两个号正是这样死的：
// 7772869 在 09:48 续出 09:49 到期的票，7774611 在 12:25 续出 12:27 到期的票，
// 两个时刻都正好是各自 30 天会话的终点。
func TestRefreshMarksSessionCappedWhenTokenBarelyExtends(t *testing.T) {
	soon := time.Now().Add(90 * time.Second).UnixMilli()
	c, _ := fakeUpstream(t, map[string]http.HandlerFunc{
		"/auth/refresh": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"code":0,"data":{"access_token":"cap-token","refresh_token":"cap-refresh","expires_at":%d}}`, soon)
		},
	})
	a := &auth.Auth{Data: auth.Data{UID: "u", AccessToken: "old", RefreshToken: "ref"}}
	if err := c.RefreshToken(context.Background(), a); err == nil {
		t.Fatal("续出 90 秒的票必须判成会话到顶，实际没有报错")
	}
	if !a.SessionCappedValue() {
		t.Fatal("没有打上 SessionCapped 标记")
	}
	if a.NeedsRefresh(10*time.Minute) || a.NeedsRefresh(time.Hour) {
		t.Fatal("到顶后任何窗口都不该再判临期：否则每个请求都会重续一次")
	}
}

// TestSetTokensClearsSessionCappedOnHealthyToken 换到健康新票要解除到顶标记。
func TestSetTokensClearsSessionCappedOnHealthyToken(t *testing.T) {
	a := &auth.Auth{Data: auth.Data{UID: "u"}}
	a.MarkSessionCapped()
	a.SetTokens("t", "r", time.Now().Add(48*time.Hour).UnixMilli())
	if a.SessionCappedValue() {
		t.Fatal("新票还有 48 小时，到顶标记应当解除")
	}
}

// TestSetTokensKeepsSessionCappedOnStubToken 到顶那一刻的残票不能解除标记：
// 否则刚打完标记就被自己的残票解掉，等于没打。
func TestSetTokensKeepsSessionCappedOnStubToken(t *testing.T) {
	a := &auth.Auth{Data: auth.Data{UID: "u"}}
	a.MarkSessionCapped()
	a.SetTokens("t", "r", time.Now().Add(time.Minute).UnixMilli())
	if !a.SessionCappedValue() {
		t.Fatal("1 分钟的残票不该被当成健康新票")
	}
}
