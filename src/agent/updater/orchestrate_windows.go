//go:build windows

// orchestrate_windows.go — Windows 平台实现：SYSTEM 静默执行安装器
// （spec §9.3）、自检的服务健康判据、平台缝装配。
package updater

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// installerArgs 静默安装参数（spec §9.3；/DIR 指向安装目录）。
//
// /DIR 不带内嵌引号（真机发现的坑，2026-09-03）：Go 的 exec 会把
// `"/DIR=\"C:\Program Files\XNC\""` 作为整条命令行转义，Inno 的解析器
// 不认 \"，DIR 值带着字面引号进来 → "Folder names cannot include \""
// → 安装中止退出码 3。单个 argv 元素 + 外层引号（Go 自动）即可让空格
// 路径原样到达——等价于 cmd 语法 `/DIR="C:\Program Files\XNC"`。
//
// /ALLOWDOWNGRADE：本路径兼用于回滚（installer-cache 上一版安装器即
// 降级）——安装器的降级守卫（InitializeSetup）放行此旁路；升级场景下
// 该参数无害。
func installerArgs(installDir string) []string {
	return []string{"/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/ALLOWDOWNGRADE", "/DIR=" + installDir}
}

// platform 装配平台缝（New 已调用；直接构造 Updater 的测试再保险调用，
// 幂等——仅填 nil 缝）。
func (u *Updater) platform() {
	if u.execInstaller == nil {
		u.execInstaller = func(path string) error {
			return RunInstallerProcess(path, u.installDir(), u.installerTimeout())
		}
	}
	if u.serviceHealthy == nil {
		u.serviceHealthy = coreServiceHealthy
	}
	if u.registerWatchdogTask == nil {
		u.registerWatchdogTask = u.registerWatchdog
	}
	if u.deleteWatchdogTask == nil {
		u.deleteWatchdogTask = deleteWatchdog
	}
}

// RunInstallerProcess 执行安装器并等待：真 setup.exe 直 exec；.cmd/.bat
// 假安装器（单测）经 cmd.exe /c（CreateProcess 不解析批处理）。超时杀
// 进程返回错误——调用方按失败回滚。
func RunInstallerProcess(path, installDir string, timeout time.Duration) error {
	args := installerArgs(installDir)
	var cmd *exec.Cmd
	switch strings.ToLower(filepath.Ext(path)) {
	case ".cmd", ".bat":
		cmd = exec.Command("cmd.exe", append([]string{"/c", path}, args...)...)
	default:
		cmd = exec.Command(path, args...)
	}
	cmd.Dir = filepath.Dir(path)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start installer: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err == nil {
			return nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("installer exit code %d", ee.ExitCode())
		}
		return err
	case <-timer.C:
		// 击杀整棵进程树（taskkill /T /F）：安装器会 spawn powershell
		// 子进程（prep/post 脚本），只杀根进程会把半途的子进程留在系统里。
		_ = exec.Command("taskkill", "/PID", fmt.Sprint(cmd.Process.Pid), "/T", "/F").Run()
		<-done
		return fmt.Errorf("installer timed out after %s", timeout)
	}
}

// coreServiceHealthy 自检判据（§9.4"服务健康"）：XNCCore 存在时须
// Running——StartPending 容忍（吸收 feature/oneclick-install 8758b82：
// 对启动中的服务判不健康会造成慢启动误回滚）；服务缺失（未装 desktop
// 组件）/SCM 不可达 = 健康（无从判断，自检不因此回滚）。XNCAgent 即本
// 进程，天然健康。
func coreServiceHealthy() error {
	m, err := mgr.Connect()
	if err != nil {
		return nil
	}
	defer m.Disconnect()
	s, err := m.OpenService(coreServiceNm)
	if err != nil {
		return nil // 服务不存在
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return nil
	}
	if st.State == svc.Running || st.State == svc.StartPending {
		return nil
	}
	return fmt.Errorf("%s state=%v", coreServiceNm, st.State)
}

// samePath 大小写无关的绝对路径等价（回滚原地执行时源==目的的判断）。
func samePath(a, b string) (bool, error) {
	aa, err := filepath.Abs(a)
	if err != nil {
		return false, err
	}
	bb, err := filepath.Abs(b)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(aa, bb), nil
}
