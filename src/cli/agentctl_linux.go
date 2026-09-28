//go:build linux

package main

import (
	"context"
	"fmt"
	"net"
)

// agentctlPipeName Linux 默认控制 socket（与 agent/agentctl 的 Linux 实现
// 一致；socket 0700 root-only，本机操作需 sudo）。
func agentctlPipeName() string { return "/run/xnc/agentctl.sock" }

// dialAgentCtlPipe 拨 unix socket（生产拨号；测试经 agentctlDial 注入）。
// 权限类失败（非 root 连 0700 socket）在错误串带 sudo 提示。
func dialAgentCtlPipe(ctx context.Context, pipe string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", pipe)
	if err != nil {
		return nil, fmt.Errorf("dial %s (try sudo): %w", pipe, err)
	}
	return conn, nil
}
