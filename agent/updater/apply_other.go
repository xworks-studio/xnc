//go:build !windows

// apply_other.go — 非 Windows 桩：v1 自更新仅支持 Windows 服务形态。
package updater

import "fmt"

func (u *Updater) apply(stageDir string) error {
	return fmt.Errorf("self-update not supported on this platform (Windows service only)")
}

// RunApply 非 Windows 桩。
func RunApply(stageDir, parentPID string, log func(format string, args ...any)) error {
	return fmt.Errorf("apply-update not supported on this platform")
}

// ConnectedMarkerPath 桩路径（不可达）。
func ConnectedMarkerPath(stateDir string) string { return stateDir + "/update-connected.ok" }
