//go:build windows

package main

import (
	"log/slog"
	"os"
	"path/filepath"

	"xnc/agent"
	"xnc/agent/svcapp"
)

// runAgent：由 SCM 启动时进入服务模式；否则前台运行（调试）。
// serviceName 是本进程在 SCM 中的注册名（install 时经 --service-name
// 写入服务 argv；空 = 生产默认 XNCAgent）。desktopCorePipe/SecretHex 是
// dev 服务拓扑 seam（等价 XNC_DESKTOP_CORE_PIPE/…_SECRET_HEX；SCM 进程
// 无法继承交互环境变量，dev 安装把凭据放进服务 argv —— 与 core
// --smoke-secret 同一 dev-only plaintext binPath 先例）。
func runAgent(server, token, stateDir, serviceName, desktopCorePipe, desktopCoreSecretHex string) error {
	if desktopCorePipe != "" && desktopCoreSecretHex != "" {
		os.Setenv("XNC_DESKTOP_CORE_PIPE", desktopCorePipe)
		os.Setenv("XNC_DESKTOP_CORE_SECRET_HEX", desktopCoreSecretHex)
	}
	a := &agent.Agent{ServerURL: server, Token: token, StateDir: stateDir}
	if svcapp.IsService() {
		// 服务上下文无有效 stdout/stderr——日志落盘 state 目录（否则
		// slog 默认写入无效句柄，诊断全部丢失）。
		if f, ferr := os.OpenFile(filepath.Join(stateDir, "agent-service.log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
			slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))
		}
		return svcapp.Run(a, serviceName)
	}
	return a.Run(cmdContext())
}

// installService 以当前可执行文件安装服务（名/显示名由 serviceName 决定；
// 空 = XNCAgent 生产默认）。
// 服务参数必须逐词传递：CreateService 对每个元素单独做 EscapeArg，
// 任何含空格的整串（以及含空格的 --state-dir 路径）才能被正确引用。
func installService(server, token, stateDir, serviceName, desktopCorePipe, desktopCoreSecretHex string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return svcapp.Install(exe, svcapp.ServiceConfig{Name: serviceName},
		buildServiceArgs(server, token, stateDir, serviceName, desktopCorePipe, desktopCoreSecretHex)...)
}

// buildServiceArgs 构造服务命令行参数，每个旗标一个独立 argv 元素。
// 非 XNCAgent 服务名/显式 state 目录都必须写进 argv：服务模式读回
// 自己的名字与隔离状态目录（dev: XNCAgentDev + ProgramData\XNCAgentDev）。
func buildServiceArgs(server, token, stateDir, serviceName, desktopCorePipe, desktopCoreSecretHex string) []string {
	args := []string{"run", "--server=" + server, "--state-dir=" + stateDir}
	if serviceName != "" && serviceName != svcapp.DefaultServiceName {
		args = append(args, "--service-name="+serviceName)
	}
	if desktopCorePipe != "" {
		args = append(args, "--desktop-core-pipe="+desktopCorePipe)
	}
	if desktopCoreSecretHex != "" {
		args = append(args, "--desktop-core-secret-hex="+desktopCoreSecretHex)
	}
	if token != "" {
		args = append(args, "--token="+token)
	}
	return args
}

func uninstallService(serviceName string) error { return svcapp.Uninstall(serviceName) }

// defaultStateDir：服务以 SYSTEM 运行，状态放在 %ProgramData%\XNCAgent。
func defaultStateDir() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = "."
	}
	return filepath.Join(pd, "XNCAgent")
}
