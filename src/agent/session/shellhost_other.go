//go:build !windows && !linux

// shellhost_other.go — 其余平台（darwin 等）桩:生产形态 Windows 依赖
// XNCCore(令牌语义)、Linux 为 direct PTY 宿主(shellhost_linux.go)。
// 本桩保证 session 包可移植构建,创建一律 CORE_UNAVAILABLE。
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
