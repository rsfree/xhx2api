package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testPNG 是一段足够骗过魔数判定的最小 PNG。
func testPNG(tag string) []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), []byte(tag)...)
}

// TestImageRefAddrBlocked 守 SSRF 的第一层：哪些地址永远不许连。
//
// 这张表就是「网关会不会变成内网探测器」的全部依据，少一行就等于开一个口子。
// 分段覆盖而不是只测 127.0.0.1：NAT64 / 6to4 / CGNAT 都能绕过直觉判断。
func TestImageRefAddrBlocked(t *testing.T) {
	blocked := []string{
		// 环回（含 IPv4-mapped IPv6）
		"127.0.0.1", "127.1.2.3", "::1", "::ffff:127.0.0.1",
		// 私网
		"10.0.0.1", "172.16.0.1", "172.31.255.255", "192.168.1.1", "fc00::1",
		// 链路本地 + 云元数据
		"169.254.169.254", "fe80::1",
		// 未指定与本网络段
		"0.0.0.0", "::", "0.0.0.1",
		// CGNAT
		"100.64.0.1",
		// IETF 协议分配 / 文档 / 基准测试
		"192.0.0.170", "192.0.2.1", "198.51.100.1", "203.0.113.1", "198.18.0.1",
		// 保留与广播
		"240.0.0.1", "255.255.255.255",
		// 组播
		"224.0.0.1", "ff02::1",
		// NAT64 / 6to4 / 丢弃前缀 / 文档 v6
		"64:ff9b::7f00:1", "64:ff9b:1::1", "2002:7f00:1::1", "100::1", "2001:db8::1",
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("测试用例写错：%q 不是 IP", s)
		}
		if err := imageRefAddrBlocked(ip); err == nil {
			t.Errorf("%s 应当被拒绝", s)
		}
	}

	allowed := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:4700:4700::1111"}
	for _, s := range allowed {
		if err := imageRefAddrBlocked(net.ParseIP(s)); err != nil {
			t.Errorf("%s 是公网地址，不该被拒：%v", s, err)
		}
	}
}

// TestParseImageRefURL 守 SSRF 的第一层：URL 形态本身。
func TestParseImageRefURL(t *testing.T) {
	bad := []string{
		"file:///etc/passwd",
		"gopher://example.com/1",
		"ftp://example.com/a.png",
		"http://user:pass@example.com/a",
		"http:///a.png",
		"://nope",
		"javascript:alert(1)//x.png",
		"https://[::1/a.png",
	}
	for _, raw := range bad {
		if _, err := parseImageRefURL(raw); err == nil {
			t.Errorf("%q 应当被拒", raw)
		}
	}
	// 两种最容易写错形态的文案要能指导调用方自查。
	if _, err := parseImageRefURL("http://user:pass@example.com/a"); err == nil || !strings.Contains(err.Error(), "用户名") {
		t.Errorf("userinfo 应当报「不能带用户名/密码」，实际：%v", err)
	}
	if _, err := parseImageRefURL("http:///a.png"); err == nil || !strings.Contains(err.Error(), "主机") {
		t.Errorf("缺主机应当报「缺少主机名」，实际：%v", err)
	}

	good := []string{
		"http://example.com/a.png",
		"https://cdn.example.com/x.png?sig=1&x=2",
		"HTTPS://EXAMPLE.COM/UPPER.PNG",
	}
	for _, raw := range good {
		if _, err := parseImageRefURL(raw); err != nil {
			t.Errorf("%q 应当合法：%v", raw, err)
		}
	}
}

// TestImageRefHostAllowed 守白名单的后缀边界：cdn.example.com 不能放行
// evilcdn.example.com（这是最常见的白名单写错）。
func TestImageRefHostAllowed(t *testing.T) {
	f := imageRefFetcher{allowHosts: []string{"cdn.example.com", "s3.amazonaws.com"}}
	cases := map[string]bool{
		"cdn.example.com":          true,
		"a.cdn.example.com":        true,
		"evilcdn.example.com":      false,
		"cdn.example.com.evil.com": false,
		"s3.amazonaws.com":         true,
		"b.s3.amazonaws.com":       true,
		"nots3.amazonaws.com":      false,
		"example.com":              false,
	}
	for host, want := range cases {
		if got := f.hostAllowed(host); got != want {
			t.Errorf("hostAllowed(%q) = %v，期望 %v", host, got, want)
		}
	}
	if !(imageRefFetcher{}).hostAllowed("anything.example") {
		t.Error("白名单为空时应当不限域名")
	}
}

// TestImageRefFetcherFetch 覆盖下载成功与四类拒绝。
//
// 注入 transport 是因为 httptest 监听 127.0.0.1、会被 IP 守卫拦掉；
// 守卫本身由 TestImageRefAddrBlocked 与 TestImageRefFetcherRejectsLoopback 守。
func TestImageRefFetcherFetch(t *testing.T) {
	png := testPNG("ok")
	mux := http.NewServeMux()
	mux.HandleFunc("/ok.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream") // CDN 常态，不该影响判定
		_, _ = w.Write(png)
	})
	mux.HandleFunc("/html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png") // 撒谎的 Content-Type
		_, _ = w.Write([]byte("<html>not an image</html>"))
	})
	mux.HandleFunc("/big-length", func(w http.ResponseWriter, r *http.Request) {
		// 有 Content-Length：应当在读体之前就拒。
		_, _ = w.Write(append(png, bytes.Repeat([]byte{0}, 4096)...))
	})
	mux.HandleFunc("/big-chunked", func(w http.ResponseWriter, r *http.Request) {
		// 无 Content-Length：靠 LimitReader 兜住。
		fl, _ := w.(http.Flusher)
		_, _ = w.Write(png)
		for i := 0; i < 8; i++ {
			_, _ = w.Write(bytes.Repeat([]byte{0}, 512))
			if fl != nil {
				fl.Flush()
			}
		}
	})
	mux.HandleFunc("/empty", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/404", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := imageRefFetcher{transport: &http.Transport{}, maxBytes: 1024}
	ctx := context.Background()

	blob, err := f.fetch(ctx, srv.URL+"/ok.png")
	if err != nil {
		t.Fatalf("正常图片应当下载成功：%v", err)
	}
	if !bytes.Equal(blob, png) {
		t.Fatalf("下载内容不一致：%q", blob)
	}

	for _, tc := range []struct{ path, want string }{
		{"/html", "不是图片"},
		{"/big-length", "超过"},
		{"/big-chunked", "超过"},
		{"/empty", "空响应"},
		{"/404", "HTTP 404"},
	} {
		if _, err := f.fetch(ctx, srv.URL+tc.path); err == nil {
			t.Errorf("%s 应当失败", tc.path)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s 的错误是 %q，期望包含 %q", tc.path, err, tc.want)
		}
	}
}

// TestImageRefFetcherRejectsLoopback 走**真实** transport，验证守卫在生产路径上生效。
func TestImageRefFetcherRejectsLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(testPNG("x"))
	}))
	defer srv.Close()

	f := imageRefFetcher{} // 零值 = 安全默认
	_, err := f.fetch(context.Background(), srv.URL+"/a.png")
	if err == nil {
		t.Fatal("环回地址必须被拒")
	}
	// 对外文案不能说出「环回」「127.0.0.1」这类内网结论。
	if !strings.Contains(err.Error(), "不允许访问") {
		t.Fatalf("文案应当收敛成「不允许访问」，实际：%v", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "环回") {
		t.Fatalf("对外错误不该泄露内网细节：%v", err)
	}
}

// TestResolveImageRefsParallelAndOrdered 同时守两件事：
//   - 并行：慢图不该串行等待（用「同时在途数」证明，而不是计时，避免抖动）
//   - 保序：结果下标必须与输入下标一致（多图生图靠顺序表达语义）
func TestResolveImageRefsParallelAndOrdered(t *testing.T) {
	var inFlight, maxInFlight atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond)
		_, _ = w.Write(testPNG(r.URL.Path))
	}))
	defer srv.Close()

	h := &Handler{cfg: Config{imageFetchTransport: &http.Transport{}}}
	refs := []string{srv.URL + "/a", srv.URL + "/b", srv.URL + "/c", srv.URL + "/d"}
	blobs, err := h.resolveImageRefs(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), refs)
	if err != nil {
		t.Fatalf("并行下载失败：%v", err)
	}
	if len(blobs) != len(refs) {
		t.Fatalf("结果数 %d，期望 %d", len(blobs), len(refs))
	}
	for i, want := range []string{"a", "b", "c", "d"} {
		if !bytes.Contains(blobs[i], []byte(want)) {
			t.Errorf("第 %d 张是 %q，期望含 %q（顺序错了）", i, blobs[i], want)
		}
	}
	if got := maxInFlight.Load(); got < 2 {
		t.Fatalf("同时在途只有 %d 个，说明下载是串行的", got)
	}
}

// TestResolveImageRefsMixedOrder 守「base64 与 URL 混排时下标不串」。
func TestResolveImageRefsMixedOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(testPNG("url"))
	}))
	defer srv.Close()

	local := testPNG("b64")
	h := &Handler{cfg: Config{imageFetchTransport: &http.Transport{}}}
	blobs, err := h.resolveImageRefs(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), []string{
		"data:image/png;base64," + base64.StdEncoding.EncodeToString(local),
		srv.URL + "/x",
	})
	if err != nil {
		t.Fatalf("混排解析失败：%v", err)
	}
	if !bytes.Equal(blobs[0], local) {
		t.Errorf("第 0 张应当是 base64 那张")
	}
	if !bytes.Contains(blobs[1], []byte("url")) {
		t.Errorf("第 1 张应当是下载那张")
	}
}

// TestResolveImageRefsAnyFailureFailsWhole 守「不静默少一张」。
func TestResolveImageRefsAnyFailureFailsWhole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/bad") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(testPNG("ok"))
	}))
	defer srv.Close()

	h := &Handler{cfg: Config{imageFetchTransport: &http.Transport{}}}
	_, err := h.resolveImageRefs(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), []string{
		srv.URL + "/good", srv.URL + "/bad", srv.URL + "/good2",
	})
	if err == nil {
		t.Fatal("一张失败必须整体失败：少一张会让多图生图的语义变成另一张")
	}
}

// TestLocalImageRef 守「自家的图直读盘」：不发网络请求、不越出目录。
func TestLocalImageRef(t *testing.T) {
	dir := t.TempDir()
	png := testPNG("local")
	name := imageID(png) + ".png"
	if err := os.WriteFile(filepath.Join(dir, name), png, 0o600); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: Config{ImageDir: dir, ImagePublicBase: "https://gw.example.com"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)

	blob, handled, err := h.localImageRef(req, "https://gw.example.com/v1/images/"+name)
	if !handled || err != nil {
		t.Fatalf("自家图片应当直读成功（handled=%v err=%v）", handled, err)
	}
	if !bytes.Equal(blob, png) {
		t.Fatal("读到的内容不对")
	}

	// 带查询串也要认（客户端常给 url 加时间戳参数）。
	if _, handled, err := h.localImageRef(req, "https://gw.example.com/v1/images/"+name+"?t=1"); !handled || err != nil {
		t.Fatalf("带查询串应当仍能直读（handled=%v err=%v）", handled, err)
	}

	// 前缀命中但文件不在：报告失效，而不是回落去网络绕一圈。
	if _, handled, err := h.localImageRef(req, "https://gw.example.com/v1/images/"+strings.Repeat("a", 32)+".png"); !handled || err == nil {
		t.Fatalf("文件不在时应当 handled=true 且报错（handled=%v err=%v）", handled, err)
	}

	// 别人的域名不归本地管。
	if _, handled, _ := h.localImageRef(req, "https://evil.example.com/v1/images/"+name); handled {
		t.Fatal("非自家域名不该被本地拦截")
	}

	// 穿越：路径里带 .. 一律拒绝（imageNameOK 是唯一的文件名闸门）。
	for _, bad := range []string{
		"https://gw.example.com/v1/images/../../etc/passwd",
		"https://gw.example.com/v1/images/" + name + "/../../x",
		"https://gw.example.com/v1/images/notahash.png",
	} {
		if _, handled, err := h.localImageRef(req, bad); !handled || err == nil {
			t.Errorf("%q 应当被拒绝（handled=%v err=%v）", bad, handled, err)
		}
	}
}

// TestImageRefBytesHonoursDisableSwitch 守开关：关掉载体时连本地图也不走。
func TestImageRefBytesHonoursDisableSwitch(t *testing.T) {
	h := &Handler{cfg: Config{DisableImageURLInput: true}}
	_, err := h.imageRefBytes(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil),
		"https://example.com/a.png")
	if err == nil || !strings.Contains(err.Error(), "url_input") {
		t.Fatalf("开关关闭时应当报 images.url_input，实际：%v", err)
	}
}
