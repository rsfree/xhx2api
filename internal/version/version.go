// Package version 保存网关版本号。
//
// 版本号由构建期 -ldflags 注入（见 .github/workflows 与 Dockerfile），本地
// `go build` 不注入时恒为 "dev"。面板顶部展示它，便于确认线上跑的是哪个包。
package version

// Version 是当前版本；正式构建由 -X 注入，例如：
//
//	go build -ldflags "-X github.com/rsfree/xhx2api/internal/version.Version=v2026.10.26.1800"
var Version = "v0.1.9"

// String 返回展示用版本号（"dev" 时面板显示「开发版」由前端处理）。
func String() string { return Version }
