//go:build !windows

package main

import (
	"context"
	"fmt"
	"net"
)

// dialAgentCtlPipe：控制管道仅存在于 Windows（agent 服务所在平台）；其他
// 平台一律不可达（测试经 agentctlDial 缝隙注入假管道，不受此影响）。
func dialAgentCtlPipe(_ context.Context, _ string) (net.Conn, error) {
	return nil, fmt.Errorf("agent control pipe is only available on Windows")
}
