//go:build windows

// holder_windows.go — IDD 软件设备的会话内持有者（2026-09-10 会话亲和
// 修复）。SwDeviceCreate 由哪个会话的进程调用，UMDF 驱动宿主（WUDFHost）
// 就落在哪个会话，IddCx 适配器随之绑定该会话：agent（SYSTEM，会话 0）
// 直接创建会把虚拟屏绑到会话 0 的不可见显示，用户会话永远看不到。
// 因此设备创建改由本文件把 `xnc idd-hold`（CLI 隐藏命令，见
// src/cli/cmd_idd_hold.go）拉起在活动控制台会话；agent 只持进程句柄与
// stdin 管道写端。插/拔屏 IOCTL 仍走全局设备接口（openDeviceInterface，
// 会话无关），驱动未加载时按会话内重试语义处理。
//
// 生命周期：agent 退出/崩溃 → 管道写端关闭 → 持有者读到 EOF → 关句柄
// → 设备被 PnP 移除（等价于 SwDeviceCreate 的 Handle 生命周期）。
package display

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// holderArgs 由同安装目录的 CLI 提供（版本同源，安装器同一分发）。
	holderArgs = "idd-hold"
	// holderLogRel 持有者 stdout/stderr 落点（StateDir logs 目录，契约 3）。
	holderLogRel = `logs\idd-holder.log`
	// holderReadyTimeout 设备接口出现（驱动包加载完成）的最长等待。
	holderReadyTimeout = 15 * time.Second
	// holderStopTimeout 优雅退出（关 stdin 写端）的等待上限，超时强杀。
	holderStopTimeout = 2 * time.Second
)

// startHolder 在活动控制台会话拉起 `xnc idd-hold`，等设备接口就绪后
// 返回持有进程句柄与 stdin 管道写端。无控制台会话（登录屏/无人登录）
// 返回错误——调用方视为可重试的延迟条件。
func startHolder() (proc, stdinW windows.Handle, err error) {
	session := windows.WTSGetActiveConsoleSessionId()
	if session == 0xFFFFFFFF {
		return 0, 0, fmt.Errorf("display: no active console session (device deferred)")
	}

	exe, err := os.Executable()
	if err != nil {
		return 0, 0, fmt.Errorf("display: os.Executable: %w", err)
	}
	exePath := filepath.Join(filepath.Dir(exe), "xnc.exe")

	var token windows.Token
	if err := windows.WTSQueryUserToken(session, &token); err != nil {
		return 0, 0, fmt.Errorf("display: WTSQueryUserToken(session %d): %w", session, err)
	}
	defer token.Close()

	// UAC 分体 token：WTSQueryUserToken 给的是过滤后的非提权 token，而
	// SwDeviceCreate（驱动实例安装）需要管理员特权 → 换 linked token
	// （管理员用户才有；标准用户换不到，持有者会以 0x80070005 失败并在
	// 日志可见）。linked token 仍属同一会话，会话亲和不变。
	if linked, err := linkedToken(token); err == nil {
		token.Close()
		token = linked
	}

	// 用户 token 派生环境（契约 4：子进程读 XNC_* 旋钮走注册表环境，
	// 此处不含任何密钥）。
	var envBlock *uint16
	if err := windows.CreateEnvironmentBlock(&envBlock, token, false); err != nil {
		return 0, 0, fmt.Errorf("display: CreateEnvironmentBlock: %w", err)
	}
	defer windows.DestroyEnvironmentBlock(envBlock)

	// stdin 管道：agent 持写端，持有者读至 EOF 即释放设备。
	var stdinR windows.Handle
	if err := windows.CreatePipe(&stdinR, &stdinW, nil, 0); err != nil {
		return 0, 0, fmt.Errorf("display: CreatePipe: %w", err)
	}
	ok := false
	defer func() {
		if !ok { // 失败路径：回收
			windows.CloseHandle(stdinW)
		}
		windows.CloseHandle(stdinR) // 子进程继承副本后父进程关闭
	}()

	// 读端须可继承（CreateProcessAsUser bInheritHandles=TRUE 只传继承句柄）。
	if err := windows.SetHandleInformation(stdinR, windows.HANDLE_FLAG_INHERIT, 1); err != nil {
		return 0, 0, fmt.Errorf("display: SetHandleInformation(stdin): %w", err)
	}

	// 持有者 stdout/stderr → logs\idd-holder.log（可继承，父进程事后关闭）。
	logFile, err := openInheritable(filepath.Join(os.Getenv("ProgramData"), "XNC", holderLogRel))
	if err != nil {
		return 0, 0, err
	}
	defer windows.CloseHandle(logFile)

	exePtr, err := windows.UTF16PtrFromString(exePath)
	if err != nil {
		return 0, 0, err
	}
	cmdLine, err := windows.UTF16PtrFromString(fmt.Sprintf("\"%s\" %s", exePath, holderArgs))
	if err != nil {
		return 0, 0, err
	}

	si := new(windows.StartupInfo)
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput = stdinR
	si.StdOutput = logFile
	si.StdErr = logFile
	// 注意（2026-09-11 YOGA9 实测回退）：不要指定 lpDesktop=winsta0\default——
	// 交互桌面创建会导致 OS 不查询模式回调（CommitModes 空模式、永不分配
	// swapchain）；服务窗口站 + 会话 1 token 的持有者（继承 agent 窗口站）
	// 才是已验证可分配 swapchain 的形态（20:50 实测）。

	var pi windows.ProcessInformation
	if err := windows.CreateProcessAsUser(token, exePtr, cmdLine, nil, nil, true,
		windows.CREATE_NO_WINDOW|windows.CREATE_UNICODE_ENVIRONMENT, envBlock, nil, si, &pi); err != nil {
		return 0, 0, fmt.Errorf("display: CreateProcessAsUser(xnc %s): %w", holderArgs, err)
	}
	windows.CloseHandle(pi.Thread)

	// 设备接口就绪（驱动包首次加载可达数秒）：轮询等待；持有者提前退出
	// （如非提权用户创建被拒）则立即失败，不空等 15s。
	deadline := time.Now().Add(holderReadyTimeout)
	for {
		dev, err := openDeviceInterface()
		if err == nil {
			windows.CloseHandle(dev)
			break
		}
		if res, _ := windows.WaitForSingleObject(pi.Process, 0); res == windows.WAIT_OBJECT_0 {
			stopHolder(pi.Process, stdinW)
			stdinW = 0
			return 0, 0, fmt.Errorf("display: idd holder exited before device ready (see logs/idd-holder.log)")
		}
		if time.Now().After(deadline) {
			// 超时：回收持有者，返回错误（下次触发重试）。
			stopHolder(pi.Process, stdinW)
			stdinW = 0
			return 0, 0, fmt.Errorf("display: idd device interface not ready after %v: %w", holderReadyTimeout, err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	ok = true
	return pi.Process, stdinW, nil
}

// linkedToken 返回 UAC 分体 token 的提权版本（管理员用户）。标准用户
// 无 linked token，返回错误由调用方按原 token 继续。
func linkedToken(t windows.Token) (windows.Token, error) {
	var linked windows.Token
	var ret uint32
	err := windows.GetTokenInformation(t, windows.TokenLinkedToken,
		(*byte)(unsafe.Pointer(&linked)), uint32(unsafe.Sizeof(linked)), &ret)
	if err != nil {
		return 0, err
	}
	return linked, nil
}

// stopHolder 结束持有进程：先优雅（关 stdin 写端 → 持有者 EOF 自清理），
// 超时强杀。设备随持有进程句柄释放而移除。
func stopHolder(proc, stdinW windows.Handle) {
	if stdinW != 0 {
		windows.CloseHandle(stdinW)
	}
	if proc == 0 {
		return
	}
	res, err := windows.WaitForSingleObject(proc, uint32(holderStopTimeout.Milliseconds()))
	if err != nil || res != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(proc, 1)
		_, _ = windows.WaitForSingleObject(proc, 2000)
	}
	windows.CloseHandle(proc)
}

// openInheritable 打开（或重建）可继承句柄的日志文件。
func openInheritable(path string) (windows.Handle, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return 0, fmt.Errorf("display: mkdir holder log dir: %w", err)
	}
	sa := &windows.SecurityAttributes{
		Length:        uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		InheritHandle: 1,
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, sa,
		windows.CREATE_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return 0, fmt.Errorf("display: open holder log: %w", err)
	}
	return h, nil
}
