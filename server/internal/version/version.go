// Package version 承载 server 构建版本（/api/health 上报）。
package version

// Version 是 server 构建版本，构建时经 ldflags 注入（与 agent bundle 发布
// 版本保持一致，版本单一来源）：
//
//	go build -ldflags "-X xnc/server/internal/version.Version=0.4.6" ./cmd/xnc-server
//
// 未注入（本地开发构建 / compose 未传 XNC_VERSION）回落 0.0.0-dev。
var Version = "0.0.0-dev"
