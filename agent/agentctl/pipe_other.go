//go:build !windows

// pipe_other.go — 非 Windows 桩：named pipe 不存在，控制面不可用。Listen
// 随 ctx 生命周期阻塞（保持 agent.Run 编排统一），不会接受任何连接；依赖
// 接口与协议类型在 agentctl.go（平台无关）。
package agentctl

import (
	"context"
	"errors"
	"net"
)

// Listen 非 Windows：无管道，阻塞至 ctx 取消。
func (s *Server) Listen(ctx context.Context) error {
	if s.IsAdminConn == nil {
		s.IsAdminConn = func(net.Conn) (bool, error) {
			return false, errors.New("agentctl: admin check unsupported on this platform")
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

// RoundTrip 非 Windows：无管道可连。
func RoundTrip(_ context.Context, _ string, _ Request) (Response, error) {
	return Response{}, errors.New("agentctl: named pipe unsupported on this platform")
}
