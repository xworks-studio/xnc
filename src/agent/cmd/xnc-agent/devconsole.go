// run-dev-console：一次性开发拓扑代理宿主（M1-Slice2 dev loop）。
// 与生产 XNCAgent 服务完全隔离：绝不接触 SCM、状态目录每次运行在
// %TEMP%\xnc-dev-agent-<pid> 全新创建并在退出时整体删除、server/token
// 全部来自命令行。复用既有 enroll/connect/identity 逻辑（DPAPI
// LOCAL_MACHINE 保护私钥，temp 目录同样安全）。
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"xnc/agent"
)

// devStateDirBase 是 temp 下一次性状态目录的前缀（%TEMP%\xnc-dev-agent-<pid>）。
const devStateDirBase = "xnc-dev-agent-"

// freshDevStateDir 创建本次运行专属的一次性状态目录：
// 同名残留（pid 复用的上次运行）先整体删除，保证「全新」。
func freshDevStateDir() (string, error) {
	dir := DevStateDirForPid(os.Getpid())
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("dev state dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("dev state dir: %w", err)
	}
	return dir, nil
}

// DevStateDirForPid 返回给定 pid 的状态目录路径（不创建、不删除）。
func DevStateDirForPid(pid int) string {
	return filepath.Join(os.TempDir(), devStateDirBase+strconv.Itoa(pid))
}

// devNodeName：--name 为空时默认 <hostname>-DEV，节点列表里一眼可辨。
func devNodeName(name string) string {
	if name != "" {
		return name
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		h = "dev-node"
	}
	return h + "-DEV"
}

// devMachineID 为本次运行生成唯一 machineId。必须随机化：dev server 的
// enroll 幂等键是 (cluster, machineId)——复用真实 machineId + 每次新身份
// 密钥会被 409 node_already_enrolled 拒绝；每次唯一则各注册为新节点
// （dev server 节点表是抛物线积累，docker compose down -v 即重置）。
func devMachineID(name string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// CSPRNG 失败极罕见；退化为 pid 保证仍然唯一
		b = []byte(fmt.Sprintf("%08x", os.Getpid()))
	}
	return "dev-" + hex.EncodeToString(b) + "-" + name
}

// runDevConsole 前台运行 dev agent：Ctrl+C（ctx 取消）视为正常退出，
// 状态目录随返回整体删除。desktopCorePipe/desktopCoreSecretHex 是 dev
// 桌面采集 seam（M1-Slice2 T6）：齐全时映射到 XNC_DESKTOP_CORE_PIPE /
// XNC_DESKTOP_CORE_SECRET_HEX（agent/desktop.NewHandlerFromEnv 的注册开关；
// core 以 --console --smoke-secret 诊断模式跑时 pipe 名/secret 即其 argv）。
// secret 只进环境变量，绝不入日志。
func runDevConsole(server, token, name, logFile, desktopCorePipe, desktopCoreSecretHex string) error {
	if desktopCorePipe != "" && desktopCoreSecretHex != "" {
		os.Setenv("XNC_DESKTOP_CORE_PIPE", desktopCorePipe)
		os.Setenv("XNC_DESKTOP_CORE_SECRET_HEX", desktopCoreSecretHex)
		slog.Info("dev agent: desktop kind enabled (core pipe configured)") // 无 secret
	}
	if logFile != "" {
		if f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			defer f.Close()
			slog.SetDefault(slog.New(slog.NewTextHandler(
				io.MultiWriter(os.Stderr, f), nil)))
		} else {
			slog.Warn("dev agent: cannot open log file", "path", logFile, "err", err)
		}
	}

	stateDir, err := freshDevStateDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(stateDir) // 退出即清：正常/错误/Ctrl+C 一致

	nodeName := devNodeName(name)
	slog.Info("dev agent starting",
		"server", server, "node", nodeName,
		"stateDir", stateDir, "logFile", logFile) // token 永不入日志

	a := &agent.Agent{
		ServerURL: server, Token: token, StateDir: stateDir,
		HostnameOverride:  nodeName,
		MachineIDOverride: devMachineID(nodeName),
	}
	err = a.Run(cmdContext())
	if errors.Is(err, context.Canceled) {
		slog.Info("dev agent stopped (interrupt); state removed", "stateDir", stateDir)
		return nil
	}
	return err
}
