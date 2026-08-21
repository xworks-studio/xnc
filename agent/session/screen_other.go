// screen_other.go — 非 Windows 桩：无 named pipe，dialPipe 恒失败 →
// Subscribe 启动 helper 失败 → state 保持 no_session → ScreenHandler 仅发
// SCREEN_BEGIN {state:"no_session"} 即收线。
//
//go:build !windows

package session

import (
	"errors"
	"net"
)

// dialPipe 非 Windows 无实现。
func dialPipe(string) (net.Conn, error) {
	return nil, errors.New("screen streaming requires a windows agent")
}
