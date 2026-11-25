package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rsfree/xhx2api/internal/upstream"
)

// 图片 URL 支持：上游只回 base64，而一张 4K 图的 base64 有 8MB+，
// 塞给前端、工作流或消息队列都很难用。所以网关默认（images.return_url=true）
// 把图落盘/传对象存储，并提供 /v1/images/<id> 直接访问；客户端没传
// response_format 时也只回 url。

// ImageUploader 把图片传到外部存储并返回可访问 URL。
//
// 由 cmd/server 按配置注入（目前是 cosx.Client）；server 包只依赖这个接口，
// 不 import 任何对象存储实现——换 S3/MinIO 时只需要再写一个实现。
type ImageUploader interface {
	Put(ctx context.Context, name string, data []byte, contentType string) (string, error)
}

// imageContentType 按扩展名给出上传用的 Content-Type。
func imageContentType(ext string) string {
	switch ext {
	case ".jpg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

// imageExtByFormat 把上游的 output_format 映射成扩展名。
func imageExtByFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "jpeg", "jpg":
		return ".jpg"
	case "webp":
		return ".webp"
	default:
		return ".png"
	}
}

// imageID 是内容寻址的图片 id：sha256 前 16 字节。
//
// 用内容哈希而不是自增/随机名有两个好处：同一张图重复生成不会产生新文件，
// 且文件名不可枚举（免鉴权的静态路由必须防遍历）。
func imageID(blob []byte) string {
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:16])
}

// noteSigner 把这张图的 C2PA 签名者记到请求日志上。
//
// 一次请求可能回多张图：全部同源就记那个名字，出现第二个不同来源就记 "mixed"。
// 不要用最后一张覆盖前面——"混合后端"恰恰是最需要被看见的那个事实。
//
// 没有任何清单时不写：空串本身是有意义的信息（这条路径不产出溯源凭证），
// 与"还没解析到"混为一谈就会让面板上的比例失真。
func noteSigner(tr *requestTrace, blob []byte) {
	if tr == nil {
		return
	}
	tr.c2pa = mergeSigner(tr.c2pa, upstream.C2PASigner(blob))
}

// mergeSigner 把新一张图的签名者并进已有结论。
//
// 空值只表示"这张没有凭证"，不能覆盖已知来源：一次请求里两张图，第二张无凭证
// 就把第一张的 OpenAI 抹成空，会让面板上的"无凭证率"虚高。
func mergeSigner(cur, next string) string {
	if next == "" {
		return cur
	}
	if cur == "" || cur == next {
		return next
	}
	return "mixed"
}

// attachImageURLs 把响应里每张图落盘（或传对象存储），按 format 决定回什么：
//
//	url   —— 只回 url，删掉 b64_json（默认；省掉几 MB base64）
//	both  —— url 与 b64_json 都给
//
// 只对**真的拿到链接**的那张做处理：失败的那张保留 b64_json，客户端仍能取到图，
// 不会因为一次落盘失败让整次生成白费。返回改写后的响应与成功拿到 url 的张数。
func (h *Handler) attachImageURLs(r *http.Request, body []byte, format string, tr *requestTrace) ([]byte, int, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, 0, fmt.Errorf("响应不是 JSON: %w", err)
	}
	items, _ := payload["data"].([]any)
	if len(items) == 0 {
		return body, 0, nil
	}
	ext := imageExtByFormat(fmt.Sprint(payload["output_format"]))
	base := h.imagePublicBase(r)
	if h.cfg.ImageDir != "" {
		if err := os.MkdirAll(h.cfg.ImageDir, 0o700); err != nil {
			return body, 0, fmt.Errorf("建图片目录失败: %w", err)
		}
	}
	attached := 0
	for _, it := range items {
		obj, ok := it.(map[string]any)
		if !ok {
			continue
		}
		b64, _ := obj["b64_json"].(string)
		if b64 == "" {
			continue
		}
		blob, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			log.Printf("WARN: [images] base64 解码失败: %v", err)
			continue
		}
		noteSigner(tr, blob)
		name := imageID(blob) + ext
		// 优先对象存储：多副本/容器部署下本地盘不共享，外链也更好用。
		link := ""
		if h.cfg.ImageUploader != nil {
			u, uerr := h.cfg.ImageUploader.Put(r.Context(), name, blob, imageContentType(ext))
			if uerr != nil {
				log.Printf("WARN: [images] 上传对象存储失败，回落本地: %v", uerr)
			} else {
				link = u
			}
		}
		if link == "" && h.cfg.ImageDir != "" {
			path := filepath.Join(h.cfg.ImageDir, name)
			if _, statErr := os.Stat(path); statErr != nil {
				if werr := os.WriteFile(path, blob, 0o600); werr != nil {
					log.Printf("WARN: [images] 写图失败 %s: %v", path, werr)
					continue
				}
			}
			link = base + "/v1/images/" + name
		}
		if link == "" {
			// 既没上传成功也没有本地目录：不编造 url，保留 b64 让客户端自取。
			continue
		}
		obj["url"] = link
		attached++
		if format == "url" {
			delete(obj, "b64_json")
		}
	}
	if attached > 0 && h.cfg.ImageDir != "" {
		// 容量闸门：默认开 url 之后每张图都落盘，不设上限迟早撑满磁盘。
		sweepImageDir(h.cfg.ImageDir, h.cfg.ImageMaxTotalMB)
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return body, attached, err
	}
	return out, attached, nil
}

var (
	imageSweepMu sync.Mutex
	imageSweptAt time.Time
)

// imageSweepInterval 让容量回收最多每分钟跑一次：每写一张图都全目录 stat
// 一遍，在几千个文件时纯属浪费。
const imageSweepInterval = time.Minute

// sweepImageDir 把目录压回总量上限内：按 mtime 从旧到新删，直到降到 90%。
//
// 只删 imageNameOK 认得的文件——目录里万一混进别的东西，一个都不能动。
// maxMB <= 0 表示不限制。返回删掉的文件数与释放字节数。
//
// 注意删掉的旧链接会 404：内容寻址只是让同一张图不重复落盘，
// 不代表这里是永久存储。
func sweepImageDir(dir string, maxMB int) (files int, freed int64) {
	if dir == "" || maxMB <= 0 {
		return 0, 0
	}
	imageSweepMu.Lock()
	defer imageSweepMu.Unlock()
	if time.Since(imageSweptAt) < imageSweepInterval {
		return 0, 0
	}
	imageSweptAt = time.Now()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	type img struct {
		name string
		size int64
		mod  time.Time
	}
	all := make([]img, 0, len(entries))
	var total int64
	for _, e := range entries {
		if e.IsDir() || !imageNameOK(e.Name()) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		all = append(all, img{name: e.Name(), size: info.Size(), mod: info.ModTime()})
		total += info.Size()
	}
	limit := int64(maxMB) << 20
	if total <= limit {
		return 0, 0
	}
	// 删到 90%：留点余量，免得刚删完下一张图又触发一轮全目录扫描。
	target := limit / 10 * 9
	sort.Slice(all, func(i, j int) bool { return all[i].mod.Before(all[j].mod) })
	for _, f := range all {
		if total <= target {
			break
		}
		if rerr := os.Remove(filepath.Join(dir, f.name)); rerr != nil {
			continue
		}
		total -= f.size
		files++
		freed += f.size
	}
	if files > 0 {
		log.Printf("INFO: [images] 图片目录超过 %dMB，回收 %d 个最旧的文件（释放 %.1fMB）",
			maxMB, files, float64(freed)/(1<<20))
	}
	return files, freed
}

// imagePublicBase 决定写进 url 的对外基址。
//
// 优先用配置的 public_base：反向代理后面 r.Host 是内网名，直接把内网地址
// 写进 url，客户端拿到也访问不了。没配就按请求头推导，够本机直连用。
func (h *Handler) imagePublicBase(r *http.Request) string {
	if v := strings.TrimRight(strings.TrimSpace(h.cfg.ImagePublicBase), "/"); v != "" {
		return v
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); v != "" {
		scheme = v
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// imageFile 提供落盘图片的访问（GET /v1/images/{name}）。
//
// 不带鉴权：浏览器 <img src>、Markdown 预览、第三方下载器都发不了
// Authorization 头。安全性靠文件名——内容 sha256 的前 16 字节，不可枚举。
func (h *Handler) imageFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if h.cfg.ImageDir == "" || !imageNameOK(name) {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(h.cfg.ImageDir, name)
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, r)
		return
	}
	// 内容寻址 => 文件永远不会变，可以永久缓存。
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, path)
}

// imageNameOK 校验文件名形如 <32 位小写 hex>.<png|jpg|webp>。
func imageNameOK(name string) bool {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return false
	}
	dot := strings.LastIndex(name, ".")
	if dot <= 0 {
		return false
	}
	id, ext := name[:dot], name[dot:]
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	switch ext {
	case ".png", ".jpg", ".webp":
		return true
	}
	return false
}

// resetImageSweepThrottle 清掉节流时间戳（测试用：否则同一分钟内第二次调用直接返回）。
func resetImageSweepThrottle() {
	imageSweepMu.Lock()
	imageSweptAt = time.Time{}
	imageSweepMu.Unlock()
}
