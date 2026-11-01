package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
)

// 等响应头超时必须判成 gateway。
//
// 判成 server 的代价是实打实的：handler 会按「值得换号」再试 3 次，每次又等
// 一轮首字节超时（线上实测单个请求 360s），并且给 3 个本身没问题的账号各记
// 一次失败，够 3 次就熔断。超时换号毫无意义——换个号一样慢。
func TestChatStreamHeaderTimeoutIsGatewayNotServer(t *testing.T) {
	release := make(chan struct{})
	c, _ := fakeUpstream(t, map[string]http.HandlerFunc{
		"/llm/chat/completions": func(w http.ResponseWriter, r *http.Request) {
			<-release // 一直不返响应头
		},
	})
	t.Cleanup(func() { close(release) })
	c.SetTimeouts(0, 80*time.Millisecond, 0)

	a := &auth.Auth{Data: auth.Data{UID: "u", AccessToken: "tok"}}
	_, status, _, err := c.ChatStream(context.Background(), a, []byte(`{"model":"m","stream":true}`), "")

	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("期望 *upstream.Error，实际 %v", err)
	}
	if ue.Kind != ErrGateway {
		t.Fatalf("kind = %v，期望 gateway（判成 server 会换号重试 3 次）", ue.Kind)
	}
	if status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d，期望 504", status)
	}
}

// 连不上（这里用已关闭的端口模拟）仍要判 server：那才是「换个号可能就行」。
func TestChatStreamDialErrorStaysServer(t *testing.T) {
	c := New()
	c.SetAgentHosts("http://127.0.0.1:1/llm", "http://127.0.0.1:1/auth")

	a := &auth.Auth{Data: auth.Data{UID: "u", AccessToken: "tok"}}
	_, _, _, err := c.ChatStream(context.Background(), a, []byte(`{"model":"m"}`), "")

	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("期望 *upstream.Error，实际 %v", err)
	}
	if ue.Kind != ErrServer && ue.Kind != ErrGateway {
		t.Fatalf("kind = %v，期望 server", ue.Kind)
	}
	if ue.Kind == ErrGateway {
		t.Fatal("连接被拒不应判成超时")
	}
}

// 流已经 200、但上游卡住不再吐字节：必须由空闲看门狗掐断，
// 否则这条流会一直占着连接和在途名额（线上见过 877s 的请求）。
func TestChatStreamIdleWatchdogCutsStalledStream(t *testing.T) {
	c, _ := fakeUpstream(t, map[string]http.HandlerFunc{
		"/llm/chat/completions": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"你"}}]}`+"\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done() // 之后一个字节都不再吐
		},
	})
	c.SetTimeouts(0, 0, 100*time.Millisecond)

	a := &auth.Auth{Data: auth.Data{UID: "u", AccessToken: "tok"}}
	stream, _, _, err := c.ChatStream(context.Background(), a, []byte(`{"model":"m","stream":true}`), "")
	if err != nil {
		t.Fatalf("首帧应当成功取到：%v", err)
	}
	defer stream.Close()

	started := time.Now()
	_, aerr := Aggregate(stream, "m")
	elapsed := time.Since(started)

	if elapsed > 2*time.Second {
		t.Fatalf("空闲看门狗没生效，卡了 %s", elapsed)
	}
	var ue *Error
	if !errors.As(aerr, &ue) || ue.Kind != ErrGateway {
		t.Fatalf("期望 gateway 类的空闲超时，实际 %v", aerr)
	}
}
