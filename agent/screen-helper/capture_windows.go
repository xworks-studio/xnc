//go:build windows

// capture_windows.go — T4 占位：DXGI 捕获（T5）就位前仅发送状态帧，
// 保持 pipe 协议可验证。
package main

import (
	"context"
	"errors"
	"net"
	"time"
)

// errDXGIUnavailable 占位——T5 实现真实 DXGI Desktop Duplication。
var errDXGIUnavailable = errors.New("dxgi capture not yet implemented")

// captureLoop T4 占位主循环：每秒发送 0x03 "capturing" 状态帧。
func captureLoop(ctx context.Context, conn net.Conn, _ time.Duration) error {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := writeFrame(conn, pipeFrameState, []byte("capturing")); err != nil {
				return err
			}
		}
	}
}
