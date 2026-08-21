// screen_windows.go — Windows 侧实现：
//   - dialPipe：named pipe 客户端，连接 helper 的 xnc-screen-* pipe（helper
//     进程先建 pipe 再被 dial，重试窗口覆盖进程启动延迟）。
//   - launchHelperAsUser：Session 0 桥接——agent 以 Windows 服务运行于
//     Session 0，其直接子进程同样位于 Session 0，无法访问用户桌面 / DXGI。
//     经 WTSGetActiveConsoleSessionId + WTSQueryUserToken + CreateProcessAsUser
//     （SysProcAttr.Token 使 os/exec 底层改走 CreateProcessAsUser）将 helper
//     拉进用户控制台会话。
//
//go:build windows

package session

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"syscall"
	"time"

	winio "github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const screenPipeDialTimeout = 5 * time.Second

// dialPipe 连接 \\.\pipe\<name>（PIPE_BUSY / 尚未创建时重试直至超时）。
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

// launchHelperAsUser 在用户控制台会话中启动 helper 并返回已启动的进程：
//
//  1. WTSGetActiveConsoleSessionId 定位活动控制台会话；
//  2. WTSQueryUserToken 取该会话用户的访问令牌（需 SE_TCB 特权，SYSTEM
//     服务持有；令牌在 Start 返回后即关）；
//  3. SysProcAttr.Token 令 os/exec 底层改走 CreateProcessAsUser——helper
//     进入用户桌面会话，可访问 DXGI / GDI。
//
// 令牌查询失败（agent 非服务、本就运行于交互会话的开发场景）时回退普通
// CreateProcess——子进程与 agent 同会话，行为等价。CREATE_NO_WINDOW 防止
// helper 在用户桌面弹出控制台窗口。stderr 非 nil 时接管 helper 的输出
// （Snapshot 用于错误诊断）。
func launchHelperAsUser(log *slog.Logger, helperPath string, stderr io.Writer, args ...string) (*exec.Cmd, error) {
	cmd := exec.Command(helperPath, args...)
	if stderr != nil {
		cmd.Stdout, cmd.Stderr = stderr, stderr
	}
	attr := &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	consoleSession := windows.WTSGetActiveConsoleSessionId()
	var token windows.Token
	if err := windows.WTSQueryUserToken(consoleSession, &token); err != nil {
		if log != nil {
			log.Warn("screen helper: WTSQueryUserToken failed, launching in agent session",
				"consoleSession", consoleSession, "err", err)
		}
	} else {
		defer token.Close()
		attr.Token = syscall.Token(token)
	}
	cmd.SysProcAttr = attr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start screen helper: %w", err)
	}
	return cmd, nil
}
