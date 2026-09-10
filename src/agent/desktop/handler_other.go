//go:build !windows

// handler_other.go — 非 Windows：desktop kind 不注册（NewHandler 恒 nil，
// 引擎走既有 refused 路径）。
package desktop

import "log/slog"

func NewHandler(_, _ string, _ *slog.Logger) *struct{} { return nil }
