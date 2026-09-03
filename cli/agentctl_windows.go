//go:build windows

package main

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
)

// dialAgentCtlPipe 连接 agent 控制管道（生产拨号；测试经 agentctlDial 注入）。
func dialAgentCtlPipe(ctx context.Context, pipe string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, pipe)
}
