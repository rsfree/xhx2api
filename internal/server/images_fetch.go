package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 图片引用的第四种载体：http(s) URL。
//
// image-adapter §0.4 把 `image` 的合法载体定为三种字符串（URL / data URI / 裸
// base64），所以「不接受 URL」不是契约的选项。但本网关的上游 /images/edits 只吃
// multipart 字节，没有「把 URL 转给上游自己拉」这条路——URL 必须在这里被换成字节。
//
// 「换成字节」= 网关替调用方发起一次网络请求。谁发起，谁就要校验目标，所以这个
// 文件的主体不是下载，而是准入门。image-adapter §2.3 的措辞值得抄下来：
// 「不能用『关掉下载』当防护 —— 上游只收内联时，URL 垫图必然要下载。」
//
// 三层守卫各管一件事，缺一层就等于没做：
//
//	URL 层  parseImageRefURL        只允许 http/https、不允许 userinfo
//	IP 层   imageRefAddrBlocked     拒绝环回/私网/链路本地/元数据/组播/保留段
//	连接层  imageRefDialContext     解析→校验→**直连该 IP**（防 DNS rebinding）
//
// 第三层最容易被写成「先 LookupIP 校验，再让 Transport 自己解析」。那是两次独立
// 解析：攻击者把 TTL 设成 0，第一次答公网地址通过校验，第二次答 127.0.0.1 就进了
// 内网。把解析收进 DialContext 之后，实际 connect(2) 的目标就是被校验过的那个 IP。

const (
	// imageRefMaxRedirects 是允许跟随的重定向跳数。
	// 每跳都会重新拨号，也就重新过一遍 IP 校验（见 imageRefDialContext）。
	imageRefMaxRedirects = 3
	// imageRefDialTimeout 是单次 TCP 连接的上限；比整次下载预算短，让「慢源」尽快失败。
	imageRefDialTimeout = 5 * time.Second
	// imageRefHeaderTimeout 等响应头的上限。图源常是 CDN，正常秒回。
	imageRefHeaderTimeout = 10 * time.Second
)

// imageRefBlockedNets 是 net.IP 自带判断之外还要显式封掉的网段。
//
// 判据只有一条：**该段在真实公网上不可路由，或者能间接指向内网/本机**。
// 只封「看起来像内网」的段是不够的 —— NAT64、0.0.0.0/8、CGNAT 都能绕过直觉。
var imageRefBlockedNets = func() []struct {
	net *net.IPNet
	why string
} {
	raw := []struct{ cidr, why string }{
		// 0.0.0.0/8：IsUnspecified 只认 0.0.0.0 这一个点，而 0.0.0.1 在
		// Linux 上同样会被路由到本机。
		{"0.0.0.0/8", "本网络段"},
		// 100.64/10：运营商级 NAT。云上很多「内网」也借这段，且从公网不可达。
		{"100.64.0.0/10", "运营商级 NAT"},
		// 192.0.0.0/24：IETF 协议分配段（含 NAT64 发现地址）。
		{"192.0.0.0/24", "IETF 协议分配段"},
		// 三个 TEST-NET 段：文档专用，但内网常被借来当占位/回环别名。
		{"192.0.2.0/24", "文档保留段"},
		{"198.51.100.0/24", "文档保留段"},
		{"203.0.113.0/24", "文档保留段"},
		{"198.18.0.0/15", "基准测试保留段"},
		// 240/4 含 255.255.255.255（广播）与一批保留值。
		{"240.0.0.0/4", "保留段"},
		// NAT64：64:ff9b::/96 能把任意 IPv4 编进 IPv6，是绕过 v4 检查的标准手法。
		{"64:ff9b::/96", "NAT64 映射段"},
		{"64:ff9b:1::/48", "本地 NAT64 映射段"},
		{"100::/64", "丢弃前缀"},
		{"2001:db8::/32", "文档保留段"},
		// 6to4 内嵌 IPv4（2002:AABB:CCDD::/48 里的 AABB:CCDD 是 v4 地址）。
		{"2002::/16", "6to4 隧道段"},
	}
	out := make([]struct {
		net *net.IPNet
		why string
	}, 0, len(raw))
	for _, r := range raw {
		_, n, err := net.ParseCIDR(r.cidr)
		if err != nil {
			// 常量写错属于编码错误，启动即炸比线上静默放行好。
			panic("images_fetch: 非法 CIDR " + r.cidr)
		}
		out = append(out, struct {
			net *net.IPNet
			why string
		}{n, r.why})
	}
	return out
}()

// imageRefAddrBlocked 判断某个 IP 是否禁止作为下载目标。
//
// 返回的文案**不带 IP、不带主机名**：这个错误会回到调用方手里，如果把
// 「10.1.2.3 是私有地址」原样吐出去，网关就成了内网探测器——先试公网域名，
// 再看报错里有没有内网地址，比盲扫快得多。完整地址只进日志。
func imageRefAddrBlocked(ip net.IP) error {
	if ip == nil {
		return errors.New("地址无法解析")
	}
	// IPv4-mapped IPv6（::ffff:127.0.0.1）先归一成 4 字节，后面的段判断才对得上。
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsUnspecified():
		return errors.New("未指定地址")
	case ip.IsLoopback():
		return errors.New("环回地址")
	case ip.IsPrivate():
		return errors.New("私有地址")
	case ip.IsLinkLocalUnicast():
		// 169.254.169.254（云元数据）就在这一段，不必单独列。
		return errors.New("链路本地地址")
	case ip.IsInterfaceLocalMulticast(), ip.IsLinkLocalMulticast(), ip.IsMulticast():
		return errors.New("组播地址")
	}
	for _, b := range imageRefBlockedNets {
		if b.net.Contains(ip) {
			return fmt.Errorf("%s", b.why)
		}
	}
	return nil
}

// parseImageRefURL 做纯字符串校验（不做 DNS），返回规范化 URL。
func parseImageRefURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("不是合法 URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return nil, fmt.Errorf("只支持 http/https，收到 %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("URL 缺少主机名")
	}
	// userinfo 是经典的「看着像 A、连的是 B」手法（http://trusted@127.0.0.1/）。
	// 图片地址不需要认证，直接拒绝比解释给调用方听更省事。
	if u.User != nil {
		return nil, errors.New("URL 不能带用户名/密码")
	}
	return u, nil
}

// imageRefDialContext 解析 → 校验 → 直连校验过的 IP。
//
// 这里刻意不看 Transport 传进来的 addr 里的主机名去「先存后用」：每次拨号
// （含重定向的每一跳）都重新解析、重新校验，连接目标即校验目标。
func imageRefDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("地址格式错误")
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("解析图源域名失败")
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("图源域名没有解析出地址")
	}
	// 任何一个候选被禁就整体拒绝。
	//
	// 为什么不跳过被禁的、连剩下的合法地址：能同时解析出公网与内网的域名，
	// 除了「就近调度」之外还有一种解释——攻击者在探内网。此时不该赌运气。
	for _, ipa := range ips {
		if berr := imageRefAddrBlocked(ipa.IP); berr != nil {
			return nil, fmt.Errorf("拒绝下载：目标地址是%s", berr)
		}
	}
	dialer := &net.Dialer{Timeout: imageRefDialTimeout, KeepAlive: 30 * time.Second}
	var firstErr error
	for _, ipa := range ips {
		conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
		if derr == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = derr
		}
	}
	if firstErr == nil {
		firstErr = errors.New("没有可用地址")
	}
	return nil, fmt.Errorf("连接图源失败: %w", firstErr)
}

// newImageRefTransport 造一个只服务于「下载图片引用」的 Transport。
func newImageRefTransport() *http.Transport {
	return &http.Transport{
		// 🔴 Proxy 必须显式置 nil，不能留给零值之外的任何默认。
		// Transport 默认读 HTTP_PROXY/HTTPS_PROXY；一旦走代理，DialContext 拨的是
		// 代理地址，真实目标藏在 CONNECT 行里，上面那层 IP 校验被整体绕过。
		// 代价：需要代理才能出网的环境里 URL 垫图会失败。这是有意的取舍——
		// 「把校验权外包给代理」本身就是这一段代码要避免的事。
		Proxy:       nil,
		DialContext: imageRefDialContext,
		// 强制 HTTP/1.1：自定义 DialContext 时 h2 的协商路径会变复杂，
		// 而图片下载对多路复用没有任何需求。
		ForceAttemptHTTP2:     false,
		TLSHandshakeTimeout:   imageRefDialTimeout,
		ResponseHeaderTimeout: imageRefHeaderTimeout,
		// 一次下载用完就丢连接：不给 keep-alive 留「下一张图复用上一条已验证连接」的机会。
		DisableKeepAlives: true,
	}
}

// imageRefFetcher 把一个 URL 引用下载成字节。
//
// 零值可用；零值即安全默认（自建 Transport + 三层守卫）。
type imageRefFetcher struct {
	// transport 为 nil 时用 newImageRefTransport()。
	//
	// 只有同包测试会设它：httptest 监听 127.0.0.1，会被 IP 守卫拦掉，所以
	// 需要一条「能连本机」的通道来验证下载逻辑。生产装配不设，配置也触达不到。
	transport http.RoundTripper
	// allowHosts 非空时只允许这些域名（后缀匹配，见 hostAllowed）。
	allowHosts []string
	// maxBytes 单张图片上限（0 = 20MB）。
	maxBytes int64
}

// hostAllowed 判断域名是否在白名单里。
//
// 后缀匹配按标签边界来：白名单 `cdn.example.com` 不能放行 `evilcdn.example.com`。
func (f imageRefFetcher) hostAllowed(host string) bool {
	if len(f.allowHosts) == 0 {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, raw := range f.allowHosts {
		want := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(raw, ".")))
		if want == "" {
			continue
		}
		if host == want || strings.HasSuffix(host, "."+want) {
			return true
		}
	}
	return false
}

func (f imageRefFetcher) limit() int64 {
	if f.maxBytes > 0 {
		return f.maxBytes
	}
	return maxImageBytes
}

func (f imageRefFetcher) roundTripper() http.RoundTripper {
	if f.transport != nil {
		return f.transport
	}
	return newImageRefTransport()
}

// fetch 下载并校验一张图。
//
// 超时由调用方通过 ctx 控制（整次请求共享一个预算），这里只管单次请求的
// 连接与响应头超时。
func (f imageRefFetcher) fetch(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := parseImageRefURL(rawURL)
	if err != nil {
		return nil, err
	}
	if !f.hostAllowed(u.Hostname()) {
		return nil, errors.New("图源域名不在 images.url_fetch_allow_hosts 白名单里")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("构造图源请求失败")
	}
	// 不发网关自己的 UA / 版本：图源不需要知道请求来自哪个版本的网关。
	req.Header.Set("User-Agent", "xhx2api-image-fetch")
	req.Header.Set("Accept", "image/*")

	client := &http.Client{
		Transport: f.roundTripper(),
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= imageRefMaxRedirects {
				return fmt.Errorf("重定向超过 %d 次", imageRefMaxRedirects)
			}
			// 形态每跳重判；IP 层由 DialContext 自动覆盖（每跳都会重新拨号）。
			if _, perr := parseImageRefURL(next.URL.String()); perr != nil {
				return perr
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("下载图源超时")
		}
		// 这里可能带着「目标地址是私有地址」之类的结论，也可能是连接错误。
		// 都只回一句笼统的：具体原因进日志。
		log.Printf("WARN: [images] 下载图片引用失败 url=%s: %v", rawURL, err)
		return nil, fmt.Errorf("下载图源失败: %s", imageRefPublicReason(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("图源返回 HTTP %d", resp.StatusCode)
	}
	limit := f.limit()
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("图片超过 %d MB", limit>>20)
	}
	blob, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("下载图源超时")
		}
		return nil, errors.New("读取图源响应失败")
	}
	if int64(len(blob)) > limit {
		return nil, fmt.Errorf("图片超过 %d MB", limit>>20)
	}
	if len(blob) == 0 {
		return nil, errors.New("图源返回空响应")
	}
	// Content-Type 不可信（CDN 回图片给 application/octet-stream 是常态），
	// 但「下回来的是不是图」必须有结论：把一段 HTML 塞给上游只会换来一条
	// 看不懂的错。按魔数判。
	if _, ok := sniffImageFormat(blob); !ok {
		return nil, errors.New("目标不是图片（按字节判定）")
	}
	return blob, nil
}

// imageRefPublicReason 把下载错误收敛成一句能给调用方看的话。
//
// 两类错误要分开：守卫判定的拒绝是「你的地址不合规」，网络错误是「图源不可达」。
// 前者可以帮助调用方自查，后者只会引导他去猜内网拓扑。
func imageRefPublicReason(err error) string {
	msg := err.Error()
	for _, marker := range []string{"拒绝下载", "私有地址", "环回地址", "链路本地", "组播", "未指定", "NAT64", "保留", "6to4", "本网络"} {
		if strings.Contains(msg, marker) {
			return "目标地址不允许访问（仅支持公网图源）"
		}
	}
	if strings.Contains(msg, "重定向超过") {
		return "图源重定向次数过多"
	}
	return "图源不可达"
}

// localImageRef 判断 URL 是否指向本网关自己落盘的图，是则直接读盘。
//
// 这不是彩蛋优化，而是最常见的用法：网关默认把生成的图落盘并回一个 url，
// 用户拿那个 url 继续改（迭代编辑）就是「自己请求自己」——过一次 nginx、
// 一次鉴权、一次 4K 图网络往返，最后读到的是同一块磁盘。
//
// 返回 (blob, handled, err)：handled=false 表示这个 URL 不归本地管，交给下载路径。
func (h *Handler) localImageRef(r *http.Request, rawURL string) ([]byte, bool, error) {
	if h.cfg.ImageDir == "" {
		return nil, false, nil
	}
	bases := make([]string, 0, 2)
	if v := strings.TrimRight(strings.TrimSpace(h.cfg.ImagePublicBase), "/"); v != "" {
		bases = append(bases, v)
	}
	// 内网直连时 r.Host 是内网名，配置的 public_base 是公网域名：两个都要认，
	// 否则同一次迭代编辑在面板里能用、在容器里就变成绕公网一圈。
	if v := strings.TrimRight(h.imagePublicBase(r), "/"); v != "" {
		dup := false
		for _, b := range bases {
			if b == v {
				dup = true
				break
			}
		}
		if !dup {
			bases = append(bases, v)
		}
	}
	const pathPrefix = "/v1/images/"
	for _, base := range bases {
		if !strings.HasPrefix(rawURL, base+pathPrefix) {
			continue
		}
		name := strings.TrimPrefix(rawURL, base+pathPrefix)
		if i := strings.IndexAny(name, "?#"); i >= 0 {
			name = name[:i]
		}
		if !imageNameOK(name) {
			return nil, true, errors.New("图片名不合法")
		}
		path := filepath.Join(h.cfg.ImageDir, name)
		if _, err := os.Stat(path); err != nil {
			// 前缀命中但文件不在：容量回收删掉了，或这台实例从没落过盘。
			// 不回落走网络——自己请求自己最终也是 404，只会白等一次超时。
			return nil, true, errors.New("图片不存在或已被回收")
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			return nil, true, errors.New("读取本地图片失败")
		}
		if len(blob) > maxImageBytes {
			return nil, true, fmt.Errorf("单张图超过 %d MB", maxImageBytes>>20)
		}
		return blob, true, nil
	}
	return nil, false, nil
}
