package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net/http/httptest"
	"testing"
	"time"
)

// selfSignedDER 造一张带 CN 的自签证书，模拟 C2PA 清单里的 leaf 证书。
func selfSignedDER(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书: %v", err)
	}
	return der
}

// 生图落盘这条路上必须把「谁签的」记下来：上游同一个模型名会在 Azure 与
// OpenAI 之间漂移，少了这一列就只能事后扫图猜分布。
func TestAttachImageURLsRecordsC2PASigner(t *testing.T) {
	h := &Handler{cfg: Config{ImageDir: t.TempDir(), ImageMaxTotalMB: 64}}

	blob := append([]byte("jumb\x00jumdc2pajumdcbor"), selfSignedDER(t, "OpenAI Media Service")...)
	blob = append(blob, make([]byte, 64)...)
	b64 := base64.StdEncoding.EncodeToString(blob)
	body := []byte(fmt.Sprintf(`{"output_format":"png","data":[{"b64_json":%q}]}`, b64))

	tr := &requestTrace{}
	req := httptest.NewRequest("POST", "/v1/images/generations", nil)
	if _, attached, err := h.attachImageURLs(req, body, "url", tr); err != nil || attached != 1 {
		t.Fatalf("落盘失败: err=%v attached=%d", err, attached)
	}
	if tr.c2pa != "OpenAI Media Service" {
		t.Fatalf("日志里的签名者 = %q，想要 OpenAI Media Service", tr.c2pa)
	}

	// 这一列必须真的进到对外的事件结构里，否则面板永远看不到
	if ev := tr.event(200); ev.C2PASigner != "OpenAI Media Service" {
		t.Fatalf("event 丢了签名者: %q", ev.C2PASigner)
	}
}

// 没有清单时不能伪造一个名字出来：空串本身就是"这条路径不产出溯源凭证"的信息。
func TestAttachImageURLsLeavesSignerEmptyWithoutManifest(t *testing.T) {
	h := &Handler{cfg: Config{ImageDir: t.TempDir(), ImageMaxTotalMB: 64}}
	b64 := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nplain"))
	body := []byte(fmt.Sprintf(`{"output_format":"png","data":[{"b64_json":%q}]}`, b64))

	tr := &requestTrace{}
	req := httptest.NewRequest("POST", "/v1/images/generations", nil)
	if _, _, err := h.attachImageURLs(req, body, "url", tr); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	if tr.c2pa != "" {
		t.Fatalf("无清单时签名者应为空，实际 %q", tr.c2pa)
	}
}
