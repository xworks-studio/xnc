//go:build windows

// pipe_windows.go — winio named pipe 服务端（真机控制面，spec §6.3）：
// 连接方为同机 CLI（xnc register/deregister/status）。DACL 只控连接面：
// SYSTEM + Administrators + Interactive（SDDL SY/BA/IU）—— 普通交互用户可
// 注册/查询，反注册另经连接方进程令牌的 Administrators 检查（connIsAdmin，
// spec §6.3「反注册要求 Admins」）。协议为单行 JSON 一问一答，不含 secret
// 握手（本机 DACL 即边界；jwt 只在请求内单向经过，不落盘）。
package agentctl

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// pipeSDDL：DACL = SYSTEM + Administrators + Interactive（spec §6.3）。
// IU（S-1-5-4）由 Windows 在连接时按客户端令牌求值——交互登录会话的令牌
// 自动携带，服务/网络会话不携带，即「interactive logon via token check」
// 的 DACL 形态；BUILTIN\Users 不整组授予。
const pipeSDDL = "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;IU)"

// defaultPipeName Windows 默认管道名（CLI/安装器按此连接）。
func defaultPipeName() string { return `\\.\pipe\xnc-agentctl` }

// Listen 阻塞 accept 循环：一次服务一个连接（op 天然串行；后续连接在管道
// 排队）。ctx 取消关闭监听并返回（优雅停机）。
func (s *Server) Listen(ctx context.Context) error {
	if s.IsAdminConn == nil {
		s.IsAdminConn = connIsAdmin
	}
	ln, err := winio.ListenPipe(s.name(), &winio.PipeConfig{
		SecurityDescriptor: pipeSDDL,
		MessageMode:        false, // 字节流 + 行协议（与 core 管道一致的读法）
		InputBufferSize:    64 * 1024,
		OutputBufferSize:   64 * 1024,
	})
	if err != nil {
		return fmt.Errorf("agentctl: listen %s: %w", s.name(), err)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("agentctl: accept: %w", err)
		}
		s.handleConn(ctx, conn)
	}
}

// connIsAdmin 校验连接方是否为管理员：管道句柄 → 客户端 PID → 打开其进程
// 令牌 → Administrators（启用态）成员检查。CheckTokenMembership 只接受
// 模拟令牌，主令牌须先 DuplicateTokenEx（SecurityImpersonation）转换。
func connIsAdmin(conn net.Conn) (bool, error) {
	f, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return false, errors.New("agentctl: connection has no pipe handle")
	}
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(windows.Handle(f.Fd()), &pid); err != nil {
		return false, fmt.Errorf("get client pid: %w", err)
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false, fmt.Errorf("open client process %d: %w", pid, err)
	}
	defer windows.CloseHandle(h) //nolint:errcheck // 清理路径
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &tok); err != nil {
		return false, fmt.Errorf("open client token: %w", err)
	}
	defer tok.Close() //nolint:errcheck // 清理路径
	var imp windows.Token
	if err := windows.DuplicateTokenEx(tok, 0, nil,
		windows.SecurityImpersonation, windows.TokenImpersonation, &imp); err != nil {
		return false, fmt.Errorf("duplicate client token: %w", err)
	}
	defer imp.Close() //nolint:errcheck // 清理路径
	adminSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false, fmt.Errorf("build admins sid: %w", err)
	}
	return imp.IsMember(adminSID)
}

// RoundTrip 连接控制管道完成一次一问一答（CLI/测试用；ctx 控制拨号与收发）。
func RoundTrip(ctx context.Context, pipe string, req Request) (Response, error) {
	conn, err := winio.DialPipeContext(ctx, pipe)
	if err != nil {
		return Response{}, fmt.Errorf("agentctl: dial %s: %w", pipe, err)
	}
	return roundTripJSON(ctx, conn, req)
}
