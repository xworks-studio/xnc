//go:build !windows

// orchestrate_other.go — 非 Windows 桩：安装器编排仅支持 Windows 服务
// 形态（与 bundle 期一致）。
package updater

import (
	"errors"
	"path/filepath"
	"strings"
	"time"
)

// platform 装配桩（全部返回错误；核心逻辑平台无关，仅执行层缺席）。
func (u *Updater) platform() {
	if u.execInstaller == nil {
		u.execInstaller = func(string) error {
			return errors.New("installer orchestration not supported on this platform (Windows only)")
		}
	}
	if u.serviceHealthy == nil {
		u.serviceHealthy = func() error { return nil }
	}
	if u.registerWatchdogTask == nil {
		u.registerWatchdogTask = func(time.Time) error {
			return errors.New("watchdog not supported on this platform")
		}
	}
	if u.deleteWatchdogTask == nil {
		u.deleteWatchdogTask = func() {}
	}
}

// samePath 桩。
func samePath(a, b string) (bool, error) {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b)), nil
}
