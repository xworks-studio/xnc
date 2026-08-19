//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"xnc/agent"
	"xnc/agent/svcapp"
)

// runAgent：由 SCM 启动时进入服务模式；否则前台运行（调试）。
func runAgent(server, token, stateDir string) error {
	a := &agent.Agent{ServerURL: server, Token: token, StateDir: stateDir}
	if svcapp.IsService() {
		return svcapp.Run(a)
	}
	return a.Run(cmdContext())
}

// installService 以当前可执行文件安装 XNCAgent 服务，服务参数携带 run 及其旗标。
func installService(server, token, stateDir string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := fmt.Sprintf("run --server=%s --state-dir=%s", server, stateDir)
	if token != "" {
		args += " --token=" + token
	}
	return svcapp.Install(exe, args)
}

func uninstallService() error { return svcapp.Uninstall() }

// defaultStateDir：服务以 SYSTEM 运行，状态放在 %ProgramData%\XNCAgent。
func defaultStateDir() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = "."
	}
	return filepath.Join(pd, "XNCAgent")
}
