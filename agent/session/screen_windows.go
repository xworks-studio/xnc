//go:build windows

// screen_windows.go — Session 0 服务 → 用户控制台会话的 helper 桥接。
//
// 直接用 CreateProcessAsUserW（不走 os/exec——后者无法设置
// STARTUPINFO.lpDesktop）：不显式指定桌面时子进程继承服务调用的 window
// station，WGC CreateForMonitor 以 0x80070424（ERROR_SERVICE_NOT_ACTIVE）
// 拒绝，GDI 捕获的也是服务桌面而非用户桌面。lpDesktop 必须是
// winsta0\default。
package session

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	winio "github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// helperProc 封装经 CreateProcessAsUser 启动的 helper 进程。
type helperProc struct {
	pid      uint32
	handle   windows.Handle
	pipeR    windows.Handle // stderr 管道读端（0 = stderr 直写文件句柄）
	pipeW    windows.Handle // 父进程持有的写端（CreateProcess 后立即关闭）
	copyDone chan struct{}  // stderr 管道拷贝协程结束信号（pipeR > 0 时有效)
}

// Kill 终止 helper（幂等）。
func (p *helperProc) Kill() error {
	if p == nil || p.handle == 0 {
		return nil
	}
	return windows.TerminateProcess(p.handle, 1)
}

// Wait 等待退出。stderr 走管道时先等拷贝协程读尽（子进程关闭写端）再等
// 进程对象。退出码非 0 返回错误。
func (p *helperProc) Wait() error {
	if p == nil || p.handle == 0 {
		return fmt.Errorf("helper process not started")
	}
	if p.pipeR != 0 {
		<-p.copyDone
	}
	if _, err := windows.WaitForSingleObject(p.handle, windows.INFINITE); err != nil {
		return err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(p.handle, &code); err != nil {
		return err
	}
	windows.CloseHandle(p.handle)
	p.handle = 0
	if code != 0 {
		return fmt.Errorf("exit code %d", code)
	}
	return nil
}

// launchHelperAsUser 在用户控制台会话（winsta0\default 桌面）启动 helper。
// stderr 为 nil 时丢弃输出；为 *os.File 时直接继承其句柄；其他 Writer 经
// 匿名管道中继（子进程写端继承，本进程读端拷贝至 writer）。
func launchHelperAsUser(log *slog.Logger, helperPath string, stderr io.Writer, args ...string) (*helperProc, error) {
	// 组装命令行（应用名带引号转义）。
	cmdline := `"` + helperPath + `"`
	for _, a := range args {
		cmdline += ` "` + a + `"`
	}
	cmd16, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return nil, err
	}
	app16, err := windows.UTF16PtrFromString(helperPath)
	if err != nil {
		return nil, err
	}
	dir, err := windows.UTF16PtrFromString(filepath.Dir(helperPath))
	if err != nil {
		return nil, err
	}

	p := &helperProc{}
	var stdH windows.Handle
	if f, ok := stderr.(*os.File); ok {
		stdH = windows.Handle(f.Fd())
		if err := windows.SetHandleInformation(stdH, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return nil, fmt.Errorf("make stderr inheritable: %w", err)
		}
	} else if stderr != nil {
		var w windows.Handle
		if err := windows.CreatePipe(&p.pipeR, &w, &windows.SecurityAttributes{InheritHandle: 1}, 0); err != nil {
			return nil, fmt.Errorf("stderr pipe: %w", err)
		}
		stdH = w
		done := make(chan struct{})
		p.copyDone = done
		p.pipeW = w // 父进程写端：CreateProcess 成功后立即关闭（否则读端等不到 EOF）
		r := os.NewFile(uintptr(p.pipeR), "helper-stderr")
		go func() {
			io.Copy(stderr, r)
			r.Close()
			close(done)
		}()
	}

	si := &windows.StartupInfo{
		Flags:     windows.STARTF_USESTDHANDLES,
		StdOutput: stdH,
		StdErr:    stdH,
	}
	var pi windows.ProcessInformation
	var token windows.Token
	session := activeUserSession()
	if err = windows.WTSQueryUserToken(session, &token); err != nil {
		if log != nil {
			log.Warn("screen helper: WTSQueryUserToken failed, launching in agent session",
				"session", session, "err", err)
		}
		// 无用户令牌（无活动用户会话）——普通 CreateProcess，桌面设置
		// 同样生效（此时捕获大概率不可用，仅保持行为确定）。
		err = windows.CreateProcess(app16, cmd16, nil, nil, true,
			windows.CREATE_NO_WINDOW, nil, dir, si, &pi)
	} else {
		defer token.Close()
		// 用户环境上下文：env=NULL 时子进程继承调用方（SYSTEM 服务）的
		// 环境——WGC 在用户令牌 + SYSTEM 环境的混合上下文里只捕获到黑
		// 帧。直接传 CreateEnvironmentBlock 的块会被 CreateProcessAsUser
		// 以 ERROR_INVALID_PARAMETER 拒绝（TB16G7 实测），改用 cmd.exe
		// 引导：以用户令牌启动 cmd，由它设置用户环境变量后启动 helper。
		// err 必须赋给外层变量——曾经的 if 内 err 遮蔽把失败静默吞成
		// "幽灵成功"（err==nil 且 PROCESS_INFORMATION 全零）。
		r1, _, e1 := procCreateProcessAsUserW.Call(
			uintptr(token),
			uintptr(unsafe.Pointer(app16)), uintptr(unsafe.Pointer(cmd16)),
			0, 0, 1, uintptr(windows.CREATE_NO_WINDOW),
			0, uintptr(unsafe.Pointer(dir)),
			uintptr(unsafe.Pointer(si)), uintptr(unsafe.Pointer(&pi)))
		if r1 == 0 {
			err = e1
		}
	}
	if stdH != 0 && p.pipeR == 0 {
		windows.SetHandleInformation(stdH, windows.HANDLE_FLAG_INHERIT, 0)
	}
	if err != nil {
		if p.pipeR != 0 {
			windows.CloseHandle(p.pipeW)
			<-p.copyDone
		}
		return nil, fmt.Errorf("start screen helper: %w", err)
	}
	// 立即关闭父进程持有的管道写端——子进程退出后读端即可见 EOF。
	if p.pipeW != 0 {
		windows.CloseHandle(p.pipeW)
		p.pipeW = 0
	}
	windows.CloseHandle(pi.Thread)
	p.pid, p.handle = pi.ProcessId, pi.Process
	if log != nil {
		log.Info("screen helper launched", "pid", p.pid, "handle", p.handle, "pipe", p.pipeR != 0)
	}
	return p, nil
}

// activeUserSession 返回当前活动用户会话 ID：枚举会话取 WTSActive 状态者
// （物理控制台或 RDP 会话均命中——WTSGetActiveConsoleSessionId 只看物理
// 控制台，用户经 RDP 使用机器时取不到令牌，WGC 会以 0x80070424 失败）。
// 枚举失败回退物理控制台会话。
func activeUserSession() uint32 {
	var sessions *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &sessions, &count); err == nil {
		defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessions)))
		list := unsafe.Slice(sessions, count)
		for i := range list {
			if list[i].State == windows.WTSActive {
				return list[i].SessionID
			}
		}
	}
	return windows.WTSGetActiveConsoleSessionId()
}

// dialPipe 连接 helper 的 named pipe（helper 先监听，agent 拉起后回连，
// 短暂重试直到超时）。
func dialPipe(name string) (net.Conn, error) {
	path := `\\.\pipe\` + name
	deadline := time.Now().Add(screenPipeDialTimeout)
	for {
		conn, err := winio.DialPipe(path, nil)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// screenPipeDialTimeout — agent 回连 helper pipe 的重试窗口。
const screenPipeDialTimeout = 5 * time.Second

// procCreateProcessAsUserW — kernel32 直连（绕开 advapi32 转发）。
var procCreateProcessAsUserW = windows.NewLazySystemDLL("kernel32.dll").NewProc("CreateProcessAsUserW")
