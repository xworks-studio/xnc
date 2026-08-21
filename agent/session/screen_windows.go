// screen_windows.go — Windows named pipe 客户端：连接 helper 的 xnc-screen-*
// pipe（helper 进程先建 pipe 再被 dial，重试窗口覆盖进程启动延迟）。
//
//go:build windows

package session

import (
	"net"
	"time"

	winio "github.com/Microsoft/go-winio"
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
