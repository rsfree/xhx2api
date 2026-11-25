package upstream

import (
	"bytes"
	"crypto/x509"
	"encoding/binary"
	"strings"
)

// c2paMarker 是 C2PA/JUMBF 超级盒的标识。
//
// 用它在字节流里定位清单，而不是按 PNG(caBX)/JPEG(APP11)/WebP(chunk) 逐个解析
// 容器：三种内核对同一份 JUMBF 只是"套壳"不同，标记本身写法一致。代价是遇到
// 被分片的大清单时只能看到第一片，但签名证书在清单靠前的位置，够用。
var c2paMarker = []byte("jumdc2pa")

// C2PASigner 返回图片里 C2PA 清单的签名者 Common Name。
//
// 只回答「谁签的」，不回答「签名是否有效」。验签要拉 trust list、验证书链、
// 校时间戳，一旦放进请求路径，网关就会依赖一个外部信任源——离线环境直接失效，
// 而这段代码的价值只是"观测这次请求落到哪个后端"，不值得为此绑死。
// 签名有效性交给离线 c2pa-rs / c2patool（见 AGENTS.md 的实测记录）。
//
// 返回值约定：
//
//	""        —— 没有清单：上游这条路径不写溯源凭证
//	"unknown" —— 有清单但取不到证书（结构不认识）
//	其它      —— 证书 Subject CN，如 "OpenAI Media Service"
func C2PASigner(blob []byte) string {
	idx := bytes.Index(blob, c2paMarker)
	if idx < 0 {
		return ""
	}
	if cn := firstCertCN(blob[idx:]); cn != "" {
		return cn
	}
	return "unknown"
}

// firstCertCN 扫 DER SEQUENCE，返回第一个能解析成 X.509 且带 CN 的证书。
//
// 为什么不解析 COSE_Sign1 的 x5chain：那是 CBOR，得引一个 CBOR 库，而本项目
// 目前没有第三方解析依赖；证书本身是自描述的 DER，ParseCertificate 会做完整
// 的长度/结构校验，扫到噪声数据的代价只是一次快速失败。
//
// 规范要求 x5chain 里 leaf 在前，所以第一个解析成功的就是签名者证书。
func firstCertCN(b []byte) string {
	for i := 0; i+2 < len(b); i++ {
		if b[i] != 0x30 {
			continue
		}
		var total int
		switch b[i+1] {
		case 0x81:
			if i+3 > len(b) {
				continue
			}
			total = 3 + int(b[i+2])
		case 0x82:
			if i+4 > len(b) {
				continue
			}
			total = 4 + int(binary.BigEndian.Uint16(b[i+2:i+4]))
		case 0x83:
			if i+5 > len(b) {
				continue
			}
			total = 5 + (int(b[i+2])<<16 | int(b[i+3])<<8 | int(b[i+4]))
		default:
			continue
		}
		if total <= 0 || i+total > len(b) {
			continue
		}
		cert, err := x509.ParseCertificate(b[i : i+total])
		if err != nil {
			continue
		}
		if cn := strings.TrimSpace(cert.Subject.CommonName); cn != "" {
			return cn
		}
	}
	return ""
}
