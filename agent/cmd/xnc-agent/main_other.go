//go:build !windows

package main

import (
	"errors"

	"xnc/agent"
)

// 非 Windows：run 仍可前台运行（agent 核心跨平台，供调试）；
// 服务宿主（install/uninstall）仅 Windows。
func runAgent(server, token, stateDir string) error {
	a := &agent.Agent{ServerURL: server, Token: token, StateDir: stateDir}
	return a.Run(cmdContext())
}

func installService(_, _, _ string) error {
	return errors.New("service host is only supported on Windows")
}

func uninstallService() error {
	return errors.New("service host is only supported on Windows")
}

// defaultStateDir：无 ProgramData 约定，落在工作目录下。
func defaultStateDir() string { return "./xnc-agent-state" }
