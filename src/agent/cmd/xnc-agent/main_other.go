//go:build !windows

package main

import (
	"errors"
	"runtime"

	"xnc/agent"
)

// 非 Windows：run 仍可前台运行（agent 核心跨平台，供调试）；
// 服务宿主（install/uninstall）仅 Windows。
func runAgent(server, token, stateDir, _ /*serviceName*/, _ /*desktopCorePipe*/, _ /*desktopCoreSecretHex*/ string) error {
	a := &agent.Agent{ServerURL: server, Token: token, StateDir: stateDir}
	return a.Run(cmdContext())
}

func installService(_, _, _, _, _, _ string) error {
	return errors.New("service host is only supported on Windows")
}

func uninstallService(_ string) error {
	return errors.New("service host is only supported on Windows")
}

// defaultStateDir：linux 用 /var/lib/xnc（systemd 服务布局，install.sh
// 创建；binding/identity 落盘时以 0700/0600 收紧）；其余非 Windows 平台
// （开发调试）落在工作目录下。
func defaultStateDir() string {
	if runtime.GOOS == "linux" {
		return "/var/lib/xnc"
	}
	return "./xnc-agent-state"
}
