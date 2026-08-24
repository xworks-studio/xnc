//go:build !windows

// core_other.go — 非 Windows 桩:desktop 采集依赖 xnc-core/xnc-desktop
// (Win32 named pipe + DXGI),别的平台没有实现;kind 不注册,
// session_loopback_test 仍可跑(fake 源不碰 pipe)。
package desktop

import "log/slog"

// NewHandlerFromEnv 恒返回 nil:非 Windows 无 core 采集,desktop kind 不注册。
func NewHandlerFromEnv(*slog.Logger) *Handler { return nil }

// NewHandler 恒返回 nil(同上;生产凭据回退是 Windows XNCCore 服务约定)。
func NewHandler(stateDir string, log *slog.Logger) *Handler { return nil }
