package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/rsfree/xhx2api/internal/auth"
	"github.com/rsfree/xhx2api/internal/pool"
	"github.com/rsfree/xhx2api/internal/upstream"
)

// imageRequest 是转发一次图片任务的规范形状。
//
// 为什么要这一层：OpenAI 把 generations 与 edits 拆成两个端点，纯粹是传输
// 差异（JSON vs multipart），不是语义差异。上游更极端——/images/gen 收到
// `image` 会**静默忽略**（实测 image_tokens=0，出的是无关的新图），图生图
// 只有 /images/edits 认。所以网关必须在内部按「有没有输入图」分流，
// 而不能把客户端的 image 原样透传给 /images/gen 假装支持。
type imageRequest struct {
	prompt string
	// images 为空 = 纯文生图；非空 = 走 edits。
	images [][]byte
	mask   []byte
	// fields 是白名单参数（size/quality/n/...），原样带过去。
	fields map[string]string
	// format 是这次响应的形态：url（默认）/ b64_json / both。
	// 客户端没传 response_format 时取配置的默认值。
	format string
	title  string
	// model 是客户端填的模型名，只用于请求日志/面板归因。
	//
	// 上游 /images/gen 完全忽略这个字段（实测 gpt-image-2、gpt-image-2.5 和
	// 捏造名都 200，image_tokens 一模一样），所以它不参与路由；但用户排查
	// 「为什么我要 gpt-image-2 出来不像」时需要看到自己填了什么。
	model string
}

// 单图与总量的上限，与上游 gpt-image-2 的 20MB/张对齐。
const (
	maxImageBytes   = 20 << 20
	maxImagesPerReq = 16
	// maxImageJSONBytes 是图片 JSON 请求体上限，与 multipart 的 64MiB 对齐
	// （nginx 的 client_max_body_size 也是 64m）。
	maxImageJSONBytes = 64 << 20
)

// 允许透传给上游的参数（其余一律丢弃：上游对未声明字段严格校验）。
var imageFieldWhitelist = []string{"size", "quality", "n", "background", "output_format"}

// imageRateRetryDelay 是生图遇到 429 后原地重试前的等待。
//
// 实测 429 的正文是 `{"code":200103,"message":"limiter_request_error",
// "details":"request exceed max time, retry after:0s"}`，1 秒后重试必成功
// （12 并发 → 首发 10 成功 + 2 个 429 → 重试 12/12）。这里留一倍余量给
// 上游的滑动窗口，同时相对 60 秒的生成时间可以忽略。
const imageRateRetryDelay = 2 * time.Second

// imageGenerations 处理 POST /v1/images/generations。
//
// 这个端点同时承担文生图与图生图：
//   - 没有 `image` 字段 → 走上游 /images/gen（纯文生图）；
//   - 带 `image`（字符串或数组）→ 网关自己转成 multipart 打 /images/edits。
//
// `image` 支持 data URI 与裸 base64；`null` / `[]` / 空串一律视为「没有图」，
// 和 OpenAI SDK 的两种空值写法对齐。
func (h *Handler) imageGenerations(w http.ResponseWriter, r *http.Request) {
	h.handleImageJSON(w, r, time.Now(), traceFrom(r.Context()))
}

// handleImageJSON 解析 OpenAI Images 的 JSON 形态。
//
// generations 与 edits 共用它：两个端点的差别只在传输方式（JSON vs multipart），
// 语义完全一致，所以 /v1/images/edits 收到 JSON 也走这里（见 imageEdits）。
func (h *Handler) handleImageJSON(w http.ResponseWriter, r *http.Request, started time.Time, tr *requestTrace) {
	// 用图专用的上限：base64 有 4/3 膨胀，一张 4K PNG 就要 7MB，
	// 沿用对话的 32MiB 会让「两张 4K 图生图」直接 413。
	limit := h.cfg.MaxBodyBytes
	if limit < maxImageJSONBytes {
		limit = maxImageJSONBytes
	}
	body, err := readBody(r, limit)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
		return
	}
	req := imageRequest{prompt: strings.TrimSpace(upstream.ImagePromptOf(body)), fields: map[string]string{}}

	refs, err := extractImageRefs(payload["image"])
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// mask 只在有图时有意义（无图还给 mask 是调用方搞错了）。
	if maskRef, _ := payload["mask"].(string); strings.TrimSpace(maskRef) != "" {
		if len(refs) == 0 {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
				"mask 需要配合 image 使用：纯文生图没有可遮罩的区域")
			return
		}
		if req.mask, err = decodeImageRef(maskRef); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "mask 解码失败: "+err.Error())
			return
		}
	}
	if req.prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "prompt 必填（上游对空 prompt 直接 400）")
		return
	}
	for _, ref := range refs {
		blob, derr := decodeImageRef(ref)
		if derr != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "image 解码失败: "+derr.Error())
			return
		}
		if len(blob) > maxImageBytes {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("单张图超过 %d MB", maxImageBytes>>20))
			return
		}
		req.images = append(req.images, blob)
	}
	if len(req.images) > maxImagesPerReq {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("一次最多 %d 张输入图", maxImagesPerReq))
		return
	}

	req.format, err = h.resolveResponseFormat(payload["response_format"])
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	req.title, _ = payload["title"].(string)
	req.model, _ = payload["model"].(string)
	req.model = strings.TrimSpace(req.model)
	for _, key := range imageFieldWhitelist {
		if v, ok := payload[key]; ok {
			if s := scalarString(v); s != "" {
				req.fields[key] = s
			}
		}
	}
	h.dispatchImage(w, r, started, tr, req)
}

// imageEdits 处理 POST /v1/images/edits。
//
// 三种请求形态都收，语义完全一致（都是「带输入图的图生图」）：
//
//	Content-Type: application/json                            → 与 generations 同一个解析器
//	Content-Type: multipart/form-data                          → image/image[] 文件或 base64 文本
//	Content-Type: application/x-www-form-urlencoded            → curl -d 的默认编码
//
// 为什么不只认 multipart：OpenAI 的 edits 规范只写了 multipart，但实际客户端
// 五花八门——有人发 JSON（因为 generations 就是 JSON），有人用 curl -d（默认
// urlencoded）。因为「编码不同」回一个看不懂的 400，纯属刁难调用方。
func (h *Handler) imageEdits(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	tr := traceFrom(r.Context())

	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "application/json") {
		h.handleImageJSON(w, r, started, tr)
		return
	}

	const maxEditBytes = 64 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxEditBytes)

	var values map[string][]string
	var files map[string][]*multipart.FileHeader
	urlencoded := strings.HasPrefix(ct, "application/x-www-form-urlencoded")

	if urlencoded {
		if err := r.ParseForm(); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "表单解析失败: "+err.Error())
			return
		}
		values = r.PostForm
	} else {
		if err := r.ParseMultipartForm(maxEditBytes); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
				"需要 multipart/form-data、application/json 或 x-www-form-urlencoded（image 传文件或 base64）："+err.Error())
			return
		}
		defer func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()
		values = r.MultipartForm.Value
		files = r.MultipartForm.File
	}

	req, err := buildFormImageRequest(values, files, urlencoded)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	req.format, err = h.resolveResponseFormat(formFirst(values, "response_format"))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	h.dispatchImage(w, r, started, tr, req)
}

// formFirst 取表单里某个键的第一个值（表单值天然是 []string）。
func formFirst(values map[string][]string, key string) string {
	if v := values[key]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// buildFormImageRequest 把表单拆成规范请求。multipart 与 urlencoded 共用。
//
// urlencoded=true 时要把 base64 里的 `+` 还原回来：表单解码会把 `+` 当空格，
// 而 base64 字母表里本来就有 `+`——不还原的话，含 `+` 的图会静默解错/解不出。
func buildFormImageRequest(values map[string][]string, files map[string][]*multipart.FileHeader, urlencoded bool) (imageRequest, error) {
	req := imageRequest{prompt: strings.TrimSpace(formFirst(values, "prompt")), fields: map[string]string{}}
	if req.prompt == "" {
		return req, fmt.Errorf("prompt 必填")
	}

	// 图片有两种进法，都要收：
	//   1) 文件字段（image / image[]）—— 浏览器表单、curl -F、SDK 都是这种；
	//   2) 文本字段（同名，值是 base64 或 data URI）—— 把 JSON 硬转表单的
	//      客户端常这样发；只认文件会让它拿到「image 必填」的 400。
	var refs []string
	for _, key := range []string{"image", "image[]"} {
		for _, fh := range files[key] {
			// 空 part 表示「没附文件」（浏览器未选文件、或读取失败），
			// 与 JSON 侧的 null 同义，不能因此报错。
			if fh.Size == 0 {
				continue
			}
			if fh.Size > maxImageBytes {
				return req, fmt.Errorf("%s 超过 %d MB", fh.Filename, maxImageBytes>>20)
			}
			f, oerr := fh.Open()
			if oerr != nil {
				return req, fmt.Errorf("读取上传图片失败: %w", oerr)
			}
			blob, rerr := io.ReadAll(io.LimitReader(f, maxImageBytes))
			_ = f.Close()
			if rerr != nil {
				return req, fmt.Errorf("读取上传图片失败: %w", rerr)
			}
			req.images = append(req.images, blob)
		}
		for _, v := range values[key] {
			if strings.TrimSpace(v) != "" {
				refs = append(refs, v)
			}
		}
	}
	for _, ref := range refs {
		if urlencoded {
			ref = strings.ReplaceAll(ref, " ", "+")
		}
		blob, derr := decodeImageRef(ref)
		if derr != nil {
			return req, fmt.Errorf("image 解码失败: %w", derr)
		}
		if len(blob) > maxImageBytes {
			return req, fmt.Errorf("单张图超过 %d MB", maxImageBytes>>20)
		}
		req.images = append(req.images, blob)
	}
	if len(req.images) == 0 {
		return req, fmt.Errorf("image 必填（字段名 image 或 image[]，可传文件或 base64/data URI）")
	}
	if len(req.images) > maxImagesPerReq {
		return req, fmt.Errorf("一次最多 %d 张输入图", maxImagesPerReq)
	}

	// mask：文件优先，其次是文本形态。
	if fhs := files["mask"]; len(fhs) > 0 && fhs[0].Size > 0 {
		f, oerr := fhs[0].Open()
		if oerr != nil {
			return req, fmt.Errorf("读取 mask 失败: %w", oerr)
		}
		defer f.Close()
		blob, rerr := io.ReadAll(io.LimitReader(f, maxImageBytes))
		if rerr != nil {
			return req, fmt.Errorf("读取 mask 失败: %w", rerr)
		}
		req.mask = blob
	} else if v := strings.TrimSpace(formFirst(values, "mask")); v != "" {
		if urlencoded {
			v = strings.ReplaceAll(v, " ", "+")
		}
		blob, derr := decodeImageRef(v)
		if derr != nil {
			return req, fmt.Errorf("mask 解码失败: %w", derr)
		}
		req.mask = blob
	}

	req.title = strings.TrimSpace(formFirst(values, "title"))
	req.model = strings.TrimSpace(formFirst(values, "model"))
	for _, key := range imageFieldWhitelist {
		if v := strings.TrimSpace(formFirst(values, key)); v != "" {
			req.fields[key] = v
		}
	}
	return req, nil
}

// dispatchImage 按「有没有输入图」选上游端点。
func (h *Handler) dispatchImage(w http.ResponseWriter, r *http.Request, started time.Time,
	tr *requestTrace, req imageRequest) {
	if tr != nil {
		tr.model = req.model
		if tr.model == "" {
			tr.model = upstream.ImageModelID
		}
		tr.stream = false
	}
	// 上游有两条尺寸约束（都会让用户白等或拿到小图）：
	//   - 最长边 ≤ 3840：超了先跑 106 秒再回 400；
	//   - 总像素 ≤ 3840×2160：超了不报错，静默降级成更小的图（实测 3840x3840 → 2880x2880）。
	// 所以网关在转发前统一等比压缩，并在响应头标注实际提交的尺寸。
	if raw := req.fields["size"]; raw != "" {
		if fixed, changed := upstream.ClampImageSize(raw); changed {
			req.fields["size"] = fixed
			log.Printf("INFO: [images] size %q 超上游约束（最长边 ≤ %d、像素 ≤ %d），已等比压到 %q",
				raw, upstream.MaxImageEdge, upstream.MaxImagePixels, fixed)
			w.Header().Set("X-XHX-Image-Size", fixed)
		}
	}

	if len(req.images) == 0 {
		// 文生图：保持 JSON 直通 /images/gen。
		payload := map[string]any{"prompt": req.prompt}
		for k, v := range req.fields {
			payload[k] = v
		}
		out, err := json.Marshal(payload)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "rewrite body: "+err.Error())
			return
		}
		h.serveImage(w, r, started, tr, req.format, func(ctx context.Context, a *auth.Auth) (*upstream.ImageReply, error) {
			return h.cfg.Upstream.GenerateImage(ctx, a, out, req.title)
		})
		return
	}

	// 图生图：上游只认 multipart，网关在这里转。
	body, contentType, err := buildEditMultipart(req)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "构造上游请求失败: "+err.Error())
		return
	}
	h.serveImage(w, r, started, tr, req.format, func(ctx context.Context, a *auth.Auth) (*upstream.ImageReply, error) {
		return h.cfg.Upstream.EditImage(ctx, a, body, contentType, req.title)
	})
}

// buildEditMultipart 把规范请求拼成上游 /images/edits 要的 multipart。
//
// 文件名按魔数给扩展名：上游会看扩展名判断图片类型，全叫 .png 会让 jpeg
// 输入走错解码分支。
func buildEditMultipart(req imageRequest) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("prompt", req.prompt); err != nil {
		return nil, "", err
	}
	for _, key := range imageFieldWhitelist {
		if v := req.fields[key]; v != "" {
			if err := mw.WriteField(key, v); err != nil {
				return nil, "", err
			}
		}
	}
	for i, blob := range req.images {
		fw, err := mw.CreateFormFile("image", fmt.Sprintf("image-%d%s", i, sniffImageExt(blob)))
		if err != nil {
			return nil, "", err
		}
		if _, err := fw.Write(blob); err != nil {
			return nil, "", err
		}
	}
	if len(req.mask) > 0 {
		fw, err := mw.CreateFormFile("mask", "mask"+sniffImageExt(req.mask))
		if err != nil {
			return nil, "", err
		}
		if _, err := fw.Write(req.mask); err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// shouldRotateImage 决定生图失败后要不要换号继续。
//
// 只有「客户端还在等」的类别才换：session_dead / server / gateway 是账号或
// 上游的问题，换个号可能就成了。
//
// 🔴 client（499）不能换。它是调用方自己断开——换号继续生成只会白烧一次上游
// 配额、占着账号并发额度，还会连记几个 499 把面板失败率拉高。对话路径由
// shouldRotate 的白名单天然挡住了这一类，生图这里曾经写成黑名单并且把 client
// 归到了「可换号」那一侧（2026-10-26 实测：客户端 20s 超时后网关换了第二个号
// 又完整跑了一遍，两次都记 499）。
func shouldRotateImage(kind pool.ErrKind) bool {
	switch kind {
	case "session_dead", "server", "gateway":
		return true
	}
	return false
}

// resolveResponseFormat 决定这次响应回什么形态。
//
// 客户端没传（空 / null / None）时用配置的默认值（默认 url）；传了就以客户端
// 为准，只接受 url / b64_json。"both" 只作为配置项存在——它意味着多传几 MB base64，
// 客户端真要两者，完全可以从 url 自己拉一次。
func (h *Handler) resolveResponseFormat(v any) (string, error) {
	raw := strings.ToLower(strings.TrimSpace(scalarString(v)))
	// JSON 的 null 解出来就是 nil（scalarString 给空串）；multipart 里删掉字段
	// 也是空串。再容忍字面量 "none"/"null"——Python 侧
	// `response_format=None` 被某些客户端直接拼成字符串发过来，
	// 那不是「要 None 形态」，语义与不传完全一样。
	if raw == "none" || raw == "null" {
		raw = ""
	}
	if raw == "" {
		switch f := strings.ToLower(strings.TrimSpace(h.cfg.ImageDefaultFormat)); f {
		case "b64_json":
			return "b64_json", nil
		case "both":
			if h.canServeURL() {
				return "both", nil
			}
			return "b64_json", nil
		default: // "url"（默认）或未配置
			if h.canServeURL() {
				return "url", nil
			}
			// 没点名要 url 时不报错：静默回落 base64，功能不因此中断。
			return "b64_json", nil
		}
	}
	switch raw {
	case "url":
		if !h.canServeURL() {
			return "", fmt.Errorf("图片 url 不可用：images.return_url 已关闭且未接对象存储；去掉 response_format，或显式传 b64_json")
		}
		return "url", nil
	case "b64_json":
		return "b64_json", nil
	}
	return "", fmt.Errorf("response_format 只支持 url 或 b64_json，收到 %q", raw)
}

// canServeURL 表示网关此刻真能把图变成链接：本地目录与对象存储二者其一。
func (h *Handler) canServeURL() bool {
	return h.cfg.ImageReturnURL && (h.cfg.ImageDir != "" || h.cfg.ImageUploader != nil)
}

// extractImageRefs 接受 null / 字符串 / 字符串数组，空值表示没有图。
func extractImageRefs(v any) ([]string, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, nil
		}
		return []string{t}, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("image 数组里只能是字符串（data URI 或 base64）")
			}
			if strings.TrimSpace(s) == "" {
				continue
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("image 必须是字符串、字符串数组或 null（支持 base64 与 data URI）")
}

// decodeImageRef 把 data URI 或裸 base64 解成字节。
//
// 不支持 http(s) URL：那需要网关替调用方去抓取，会引入 SSRF 面（内网地址、
// 元数据服务）。要这种形态就让客户端自己下载后转 base64。
func decodeImageRef(ref string) ([]byte, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("空图片")
	}
	if strings.HasPrefix(ref, "data:") {
		comma := strings.Index(ref, ",")
		if comma < 0 {
			return nil, fmt.Errorf("data URI 缺少逗号")
		}
		if !strings.Contains(ref[:comma], ";base64") {
			return nil, fmt.Errorf("只支持 base64 编码的 data URI")
		}
		ref = ref[comma+1:]
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return nil, fmt.Errorf("暂不支持 URL 形态：请由客户端下载后转成 base64 或 data URI")
	}
	blob, err := base64.StdEncoding.DecodeString(ref)
	if err != nil {
		// 兼容 URL-safe 与缺省 padding 的写法。
		if alt, altErr := base64.RawURLEncoding.DecodeString(strings.TrimRight(ref, "=")); altErr == nil {
			return alt, nil
		}
		return nil, fmt.Errorf("不是合法 base64: %w", err)
	}
	return blob, nil
}

// sniffImageExt 按魔数给扩展名。
func sniffImageExt(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return ".png"
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return ".jpg"
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return ".gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return ".webp"
	}
	return ".png"
}

// scalarString 把 JSON 标量渲染成字符串（n 可能是数字或字符串）。
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return ""
}

// imageSender 发一次图片请求；由调用方决定打哪个上游端点。
type imageSender func(ctx context.Context, a *auth.Auth) (*upstream.ImageReply, error)

// serveImage 是生图与改图共用的执行段：选号 → 调上游 → 计量 → 回写。
//
// 抽出来是为了让两条路由共享同一套换号与错误分类：图生图的失败模式和生图
// 完全一致（超时不换号、账号级错误才换号），复制一份迟早会分叉。
func (h *Handler) serveImage(w http.ResponseWriter, r *http.Request, started time.Time,
	tr *requestTrace, format string, send imageSender) {
	var lastErr error
	var lastKind pool.ErrKind
	excluded := map[string]bool{}
	for attempt := 0; attempt < h.cfg.MaxRotate; attempt++ {
		if tr != nil {
			tr.attempts = attempt + 1
		}
		lease, perr := h.pickForRequest(r.Context(), "", excluded)
		if perr != nil {
			if lastErr == nil {
				lastErr = perr
			}
			break
		}
		a := lease.Auth()
		if a == nil {
			lease.ReleaseSuccess(0)
			lastErr = pool.ErrNoAccount
			break
		}
		if tr != nil {
			tr.uid = a.UID
			tr.account = a.Snapshot().Nickname
		}
		if _, rerr := h.cfg.Upstream.RefreshTokenIfNeeded(r.Context(), a, h.cfg.RefreshSkew); rerr != nil {
			log.Printf("WARN: [refresh] %s: %v", a.UID, rerr)
		}

		timeout := h.cfg.ImageTimeout
		if timeout <= 0 {
			// 兜底与 config 默认值保持一致：4K 压缩后实测能到 267s，5 分钟太紧。
			timeout = 10 * time.Minute
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		reply, gerr := send(ctx, a)
		elapsed := time.Since(started)
		if gerr != nil {
			cancel()
			kind := classifyErr(gerr, 0, gerr.Error())
			lastKind, lastErr = kind, gerr
			if tr != nil {
				tr.errKind, tr.errMsg = kind, gerr.Error()
			}
			log.Printf("WARN: [images] uid=%s attempt=%d kind=%s: %v", a.UID, attempt+1, kind, gerr)

			// 429 是瞬时过载，不是账号故障：实测 12 并发生图首发 10 成功、
			// 2 个 429（立即返回、retry after: 0s），等 1 秒重试**全部成功**。
			// 换号帮不上忙（瓶颈是单账号并发配额），罚号更没道理（会把好号
			// 打进软冷却，连带影响同一账号的对话请求）。所以原地等一小会再来。
			if kind == upstream.ErrSoftRate.String() && attempt+1 < h.cfg.MaxRotate {
				lease.ReleaseSuccess(elapsed) // 不计失败、不触发冷却
				if tr != nil {
					tr.rotations++
				}
				log.Printf("INFO: [images] uid=%s 429 瞬时过载，%s 后原地重试", a.UID, imageRateRetryDelay)
				select {
				case <-time.After(imageRateRetryDelay):
				case <-r.Context().Done():
					writeOpenAIError(w, http.StatusRequestTimeout, "client_closed", "客户端已断开")
					return
				}
				continue
			}

			lease.Release(&kind, gerr.Error(), elapsed)
			if !shouldRotateImage(kind) {
				break
			}
			excluded[a.UID] = true
			if tr != nil {
				tr.rotations++
			}
			continue
		}
		cancel()
		lease.ReleaseSuccess(elapsed)
		promptTokens, completionTokens, total := tokenCounts(reply.Usage())
		if tr != nil {
			tr.prompt, tr.completion, tr.total = promptTokens, completionTokens, total
		}
		h.recordUsage(tr, upstream.ImageModelID, false, promptTokens, completionTokens, total, elapsed)
		body := reply.Body
		// b64_json 时完全不碰磁盘：客户端明确要 base64，没必要为它写一份。
		if format != "b64_json" && h.canServeURL() {
			out, attached, uerr := h.attachImageURLs(r, body, format, tr)
			switch {
			case uerr != nil:
				// 落盘失败不该让整次生成白费：图还是给客户端，只是没有 url。
				log.Printf("WARN: [images] 生成 url 失败，回落 base64: %v", uerr)
				w.Header().Set("X-XHX-Image-URL", "unavailable")
			case attached == 0:
				log.Printf("WARN: [images] 没有一张图拿到 url，回落 base64")
				w.Header().Set("X-XHX-Image-URL", "unavailable")
			default:
				body = out
				w.Header().Set("X-XHX-Image-URL", "ok")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}

	status, code, msg := errorResponse(lastErr, lastKind, h.cfg.Pool.Counts())
	writeOpenAIError(w, status, code, msg)
}
