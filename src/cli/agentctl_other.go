//go:build !windows && !linux

package main

import (
	"context"
	"fmt"
	"net"
)

// dialAgentCtlPipe：控制面传输仅存在于 Windows（named pipe）与 Linux
// （unix socket）；其他平台一律不可达（测试经 agentctlDial 缝隙注入假
// 连接，不受此影响）。
func dialAgentCtlPipe(_ context.Context, _ string) (net.Conn, error) {
	return nil, fmt.Errorf("agent control pipe is only available on Windows and Linux")
}

func agentctlPipeName() string { return "" }
