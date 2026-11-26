package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"

	// 只为 image.DecodeConfig 注册格式（读 mask / 底图尺寸，不解码像素）。
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

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

	// maxImageTotalBytes 是一次请求里所有输入图的总字节上限。
	//
	// 单张 20MB × 16 张 = 320MB，全部会进内存再拼成 multipart 发给上游：
	// 既撑不住，也超过上游与 nginx 的 64MiB。取 48MiB 给 multipart 边界留余量。
	//
	// URL 载体让这条限制从「客户端要上传多少」变成了「服务端要拉多少」——
	// 16 个链接的请求体只有 1KB，但代价全落在网关这边，所以必须单独卡总量。
	maxImageTotalBytes = 48 << 20

	// imageRefFetchConcurrency 是一批 URL 垫图的并发上限。
	//
	// 不按张数全开：16 张同时打同一个 CDN 基本等于自我限流。
	// 6 并发跑 16 张只要 3 批，与总预算（默认 30s）吻合。
	imageRefFetchConcurrency = 6

	// defaultImageURLFetchTimeout 是一批 URL 垫图的总预算（不是单张）。
	defaultImageURLFetchTimeout = 30 * time.Second
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
// 按 OpenAI 的契约，这个端点**只做文生图**：官方 generations 的参数表里没有
// `image`，图生图是 /v1/images/edits 的职责。所以默认收到 `image` 会 400 并
// 指向 edits——不静默忽略，也不偷偷转成图生图（后者会让客户端以为自己在用
// OpenAI，实际拿到的是另一个端点的语义）。
//
// 历史用法「把图塞给 generations」由 images.generations_accepts_image 保留：
// 打开后网关按「有图就转 edits」处理，与旧版本一致。
func (h *Handler) imageGenerations(w http.ResponseWriter, r *http.Request) {
	h.handleImageJSON(w, r, time.Now(), traceFrom(r.Context()), false)
}

// handleImageJSON 解析 OpenAI Images 的 JSON 形态。
//
// generations 与 edits 共用它：两个端点的差别在传输方式（JSON vs multipart）
// 之外，还有一处语义差别——**generations 不带输入图**（editsOnly=false）。
// 其余解析、载体换算、预算校验完全一致，复制一份迟早会分叉。
func (h *Handler) handleImageJSON(w http.ResponseWriter, r *http.Request, started time.Time, tr *requestTrace, editsOnly bool) {
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
	// 张数在下载之前判：超限是零成本的结论，不该先替调用方拉 20 张图再报错。
	if len(refs) > maxImagesPerReq {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("一次最多 %d 张输入图", maxImagesPerReq))
		return
	}
	// mask 只在有图时有意义（无图还给 mask 是调用方搞错了）。
	maskRef, _ := payload["mask"].(string)
	maskRef = strings.TrimSpace(maskRef)
	if maskRef != "" && len(refs) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"mask 需要配合 image 使用：纯文生图没有可遮罩的区域")
		return
	}
	// mask 只对单张底图有定义。
	//
	// 区域 targeting 落在哪张图上没有答案，OpenAI 也只在单图上定义 mask；
	// 上游收到「多图 + mask」的行为未测。与其让它挑一张、把另外几张整图重画，
	// 不如在零成本的地方拒掉（image-adapter §1.6.4）。
	if maskRef != "" && len(refs) > 1 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("mask 只支持单张底图，当前给了 %d 张", len(refs)))
		return
	}
	// 双端点分工：generations 只做文生图。
	//
	// 拒绝放在下载之前——判定一个请求不合规不该先花 30 秒替它拉图。
	// 不选「静默忽略 image」：OpenAI 会忽略未知字段，但那会让调用方以为
	// 图生图生效了，而实际拿到一张无关的新图（上游 /images/gen 正是这么做的，
	// 见 upstream/images.go 的实测记录）。宁可明确失败。
	if !editsOnly && !h.cfg.ImageGenerationsAcceptsImage && (len(refs) > 0 || maskRef != "") {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"图生图请用 POST /v1/images/edits（OpenAI 的 generations 只做文生图）；"+
				"要继续在 generations 上传图，把 images.generations_accepts_image 设为 true")
		return
	}
	if req.prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "prompt 必填（上游对空 prompt 直接 400）")
		return
	}

	// URL 载体要下载：整批共享一个预算（单张的连接/响应头超时更短，见 images_fetch.go）。
	ctx, cancel := h.imageFetchContext(r)
	defer cancel()

	// mask 与 images 并行：两者都是 URL 时，没有理由让主图等遮罩先下完。
	// 提前 return 时 defer cancel 会把还在跑的 mask 下载一并取消，不会泄漏。
	var (
		maskBlob []byte
		maskErr  error
		maskDone chan struct{}
	)
	if maskRef != "" {
		maskDone = make(chan struct{})
		go func(ref string) {
			defer close(maskDone)
			maskBlob, maskErr = h.imageRefBytes(ctx, r, ref)
		}(maskRef)
	}
	if req.images, err = h.resolveImageRefs(ctx, r, refs); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "image 解码失败: "+err.Error())
		return
	}
	if maskDone != nil {
		<-maskDone
		if maskErr != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "mask 解码失败: "+maskErr.Error())
			return
		}
		if len(maskBlob) > maxImageBytes {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("mask 超过 %d MB", maxImageBytes>>20))
			return
		}
		req.mask = maskBlob
	}
	if err := checkImageBudget(req.images); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := checkMaskGeometry(req.images, req.mask); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
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

// imageEdits 处理 POST /v1/images/edits —— 图生图的**唯一**入口。
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
//
// 反过来，generations 不再接图（见 imageGenerations）：两个端点各有主责，
// 宽容只用在「同一个语义的不同编码」上，不用在「换个端点也能干」上。
func (h *Handler) imageEdits(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	tr := traceFrom(r.Context())

	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "application/json") {
		h.handleImageJSON(w, r, started, tr, true)
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

	parts, err := collectFormImageParts(values, files, urlencoded)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	ctx, cancel := h.imageFetchContext(r)
	defer cancel()
	req, err := h.finishFormImageRequest(ctx, r, parts)
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

// formImageParts 是表单解析出的半成品：文件已读成字节，文本引用还没解析。
//
// 拆成两步是因为文本引用可能是 URL（要下载、并行、有预算），而文件读取与
// 表单结构校验是零网络的——后者应当先把所有能判死的错误判掉，不要让调用方
// 等了 20 秒下载才发现「prompt 是空的」。
type formImageParts struct {
	prompt   string
	files    [][]byte
	refs     []string
	maskFile []byte
	maskRef  string
	title    string
	model    string
	fields   map[string]string
}

// collectFormImageParts 把表单拆成结构 + 待解析引用。multipart 与 urlencoded 共用。
//
// urlencoded=true 时要把 base64 里的 `+` 还原回来：表单解码会把 `+` 当空格，
// 而 base64 字母表里本来就有 `+`——不还原的话，含 `+` 的图会静默解错/解不出。
// 这条对 URL 一并生效：两者都是「客户端没按 %2B/%20 编码」的同一种容错。
func collectFormImageParts(values map[string][]string, files map[string][]*multipart.FileHeader, urlencoded bool) (formImageParts, error) {
	parts := formImageParts{prompt: strings.TrimSpace(formFirst(values, "prompt")), fields: map[string]string{}}
	if parts.prompt == "" {
		return parts, fmt.Errorf("prompt 必填")
	}

	// 图片有三种进法，都要收：
	//   1) 文件字段（image / image[]）—— 浏览器表单、curl -F、SDK 都是这种；
	//   2) 文本字段（同名，值是 base64 或 data URI）—— 把 JSON 硬转表单的
	//      客户端常这样发；只认文件会让它拿到「image 必填」的 400；
	//   3) 文本字段里放 http(s) URL —— 要下载，所以只登记、留给第二步并行处理。
	for _, key := range []string{"image", "image[]"} {
		for _, fh := range files[key] {
			// 空 part 表示「没附文件」（浏览器未选文件、或读取失败），
			// 与 JSON 侧的 null 同义，不能因此报错。
			if fh.Size == 0 {
				continue
			}
			if fh.Size > maxImageBytes {
				return parts, fmt.Errorf("%s 超过 %d MB", fh.Filename, maxImageBytes>>20)
			}
			f, oerr := fh.Open()
			if oerr != nil {
				return parts, fmt.Errorf("读取上传图片失败: %w", oerr)
			}
			blob, rerr := io.ReadAll(io.LimitReader(f, maxImageBytes))
			_ = f.Close()
			if rerr != nil {
				return parts, fmt.Errorf("读取上传图片失败: %w", rerr)
			}
			parts.files = append(parts.files, blob)
		}
		for _, v := range values[key] {
			if strings.TrimSpace(v) == "" {
				continue
			}
			if urlencoded {
				v = strings.ReplaceAll(v, " ", "+")
			}
			parts.refs = append(parts.refs, v)
		}
	}
	if len(parts.files)+len(parts.refs) == 0 {
		return parts, fmt.Errorf("image 必填（字段名 image 或 image[]，可传文件、base64/data URI 或 http(s) URL）")
	}
	if len(parts.files)+len(parts.refs) > maxImagesPerReq {
		return parts, fmt.Errorf("一次最多 %d 张输入图", maxImagesPerReq)
	}

	// mask：文件优先，其次是文本形态（文本可能是 URL，同理留到第二步）。
	if fhs := files["mask"]; len(fhs) > 0 && fhs[0].Size > 0 {
		f, oerr := fhs[0].Open()
		if oerr != nil {
			return parts, fmt.Errorf("读取 mask 失败: %w", oerr)
		}
		defer f.Close()
		blob, rerr := io.ReadAll(io.LimitReader(f, maxImageBytes))
		if rerr != nil {
			return parts, fmt.Errorf("读取 mask 失败: %w", rerr)
		}
		parts.maskFile = blob
	} else if v := strings.TrimSpace(formFirst(values, "mask")); v != "" {
		if urlencoded {
			v = strings.ReplaceAll(v, " ", "+")
		}
		parts.maskRef = v
	}
	// mask 只对单张底图有定义，理由见 handleImageJSON。放在下载之前：
	// 这个结论不依赖任何一张图的字节。
	if (parts.maskFile != nil || parts.maskRef != "") && len(parts.files)+len(parts.refs) > 1 {
		return parts, fmt.Errorf("mask 只支持单张底图，当前给了 %d 张", len(parts.files)+len(parts.refs))
	}

	parts.title = strings.TrimSpace(formFirst(values, "title"))
	parts.model = strings.TrimSpace(formFirst(values, "model"))
	for _, key := range imageFieldWhitelist {
		if v := strings.TrimSpace(formFirst(values, key)); v != "" {
			parts.fields[key] = v
		}
	}
	return parts, nil
}

// finishFormImageRequest 解析文本引用并拼成规范请求。URL 引用在这里并行下载。
//
// 顺序保持「文件在前、文本引用在后」：历史上就是这么拼的，客户端不该因为
// 某个引用从 base64 换成 URL 而发现图片顺序变了。
func (h *Handler) finishFormImageRequest(ctx context.Context, r *http.Request, parts formImageParts) (imageRequest, error) {
	req := imageRequest{
		prompt: parts.prompt, title: parts.title, model: parts.model,
		fields: parts.fields, images: parts.files, mask: parts.maskFile,
	}
	// mask 与引用并行，理由同 handleImageJSON：没有依赖关系的下载不该排队。
	var (
		maskBlob []byte
		maskErr  error
		maskDone chan struct{}
	)
	if req.mask == nil && parts.maskRef != "" {
		maskDone = make(chan struct{})
		go func(ref string) {
			defer close(maskDone)
			maskBlob, maskErr = h.imageRefBytes(ctx, r, ref)
		}(parts.maskRef)
	}
	blobs, err := h.resolveImageRefs(ctx, r, parts.refs)
	if err != nil {
		return req, fmt.Errorf("image 解码失败: %w", err)
	}
	req.images = append(req.images, blobs...)
	if maskDone != nil {
		<-maskDone
		if maskErr != nil {
			return req, fmt.Errorf("mask 解码失败: %w", maskErr)
		}
		req.mask = maskBlob
	}
	if err := checkImageBudget(req.images); err != nil {
		return req, err
	}
	if err := checkMaskGeometry(req.images, req.mask); err != nil {
		return req, err
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

// isHTTPRef 判断引用是不是 http(s) 地址。
//
// 只看前缀，不做 URL 解析：base64 字母表里没有 `:` 和 `/`，所以这个判断对
// 「裸 base64 恰好含 http」没有误判风险，而它要在每张图上都跑一次。
func isHTTPRef(ref string) bool {
	lower := strings.ToLower(strings.TrimSpace(ref))
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// imageFetchContext 给一批 URL 垫图套上总预算。
//
// 单张有自己的连接超时（5s）与响应头超时（10s），但整批需要一个上限：
// 16 张并行、每张都可能卡满，没有总预算时请求会被拖到调用方先放弃。
func (h *Handler) imageFetchContext(r *http.Request) (context.Context, context.CancelFunc) {
	d := h.cfg.ImageURLFetchTimeout
	if d <= 0 {
		d = defaultImageURLFetchTimeout
	}
	return context.WithTimeout(r.Context(), d)
}

// imageRefBytes 解析单个引用：本地落盘直读，其余 URL 走受控下载。
func (h *Handler) imageRefBytes(ctx context.Context, r *http.Request, ref string) ([]byte, error) {
	ref = strings.TrimSpace(ref)
	if !isHTTPRef(ref) {
		return decodeImageData(ref)
	}
	if h.cfg.DisableImageURLInput {
		return nil, errors.New("URL 形态已关闭（images.url_input=false），请改传 base64 或 data URI")
	}
	// 先看是不是自己的图：默认配置下「拿上一张的 url 继续改」是最高频的用法，
	// 这种请求不该绕一圈网络回到自己的磁盘。
	if blob, handled, err := h.localImageRef(r, ref); handled {
		return blob, err
	}
	f := imageRefFetcher{
		transport:  h.cfg.imageFetchTransport,
		allowHosts: h.cfg.ImageURLFetchAllowHosts,
		maxBytes:   maxImageBytes,
	}
	return f.fetch(ctx, ref)
}

// resolveImageRefs 把一批引用解析成字节：URL 并行下载，结果**按输入顺序**返回。
//
// 保序是契约（多图生图里「把第 1 张的风格套到第 2 张上」靠的就是顺序），
// 并行是必要的：16 张串行 × 单张 10s 会把一次请求拖到 160s，而调用方
// 只是给了 16 个链接。
//
// 任何一张失败就整体失败，不做「跳过坏的、返回剩下的」：静默少一张会让
// 「合成这两张」变成「合成另一张」——请求 200、图也出来了，内容是错的。
func (h *Handler) resolveImageRefs(ctx context.Context, r *http.Request, refs []string) ([][]byte, error) {
	out := make([][]byte, len(refs))
	type job struct {
		idx int
		ref string
	}
	jobs := make([]job, 0, len(refs))
	for i, ref := range refs {
		// 纯 base64 / data URI 是 CPU 工作，就地解掉，不占并发名额。
		if !isHTTPRef(ref) {
			blob, err := decodeImageData(ref)
			if err != nil {
				return nil, err
			}
			out[i] = blob
			continue
		}
		jobs = append(jobs, job{idx: i, ref: ref})
	}
	if len(jobs) == 0 {
		return out, nil
	}

	// 第一个真错误决定返回文案；cancel 只是让还在排队的尽快退出，
	// 它们随后产生的「已取消」不会盖掉 firstErr。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, imageRefFetchConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			blob, err := h.imageRefBytes(ctx, r, j.ref)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					// 只报「第几张」，不抄 URL：错误体会进客户端日志/工单，
					// 而链接的 path/query 可能带对象存储签名。多张时才加序号，
					// 单张说「第 1 张」纯属噪音。
					where := ""
					if len(refs) > 1 {
						where = fmt.Sprintf("第 %d 张: ", j.idx+1)
					}
					firstErr = fmt.Errorf("%s%w", where, err)
					cancel()
				}
				mu.Unlock()
				return
			}
			out[j.idx] = blob
		}(j)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// checkImageBudget 校验单张与总量上限。返回的文案直接给调用方看。
func checkImageBudget(images [][]byte) error {
	var total int64
	for _, blob := range images {
		if len(blob) > maxImageBytes {
			return fmt.Errorf("单张图超过 %d MB", maxImageBytes>>20)
		}
		total += int64(len(blob))
	}
	if total > maxImageTotalBytes {
		return fmt.Errorf("输入图总量超过 %d MB（单张上限 %d MB，一次最多 %d 张）",
			maxImageTotalBytes>>20, maxImageBytes>>20, maxImagesPerReq)
	}
	return nil
}

// imageBounds 只读文件头拿宽高，不解码像素。
//
// 依赖标准库已注册的格式（PNG / JPEG / GIF）；WebP 要 x/image，这里不引入——
// 读不出就返回 ok=false，调用方跳过尺寸校验，宁可不查也不误拒。
func imageBounds(blob []byte) (w, h int, ok bool) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(blob))
	if err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}

// checkMaskGeometry 校验 mask 与底图同尺寸。
//
// image-adapter §1.6.1 要求「尺寸不同就重采样到底图」。这里选择**拒单**而不是
// 重采样，理由有两条：
//   - 重采样要引入解码 + 缩放（WebP 还得加依赖），而这条路径只有一个消费方（上游）；
//   - 上游对「mask 尺寸与底图不一致」的反应**没测过**：它可能自己缩放，也可能
//     忽略 mask 整图重画。后者是「调用方以为只改一块、实际整张被重画」，
//     属于没有中间态的错——在测出结论之前，明确失败比赌它宽容好。
//
// 读不出尺寸（WebP / 损坏）时不拦：没有把握的判断不该变成 400。
func checkMaskGeometry(images [][]byte, mask []byte) error {
	if len(mask) == 0 || len(images) == 0 {
		return nil
	}
	bw, bh, ok := imageBounds(images[0])
	if !ok {
		return nil
	}
	mw, mh, ok := imageBounds(mask)
	if !ok {
		return nil
	}
	if bw != mw || bh != mh {
		return fmt.Errorf("mask 尺寸 %dx%d 与底图 %dx%d 不一致：请先重采样到同一尺寸"+
			"（网关不做缩放，避免改变可编辑区域的边界）", mw, mh, bw, bh)
	}
	return nil
}

// decodeImageData 把 data URI 或裸 base64 解成字节。
//
// http(s) 引用不在这里处理：它需要发网络请求，走 imageRefBytes。这个函数是纯的，
// 只做编码换算。
func decodeImageData(ref string) ([]byte, error) {
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
	if isHTTPRef(ref) {
		// 调用方走错了分支：把 URL 当 base64 解只会得到一条看不懂的错。
		return nil, fmt.Errorf("内部错误：URL 引用应走下载路径")
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

// sniffImageFormat 按魔数判图片类型；ok=false 表示这些字节不像任何已知图片。
//
// 只认魔数，不认 Content-Type：CDN 回图片给 application/octet-stream 是常态，
// 反过来给一段 HTML 标 image/png 也不稀奇。下载路径靠它挡掉「下回来的不是图」。
func sniffImageFormat(b []byte) (string, bool) {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return ".png", true
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return ".jpg", true
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return ".gif", true
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return ".webp", true
	}
	return "", false
}

// sniffImageExt 给上游的 multipart 文件名用扩展名。
//
// 判不出时兜底 .png：上游按扩展名选解码分支，给它一个不认的扩展名只会换来
// 一条看不懂的错；至少 png 分支还能给出「这不是图」的结论。
func sniffImageExt(b []byte) string {
	if ext, ok := sniffImageFormat(b); ok {
		return ext
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
