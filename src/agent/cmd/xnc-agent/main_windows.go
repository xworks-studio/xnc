//go:build windows

package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"xnc/agent"
	"xnc/agent/session"
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
	// 搬移失败。SCM 上下文里默认 logger 写向无效句柄，迁移消息由
	// migrateLegacyStateDir 缓冲、待文件 logger 装好后回放（见下）。
	var migrationLogs []migrationLog
	if stateDir == defaultStateDir() {
		stateDir, migrationLogs = migrateLegacyStateDir(stateDir)
	}
	// 会话脚本残留清扫(规范 §3.3):tmp\ 与根位置的 24h 前 xnc-*.ps1。
	session.CleanupStaleSessionScripts(stateDir)
	a := &agent.Agent{ServerURL: server, Token: token, StateDir: stateDir}
	if svcapp.IsService() {
		// 服务上下文无有效 stdout/stderr——日志落盘（否则 slog 默认写入
		// 无效句柄，诊断全部丢失）。2026-09-15 规范 §3:运行日志归
		// stateDir\logs\（目录按需创建；core/host/shell 同目录，xnc.iss
		// [Dirs] 安装期双保险）。历史根位置文件一次性 rename 迁移;写侧
		// 带大小轮转（rotatingWriter,规范 §3.2:8MB×3 份封顶）。
		logsDir := filepath.Join(stateDir, "logs")
		if err := os.MkdirAll(logsDir, 0o700); err != nil {
			// 目录建不起来（极端 ACL）：回落根位置——日志本身绝不能丢。
			slog.Warn("logs dir unavailable, falling back to state root", "err", err)
		} else {
			legacy := filepath.Join(stateDir, "agent-service.log")
			migrated := filepath.Join(logsDir, "agent-service.log")
			if _, serr := os.Stat(legacy); serr == nil {
				if _, serr2 := os.Stat(migrated); os.IsNotExist(serr2) {
					if rerr := os.Rename(legacy, migrated); rerr != nil {
						slog.Warn("agent-service.log migration failed (appending in place)", "err", rerr)
					}
				}
			}
		}
		if w, werr := newRotatingWriter(agentServiceLogPath(stateDir)); werr == nil {
			slog.SetDefault(slog.New(slog.NewTextHandler(w, nil)))
		}
		// logger 已装好：回放迁移期间缓冲的诊断（此前默认句柄无效）。
		replayMigrationLogs(migrationLogs)
		return svcapp.Run(a, serviceName)
	}
	replayMigrationLogs(migrationLogs) // 前台模式：默认 stderr 句柄有效
	return a.Run(cmdContext())
}

// agentServiceLogPath 服务日志路径：logs\ 目录可用即落其中（规范 §3），
// 否则回落历史根位置（与旧版/AGENTS.md §7 兼容）。
func agentServiceLogPath(stateDir string) string {
	logsDir := filepath.Join(stateDir, "logs")
	if st, err := os.Stat(logsDir); err == nil && st.IsDir() {
		return filepath.Join(logsDir, "agent-service.log")
	}
	return filepath.Join(stateDir, "agent-service.log")
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

// migrationLog 一条延迟回放的迁移日志。迁移必须先于服务日志打开执行
// （日志文件在新目录、句柄会占住目录使搬移失败），而 SCM 上下文里默认
// logger 写向无效句柄——消息缓冲在此，待文件 logger 装好后回放，否则
// 生产环境丢失「失败保留旧目录」等迁移诊断。
type migrationLog struct {
	level slog.Level
	msg   string
	args  []any
}

// replayMigrationLogs 回放缓冲的迁移日志（须在最终 logger 安装后调用）。
func replayMigrationLogs(logs []migrationLog) {
	for _, e := range logs {
		slog.Log(context.Background(), e.level, e.msg, e.args...)
	}
}

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
// 返回本次运行实际使用的状态目录 + 缓冲待回放的日志（不直接写 slog：
// 调用时最终 logger 可能尚未安装，见 migrationLog）。
func migrateLegacyStateDir(newDir string) (string, []migrationLog) {
	oldDir := filepath.Join(filepath.Dir(newDir), legacyStateDirName)
	_, oldErr := os.Stat(oldDir)
	_, newErr := os.Stat(newDir)
	var logs []migrationLog
	switch {
	case oldErr == nil && newErr == nil:
		logs = append(logs, migrationLog{slog.LevelWarn,
			"state dir: legacy and new both exist; using new",
			[]any{"legacy", oldDir, "new", newDir}})
	case oldErr == nil:
		if err := os.Rename(oldDir, newDir); err != nil {
			logs = append(logs, migrationLog{slog.LevelWarn,
				"state dir migration failed; using legacy dir this run",
				[]any{"legacy", oldDir, "new", newDir, "err", err}})
			return oldDir, logs
		}
		logs = append(logs, migrationLog{slog.LevelInfo,
			"state dir migrated", []any{"from", oldDir, "to", newDir}})
	default:
		if err := os.MkdirAll(newDir, 0o700); err != nil {
			logs = append(logs, migrationLog{slog.LevelWarn,
				"state dir create failed", []any{"dir", newDir, "err", err}})
		}
	}
	return newDir, logs
}
