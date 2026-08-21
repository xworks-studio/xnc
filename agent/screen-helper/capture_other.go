//go:build !windows

// capture_other.go — 非 Windows 桩：screen 捕获仅支持 Windows（DXGI/GDI）。
package main

import (
	"context"
	"errors"
	"net"
	"time"
)

// errUnsupported 报告当前平台不支持屏幕捕获。
var errUnsupported = errors.New("screen capture not supported on this platform")

// captureLoop 非 Windows 下仅发送状态帧后持续等待 ctx 取消（协议验证用）。
func captureLoop(ctx context.Context, conn net.Conn, _ captureOpts) error {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := writeFrame(conn, pipeFrameState, []byte("no_session")); err != nil {
				return err
			}
		}
	}
}

// listenPipe 桩：非 Windows 无 named pipe 服务端。
func listenPipe(ctx context.Context, name string) (net.Conn, error) {
	return nil, errUnsupported
}

// captureWGCSingle 桩。
func captureWGCSingle(timeoutMs uint) ([]byte, int, int, error) {
	return nil, 0, 0, errUnsupported
}

// newScreenCapturer 桩。
func newScreenCapturer() (screenCapturer, string, error) {
	return nil, "", errUnsupported
}

// ddaSingleShot 桩。
func ddaSingleShot(timeoutMs uint) ([]byte, int, int, error) {
	return nil, 0, 0, errUnsupported
}
