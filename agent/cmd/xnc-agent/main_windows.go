//go:build windows

package main

import (
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

// installService 以当前可执行文件安装 XNCAgent 服务。
// 服务参数必须逐词传递：CreateService 对每个元素单独做 EscapeArg，
// 任何含空格的整串（以及含空格的 --state-dir 路径）才能被正确引用。
func installService(server, token, stateDir string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return svcapp.Install(exe, buildServiceArgs(server, token, stateDir)...)
}

// buildServiceArgs 构造服务命令行参数，每个旗标一个独立 argv 元素。
func buildServiceArgs(server, token, stateDir string) []string {
	args := []string{"run", "--server=" + server, "--state-dir=" + stateDir}
	if token != "" {
		args = append(args, "--token="+token)
	}
	return args
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
