// 一次性校验工具：对给定图片打印 C2PA 签名者，用来和离线的 python 统计对照。
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rsfree/xhx2api/internal/upstream"
)

func main() {
	for _, p := range os.Args[1:] {
		b, err := os.ReadFile(p)
		if err != nil {
			fmt.Printf("%-46s ERR %v\n", filepath.Base(p), err)
			continue
		}
		fmt.Printf("%-46s -> %q\n", filepath.Base(p), upstream.C2PASigner(b))
	}
}
