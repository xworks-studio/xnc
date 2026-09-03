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
	// 首启迁移（仅当使用新默认目录时；显式 --state-dir 不迁移）：
	// 必须先于服务日志打开——日志落在新目录，且日志句柄会占住目录使
	// 搬移失败。
	if stateDir == defaultStateDir() {
		stateDir = migrateLegacyStateDir(stateDir)
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

// defaultStateDir：服务以 SYSTEM 运行，状态放在 %ProgramData%\XNC
//（设计 §3.3 机器级状态统一目录；旧 XNCAgent 由 migrateLegacyStateDir
// 首启迁移）。
func defaultStateDir() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = "."
	}
	return filepath.Join(pd, "XNC")
}

// legacyStateDirName 旧状态目录名（首启迁移源，设计 §14）。
const legacyStateDirName = "XNCAgent"

// migrateLegacyStateDir 首启状态目录迁移（设计 §14）：StateDir 统一为
// %ProgramData%\XNC。规则：
//   - 旧目录存在、新目录不存在 → 整体 rename 搬移（identity.json 等
//     全部随迁——搬移而非复制，绝不产生两份状态）；
//   - 两者并存 → 以新目录为准，记警告（不合并、不删除）；
//   - 搬移失败（目录被占用等）→ 保留旧目录（identity 必须存活），本次
//     运行退回旧目录，下次启动重试；
//   - 都不存在 → 创建新目录（空转态也要写服务日志）。0700 与
//     identity.Save 同一权限约定（Go 映射为 SYSTEM+Administrators+属主）。
//
// 返回本次运行实际使用的状态目录。
func migrateLegacyStateDir(newDir string) string {
	oldDir := filepath.Join(filepath.Dir(newDir), legacyStateDirName)
	_, oldErr := os.Stat(oldDir)
	_, newErr := os.Stat(newDir)
	switch {
	case oldErr == nil && newErr == nil:
		slog.Warn("state dir: legacy and new both exist; using new",
			"legacy", oldDir, "new", newDir)
	case oldErr == nil:
		if err := os.Rename(oldDir, newDir); err != nil {
			slog.Warn("state dir migration failed; using legacy dir this run",
				"legacy", oldDir, "new", newDir, "err", err)
			return oldDir
		}
		slog.Info("state dir migrated", "from", oldDir, "to", newDir)
	default:
		if err := os.MkdirAll(newDir, 0o700); err != nil {
			slog.Warn("state dir create failed", "dir", newDir, "err", err)
		}
	}
	return newDir
}
