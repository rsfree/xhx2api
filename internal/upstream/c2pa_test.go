package upstream

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

// makeSelfSigned 造一张带 CN 的自签证书，用来模拟 C2PA 的 x5chain。
func makeSelfSigned(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"Test Org"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书: %v", err)
	}
	return der
}

func TestC2PASignerExtractsCommonName(t *testing.T) {
	cert := makeSelfSigned(t, "OpenAI Media Service")
	// 真实清单里证书前面还有 JUMBF 头和签名盒，这里只保留"标记 + 噪声 + 证书"
	blob := append([]byte("jumb\x00jumdc2pajumdcbor"), cert...)
	blob = append(blob, []byte("c2pa.hash.data")...)

	if got := C2PASigner(blob); got != "OpenAI Media Service" {
		t.Fatalf("签名者 CN = %q，想要 %q", got, "OpenAI Media Service")
	}
}

func TestC2PASignerWithoutManifest(t *testing.T) {
	plain := []byte("\x89PNG\r\n\x1a\n" + string(make([]byte, 512)))
	if got := C2PASigner(plain); got != "" {
		t.Fatalf("无清单时返回 %q，想要空串", got)
	}
}

func TestC2PASignerManifestWithoutCert(t *testing.T) {
	// 有标记但没有任何证书：不能伪造成"无凭证"，也不能猜一个名字出来
	blob := []byte("jumdc2pa" + string(make([]byte, 256)))
	if got := C2PASigner(blob); got != "unknown" {
		t.Fatalf("有清单无证书时返回 %q，想要 unknown", got)
	}
}

// 老图（2026-11-23 本机生成）的清单里微软证书后面还跟着中间 CA 与根；
// 必须取 leaf（第一个），不能取到根。这里用两张证书串起来逼近真实结构。
func TestC2PASignerPrefersLeaf(t *testing.T) {
	leaf := makeSelfSigned(t, "Azure OpenAI ImageGen")
	root := makeSelfSigned(t, "Microsoft C2PA Root CA")
	blob := append([]byte("jumdc2pa"), leaf...)
	blob = append(blob, root...)

	if got := C2PASigner(blob); got != "Azure OpenAI ImageGen" {
		t.Fatalf("取到了 %q，应该是 leaf 的 CN", got)
	}
}
