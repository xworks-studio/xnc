//go:build windows

// apply_windows.go — Windows 自更新之舞（设计文档核心路径）。
//
// apply：spawn 自身子进程 `xnc-agent.exe --apply-update <stageDir>
// <parentPID>`（脱离服务生命周期）→ 主进程 os.Exit(0)。
//
// 子进程（RunApply）：等父进程退出 → 备份换文件 → StartService →
// 等待 connected.ok 标记（新 agent 首次控制连接成功后写）→ 成功删 .old
// 退出；超时回滚（.old 复位 + 再启动）。
//
// 注意：apply 期间 XNCCore 服务停机（无 pipe server），直到新 agent 写
// 出 connected 标记——最长约为该标记窗口（~3 分钟）。
package updater

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// connectedMarker 新 agent 首次控制连接就绪后写的标记（回滚判据）。
func ConnectedMarkerPath(stateDir string) string {
	return filepath.Join(stateDir, "update-connected.ok")
}

// apply 平台入口：Windows spawn 换文件子进程（换文件在子进程内完成）。
func (u *Updater) apply(stageDir string) error {
	agentPath, err := os.Executable()
	if err != nil {
		return err
	}
	agentDir := filepath.Dir(agentPath)

	// spawn --apply-update 子进程（DETACHED：不随服务退出被杀）。
	cmd := exec.Command(agentPath, "--apply-update", stageDir, fmt.Sprint(os.Getpid()))
	cmd.Dir = agentDir
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn apply-update child: %w", err)
	}
	// 主进程退出——SCM 视为服务停止，由子进程 StartService 拉起新版。
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

	// 换文件（.old 备份只保留一代）。prod bootstrap 起 bundle 含
	// core/desktop/shell 三件套（requiredFiles），一并就位；XNCCore 服务
	// 在换文件前停、agent 验证后拉起（withCoreRestart）。
	old := make(map[string]string)
	for _, name := range requiredFiles {
		dst := filepath.Join(agentDir, name)
		if err := removeOld(dst); err != nil {
			return fmt.Errorf("backup %s: %w", name, err)
		}
		old[name] = dst
	}
	swap := func() error {
		for name, dst := range old {
			if err := os.Rename(filepath.Join(stageDir, name), dst); err != nil {
				return err
			}
		}
		// 起 agent 服务 + 等连接标记（逻辑不变，只是挪进 swap 闭包）。
		if err := startService(); err != nil {
			return err
		}
		if err := waitConnectedMarker(stateDir, log); err != nil {
			return err
		}
		// 成功：清扫备份与 staging（既有语义）。
		for _, dst := range old {
			_ = os.Remove(dst + ".old")
		}
		_ = os.RemoveAll(stageDir)
		log("apply-update: verified, cleaned up")
		return nil
	}
	rollback := func() error {
		// swap 失败：恢复 .old、重启 agent 服务（既有回滚语义）。
		// 2026-08-25 生产事故修复：waitConnectedMarker 超时回滚时新 agent
		// 可能仍在运行，运行中 exe 被锁导致 .old→exe 的 rename 失败（现场：
		// 4 个 .old 残留、新 exe 缺失、服务停止）。必须先停服务再换文件。
		if err := ensureAgentStopped(); err != nil {
			log("apply-update: rollback stop service failed: %v", err)
		}
		var rollbackErr error
		for name, dst := range old {
			if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
				rollbackErr = err
			}
			if err := os.Rename(dst+".old", dst); err != nil && !os.IsNotExist(err) {
				rollbackErr = err
			}
			_ = os.Remove(filepath.Join(stageDir, name)) // 残留清扫
		}
		_ = os.RemoveAll(stageDir)
		if err := startService(); err != nil {
			log("apply-update: rollback start service failed: %v", err)
		}
		if rollbackErr != nil {
			log("apply-update: rollback incomplete: %v", rollbackErr)
		}
		return rollbackErr
	}
	return withCoreRestart(coreSvc, swap, rollback)
}

// waitConnectedMarker 等新 agent 写 update-connected.ok（3 分钟超时）。
func waitConnectedMarker(stateDir string, log func(string, ...any)) error {
	marker := ConnectedMarkerPath(stateDir)
	_ = os.Remove(marker)
	log("apply-update: service started, waiting for connect marker")
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("new agent did not connect in time")
}

// coreServiceProd apply 之舞的 XNCCore 服务决策缝（可注入便于单测）。
type coreService interface {
	EnsureStopped() error // 服务不存在/未运行 = 成功（容错）
	EnsureStarted() error // 同上（存在即尽力拉起）
}

// coreSvc 默认实现：真 SCM。RunApply 经它停/起 XNCCore。
var coreSvc coreService = scmCoreService{coreServiceName}

const coreServiceName = "XNCCore"

// withCoreRestart：停 XNCCore → swap() → 拉 XNCCore（swap 失败先回滚再
// 拉，与恢复后的旧文件配套）。XNCCore 不存在时停/起均为容错无操作。
func withCoreRestart(c coreService, swap, rollback func() error) error {
	if err := c.EnsureStopped(); err != nil {
		return err
	}
	swapErr := swap()
	if swapErr != nil {
		_ = rollback()
	}
	if err := c.EnsureStarted(); err != nil {
		return err
	}
	return swapErr
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

// ensureAgentStopped 回滚前停 XNCAgent(运行中的 exe 会锁文件,致 .old
// 恢复失败——2026-08-25 生产事故;容错:服务未运行/不存在 = 成功)。
func ensureAgentStopped() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService("XNCAgent")
	if err != nil {
		return nil // 服务不存在
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil || st.State != svc.StartPending && st.State != svc.Running {
		return nil
	}
	_, _ = s.Control(svc.Stop)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		st, err = s.Query()
		if err != nil || st.State == svc.Stopped {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("XNCAgent did not stop within 30s")
}

// scmCoreService 停/起 XNCCore（服务缺失/未运行一律 nil——容错，apply
// 之舞不得因无 XNCCore 的部署而失败）。
type scmCoreService struct{ name string }

func (s scmCoreService) EnsureStopped() error {
	m, err := mgr.Connect()
	if err != nil {
		return nil // SCM 不可达：无从停（换文件仍可行；运行中的 core
		// 句柄会让 rename 失败——那是部署问题，走既有错误路径）
	}
	defer m.Disconnect()
	asvc, err := m.OpenService(s.name)
	if err != nil {
		return nil // 服务不存在
	}
	defer asvc.Close()
	st, err := asvc.Query()
	if err != nil || st.State != svc.StartPending && st.State != svc.Running {
		return nil
	}
	_, _ = asvc.Control(svc.Stop)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		st, err := asvc.Query()
		if err != nil || st.State != svc.StopPending && st.State != svc.StartPending {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

func (s scmCoreService) EnsureStarted() error {
	m, err := mgr.Connect()
	if err != nil {
		return nil // 容错：尽力而为
	}
	defer m.Disconnect()
	asvc, err := m.OpenService(s.name)
	if err != nil {
		return nil // 服务不存在（旧部署无 XNCCore）
	}
	defer asvc.Close()
	st, err := asvc.Query()
	if err == nil && (st.State == svc.Running || st.State == svc.StartPending) {
		return nil
	}
	_ = asvc.Start() // 容错：失败留给 XNCCore 自身的恢复启动（start= auto）
	return nil
}
