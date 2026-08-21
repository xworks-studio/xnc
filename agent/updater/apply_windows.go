//go:build windows

// apply_windows.go — Windows 自更新之舞（设计文档核心路径）。
//
// apply：杀运行中的 screen-helper（消灭孤儿）→ 删除已提取的 DLL 缓存
// （消灭陈旧 DLL——本 session 三次事故的教训）→ spawn 自身子进程
// `xnc-agent.exe --apply-update <stageDir> <parentPID>`（脱离服务生命周
// 期）→ 主进程 os.Exit(0)。
//
// 子进程（RunApply）：等父进程退出 → 备份换文件 → StartService →
// 等待 connected.ok 标记（新 agent 首次控制连接成功后写）→ 成功删 .old
// 退出；超时回滚（.old 复位 + 再启动）。
package updater

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

// connectedMarker 新 agent 首次控制连接就绪后写的标记（回滚判据）。
func ConnectedMarkerPath(stateDir string) string {
	return filepath.Join(stateDir, "update-connected.ok")
}

// apply 平台入口：Windows 杀 helper/删 DLL/spawn 子进程。
func (u *Updater) apply(stageDir string) error {
	agentPath, err := os.Executable()
	if err != nil {
		return err
	}
	agentDir := filepath.Dir(agentPath)

	// 1. 杀运行中的 helper（taskkill by image name——SYSTEM 有权）。
	if out, err := exec.Command("taskkill", "/F", "/IM", helperExe).CombinedOutput(); err == nil {
		u.Log.Info("update: killed running helpers", "out", string(out))
	}
	time.Sleep(500 * time.Millisecond) // 句柄释放窗口

	// 2. 删除已提取 DLL 缓存（新 helper 首启重新提取内嵌副本）。
	_ = os.Remove(filepath.Join(agentDir, "xnc-dda.dll"))

	// 3. spawn --apply-update 子进程（DETACHED：不随服务退出被杀）。
	cmd := exec.Command(agentPath, "--apply-update", stageDir, fmt.Sprint(os.Getpid()))
	cmd.Dir = agentDir
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn apply-update child: %w", err)
	}
	// 4. 主进程退出——SCM 视为服务停止，由子进程 StartService 拉起新版。
	u.Log.Info("update: apply-update child spawned, agent exiting for restart", "pid", cmd.Process.Pid)
	go func() { os.Exit(0) }()
	return nil
}

// RunApply --apply-update 子模式主体：换文件、重启服务、验证或回滚。
// parentPID 为调用方（服务进程）PID。
func RunApply(stageDir, parentPID string, log func(format string, args ...any)) error {
	agentPath, err := os.Executable()
	if err != nil {
		return err
	}
	agentDir := filepath.Dir(agentPath)
	stateDir := filepath.Dir(stageDir) // staging 在 stateDir 下

	log("apply-update: waiting for parent %s to exit", parentPID)
	waitProcessExit(parentPID, 30*time.Second)

	// 换文件（.old 备份只保留一代）。
	oldAgent := filepath.Join(agentDir, agentExe)
	oldHelper := filepath.Join(agentDir, helperExe)
	if err := removeOld(oldAgent); err != nil {
		return fmt.Errorf("backup agent: %w", err)
	}
	if err := removeOld(oldHelper); err != nil {
		return fmt.Errorf("backup helper: %w", err)
	}
	newAgent := filepath.Join(stageDir, agentExe)
	newHelper := filepath.Join(stageDir, helperExe)
	if err := os.Rename(newAgent, oldAgent); err != nil {
		return rollback(oldAgent, oldHelper, err)
	}
	if err := os.Rename(newHelper, oldHelper); err != nil {
		return rollback(oldAgent, oldHelper, err)
	}

	// 起服务。
	if err := startService(); err != nil {
		return rollback(oldAgent, oldHelper, err)
	}

	// 验证：新 agent 控制连接成功后写标记；超时回滚。
	marker := ConnectedMarkerPath(stateDir)
	_ = os.Remove(marker)
	log("apply-update: service started, waiting for connect marker")
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			// 成功：清扫备份与 staging。
			_ = os.Remove(oldAgent + ".old")
			_ = os.Remove(oldHelper + ".old")
			_ = os.RemoveAll(stageDir)
			log("apply-update: verified, cleaned up")
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	log("apply-update: connect marker timeout, rolling back")
	return rollback(oldAgent, oldHelper, fmt.Errorf("new agent did not connect in time"))
}

// rollback 恢复 .old 并重启服务（尽力而为；失败留给 SCM/人工）。
func rollback(agentPath, helperPath string, cause error) error {
	_ = os.Remove(agentPath)
	_ = os.Remove(helperPath)
	_ = os.Rename(agentPath+".old", agentPath)
	_ = os.Rename(helperPath+".old", helperPath)
	_ = startService()
	return cause
}

// waitProcessExit 轮询等待进程消失（OpenProcess 探测）。
func waitProcessExit(pid string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(atoi(pid)))
		if err != nil {
			return // 已退出
		}
		var code uint32
		_ = windows.GetExitCodeProcess(windows.Handle(h), &code)
		windows.CloseHandle(windows.Handle(h))
		if code != 259 { // STILL_ACTIVE
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// startService 经 SCM 启动 XNCAgent（SYSTEM 令牌有权）。
func startService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService("XNCAgent")
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Start()
}
