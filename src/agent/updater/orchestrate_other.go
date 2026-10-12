//go:build !windows && !linux

// orchestrate_other.go — 其余平台桩：在线编排仅支持 Windows（Inno 安装器）
// 与 Linux（tar.gz + systemd 版本目录切换）；本桩保证可移植构建。
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
			return errors.New("installer orchestration not supported on this platform")
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

// 命名桩：orchestrateTrigger 的平台门槛已拦截，返回 Windows 形态仅保编译。
func cacheInstallerName(version, channel string) string {
	if channel == "dev" {
		return "XNC-Installer-dev-" + version + ".exe"
	}
	return "XNC-Installer-" + version + ".exe"
}

func installerSuffix() string       { return ".exe" }
func manifestPlatformParam() string { return "" }
