// screen_other.go — 非 Windows 桩：无 named pipe，dialPipe 恒失败 →
// Subscribe 启动 helper 失败 → state 保持 no_session → ScreenHandler 仅发
// SCREEN_BEGIN {state:"no_session"} 即收线。launchHelperAsUser 同样返回
// 错误（helper 仅支持 Windows）。
//
//go:build !windows

package session

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"os/exec"
)

// dialPipe 非 Windows 无实现。
func dialPipe(string) (net.Conn, error) {
	return nil, errors.New("screen streaming requires a windows agent")
}

// launchHelperAsUser 非 Windows 无 helper，直接报错（调用方回 no_session）。
func launchHelperAsUser(*slog.Logger, string, io.Writer, ...string) (*exec.Cmd, error) {
	return nil, errors.New("screen helper requires a windows agent")
}
