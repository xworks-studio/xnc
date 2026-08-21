//go:build windows

// pipe_windows.go — named pipe 服务端。使用 go-winio ListenPipe：安全描述符
// 允许同机连接（默认仅创建者账户，agent 服务账户通过 CreateProcessAsUser 拉起
// helper 时以用户令牌运行，可连接）。
package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

// listenPipe 创建 named pipe 服务端并等待 agent 连接（超时 30s——agent 拉起
// helper 后立即回连）。
func listenPipe(ctx context.Context, name string) (net.Conn, error) {
	ln, err := winio.ListenPipe(`\\.\pipe\`+name, &winio.PipeConfig{
		MessageMode:      false, // 字节流——帧协议自带长度头
		InputBufferSize:  0,
		OutputBufferSize: 0,
		// DACL：所有者 / SYSTEM / Administrators 完全控制。agent 以 Session 0
		// 服务（SYSTEM）运行，需能连接用户会话 helper 创建的 pipe——默认 SD
		// （仅创建者）会拒绝跨令牌连接。
		SecurityDescriptor: "D:P(A;;GA;;;OW)(A;;GA;;;SY)(A;;GA;;;BA)",
	})
	if err != nil {
		return nil, fmt.Errorf("ListenPipe: %w", err)
	}

	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() {
		conn, err := ln.Accept()
		ch <- accepted{conn, err}
	}()

	select {
	case a := <-ch:
		ln.Close()
		if a.err != nil {
			return nil, fmt.Errorf("accept: %w", a.err)
		}
		return a.conn, nil
	case <-time.After(30 * time.Second):
		ln.Close()
		return nil, fmt.Errorf("pipe accept timeout")
	case <-ctx.Done():
		ln.Close()
		return nil, ctx.Err()
	}
}
