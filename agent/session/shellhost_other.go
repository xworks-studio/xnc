//go:build !windows

// shellhost_other.go — 非 Windows 桩:生产 agent 仅部署 Windows;
// 此桩保证 session 包可移植构建,创建一律 CORE_UNAVAILABLE。
package session

import "log/slog"

// DefaultShellHost 非 Windows 恒 nil(创建即 CORE_UNAVAILABLE)。
func DefaultShellHost(_ *slog.Logger) ShellHost { return nil }

type unavailableHost struct{}

func (unavailableHost) CreateShell(ShellSpec) (ShellProc, error) {
	return nil, &ShellHostError{Code: CodeCoreUnavailable}
}

// defaultSnapshotProvider 非 Windows 恒 CORE_UNAVAILABLE。
func defaultSnapshotProvider(*slog.Logger) SnapshotProvider {
	return unavailableSnapshot{}
}

type unavailableSnapshot struct{}

func (unavailableSnapshot) Snapshot(uint32) ([]byte, error) {
	return nil, &ShellHostError{Code: CodeCoreUnavailable}
}
