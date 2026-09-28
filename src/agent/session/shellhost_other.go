//go:build !windows

// shellhost_other.go — 非 Windows 桩:生产 agent 的 Windows 形态依赖
// XNCCore(令牌语义);Linux 的 direct PTY 宿主在 shellhost_linux.go
// (GOOS=linux,落地时本文件标签收窄为 !windows && !linux)。本桩保证
// session 包可移植构建,创建一律 CORE_UNAVAILABLE。
package session

import "log/slog"

// DefaultShellHost 非 Windows 恒 nil(创建即 CORE_UNAVAILABLE)。
func DefaultShellHost(_ *slog.Logger) ShellHost { return nil }

// ShellHostFromStateDir 非 Windows 恒 nil(agent.go 生产入口的桩形态;
// Linux 版见 shellhost_linux.go)。
func ShellHostFromStateDir(_ string, _ *slog.Logger) ShellHost { return nil }

type unavailableHost struct{}

func (unavailableHost) CreateShell(ShellSpec) (ShellProc, error) {
	return nil, &ShellHostError{Code: CodeCoreUnavailable}
}

// defaultSnapshotProvider 非 Windows 恒 CORE_UNAVAILABLE(签名与
// shellhost_windows.go 对齐:stateDir 参数)。
func defaultSnapshotProvider(_ string, _ *slog.Logger) SnapshotProvider {
	return unavailableSnapshot{}
}

type unavailableSnapshot struct{}

func (unavailableSnapshot) Snapshot(uint32) ([]byte, error) {
	return nil, &ShellHostError{Code: CodeCoreUnavailable}
}
