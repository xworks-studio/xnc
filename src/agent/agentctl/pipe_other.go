//go:build !windows && !linux

// pipe_other.go — 其余平台桩：控制面传输不存在（Windows = named pipe，
// Linux = unix socket）。Listen 随 ctx 生命周期阻塞（保持 agent.Run 编排
// 统一），不会接受任何连接；依赖接口与协议类型在 agentctl.go（平台无关）。
package agentctl

import (
	"context"
	"errors"
	"net"
)

func defaultPipeName() string { return "" }

// Listen 其余平台：无传输，阻塞至 ctx 取消。
func (s *Server) Listen(ctx context.Context) error {
	if s.IsAdminConn == nil {
		s.IsAdminConn = func(net.Conn) (bool, error) {
			return false, errors.New("agentctl: admin check unsupported on this platform")
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

// RoundTrip 其余平台：无可拨传输。
func RoundTrip(_ context.Context, _ string, _ Request) (Response, error) {
	return Response{}, errors.New("agentctl: control transport unsupported on this platform")
}
