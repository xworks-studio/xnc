//go:build linux

// pipe_linux.go — unix socket 服务端（Linux 真机控制面）：
// /run/xnc/agentctl.sock，目录 0755、socket 0700（root-only 连接面——
// 本机 CLI 操作需 sudo，与 Windows「管理员终端」同级约束）。协议与
// Windows 完全一致（单行 JSON 一问一答，conn.go 共用）；反注册的 admin
// 校验经 SO_PEERCRED 复核连接方 uid==0（socket 0700 下恒真，纵深防御）。
package agentctl

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

// defaultPipeName Linux 默认 socket 路径（CLI 按此连接）。
func defaultPipeName() string { return "/run/xnc/agentctl.sock" }

// Listen 阻塞 accept 循环（与 Windows 版同构）：一次服务一个连接，ctx
// 取消关闭监听并返回。/run 是 tmpfs，异常退出的 socket 残留重启即消，
// 监听前仍先清理一次（同进程重启场景）。
func (s *Server) Listen(ctx context.Context) error {
	if s.IsAdminConn == nil {
		s.IsAdminConn = connIsAdmin
	}
	if err := os.MkdirAll("/run/xnc", 0o755); err != nil {
		return fmt.Errorf("agentctl: mkdir /run/xnc: %w", err)
	}
	_ = os.Remove(s.name()) // 旧实例残留（正常停机会清理，异常退出兜底）
	ln, err := net.Listen("unix", s.name())
	if err != nil {
		return fmt.Errorf("agentctl: listen %s: %w", s.name(), err)
	}
	// root-only 连接面：umask 不受控，监听后显式 chmod 收紧。
	if err := os.Chmod(s.name(), 0o700); err != nil {
		_ = ln.Close()
		return fmt.Errorf("agentctl: chmod socket: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				_ = os.Remove(s.name())
				return nil
			}
			return fmt.Errorf("agentctl: accept: %w", err)
		}
		s.handleConn(ctx, conn)
	}
}

// connIsAdmin：SO_PEERCRED 取连接方 uid，0 = root（sudo提权）。
func connIsAdmin(conn net.Conn) (bool, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return false, errors.New("agentctl: not a unix socket connection")
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return false, fmt.Errorf("agentctl: syscall conn: %w", err)
	}
	var (
		cred    *syscall.Ucred
		socErr  error
		ctrlErr error
	)
	ctrlErr = rc.Control(func(fd uintptr) {
		cred, socErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if ctrlErr != nil {
		return false, fmt.Errorf("agentctl: control fd: %w", ctrlErr)
	}
	if socErr != nil {
		return false, fmt.Errorf("agentctl: so_peercred: %w", socErr)
	}
	return cred.Uid == 0, nil
}

// RoundTrip 连接控制 socket 完成一次一问一答（CLI/测试用；ctx 控制拨号
// 与收发）。拨号失败常见于未提权（socket 0700 root-only）——错误串带
// sudo 提示。
func RoundTrip(ctx context.Context, pipe string, req Request) (Response, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", pipe)
	if err != nil {
		return Response{}, fmt.Errorf("agentctl: dial %s (try sudo): %w", pipe, err)
	}
	return roundTripJSON(ctx, conn, req)
}
