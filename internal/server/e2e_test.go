package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
	"github.com/rsfree/xhx2api/internal/livecfg"
	"github.com/rsfree/xhx2api/internal/pool"
	"github.com/rsfree/xhx2api/internal/session"
	"github.com/rsfree/xhx2api/internal/upstream"
)

// testEnv 是一套「网关 + 假上游」的测试装置。
type testEnv struct {
	handler  *Handler
	upstream *upstream.Client
	pool     *pool.Pool
	fake     *httptest.Server
	gateway  *httptest.Server
	// chatHits 记录命中 chat 的次数（换号测试用）。
	chatHits atomic.Int64
	// imageHits / imageBody / imageHitsByModel 记录生图请求（防「把 model 转发给上游」）。
	imageHits        atomic.Int64
	imageMu          sync.Mutex
	imageBody        []byte
	imageHitsByModel []string
	imageFail        int
	// imageRateLimit 让上游先回 N 次 429（limiter_request_error），用于验证
	// 「瞬时过载原地重试」。
	imageRateLimit int
	// 图生图（/images/edits）的观测点：上游收到的是不是 multipart、有没有 image 文件。
	editHits      atomic.Int64
	editCType     string
	editPrompt    string
	editHasFile   bool
	editFileCount int
}

// setupEnv 建测试装置。fakeHandler 处理 /api/web/llm/v2/chat/completions。
func setupEnv(t *testing.T, fakeHandler http.HandlerFunc, uids ...string) *testEnv {
	t.Helper()
	env := &testEnv{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/web/llm/v2/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		env.chatHits.Add(1)
		fakeHandler(w, r)
	})
	mux.HandleFunc("/api/web/llm/v2/model_catalog", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"code": 0, "data": map[string]any{"categories": []any{
			map[string]any{"type": "对话", "models": []any{
				map[string]any{"name": "raccoon-8c4485", "description": "Raccoon Chat", "visible": true,
					"context_window": 180000, "max_tokens": 80000, "billing_multiplier": 1.0,
					"tags": []any{"thinking"}},
				map[string]any{"name": "raccoon-chat-ml-5-5", "description": "Raccoon ML", "visible": true,
					"context_window": 128000, "max_tokens": 64000, "billing_multiplier": 0.5,
					"tags": []any{"vision"}},
				// 上游把主力模型标成 visible=false（真实目录就这样），
				// 但它可调用、而且可能是默认模型。
				map[string]any{"name": "raccoon-hidden", "description": "上游未列出", "visible": false,
					"context_window": 256000, "max_tokens": 32000, "billing_multiplier": 1.0},
			}},
		}}})
	})
	mux.HandleFunc("/api/web/llm/v2/images/gen", func(w http.ResponseWriter, r *http.Request) {
		env.imageHits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		env.imageMu.Lock()
		env.imageBody = raw
		env.imageHitsByModel = append(env.imageHitsByModel, r.Header.Get("Authorization"))
		env.imageMu.Unlock()
		if env.imageRateLimit > 0 {
			env.imageMu.Lock()
			env.imageRateLimit--
			env.imageMu.Unlock()
			// 上游 429 的真实正文（实测）：code 200103 + limiter_request_error。
			writeJSON(w, 429, map[string]any{
				"code": 200103, "message": "limiter_request_error",
				"details": "request exceed max time, retry after:0s",
			})
			return
		}
		if env.imageFail > 0 {
			env.imageMu.Lock()
			env.imageFail--
			env.imageMu.Unlock()
			writeJSON(w, 500, map[string]any{"error": map[string]any{"code": "500", "message": "boom"}})
			return
		}
		writeJSON(w, 200, map[string]any{
			"created": 1792995386, "size": "1024x1024", "quality": "low", "output_format": "png",
			"data":  []any{map[string]any{"b64_json": "iVBORw0KGgo=", "url": nil, "revised_prompt": nil}},
			"usage": map[string]any{"total_tokens": 229, "input_tokens": 26, "output_tokens": 203},
		})
	})
	// 图生图：上游只吃 multipart，这里连「收到的是不是 multipart」一起记下来。
	mux.HandleFunc("/api/web/llm/v2/images/edits", func(w http.ResponseWriter, r *http.Request) {
		env.editHits.Add(1)
		env.imageMu.Lock()
		env.editCType = r.Header.Get("Content-Type")
		env.imageMu.Unlock()
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			writeJSON(w, 415, map[string]any{"error": map[string]any{
				"code": "unsupported_media_type_error", "message": "only multipart/form-data is supported"}})
			return
		}
		_, _, ferr := r.FormFile("image")
		env.imageMu.Lock()
		env.editHasFile = ferr == nil
		env.editFileCount = len(r.MultipartForm.File["image"])
		env.editPrompt = r.FormValue("prompt")
		env.imageMu.Unlock()
		if ferr != nil {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"code": "400", "message": "missing image"}})
			return
		}
		writeJSON(w, 200, map[string]any{
			"created": 1792998416, "size": "1024x1024", "quality": "low", "output_format": "png",
			"data":  []any{map[string]any{"b64_json": "iVBORw0KGgo=", "url": nil, "revised_prompt": nil}},
			"usage": map[string]any{"total_tokens": 1234, "input_tokens": 1038, "output_tokens": 196},
		})
	})
	mux.HandleFunc("/api/web/auth/v1/refresh", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"code": 0, "data": map[string]any{
			"access_token": "new-access", "refresh_token": "new-refresh",
			"expires_at": time.Now().Add(24 * time.Hour).UnixMilli(),
		}})
	})
	mux.HandleFunc("/api/web/auth/v1/user_info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"code": 0, "data": map[string]any{"id": "1001", "name": "测试号"}})
	})
	mux.HandleFunc("/api/web/points/v1/balance", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"code": 0, "data": map[string]any{
			"available_points": 1234, "daily_points": 300, "monthly_points": 0,
			"reward_points": 934, "topup_points": 0,
		}})
	})
	env.fake = httptest.NewServer(mux)
	t.Cleanup(env.fake.Close)

	up := upstream.New()
	base := env.fake.URL
	up.SetAgentHosts(base+"/api/web/llm/v2", base+"/api/web/auth/v1")
	up.SetAPIHosts(base+"/api/web/points/v1", base)
	up.SetPersistHook(func(a *auth.Auth) error { return nil })
	env.upstream = up

	p := pool.New(pool.Config{StateFile: ""})
	dir := t.TempDir()
	var auths []*auth.Auth
	if len(uids) == 0 {
		uids = []string{"1001"}
	}
	for _, uid := range uids {
		a := &auth.Auth{Data: auth.Data{UID: uid, Nickname: "号-" + uid, AccessToken: "tok-" + uid,
			RefreshToken: "ref-" + uid, ExpiresAt: time.Now().Add(24 * time.Hour).UnixMilli()}}
		a.FilePath = dir + "/" + auth.FileNameFor(uid)
		auths = append(auths, a)
	}
	p.SyncToDir(auths)
	env.pool = p

	router := session.New(session.Config{TTL: time.Minute, GCInterval: time.Minute})
	t.Cleanup(router.Close)
	env.handler = NewHandler(Config{
		Pool: p, Upstream: up, MaxRotate: 3, DefaultModel: "raccoon-8c4485",
		Aliases:        map[string]string{"gpt-4o": "raccoon-chat-ml-5-5"},
		Live:           livecfg.New(livecfg.Snapshot{}),
		Session:        router,
		StickyByPrefix: false, // 默认：前缀不粘性，靠显式 session id 或纯轮转
		Version:        "test",
		MaxBodyBytes:   4 << 20,
	})
	env.gateway = httptest.NewServer(env.handler)
	t.Cleanup(env.gateway.Close)
	return env
}

// sseChat 返回一段标准 OpenAI SSE。
func sseChat(model string, chunks ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		for _, text := range chunks {
			frame := map[string]any{
				"id": "chatcmpl-test", "object": "chat.completion.chunk", "created": 1,
				"model": model,
				"choices": []any{map[string]any{
					"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil,
				}},
			}
			raw, _ := json.Marshal(frame)
			fmt.Fprintf(w, "data: %s\n\n", raw)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprintf(w, "data: %s\n\n", `{"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"`+model+`","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}`)
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
}

// post 发一个 JSON POST 到网关。
func (e *testEnv) post(t *testing.T, path string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(e.gateway.URL+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func readJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是 JSON（HTTP %d）: %s", resp.StatusCode, raw)
	}
	return out
}

// readSSE 把 SSE 响应解析成 [event, data] 列表。
func readSSE(t *testing.T, resp *http.Response) [][2]string {
	t.Helper()
	var out [][2]string
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	event := ""
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			out = append(out, [2]string{event, strings.TrimSpace(strings.TrimPrefix(line, "data:"))})
			event = ""
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// ---------------------------------------------------------------------------
// OpenAI chat

func TestChatNonStream(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "你好", "，世界"))
	resp := env.post(t, "/v1/chat/completions", map[string]any{
		"model": "raccoon-8c4485", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	obj := readJSON(t, resp)
	if obj["object"] != "chat.completion" {
		t.Fatalf("object = %v", obj["object"])
	}
	choices := obj["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好，世界" {
		t.Fatalf("content = %q", msg["content"])
	}
	// 出站模型名必须是客户端请求的名字（不能是上游内部名）。
	if obj["model"] != "raccoon-8c4485" {
		t.Fatalf("model = %v", obj["model"])
	}
	usage := obj["usage"].(map[string]any)
	if usage["total_tokens"].(float64) != 19 {
		t.Fatalf("usage = %v", usage)
	}
}

// TestChatEchoesAliasNotUpstreamID 守卫「出站模型名用客户端写的名字」。
//
// 上游帧里的 model 永远是内部 id（raccoon-chat-ml-5-5），客户端写的是 gpt-4o，
// 就该看到 gpt-4o。上面两个用例直接拿 raccoon-8c4485 当请求名，两个名字恰好相同，
// 所以这个差异一直没被抓到（实测 2026-10-26：非流式和流式都吐上游 id）。
func TestChatEchoesAliasNotUpstreamID(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-chat-ml-5-5", "ok"))

	resp := env.post(t, "/v1/chat/completions", map[string]any{
		"model": "gpt-4o", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("非流式 HTTP %d", resp.StatusCode)
	}
	if obj := readJSON(t, resp); obj["model"] != "gpt-4o" {
		t.Fatalf("非流式 model = %v，应为客户端请求的 gpt-4o", obj["model"])
	}

	resp2 := env.post(t, "/v1/chat/completions", map[string]any{
		"model": "gpt-4o", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp2.StatusCode != 200 {
		t.Fatalf("流式 HTTP %d", resp2.StatusCode)
	}
	sawFrame := false
	for _, f := range readSSE(t, resp2) {
		if f[1] == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(f[1]), &chunk); err != nil {
			t.Fatalf("帧不是 JSON: %v", err)
		}
		if m, ok := chunk["model"].(string); ok {
			sawFrame = true
			if m != "gpt-4o" {
				t.Fatalf("流式帧 model = %q，应为 gpt-4o", m)
			}
		}
	}
	if !sawFrame {
		t.Fatal("流式没有任何带 model 的帧，断言没生效")
	}
}

func TestChatStream(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "第一段", "第二段"))
	resp := env.post(t, "/v1/chat/completions", map[string]any{
		"model": "raccoon-8c4485", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	frames := readSSE(t, resp)
	var text strings.Builder
	sawDone := false
	for _, f := range frames {
		if f[1] == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(f[1]), &chunk); err != nil {
			t.Fatalf("坏帧: %s", f[1])
		}
		choices := chunk["choices"].([]any)
		delta := choices[0].(map[string]any)["delta"].(map[string]any)
		if s, ok := delta["content"].(string); ok {
			text.WriteString(s)
		}
	}
	if text.String() != "第一段第二段" {
		t.Fatalf("拼出的内容 = %q", text.String())
	}
	if !sawDone {
		t.Fatal("缺少 [DONE]")
	}
}

func TestChatNonStreamUpstreamJSON(t *testing.T) {
	// 上游对 stream:false 回普通 JSON：网关要能包成 SSE 再聚合回来。
	env := setupEnv(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"id": "chatcmpl-json", "object": "chat.completion", "created": 1, "model": "raccoon-8c4485",
			"choices": []any{map[string]any{"index": 0,
				"message":       map[string]any{"role": "assistant", "content": "直接 JSON"},
				"finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7},
		})
	})
	resp := env.post(t, "/v1/chat/completions", map[string]any{
		"model": "raccoon-8c4485", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	obj := readJSON(t, resp)
	msg := obj["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "直接 JSON" {
		t.Fatalf("content = %q", msg["content"])
	}
}

func TestModelAlias(t *testing.T) {
	var gotModel atomic.Value
	env := setupEnv(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel.Store(body["model"])
		sseChat("raccoon-chat-ml-5-5", "ok")(w, r)
	})
	// 先刷一次目录，让目录里有 raccoon-chat-ml-5-5。
	a := env.pool.FirstAvailable()
	if _, err := env.upstream.RefreshModels(context.Background(), a, true); err != nil {
		t.Fatal(err)
	}
	resp := env.post(t, "/v1/chat/completions", map[string]any{
		"model": "gpt-4o", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	if got := gotModel.Load(); got != "raccoon-chat-ml-5-5" {
		t.Fatalf("上游收到的 model = %v（别名没生效）", got)
	}
}

// TestAliasHotReloadTakesEffect 守卫「面板保存别名后不用重启」。
//
// 面板有别名编辑框，保存回报 restart_required 为空；但别名原先只在启动时注入，
// 运行期读不到新表，用户加完别名仍然 400、以为没保存成功（2026-10-26 实测）。
func TestAliasHotReloadTakesEffect(t *testing.T) {
	const short = "deepseek-v4-1-flash-1017"
	env := setupEnv(t, sseChat("sn-deepseek-v4-1-flash-1017", "收到"))

	// 目录必须是「真目录」，否则 resolveModel 会走「目录不可用就透传」的兜底分支，
	// 短名直接被放行，测不出别名是否生效。
	a := env.pool.FirstAvailable()
	if _, err := env.upstream.RefreshModels(context.Background(), a, true); err != nil {
		t.Fatal(err)
	}

	resp := env.post(t, "/v1/chat/completions", map[string]any{
		"model": short, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 400 {
		t.Fatalf("未加别名时应当 400（既不在别名表也不在目录），实际 %d", resp.StatusCode)
	}

	// 模拟面板保存：只换快照，不重建 handler、不重启进程。
	snap := env.handler.cfg.Live.Load()
	snap.Aliases = map[string]string{short: "sn-deepseek-v4-1-flash-1017"}
	env.handler.cfg.Live.Store(snap)

	resp2 := env.post(t, "/v1/chat/completions", map[string]any{
		"model": short, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp2.StatusCode != 200 {
		t.Fatalf("热更新别名后应当 200，实际 %d", resp2.StatusCode)
	}
	if obj := readJSON(t, resp2); obj["model"] != short {
		t.Fatalf("回显 model = %v，应为客户端请求的 %s", obj["model"], short)
	}
}

func TestUnknownModelRejected(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "x"))
	a := env.pool.FirstAvailable()
	if _, err := env.upstream.RefreshModels(context.Background(), a, true); err != nil {
		t.Fatal(err)
	}
	resp := env.post(t, "/v1/chat/completions", map[string]any{
		"model": "gpt-9-turbo", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 400 {
		t.Fatalf("未知模型应当 400, got %d", resp.StatusCode)
	}
	if env.chatHits.Load() != 0 {
		t.Fatal("未知模型不该打到上游")
	}
}

func TestHiddenModelRejected(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "x"))
	env.handler.cfg.Live.Store(livecfg.Snapshot{HiddenModels: []string{"raccoon-8c4485"}})
	resp := env.post(t, "/v1/chat/completions", map[string]any{
		"model": "raccoon-8c4485", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 403 {
		t.Fatalf("黑名单模型应当 403, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// 换号与冷却

func TestRotateOn429(t *testing.T) {
	var calls atomic.Int64
	env := setupEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded","status":429}}`))
			return
		}
		sseChat("raccoon-8c4485", "换号成功")(w, r)
	}, "1001", "1002")

	resp := env.post(t, "/v1/chat/completions", map[string]any{
		"model": "raccoon-8c4485", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("应当换号成功，HTTP %d: %s", resp.StatusCode, raw)
	}
	obj := readJSON(t, resp)
	msg := obj["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "换号成功" {
		t.Fatalf("content = %v", msg["content"])
	}
	// 首次 429 不罚号：同一个号原地等 2s 重试就能成功。罚号会把一次瞬时
	// 过载放大成整池长时间冷却——2026-10-30 线上 8% 的 429 正是这样演变成
	// 75% 的 503（全池被锁 32 分钟到 1.1 小时）。所以这里应当零冷却。
	var cooling int
	for _, e := range env.pool.List() {
		if e.CooldownKind == "soft_rate" {
			cooling++
		}
	}
	if cooling != 0 {
		t.Fatalf("首次 429 不该罚号，got %d 个冷却 (%+v)", cooling, env.pool.List())
	}
}

// TestRepeated429CoolsDown 守「连续 429 才罚号」的另一半。
//
// 首次 429 是瞬时速率闸，原地重试即可；连着两次都 429 说明这个号确实被限，
// 得短冷却回避，否则每次请求都白撞它。封顶必须是分钟级：实测被判
// 「第 7 级退避、冷却 1.1h」的号直接打上游仍然 200，锁太久等于自己废号。
func TestRepeated429CoolsDown(t *testing.T) {
	env := setupEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded","status":429}}`))
	}, "1001", "1002")

	resp := env.post(t, "/v1/chat/completions", map[string]any{
		"model": "raccoon-8c4485", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 429 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("持续 429 应当把 429 透给客户端，HTTP %d: %s", resp.StatusCode, raw)
	}
	var cooling int
	for _, e := range env.pool.List() {
		if e.CooldownKind == "soft_rate" {
			cooling++
			if left := time.Until(time.Unix(e.CooldownUntil, 0)); left > 6*time.Minute {
				t.Fatalf("429 冷却封顶 5 分钟，got %s", left)
			}
		}
	}
	if cooling == 0 {
		t.Fatalf("连续 429 应当罚号，got 0 (%+v)", env.pool.List())
	}
}

func TestNoRotateOnClientError(t *testing.T) {
	env := setupEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"context length exceeded"}}`))
	}, "1001", "1002")
	resp := env.post(t, "/v1/chat/completions", map[string]any{
		"model": "raccoon-8c4485", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 400 {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	if env.chatHits.Load() != 1 {
		t.Fatalf("参数错误不该换号重试（实际请求上游 %d 次）", env.chatHits.Load())
	}
	// 也不该罚号。
	for _, e := range env.pool.List() {
		if e.CooldownUntil != 0 {
			t.Fatalf("参数错误不该冷却账号: %+v", e)
		}
	}
}

func TestStreamErrorRotates(t *testing.T) {
	// HTTP 200 但首帧是 error：必须在写任何字节前识别出来并换号。
	var calls atomic.Int64
	env := setupEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fmt.Fprint(w, `data: {"error":{"message":"额度不足，积分耗尽","status":429}}`+"\n\n")
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			return
		}
		sseChat("raccoon-8c4485", "恢复")(w, r)
	}, "1001", "1002")
	resp := env.post(t, "/v1/chat/completions", map[string]any{
		"model": "raccoon-8c4485", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	frames := readSSE(t, resp)
	var text strings.Builder
	for _, f := range frames {
		if f[1] == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(f[1]), &chunk) != nil {
			continue
		}
		if choices, ok := chunk["choices"].([]any); ok && len(choices) > 0 {
			delta := choices[0].(map[string]any)["delta"].(map[string]any)
			if s, ok := delta["content"].(string); ok {
				text.WriteString(s)
			}
		}
	}
	if text.String() != "恢复" {
		t.Fatalf("流内错误后换号，内容应为「恢复」，got %q", text.String())
	}
}

func TestEmptyStreamRotates(t *testing.T) {
	var calls atomic.Int64
	env := setupEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		sseChat("raccoon-8c4485", "第二次成功")(w, r)
	}, "1001", "1002")
	resp := env.post(t, "/v1/chat/completions", map[string]any{
		"model": "raccoon-8c4485", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	obj := readJSON(t, resp)
	msg := obj["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "第二次成功" {
		t.Fatalf("空流应当换号，content = %v", msg["content"])
	}
}

func TestRefreshOn401(t *testing.T) {
	var calls atomic.Int64
	var refreshed atomic.Bool
	env := setupEnv(t, func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		if authz == "Bearer tok-1001" && calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
			return
		}
		if authz == "Bearer new-access" {
			refreshed.Store(true)
		}
		sseChat("raccoon-8c4485", "续期后成功")(w, r)
	})
	resp := env.post(t, "/v1/chat/completions", map[string]any{
		"model": "raccoon-8c4485", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("HTTP %d: %s", resp.StatusCode, raw)
	}
	if !refreshed.Load() {
		t.Fatal("401 后应当强制续期并用新 token 重试")
	}
}

// ---------------------------------------------------------------------------
// Anthropic Messages

func TestMessagesNonStream(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "Anthropic 回复"))
	resp := env.post(t, "/v1/messages", map[string]any{
		"model": "raccoon-8c4485", "max_tokens": 1024, "system": "你是助手",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("HTTP %d: %s", resp.StatusCode, raw)
	}
	obj := readJSON(t, resp)
	if obj["type"] != "message" || obj["role"] != "assistant" {
		t.Fatalf("响应形状不对: %v", obj)
	}
	content := obj["content"].([]any)
	if len(content) == 0 {
		t.Fatal("content 为空")
	}
	first := content[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "Anthropic 回复" {
		t.Fatalf("content[0] = %v", first)
	}
	if obj["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", obj["stop_reason"])
	}
	usage := obj["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 12 || usage["output_tokens"].(float64) != 7 {
		t.Fatalf("usage = %v", usage)
	}
	if !strings.HasPrefix(obj["id"].(string), "msg_") {
		t.Fatalf("id = %v", obj["id"])
	}
}

func TestMessagesStream(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "流式", "回复"))
	resp := env.post(t, "/v1/messages", map[string]any{
		"model": "raccoon-8c4485", "max_tokens": 1024, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	frames := readSSE(t, resp)
	var events []string
	var text strings.Builder
	for _, f := range frames {
		events = append(events, f[0])
		if f[0] == "content_block_delta" {
			var payload map[string]any
			if json.Unmarshal([]byte(f[1]), &payload) != nil {
				continue
			}
			delta, _ := payload["delta"].(map[string]any)
			if t, _ := delta["text"].(string); t != "" {
				text.WriteString(t)
			}
		}
	}
	joined := strings.Join(events, ",")
	for _, required := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("缺少事件 %s（实际: %s）", required, joined)
		}
	}
	if text.String() != "流式回复" {
		t.Fatalf("拼出的文本 = %q", text.String())
	}
}

func TestMessagesToolCall(t *testing.T) {
	env := setupEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"raccoon-8c4485","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"raccoon-8c4485","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":9,"total_tokens":14}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	// 客户端声明工具，验证入站转换（tools → chat functions）。
	resp := env.post(t, "/v1/messages", map[string]any{
		"model": "raccoon-8c4485", "max_tokens": 1024,
		"tools": []any{map[string]any{
			"name": "get_weather", "description": "查天气",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{
				"city": map[string]any{"type": "string"},
			}},
		}},
		"messages": []any{map[string]any{"role": "user", "content": "北京天气"}},
	})
	obj := readJSON(t, resp)
	content := obj["content"].([]any)
	var toolUse map[string]any
	for _, raw := range content {
		if block, ok := raw.(map[string]any); ok && block["type"] == "tool_use" {
			toolUse = block
		}
	}
	if toolUse == nil {
		t.Fatalf("没有 tool_use 块: %v", content)
	}
	if toolUse["name"] != "get_weather" {
		t.Fatalf("name = %v", toolUse["name"])
	}
	input := toolUse["input"].(map[string]any)
	if input["city"] != "北京" {
		t.Fatalf("input = %v", input)
	}
	if !strings.HasPrefix(toolUse["id"].(string), "toolu_") {
		t.Fatalf("tool id 应为 toolu_ 前缀: %v", toolUse["id"])
	}
	if obj["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v", obj["stop_reason"])
	}
}

// ---------------------------------------------------------------------------
// Responses

func TestResponsesNonStream(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "Responses 回复"))
	resp := env.post(t, "/v1/responses", map[string]any{
		"model": "raccoon-8c4485", "input": "hi", "instructions": "你是助手",
	})
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("HTTP %d: %s", resp.StatusCode, raw)
	}
	obj := readJSON(t, resp)
	if obj["object"] != "response" || obj["status"] != "completed" {
		t.Fatalf("响应形状不对: %v", obj)
	}
	output := obj["output"].([]any)
	if len(output) == 0 {
		t.Fatal("output 为空")
	}
	item := output[0].(map[string]any)
	if item["type"] != "message" {
		t.Fatalf("output[0].type = %v", item["type"])
	}
	parts := item["content"].([]any)
	if parts[0].(map[string]any)["text"] != "Responses 回复" {
		t.Fatalf("text = %v", parts[0])
	}
	usage := obj["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 12 {
		t.Fatalf("usage = %v", usage)
	}
	if !strings.HasPrefix(obj["id"].(string), "resp_") {
		t.Fatalf("id = %v", obj["id"])
	}
}

func TestResponsesStream(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "第一", "第二"))
	resp := env.post(t, "/v1/responses", map[string]any{
		"model": "raccoon-8c4485", "input": "hi", "stream": true,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	frames := readSSE(t, resp)
	var events []string
	var text strings.Builder
	for _, f := range frames {
		events = append(events, f[0])
		if f[0] == "response.output_text.delta" {
			var payload map[string]any
			if json.Unmarshal([]byte(f[1]), &payload) == nil {
				if d, ok := payload["delta"].(string); ok {
					text.WriteString(d)
				}
			}
		}
	}
	joined := strings.Join(events, ",")
	for _, required := range []string{"response.created", "response.output_item.added", "response.output_text.delta", "response.completed"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("缺少事件 %s（实际: %s）", required, joined)
		}
	}
	if text.String() != "第一第二" {
		t.Fatalf("拼出的文本 = %q", text.String())
	}
}

func TestResponsesFunctionCall(t *testing.T) {
	env := setupEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"raccoon-8c4485","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"run","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	resp := env.post(t, "/v1/responses", map[string]any{
		"model": "raccoon-8c4485", "input": "hi",
		"tools": []any{map[string]any{"type": "function", "name": "run", "parameters": map[string]any{"type": "object"}}},
	})
	obj := readJSON(t, resp)
	output := obj["output"].([]any)
	found := false
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		if item["type"] == "function_call" {
			found = true
			if item["name"] != "run" {
				t.Fatalf("name = %v", item["name"])
			}
			if !strings.Contains(item["call_id"].(string), "call_1") {
				t.Fatalf("call_id = %v", item["call_id"])
			}
		}
	}
	if !found {
		t.Fatalf("没有 function_call 项: %v", output)
	}
}

// ---------------------------------------------------------------------------
// 鉴权

func TestAuthRequired(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "x"))
	env.handler.cfg.APIKey = "sk-secret"
	env.handler.cfg.Live.Store(livecfg.Snapshot{APIKey: "sk-secret"})

	// 无密钥
	resp := env.post(t, "/v1/chat/completions", map[string]any{"model": "raccoon-8c4485"})
	if resp.StatusCode != 401 {
		t.Fatalf("无密钥应当 401, got %d", resp.StatusCode)
	}
	// Anthropic 入口用 x-api-key
	req, _ := http.NewRequest(http.MethodPost, env.gateway.URL+"/v1/messages",
		strings.NewReader(`{"model":"raccoon-8c4485","max_tokens":10,"messages":[]}`))
	req.Header.Set("x-api-key", "sk-secret")
	req.Header.Set("Content-Type", "application/json")
	okResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer okResp.Body.Close()
	if okResp.StatusCode != 200 {
		raw, _ := io.ReadAll(okResp.Body)
		t.Fatalf("x-api-key 应当通过, HTTP %d: %s", okResp.StatusCode, raw)
	}
}

func TestHealthz(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "x"))
	resp, err := http.Get(env.gateway.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// 并发与粘性

func TestConcurrentRequestsNoLeak(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "并发"), "1001", "1002")
	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, _ := json.Marshal(map[string]any{
				"model":    "raccoon-8c4485",
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			})
			resp, err := http.Post(env.gateway.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				raw, _ := io.ReadAll(resp.Body)
				errs <- fmt.Errorf("HTTP %d: %s", resp.StatusCode, raw)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发请求失败: %v", err)
	}
	// 在途计数必须归零，否则账号会在 max_in_flight 次之后永久不可用。
	for _, e := range env.pool.List() {
		if e.InFlight != 0 {
			t.Fatalf("账号 %s 在途泄漏: %d", e.UID, e.InFlight)
		}
		if e.Total == 0 {
			t.Fatalf("账号 %s 一次都没被用到（轮转失效）", e.UID)
		}
	}
}

func TestSessionSticky(t *testing.T) {
	// 网关不会把 X-Session-Id 透传给上游（它只是路由键），所以用
	// Authorization 序列判断「同一会话是否落在同一个号上」。
	var mu sync.Mutex
	var seq []string
	env := setupEnv(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seq = append(seq, r.Header.Get("Authorization"))
		mu.Unlock()
		sseChat("raccoon-8c4485", "ok")(w, r)
	}, "1001", "1002")

	send := func(sid string) {
		body, _ := json.Marshal(map[string]any{
			"model":    "raccoon-8c4485",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		req, _ := http.NewRequest(http.MethodPost, env.gateway.URL+"/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Session-Id", sid)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// 交替发：A、B、A、B，粘性生效时 A 的两次相同、B 的两次相同。
	send("sess-a")
	send("sess-b")
	send("sess-a")
	send("sess-b")

	mu.Lock()
	defer mu.Unlock()
	if len(seq) != 4 {
		t.Fatalf("上游收到 %d 次请求，期望 4: %v", len(seq), seq)
	}
	if seq[0] != seq[2] {
		t.Fatalf("会话 A 两次落在不同账号: %v", seq)
	}
	if seq[1] != seq[3] {
		t.Fatalf("会话 B 两次落在不同账号: %v", seq)
	}
	if seq[0] == seq[1] {
		t.Fatalf("两个不同会话落在同一账号，粘性没有分流作用: %v", seq)
	}
}

// ---------------------------------------------------------------------------
// 鉴权

// TestModelsListsUnlistedModels 守卫「/v1/models 不漏掉可调用的模型」。
//
// 上游的 visible=false 只表示「不在它官网的模型选择器里」。实测三个 raccoon-* 全被
// 标成 false，而默认模型就是其中之一——按 visible 过滤会让客户端（靠 /v1/models
// 发现模型）永远看不到默认模型，表现就是「网关说默认是 X，列表里却没有 X」。
func TestModelsListsUnlistedModels(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")

	fetch := func() map[string]map[string]any {
		resp, err := http.Get(env.gateway.URL + "/v1/models?refresh=1")
		if err != nil {
			t.Fatalf("GET /v1/models: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("HTTP %d: %s", resp.StatusCode, raw)
		}
		var payload struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]any{}
		for _, m := range payload.Data {
			out[m["id"].(string)] = m
		}
		// 生图模型是网关合成的（不在上游目录里），这里只断言目录口径。
		// 别名（gpt-image-* / dall-e-*）同样是合成的，一并摘掉。
		for id := range out {
			if upstream.IsImageModel(id, nil) {
				delete(out, id)
			}
		}
		return out
	}

	// 默认行为：上游未列出的模型也要列出，并标注 unlisted。
	got := fetch()
	if _, ok := got["raccoon-hidden"]; !ok {
		t.Fatalf("visible=false 的模型应当仍被列出（默认模型就在这类里）: %v", keysOf(got))
	}
	if got["raccoon-hidden"]["unlisted"] != true {
		t.Fatalf("应当标注 unlisted: %+v", got["raccoon-hidden"])
	}
	if got["raccoon-8c4485"]["unlisted"] != nil && got["raccoon-8c4485"]["unlisted"] != false {
		t.Fatalf("正常模型不该有 unlisted 标记: %+v", got["raccoon-8c4485"])
	}

	// 关掉开关：回到「只列上游展示的」。
	env.handler.cfg.ExcludeUpstreamHidden = true
	got = fetch()
	if _, ok := got["raccoon-hidden"]; ok {
		t.Fatalf("关掉 include_upstream_hidden 后不该再列出: %v", keysOf(got))
	}
	if len(got) != 2 {
		t.Fatalf("关掉后应当只剩 2 个，得到 %d: %v", len(got), keysOf(got))
	}
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// 生图（/v1/images/generations）

func postImage(t *testing.T, base string, body any) (*http.Response, []byte) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/images/generations", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/images/generations: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

// TestImageGenerationsPassThrough 守卫生图主链路：图要原样回给客户端、
// usage 要记进账，并且**不能把客户端的 model 转发给上游**——
// 上游模型组固定是 raccoon-image-gen，传 dall-e-3 会让 LiteLLM 找不到组。
func TestImageGenerationsPassThrough(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")

	resp, body := postImage(t, env.gateway.URL, map[string]any{
		"model": "dall-e-3", "prompt": "一只戴帽子的橘猫", "size": "1024x1024",
		"title": "生图联调",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	var payload struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
		Size  string         `json:"size"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, body)
	}
	if len(payload.Data) != 1 || payload.Data[0].B64 == "" {
		t.Fatalf("应当原样回图: %+v", payload)
	}
	if payload.Size != "1024x1024" {
		t.Fatalf("size 丢了: %q", payload.Size)
	}
	if got := env.imageHits.Load(); got != 1 {
		t.Fatalf("上游应当被调用 1 次，实际 %d", got)
	}

	// 上游收到的 body：有 prompt/size，**没有** model 与 title。
	var sent map[string]any
	env.imageMu.Lock()
	raw := append([]byte(nil), env.imageBody...)
	env.imageMu.Unlock()
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("上游收到的不是 JSON: %v", err)
	}
	if _, ok := sent["model"]; ok {
		t.Fatalf("不能把客户端的 model 转发给上游: %s", raw)
	}
	if _, ok := sent["title"]; ok {
		t.Fatalf("title 只走 Header，不该留在 body: %s", raw)
	}
	if sent["prompt"] != "一只戴帽子的橘猫" {
		t.Fatalf("prompt 被改坏了: %v", sent["prompt"])
	}
	if sent["size"] != "1024x1024" {
		t.Fatalf("size 应当透传: %v", sent["size"])
	}
}

// TestImageSizeClampedToMaxEdge 守「超上限尺寸在网关侧压到 3840」。
//
// 实测上游 4096x4096 不是立刻拒绝：先跑 106 秒再回 400
// "The longest edge must be less than or equal to 3840"。让用户白等两分钟
// 才拿到尺寸错误是不可接受的，所以网关提前等比压缩。
func TestImageSizeClampedToMaxEdge(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	resp, body := postImage(t, env.gateway.URL, map[string]any{
		"prompt": "cat", "size": "4096x4096",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	// 4096x4096 边长和像素都超：上游会静默给 2880x2880（= 3840x2160 的像素数），
	// 网关自己先算出来，避免客户端以为拿到了 4K。
	if got := resp.Header.Get("X-XHX-Image-Size"); got != "2880x2880" {
		t.Fatalf("响应头应当标注压后的尺寸，得到 %q", got)
	}
	var sent map[string]any
	env.imageMu.Lock()
	raw := append([]byte(nil), env.imageBody...)
	env.imageMu.Unlock()
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("上游收到的不是 JSON: %v", err)
	}
	if sent["size"] != "2880x2880" {
		t.Fatalf("上游应当收到压后的尺寸，得到 %v", sent["size"])
	}
}

// TestImageSizeWithinLimitUntouched 上限内的尺寸一个字都不该改。
func TestImageSizeWithinLimitUntouched(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	resp, body := postImage(t, env.gateway.URL, map[string]any{
		"prompt": "cat", "size": "3840x2160",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-XHX-Image-Size"); got != "" {
		t.Fatalf("没压就不该带响应头，得到 %q", got)
	}
	var sent map[string]any
	env.imageMu.Lock()
	raw := append([]byte(nil), env.imageBody...)
	env.imageMu.Unlock()
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("上游收到的不是 JSON: %v", err)
	}
	if sent["size"] != "3840x2160" {
		t.Fatalf("size 被改了: %v", sent["size"])
	}
}

// TestImageRetriesOnRateLimit 守「429 原地重试」。
//
// 实测上游 429 的正文是 code 200103 / limiter_request_error，`retry after: 0s`：
// 12 并发生图首发 10 成功 + 2 个 429，等 1 秒重试全部成功。所以这不该判成
// 账号故障（换号没用、罚号更没道理），而是等一小会原地重来。
func TestImageRetriesOnRateLimit(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	env.imageMu.Lock()
	env.imageRateLimit = 1
	env.imageMu.Unlock()

	resp, body := postImage(t, env.gateway.URL, map[string]any{"prompt": "cat"})
	if resp.StatusCode != 200 {
		t.Fatalf("429 后应当原地重试成功，得到 %d: %s", resp.StatusCode, body)
	}
	if got := env.imageHits.Load(); got != 2 {
		t.Fatalf("应当打两次上游（429 → 重试），实际 %d", got)
	}
	// 用同一个账号重试：429 不是账号故障，换号会让「偶发过载」变成「全池被罚」。
	env.imageMu.Lock()
	tokens := append([]string(nil), env.imageHitsByModel...)
	env.imageMu.Unlock()
	if len(tokens) != 2 || tokens[0] != tokens[1] {
		t.Fatalf("应当用同一账号原地重试，实际 %v", tokens)
	}
}

// TestImageRateLimitGivesUpAfterAttempts 429 连着来时要放弃，不能无限重试。
func TestImageRateLimitGivesUpAfterAttempts(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	env.imageMu.Lock()
	env.imageRateLimit = 10 // 远多于 MaxRotate
	env.imageMu.Unlock()

	resp, body := postImage(t, env.gateway.URL, map[string]any{"prompt": "cat"})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("持续 429 应当把 429 透给客户端，得到 %d: %s", resp.StatusCode, body)
	}
	if got := env.imageHits.Load(); got != 3 {
		t.Fatalf("MaxRotate=3 应当最多打 3 次，实际 %d", got)
	}
}

func TestImageGenerationsRequiresPrompt(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	resp, body := postImage(t, env.gateway.URL, map[string]any{"prompt": "   "})
	if resp.StatusCode != 400 {
		t.Fatalf("空 prompt 应当 400，得到 %d: %s", resp.StatusCode, body)
	}
	if env.imageHits.Load() != 0 {
		t.Fatal("空 prompt 不该打到上游（上游会回 400/500，白等一轮）")
	}
}

func TestImageGenerationsRejectsURLFormat(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	resp, body := postImage(t, env.gateway.URL, map[string]any{"prompt": "cat", "response_format": "url"})
	if resp.StatusCode != 400 {
		t.Fatalf("response_format=url 应当 400（上游没有 URL），得到 %d: %s", resp.StatusCode, body)
	}
	if env.imageHits.Load() != 0 {
		t.Fatal("不该打到上游")
	}
}

// TestImageGenerationsRotatesOnServerError 上游 500 要换号重试（与对话一致）。
func TestImageGenerationsRotatesOnServerError(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001", "1002")
	env.imageMu.Lock()
	env.imageFail = 1
	env.imageMu.Unlock()

	resp, body := postImage(t, env.gateway.URL, map[string]any{"prompt": "cat"})
	if resp.StatusCode != 200 {
		t.Fatalf("第一次 500 后应当换号成功，得到 %d: %s", resp.StatusCode, body)
	}
	if got := env.imageHits.Load(); got != 2 {
		t.Fatalf("应当打两次上游（503 → 换号），实际 %d", got)
	}
	env.imageMu.Lock()
	tokens := append([]string(nil), env.imageHitsByModel...)
	env.imageMu.Unlock()
	if len(tokens) != 2 || tokens[0] == tokens[1] {
		t.Fatalf("两次应当用不同账号: %v", tokens)
	}
}

// TestModelsExposeImageModel 生图模型要出现在 /v1/models 里，否则客户端发现不了。
func TestModelsExposeImageModel(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	resp, err := http.Get(env.gateway.URL + "/v1/models?refresh=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload struct {
		Data []struct {
			ID   string         `json:"id"`
			Caps map[string]any `json:"capabilities"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range payload.Data {
		if m.ID != upstream.ImageModelID {
			continue
		}
		found = true
		if m.Caps["image"] != true {
			t.Fatalf("生图模型应当标 image 能力: %+v", m.Caps)
		}
	}
	if !found {
		t.Fatalf("/v1/models 里没有 %s", upstream.ImageModelID)
	}
}

// TestChatRejectsImageModel 用生图模型调对话时要给明确指引，而不是打到上游。
func TestChatRejectsImageModel(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	body, _ := json.Marshal(map[string]any{
		"model":    upstream.ImageModelID,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	resp, err := http.Post(env.gateway.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "/v1/images/generations") {
		t.Fatalf("错误信息里应当指向生图端点: %s", raw)
	}
	if env.chatHits.Load() != 0 {
		t.Fatal("不该打到上游对话接口")
	}
}

// ---------------------------------------------------------------------------
// 生图别名（gpt-image-* / dall-e-*）
//
// 2026-10-26 真号实测：上游 /images/gen **完全忽略 model**——gpt-image-2、
// gpt-image-2.5、甚至捏造的名字都 200，1024x1024 的 image_tokens 都是 196。
// 所以别名不是真路由，只是让硬编码这些名字的客户端能选中并出图。

// TestImageAliasesExposedAndRoutable 守卫三件事：别名出现在 /v1/models、
// 带 alias_of 指回合成 id、用它生图时上游仍然收不到 model（上游不认）。
func TestImageAliasesExposedAndRoutable(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")

	resp, err := http.Get(env.gateway.URL + "/v1/models?refresh=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var listed struct {
		Data []struct {
			ID      string         `json:"id"`
			Caps    map[string]any `json:"capabilities"`
			AliasOf string         `json:"alias_of"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	aliasOf := map[string]string{}
	for _, m := range listed.Data {
		byID[m.ID] = m.Caps
		aliasOf[m.ID] = m.AliasOf
	}
	for _, want := range []string{"gpt-image-1", "gpt-image-2", "gpt-image-2.5", "dall-e-3"} {
		caps, ok := byID[want]
		if !ok {
			t.Fatalf("/v1/models 里应当有别名 %s", want)
		}
		if caps["image"] != true || caps["chat"] != false {
			t.Fatalf("%s 的 capabilities 不对: %+v", want, caps)
		}
		if aliasOf[want] != upstream.ImageModelID {
			t.Fatalf("%s 应当标 alias_of=%s，得到 %q", want, upstream.ImageModelID, aliasOf[want])
		}
	}
	// 合成 id 本身不该被标成别名。
	if v := aliasOf[upstream.ImageModelID]; v != "" {
		t.Fatalf("合成 id 不该有 alias_of，得到 %q", v)
	}

	// 用别名生图：正常出图，且上游 body 里没有 model。
	iresp, ibody := postImage(t, env.gateway.URL, map[string]any{
		"model": "gpt-image-2.5", "prompt": "一只猫", "size": "1536x1024",
	})
	if iresp.StatusCode != 200 {
		t.Fatalf("别名生图应当成功，得到 %d: %s", iresp.StatusCode, ibody)
	}
	env.imageMu.Lock()
	raw := append([]byte(nil), env.imageBody...)
	env.imageMu.Unlock()
	var sent map[string]any
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("上游收到的不是 JSON: %v", err)
	}
	if _, ok := sent["model"]; ok {
		t.Fatalf("别名也不该把 model 转发给上游: %s", raw)
	}
	if sent["size"] != "1536x1024" {
		t.Fatalf("size 应当透传（这是上游唯一真正生效的参数）: %v", sent["size"])
	}
}

// TestImageAliasesDisabled 显式 aliases=[] 时别名要全部消失，且调对话
// 不再被当成生图模型拦截（回落成普通未知模型判定）。
func TestImageAliasesDisabled(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	env.handler.cfg.Live.Store(livecfg.Snapshot{ImageAliases: []string{}})

	resp, err := http.Get(env.gateway.URL + "/v1/models?refresh=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var listed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	for _, m := range listed.Data {
		if m.ID == "gpt-image-2" || m.ID == "dall-e-3" {
			t.Fatalf("aliases=[] 后不该再列 %s", m.ID)
		}
	}
	// 合成 id 仍在（它是通道本身，不是别名）。
	found := false
	for _, m := range listed.Data {
		if m.ID == upstream.ImageModelID {
			found = true
		}
	}
	if !found {
		t.Fatalf("aliases=[] 只关别名，不该连 %s 一起关掉", upstream.ImageModelID)
	}
}

// TestChatRejectsImageAlias 别名调对话要给指引，别打到上游白扣额度。
func TestChatRejectsImageAlias(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	for _, name := range []string{"gpt-image-2", "gpt-image-2.5", "dall-e-3"} {
		body, _ := json.Marshal(map[string]any{
			"model":    name,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		resp, err := http.Post(env.gateway.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("%s 调对话应当 400，得到 %d: %s", name, resp.StatusCode, raw)
		}
		if !strings.Contains(string(raw), name) || !strings.Contains(string(raw), "/v1/images/generations") {
			t.Fatalf("错误信息应当带上模型名并指向生图端点: %s", raw)
		}
	}
	if env.chatHits.Load() != 0 {
		t.Fatal("不该打到上游对话接口")
	}
}

// ---------------------------------------------------------------------------
// 思考吃光额度（reasoning_exhausted）

// sseReasoningOnly 模拟「只思考、不产出正文、额度耗尽」的上游流。
func sseReasoningOnly(model string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		frames := []map[string]any{
			{"id": "chatcmpl-r", "object": "chat.completion.chunk", "created": 1, "model": model,
				"choices": []any{map[string]any{"index": 0,
					"delta": map[string]any{"reasoning_content": "想了很久很久"}, "finish_reason": nil}}},
			{"id": "chatcmpl-r", "object": "chat.completion.chunk", "created": 1, "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "length"}},
				"usage": map[string]any{"total_tokens": 2050, "prompt_tokens": 12, "completion_tokens": 2038,
					"completion_tokens_details": map[string]any{"reasoning_tokens": 2038}}},
		}
		for _, f := range frames {
			raw, _ := json.Marshal(f)
			fmt.Fprintf(w, "data: %s\n\n", raw)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

// TestChatEscalatesWhenReasoningEatsBudget 守卫「空正文自动加额重试」。
//
// 实测 sn-sensenova-6-8-flash 的思考量能到 4000+ 字，把 1024 的额度全吃光；
// 上游不报错，只回空正文 + finish_reason=length。网关必须自己加额重试一次，
// 否则用户看到的是「模型回了空消息」。
func TestChatEscalatesWhenReasoningEatsBudget(t *testing.T) {
	var mu sync.Mutex
	var seen []int64
	fake := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			MaxTokens int64 `json:"max_tokens"`
		}
		_ = json.Unmarshal(raw, &req)
		mu.Lock()
		seen = append(seen, req.MaxTokens)
		mu.Unlock()
		if req.MaxTokens < 8192 {
			sseReasoningOnly("raccoon-8c4485")(w, r)
			return
		}
		sseChat("raccoon-8c4485", "收到")(w, r)
	}
	env := setupEnv(t, fake, "1001")

	body, _ := json.Marshal(map[string]any{
		"model":      "raccoon-8c4485",
		"max_tokens": 32,
		"messages":   []any{map[string]any{"role": "user", "content": "只回复两个字：收到"}},
	})
	resp, err := http.Post(env.gateway.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) != "收到" {
		t.Fatalf("重试后应当拿到正文: %s", raw)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("应当打两次上游（首次 + 加额重试），实际 %v", seen)
	}
	if seen[0] != 2048 {
		t.Fatalf("首次应当按默认下限 2048，实际 %d", seen[0])
	}
	if seen[1] != 8192 {
		t.Fatalf("重试应当加到 8192，实际 %d", seen[1])
	}
}

// TestStreamInjectsErrorWhenReasoningEatsBudget 流式没法重试（已经发过思考帧），
// 但必须补一帧 error：否则客户端收到一个「空的正常回复」。
func TestStreamInjectsErrorWhenReasoningEatsBudget(t *testing.T) {
	env := setupEnv(t, sseReasoningOnly("raccoon-8c4485"), "1001")

	body, _ := json.Marshal(map[string]any{
		"model":    "raccoon-8c4485",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "只回复两个字：收到"}},
	})
	resp, err := http.Post(env.gateway.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	text := string(raw)
	if !strings.Contains(text, "reasoning_exhausted") {
		t.Fatalf("流里应当有 reasoning_exhausted 错误帧:\n%s", text)
	}
	if !strings.Contains(text, "[DONE]") {
		t.Fatalf("错误帧之后仍要收尾 [DONE]:\n%s", text)
	}
}

// ---------------------------------------------------------------------------
// 图生图（/v1/images/edits）

// postImageEdit 按 OpenAI 客户端的方式发 multipart。
func postImageEdit(t *testing.T, base, prompt string, withFile bool, extra map[string]string) (*http.Response, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if prompt != "" {
		_ = mw.WriteField("prompt", prompt)
	}
	for k, v := range extra {
		_ = mw.WriteField(k, v)
	}
	if withFile {
		fw, err := mw.CreateFormFile("image", "in.png")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write([]byte("\x89PNG\r\n\x1a\nfake-image-bytes"))
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/images/edits", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/images/edits: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

// TestImageEditsForwardsMultipart 守图生图主链路。
//
// 上游 /images/edits 只吃 multipart（实测传 JSON 回 415），所以网关必须把
// image 文件搬过去、并保持 multipart 形状；模型名则不能转发。
func TestImageEditsForwardsMultipart(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	resp, body := postImageEdit(t, env.gateway.URL, "把背景换成海边日落", true,
		map[string]string{"size": "1024x1024", "model": "dall-e-3"})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "b64_json") {
		t.Fatalf("响应里没有图: %s", body)
	}
	if env.editHits.Load() != 1 {
		t.Fatalf("上游 edits 命中 %d 次，期望 1", env.editHits.Load())
	}
	env.imageMu.Lock()
	defer env.imageMu.Unlock()
	if !strings.HasPrefix(env.editCType, "multipart/form-data") {
		t.Fatalf("转发给上游的 Content-Type = %q，必须是 multipart", env.editCType)
	}
	if !env.editHasFile {
		t.Fatal("上游没收到 image 文件字段")
	}
	if env.editPrompt != "把背景换成海边日落" {
		t.Fatalf("prompt 透传错了: %q", env.editPrompt)
	}
}

// TestImageEditsRequiresImageField 缺 image 要 400，并且不该打到上游。
func TestImageEditsRequiresImageField(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	resp, body := postImageEdit(t, env.gateway.URL, "换个背景", false, nil)
	if resp.StatusCode != 400 {
		t.Fatalf("缺 image 应当 400，实际 %d: %s", resp.StatusCode, body)
	}
	if env.editHits.Load() != 0 {
		t.Fatal("参数不全不该打到上游")
	}
}

// TestImageEditsAcceptsJSONBodyWithBadImage 守错误定位：
// edits 现在收 JSON（语义与 generations 一致），但 image 不是合法 base64 时
// 必须明确说是「image 解码失败」，而不是笼统的「解析失败」。
func TestImageEditsAcceptsJSONBodyWithBadImage(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	raw, _ := json.Marshal(map[string]any{"prompt": "换个背景", "image": "base64..."})
	req, _ := http.NewRequest(http.MethodPost, env.gateway.URL+"/v1/images/edits", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 {
		t.Fatalf("非法 base64 应当 400，实际 %d: %s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "image") {
		t.Fatalf("错误提示应当点名 image: %s", out)
	}
	if env.editHits.Load() != 0 || env.imageHits.Load() != 0 {
		t.Fatal("参数错误不该打到上游")
	}
}

// ---------------------------------------------------------------------------
// 图片 URL（images.return_url）

// TestImageURLRequiresConfig 守「默认关闭时要说人话」。
//
// 上游只回 base64，直接放行 response_format=url 会让客户端拿到 url:null。
// 关着的时候必须 400，并在提示里点名配置项，否则用户不知道该怎么开。
func TestImageURLRequiresConfig(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	resp, body := postImage(t, env.gateway.URL, map[string]any{
		"prompt": "cat", "response_format": "url",
	})
	if resp.StatusCode != 400 {
		t.Fatalf("未开启 return_url 时应当 400，实际 %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "return_url") {
		t.Fatalf("提示里应当点名 return_url: %s", body)
	}
}

// TestImageURLServedWhenEnabled 守开启后的完整链路：落盘 → 响应给 url →
// 显式要 url 时不再回 base64 → 该 url 能真的取到文件。
func TestImageURLServedWhenEnabled(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	env.handler.cfg.ImageReturnURL = true
	env.handler.cfg.ImageDir = t.TempDir()

	resp, body := postImage(t, env.gateway.URL, map[string]any{
		"prompt": "cat", "response_format": "url",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	item := obj["data"].([]any)[0].(map[string]any)
	url, _ := item["url"].(string)
	if url == "" {
		t.Fatalf("没有 url: %s", body)
	}
	if _, has := item["b64_json"]; has {
		t.Fatal("显式要 url 时不该再回 b64_json（省带宽就是开它的目的）")
	}

	got, err := http.Get(url)
	if err != nil {
		t.Fatalf("取图失败: %v", err)
	}
	defer got.Body.Close()
	if got.StatusCode != 200 {
		t.Fatalf("图片地址返回 %d", got.StatusCode)
	}
	if cc := got.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("内容寻址的图应当可永久缓存，实际 Cache-Control=%q", cc)
	}
}

// enableImageURL 打开 url 能力并给一个临时目录（生产装配的等价最小形态）。
func enableImageURL(t *testing.T, env *testEnv) string {
	t.Helper()
	dir := t.TempDir()
	env.handler.cfg.ImageReturnURL = true
	env.handler.cfg.ImageDir = dir
	return dir
}

// imageItem 取响应里的第一张图，并解析成 map。
func imageItem(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, body)
	}
	items, _ := obj["data"].([]any)
	if len(items) == 0 {
		t.Fatalf("没有 data: %s", body)
	}
	item, _ := items[0].(map[string]any)
	if item == nil {
		t.Fatalf("data[0] 不是对象: %s", body)
	}
	return item
}

// TestImageDefaultsToURL 守产品口径：不传 response_format 就回 url。
//
// 默认形态是 url，且**不再附带** b64_json —— 一张 4K 图的 base64 有 8MB+，
// 两边都给等于默认形态白设。
func TestImageDefaultsToURL(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	dir := enableImageURL(t, env)

	resp, body := postImage(t, env.gateway.URL, map[string]any{"prompt": "cat"})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	item := imageItem(t, body)
	url, _ := item["url"].(string)
	if url == "" {
		t.Fatalf("默认应当回 url: %s", body)
	}
	if !strings.Contains(url, "/v1/images/") {
		t.Fatalf("url 应当指向网关自己的静态路由，得到 %q", url)
	}
	if _, has := item["b64_json"]; has {
		t.Fatal("默认只回 url，不该再附 b64_json")
	}
	if got := resp.Header.Get("X-XHX-Image-URL"); got != "ok" {
		t.Fatalf("响应头应当标注 url 可用，得到 %q", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("默认落盘 1 张，实际 %d", len(entries))
	}

	// url 必须真能取到图。
	got, err := http.Get(url)
	if err != nil {
		t.Fatalf("取图失败: %v", err)
	}
	defer got.Body.Close()
	if got.StatusCode != 200 {
		t.Fatalf("图片地址返回 %d", got.StatusCode)
	}
}

// TestImageNullFormatReturnsURL 守「response_format=None 等同不传」。
//
// Python 侧写 response_format=None 会被序列化成 null；有些客户端还会把
// 字面量 "None" 原样发过来。两种都不算「明确要 base64」。
func TestImageNullFormatReturnsURL(t *testing.T) {
	for _, v := range []any{nil, "None", "null", "", "  "} {
		env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
		enableImageURL(t, env)
		resp, body := postImage(t, env.gateway.URL, map[string]any{"prompt": "cat", "response_format": v})
		if resp.StatusCode != 200 {
			t.Fatalf("response_format=%v HTTP %d: %s", v, resp.StatusCode, body)
		}
		item := imageItem(t, body)
		if u, _ := item["url"].(string); u == "" {
			t.Fatalf("response_format=%v 应当回 url: %s", v, body)
		}
	}
}

// TestImageB64JSONKeepsBase64 守「客户端明确要 base64 就真的只给 base64」，
// 而且不该为它写盘（白占磁盘）。
func TestImageB64JSONKeepsBase64(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	dir := enableImageURL(t, env)

	resp, body := postImage(t, env.gateway.URL, map[string]any{"prompt": "cat", "response_format": "b64_json"})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	item := imageItem(t, body)
	if b, _ := item["b64_json"].(string); b == "" {
		t.Fatalf("要 b64_json 时必须回 base64: %s", body)
	}
	if u, _ := item["url"].(string); u != "" {
		t.Fatalf("只要 b64_json 时不该附 url，得到 %q", u)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("只要 base64 时不该落盘，实际 %d 个文件", len(entries))
	}
	if got := resp.Header.Get("X-XHX-Image-URL"); got != "" {
		t.Fatalf("没走 url 链路时不该有 X-XHX-Image-URL，得到 %q", got)
	}
}

// TestImageBothFormatFromConfig 守 both：配置层同时给 url 与 base64。
func TestImageBothFormatFromConfig(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	enableImageURL(t, env)
	env.handler.cfg.ImageDefaultFormat = "both"

	resp, body := postImage(t, env.gateway.URL, map[string]any{"prompt": "cat"})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	item := imageItem(t, body)
	if u, _ := item["url"].(string); u == "" {
		t.Fatalf("both 应当有 url: %s", body)
	}
	if b, _ := item["b64_json"].(string); b == "" {
		t.Fatalf("both 应当有 b64_json: %s", body)
	}
}

// TestImageURLFallbackWhenStorageMissing 守降级：配了 return_url 但既没有
// 落盘目录也没有对象存储时，默认形态必须回落 base64 而不是回 url:null。
func TestImageURLFallbackWhenStorageMissing(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	env.handler.cfg.ImageReturnURL = true
	env.handler.cfg.ImageDir = "" // 没有落盘目录，也没有 uploader

	resp, body := postImage(t, env.gateway.URL, map[string]any{"prompt": "cat"})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	item := imageItem(t, body)
	if b, _ := item["b64_json"].(string); b == "" {
		t.Fatalf("没有存储时应当回落 base64: %s", body)
	}
	if u, _ := item["url"].(string); u != "" {
		t.Fatalf("没有存储时不该编造 url，得到 %q", u)
	}
	if got := resp.Header.Get("X-XHX-Image-URL"); got != "" {
		t.Fatalf("根本没走 url 链路，不该有这个头（得到 %q）", got)
	}
}

// TestImageRejectsUnknownFormat 守「写错形态要报错，不能当没看见」。
func TestImageRejectsUnknownFormat(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	enableImageURL(t, env)

	resp, body := postImage(t, env.gateway.URL, map[string]any{"prompt": "cat", "response_format": "png"})
	if resp.StatusCode != 400 {
		t.Fatalf("非法的 response_format 应当 400，得到 %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "response_format") {
		t.Fatalf("提示里应当点名 response_format: %s", body)
	}
	if env.imageHits.Load() != 0 {
		t.Fatal("参数错误不该打到上游")
	}
}

// TestImageWriteFailureFallsBackToBase64 守「落盘失败不能丢图」：
// ImageDir 指向一个不可写的位置时，响应必须仍带 b64_json。
func TestImageWriteFailureFallsBackToBase64(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	env.handler.cfg.ImageReturnURL = true
	// 用一个「文件」当目录：MkdirAll 会失败，整条 url 链路报错。
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.handler.cfg.ImageDir = f

	resp, body := postImage(t, env.gateway.URL, map[string]any{"prompt": "cat"})
	if resp.StatusCode != 200 {
		t.Fatalf("落盘失败不该影响出图，HTTP %d: %s", resp.StatusCode, body)
	}
	item := imageItem(t, body)
	if b, _ := item["b64_json"].(string); b == "" {
		t.Fatalf("落盘失败时必须保留 base64: %s", body)
	}
	if got := resp.Header.Get("X-XHX-Image-URL"); got != "unavailable" {
		t.Fatalf("应当标注 url 不可用，得到 %q", got)
	}
}

// TestSweepImageDir 守容量闸门：超上限时只删最旧的**图片**文件。
func TestSweepImageDir(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string, size int, age time.Duration) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, bytes.Repeat([]byte("a"), size), 0o600); err != nil {
			t.Fatal(err)
		}
		ts := time.Now().Add(-age)
		if err := os.Chtimes(path, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	// 三个 1MB 的图（30min / 20min / 1min 前）+ 一个不该被动的文件。
	mk("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.png", 1<<20, 30*time.Minute)
	mk("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.png", 1<<20, 20*time.Minute)
	mk("cccccccccccccccccccccccccccccccc.png", 1<<20, time.Minute)
	mk("keep.txt", 1<<20, 60*time.Minute)

	resetImageSweepThrottle()
	files, freed := sweepImageDir(dir, 2)
	if files == 0 || freed == 0 {
		t.Fatalf("超过上限应当回收，实际 files=%d freed=%d", files, freed)
	}
	left, _ := os.ReadDir(dir)
	names := map[string]bool{}
	for _, e := range left {
		names[e.Name()] = true
	}
	if names["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.png"] {
		t.Fatal("最旧的图应当先被删")
	}
	if !names["keep.txt"] {
		t.Fatal("非图片文件一个都不能动")
	}
	if !names["cccccccccccccccccccccccccccccccc.png"] {
		t.Fatal("最新的图应当保留")
	}

	// 上限内不该删任何东西。
	resetImageSweepThrottle()
	if files, _ := sweepImageDir(dir, 100); files != 0 {
		t.Fatalf("没超上限不该删文件，实际删了 %d 个", files)
	}
	// 0 = 不限制。
	resetImageSweepThrottle()
	if files, _ := sweepImageDir(dir, 0); files != 0 {
		t.Fatalf("0 表示不限制，实际删了 %d 个", files)
	}
}

// ---------------------------------------------------------------------------
// image 字段的三种写法：字符串 / 数组 / 裸 base64

// TestGenerationsAcceptsBareBase64Array 守「数组 + 裸 base64」：
// 不带 data: 前缀也要认（很多客户端只发 base64 本体）。
func TestGenerationsAcceptsBareBase64Array(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	resp, body := postImage(t, env.gateway.URL, map[string]any{
		"prompt": "合成一张海报",
		"image":  []any{pngB64("a"), pngB64("b")},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	if env.editHits.Load() != 1 {
		t.Fatalf("应当打一次上游 edits，实际 %d", env.editHits.Load())
	}
	if env.editFileCount != 2 {
		t.Fatalf("两张裸 base64 都该转成文件发出去，实际 %d", env.editFileCount)
	}
}

// TestGenerationsAcceptsSingleBase64String 守「字符串 + 裸 base64」。
func TestGenerationsAcceptsSingleBase64String(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	resp, body := postImage(t, env.gateway.URL, map[string]any{
		"prompt": "把背景换成雪山",
		"image":  pngB64("only"),
	})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	if env.editHits.Load() != 1 || env.editFileCount != 1 {
		t.Fatalf("应当把 1 张裸 base64 转给 edits，实际 hits=%d files=%d",
			env.editHits.Load(), env.editFileCount)
	}
	if env.imageHits.Load() != 0 {
		t.Fatal("带图时不该再打 /images/gen（它会把 image 静默丢掉）")
	}
}

// TestGenerationsRejectsNonStringImageElement 守数组元素类型：
// 非字符串（数字/对象/嵌套数组）要明确报错，而不是静默丢掉一张图。
func TestGenerationsRejectsNonStringImageElement(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	resp, body := postImage(t, env.gateway.URL, map[string]any{
		"prompt": "cat",
		"image":  []any{pngB64("a"), 42},
	})
	if resp.StatusCode != 400 {
		t.Fatalf("数组里的非法元素应当 400，得到 %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "字符串") {
		t.Fatalf("提示应当说清只收字符串: %s", body)
	}
	if env.editHits.Load() != 0 || env.imageHits.Load() != 0 {
		t.Fatal("参数错误不该打到上游")
	}
}

// TestEditsAcceptsJSONBody 守「edits 也收 JSON」：语义与 generations 完全一致，
// 只因为传输方式不同就 415 没有道理。
func TestEditsAcceptsJSONBody(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	raw, _ := json.Marshal(map[string]any{
		"prompt": "把方块改成金色",
		"image":  "data:image/png;base64," + pngB64("json"),
	})
	resp, err := http.Post(env.gateway.URL+"/v1/images/edits", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("JSON 形态的 edits 应当 200，得到 %d: %s", resp.StatusCode, body)
	}
	if env.editHits.Load() != 1 {
		t.Fatalf("应当打上游 edits，实际 %d", env.editHits.Load())
	}
	if env.editPrompt != "把方块改成金色" {
		t.Fatalf("prompt 没透过去: %q", env.editPrompt)
	}
}

// TestEditsAcceptsBase64TextForm 守「multipart 里 image 是文本」：
// 把 JSON 硬转 multipart 的客户端会这么发，只认文件会让它拿到「image 必填」。
func TestEditsAcceptsBase64TextForm(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	resp, body := postImageEdit(t, env.gateway.URL, "把天空换成晚霞", false,
		map[string]string{"image": pngB64("text-form")})
	if resp.StatusCode != 200 {
		t.Fatalf("文本形态的 image 应当被接受，得到 %d: %s", resp.StatusCode, body)
	}
	if env.editHits.Load() != 1 || env.editFileCount != 1 {
		t.Fatalf("应当转成 1 个文件发给上游，实际 hits=%d files=%d",
			env.editHits.Load(), env.editFileCount)
	}
}

// TestEditsAcceptsURLEncoded 守 curl -d 的默认编码。
//
// 关键陷阱：表单解码会把 base64 里的 `+` 变成空格。不还原的话，含 `+` 的图
// 要么解不出、要么静默解错。所以这里故意挑一个含 `+` 的 base64，并且**不做**
// URL 编码直接发（就是 `curl -d 'image=<b64>'` 的行为）。
func TestEditsAcceptsURLEncoded(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")

	// 找一个 base64 里真的含 '+' 的 PNG 头素材：PNG 魔数是 8 字节，
	// 不是 3 的倍数，手算对齐很容易翻车，直接扫一遍最省事。
	blob := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 24)...)
	b64 := ""
	for i := 0; i < 256; i++ {
		blob[8] = byte(i)
		if cand := base64.StdEncoding.EncodeToString(blob); strings.Contains(cand, "+") {
			b64 = cand
			break
		}
	}
	if b64 == "" {
		t.Fatal("找不到含 '+' 的测试素材")
	}

	body := "prompt=把背景换成雪山&image=" + b64 // 故意不转义
	req, _ := http.NewRequest(http.MethodPost, env.gateway.URL+"/v1/images/edits", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("urlencoded 形态应当被接受，得到 %d: %s", resp.StatusCode, out)
	}
	if env.editFileCount != 1 {
		t.Fatalf("应当转成 1 个文件发给上游，实际 %d", env.editFileCount)
	}
	if env.editPrompt != "把背景换成雪山" {
		t.Fatalf("prompt 没透过去: %q", env.editPrompt)
	}
}

// TestEditsAcceptsBase64TextArray 守「multipart 里同名文本字段重复」＝数组。
func TestEditsAcceptsBase64TextArray(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("prompt", "合成一张海报")
	_ = mw.WriteField("image", pngB64("one"))
	_ = mw.WriteField("image", pngB64("two"))
	_ = mw.Close()

	resp, err := http.Post(env.gateway.URL+"/v1/images/edits", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	if env.editFileCount != 2 {
		t.Fatalf("两个文本 image 都该转成文件，实际 %d", env.editFileCount)
	}
}

// TestImageNameValidation 守静态路由的文件名校验（纯函数，防路径穿越）。
func TestImageNameValidation(t *testing.T) {
	ok := []string{
		"0123456789abcdef0123456789abcdef.png",
		"0123456789abcdef0123456789abcdef.jpg",
		"0123456789abcdef0123456789abcdef.webp",
	}
	for _, n := range ok {
		if !imageNameOK(n) {
			t.Fatalf("%q 应当被接受", n)
		}
	}
	bad := []string{
		"", "abc.png", "../../etc/passwd", "0123456789abcdef0123456789abcdef",
		"0123456789ABCDEF0123456789abcdef.png", // 大写 hex 不接受
		"0123456789abcdef0123456789abcde.png",  // 长度不对
		"0123456789abcdef0123456789abcdef.gif",
		"0123456789abcdef0123456789abcdef.pn/../g",
	}
	for _, n := range bad {
		if imageNameOK(n) {
			t.Fatalf("%q 应当被拒绝", n)
		}
	}
}

// ---------------------------------------------------------------------------
// 图片对象存储（images.cos）

// fakeUploader 记录调用次数与参数，并可注入失败。
type fakeUploader struct {
	calls    int
	err      error
	url      string
	lastName string
	lastType string
}

func (f *fakeUploader) Put(ctx context.Context, name string, data []byte, contentType string) (string, error) {
	f.calls++
	f.lastName, f.lastType = name, contentType
	if f.err != nil {
		return "", f.err
	}
	return f.url, nil
}

// TestImageUploaderPreferred 守「配了对象存储就优先用它」：
// url 必须是存储返回的地址，且本地盘不该再多出一份。
func TestImageUploaderPreferred(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	dir := t.TempDir()
	up := &fakeUploader{url: "https://cdn.example.com/xhx/abc.png"}
	env.handler.cfg.ImageReturnURL = true
	env.handler.cfg.ImageUploader = up
	env.handler.cfg.ImageDir = dir

	resp, body := postImage(t, env.gateway.URL, map[string]any{
		"prompt": "cat", "response_format": "url",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	if up.calls != 1 {
		t.Fatalf("上传调用 %d 次，期望 1", up.calls)
	}
	if up.lastType != "image/png" {
		t.Fatalf("Content-Type = %q，期望 image/png", up.lastType)
	}
	if !strings.HasSuffix(up.lastName, ".png") || len(up.lastName) != 36 {
		t.Fatalf("对象名应是 32 位内容哈希 + 扩展名，实际 %q", up.lastName)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatal(err)
	}
	if got := obj["data"].([]any)[0].(map[string]any)["url"]; got != up.url {
		t.Fatalf("url = %v，期望对象存储返回的 %s", got, up.url)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("上传成功时不该再落本地盘，实际多了 %d 个文件", len(entries))
	}
}

// TestImageUploaderFallbackToLocal 守降级：对象存储挂了也要出图。
// 图仍然给到客户端（b64 + 本地地址），只是 url 变成网关自己的。
func TestImageUploaderFallbackToLocal(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	dir := t.TempDir()
	up := &fakeUploader{err: errors.New("cos 502")}
	env.handler.cfg.ImageReturnURL = true
	env.handler.cfg.ImageUploader = up
	env.handler.cfg.ImageDir = dir

	resp, body := postImage(t, env.gateway.URL, map[string]any{
		"prompt": "cat", "response_format": "url",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("上传失败不该影响出图，HTTP %d: %s", resp.StatusCode, body)
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatal(err)
	}
	item := obj["data"].([]any)[0].(map[string]any)
	url, _ := item["url"].(string)
	if !strings.Contains(url, "/v1/images/") {
		t.Fatalf("降级后 url 应当是网关本地地址，实际 %q", url)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("降级后本地应落 1 个文件，实际 %d", len(entries))
	}
}

// ---------------------------------------------------------------------------
// 图生图 / 多图（generations 统一入口）

// pngB64 造一小段合法 PNG 前缀的 base64，够网关嗅探扩展名即可。
func pngB64(tag string) string {
	return base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n" + tag))
}

// TestGenerationsWithImageRoutesToEdits 守「generations 也能图生图」。
//
// 上游 /images/gen 收到 image 会静默忽略（实测 image_tokens=0、出无关新图），
// 所以网关必须自己分流到 /images/edits，不能透传后假装支持。
func TestGenerationsWithImageRoutesToEdits(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	resp, body := postImage(t, env.gateway.URL, map[string]any{
		"prompt": "把背景换成雪山", "image": "data:image/png;base64," + pngB64("a"),
	})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	if env.editHits.Load() != 1 {
		t.Fatalf("应当打上游 /images/edits，实际 editHits=%d", env.editHits.Load())
	}
	if env.imageHits.Load() != 0 {
		t.Fatalf("带图时不该再打 /images/gen，实际 %d 次", env.imageHits.Load())
	}
	if !strings.HasPrefix(env.editCType, "multipart/form-data") {
		t.Fatalf("转发给上游必须是 multipart，实际 %q", env.editCType)
	}
}

// TestGenerationsMultiImage 守多图：image 数组要全部转过去。
func TestGenerationsMultiImage(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	resp, body := postImage(t, env.gateway.URL, map[string]any{
		"prompt": "合成一张海报",
		"image":  []any{"data:image/png;base64," + pngB64("a"), pngB64("b")},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	env.imageMu.Lock()
	defer env.imageMu.Unlock()
	if env.editFileCount != 2 {
		t.Fatalf("上游应收到 2 张图，实际 %d", env.editFileCount)
	}
}

// TestGenerationsNullImageIsTextToImage 守空值语义：null / [] / "" 都是「没有图」，
// 必须回到纯文生图，而不是报 400 —— 这是 OpenAI SDK 的两种常见写法。
func TestGenerationsNullImageIsTextToImage(t *testing.T) {
	for _, v := range []any{nil, []any{}, ""} {
		env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
		resp, body := postImage(t, env.gateway.URL, map[string]any{
			"prompt": "一只猫", "image": v,
		})
		if resp.StatusCode != 200 {
			t.Fatalf("image=%v 应当按文生图处理，实际 %d: %s", v, resp.StatusCode, body)
		}
		if env.imageHits.Load() != 1 || env.editHits.Load() != 0 {
			t.Fatalf("image=%v 应当走 /images/gen（gen=%d edit=%d）", v, env.imageHits.Load(), env.editHits.Load())
		}
	}
}

// TestGenerationsRejectsURLImageRef 守 SSRF 边界：不让网关替调用方抓外链。
func TestGenerationsRejectsURLImageRef(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	resp, body := postImage(t, env.gateway.URL, map[string]any{
		"prompt": "改成水彩", "image": "https://example.com/a.png",
	})
	if resp.StatusCode != 400 {
		t.Fatalf("URL 形态应当 400，实际 %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "base64") {
		t.Fatalf("提示里应说明改用 base64: %s", body)
	}
	if env.editHits.Load() != 0 {
		t.Fatal("参数被拒时不该打上游")
	}
}

// TestEditsAcceptsMultipleFiles 守 multipart 多图：两种字段名都要收。
func TestEditsAcceptsMultipleFiles(t *testing.T) {
	for _, field := range []string{"image", "image[]"} {
		env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("prompt", "合成")
		for _, tag := range []string{"a", "b"} {
			fw, err := mw.CreateFormFile(field, tag+".png")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = fw.Write([]byte("\x89PNG\r\n\x1a\n" + tag))
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPost, env.gateway.URL+"/v1/images/edits", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s 两张图应当 200，实际 %d: %s", field, resp.StatusCode, out)
		}
		env.imageMu.Lock()
		n := env.editFileCount
		env.imageMu.Unlock()
		if n != 2 {
			t.Fatalf("%s 上游应收到 2 张图，实际 %d", field, n)
		}
	}
}

// TestMaskWithoutImageRejected 守语义：没有图就没有可遮罩的区域。
func TestMaskWithoutImageRejected(t *testing.T) {
	env := setupEnv(t, sseChat("raccoon-8c4485", "ok"), "1001")
	resp, body := postImage(t, env.gateway.URL, map[string]any{
		"prompt": "重绘天空", "mask": "data:image/png;base64," + pngB64("m"),
	})
	if resp.StatusCode != 400 {
		t.Fatalf("只有 mask 没有 image 应当 400，实际 %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "mask") {
		t.Fatalf("提示应点名 mask: %s", body)
	}
}

// TestChatDropsTemperature 守「temperature 一律不外发」。
//
// 上游对 sn-kimi-k3 只接受 temperature=1，客户端带 0.7 会被 400；网关在
// bodyfix 里统一摘掉，上游收到的 body 不该再有这个字段（2026-11-02 线上
// 57% 的失败就是它）。
func TestChatDropsTemperature(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	env := setupEnv(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(raw, &obj)
		mu.Lock()
		got = obj
		mu.Unlock()
		sseChat("raccoon-8c4485", "ok")(w, r)
	}, "1001")

	resp := env.post(t, "/v1/chat/completions", map[string]any{
		"model": "raccoon-8c4485", "temperature": 0.7, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	_ = readSSE(t, resp)
	mu.Lock()
	defer mu.Unlock()
	if _, ok := got["temperature"]; ok {
		t.Fatalf("出站 body 仍带 temperature: %v", got)
	}
	if got["model"] != "raccoon-8c4485" {
		t.Fatalf("其他字段被改坏: %v", got)
	}
}

// TestChatClampsMaxTokensAndRetries 守「超上限按报错里的上界收一次再重试」。
//
// 线上原文：400 "Range of max_tokens should be [1, 131072]"。这条是客户端
// 填大了，不该换号、不该罚号，也不该把 400 直接甩给客户端——收一次就能成。
func TestChatClampsMaxTokensAndRetries(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	env := setupEnv(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(raw, &obj)
		mu.Lock()
		bodies = append(bodies, obj)
		first := len(bodies) == 1
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Range of max_tokens should be [1, 131072]","type":"invalid_request_error"}}`))
			return
		}
		sseChat("raccoon-8c4485", "ok")(w, r)
	}, "1001")

	resp := env.post(t, "/v1/chat/completions", map[string]any{
		"model": "raccoon-8c4485", "max_tokens": 200000, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("收额度后应当成功，HTTP %d: %s", resp.StatusCode, raw)
	}
	_ = readSSE(t, resp)
	if n := env.chatHits.Load(); n != 2 {
		t.Fatalf("应重试一次（上游命中 2 次），实际 %d", n)
	}
	mu.Lock()
	second := bodies[1]
	mu.Unlock()
	if v, _ := second["max_tokens"].(float64); v != 131072 {
		t.Fatalf("重试 body 的 max_tokens = %v, want 131072", second["max_tokens"])
	}
	for _, e := range env.pool.List() {
		if e.Fail != 0 || e.FailCount != 0 {
			t.Fatalf("参数错不该记到账号头上: fail=%d fail_count=%d", e.Fail, e.FailCount)
		}
		if e.CooldownUntil != 0 {
			t.Fatalf("参数错不该冷却账号: %+v", e)
		}
	}
}

// TestClientErrorNotCountedAgainstAccount 守账号口径：client/none 不进账号统计。
//
// 否则下游传错一次参数，账号页成功率就掉一截，连着几次还会触发降权
// （DegradeThresh），把本身健康的号排到后面。
func TestClientErrorNotCountedAgainstAccount(t *testing.T) {
	env := setupEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid temperature: only 1 is allowed for this model"}}`))
	}, "1001")

	resp := env.post(t, "/v1/chat/completions", map[string]any{
		"model":    "raccoon-8c4485",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 400 {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	_ = readJSON(t, resp)
	list := env.pool.List()
	if len(list) != 1 {
		t.Fatalf("账号数 = %d", len(list))
	}
	e := list[0]
	if e.Total != 0 || e.Fail != 0 || e.FailCount != 0 || e.Success != 0 {
		t.Fatalf("client 错误污染了账号统计: total=%d success=%d fail=%d fc=%d", e.Total, e.Success, e.Fail, e.FailCount)
	}
	if e.LastError == "" {
		t.Fatal("last_error 应保留（排障用）")
	}
}
