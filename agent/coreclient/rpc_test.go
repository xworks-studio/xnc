//go:build windows

// rpc_test.go — START/STOP_CAPTURE RPC(0x0100/0x0101)用例:进程内假服务端
// 走完整握手后按 C++ core 侧布局应答固定二进制 payload,验证请求编码、
// 响应解码、错误帧透传与并发请求路径的 goroutine 安全性(pending map +
// RequestID 关联)。响应编码在测试侧独立手写(镜像 native/core 实现,
// 双实现交叉 = M0 跨语言向量精神的延续)。
package coreclient

import (
	"bytes"
	"encoding/binary"
	"net"
	"strings"
	"sync"
	"testing"

	"xnc/proto/ipc"
)

// encodeStartCaptureRespT 是测试侧服务端编码器(布局 = C++ core):
// [u32 pid][u16 nameLen][name utf8][32B secret][u32 gen]。
func encodeStartCaptureRespT(pid uint32, pipe string, secret []byte, gen uint32) []byte {
	p := make([]byte, 0, 6+len(pipe)+len(secret)+4)
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], pid)
	p = append(p, b[:]...)
	binary.LittleEndian.PutUint16(b[:2], uint16(len(pipe)))
	p = append(p, b[:2]...)
	p = append(p, pipe...)
	p = append(p, secret...)
	binary.LittleEndian.PutUint32(b[:], gen)
	p = append(p, b[:]...)
	return p
}

// startFakeCore accepts one connection, handshakes as the server, then
// answers requests until EOF:
//   - MsgStartCapture: asserts [u32 wts][u32 pad=0], first injects one
//     unsolicited event frame (the client pump must drop it, not mispair),
//     then responds with the capture descriptor (or a FlagError frame when
//     startErr != "").
//   - MsgStopCapture: responds FlagResponse with empty payload.
//   - MsgPing: responds Pong.
//
// wantWts == nil disables the wts assertion (any value accepted).
func startFakeCore(t *testing.T, ln net.Listener, secret, captureSecret []byte,
	wantWts *uint32, pid uint32, pipe string, gen uint32, startErr string) {
	t.Helper()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if err := ServerHandshake(conn, secret); err != nil {
			t.Errorf("fake core handshake: %v", err)
			return
		}
		for {
			f, err := ipc.ReadFrame(conn)
			if err != nil {
				return
			}
			switch f.MessageType {
			case MsgStartCapture:
				if wantWts != nil {
					if len(f.Payload) != 8 || binary.LittleEndian.Uint32(f.Payload) != *wantWts ||
						!bytes.Equal(f.Payload[4:8], []byte{0, 0, 0, 0}) {
						t.Errorf("start_capture payload = %x, want [u32 %d][u32 0]", f.Payload, *wantWts)
						return
					}
				}
				if startErr != "" {
					_ = ipc.WriteFrame(conn, &ipc.Frame{
						Flags: ipc.FlagResponse | ipc.FlagError, MessageType: MsgStartCapture,
						RequestID: f.RequestID, Payload: []byte(startErr),
					})
					continue
				}
				// 无关事件帧先行:客户端泵必须丢弃,不得误配对。
				_ = ipc.WriteFrame(conn, &ipc.Frame{
					Flags: ipc.FlagEvent, MessageType: 0x0105, RequestID: 0, Payload: []byte{1},
				})
				_ = ipc.WriteFrame(conn, &ipc.Frame{
					Flags: ipc.FlagResponse, MessageType: MsgStartCapture,
					RequestID: f.RequestID,
					Payload:   encodeStartCaptureRespT(pid, pipe, captureSecret, gen),
				})
			case MsgStopCapture:
				_ = ipc.WriteFrame(conn, &ipc.Frame{
					Flags: ipc.FlagResponse, MessageType: MsgStopCapture, RequestID: f.RequestID,
				})
			case ipc.MsgPing:
				_ = ipc.WriteFrame(conn, &ipc.Frame{
					Flags: ipc.FlagResponse, MessageType: ipc.MsgPong, RequestID: f.RequestID,
				})
			default:
				t.Errorf("fake core: unexpected message type %#x", f.MessageType)
				return
			}
		}
	}()
}

// TestStartCaptureRoundTrip 覆盖:请求 payload 布局([u32 wts][u32 pad=0])、
// 响应解码([u32 pid][u16 nameLen][name utf8][32B secret][u32 gen])、
// 事件帧不串扰、StopCapture 空响应即成功。
func TestStartCaptureRoundTrip(t *testing.T) {
	name, ln := listen(t)
	const secret = "test-pipe-secret"
	captureSecret := make([]byte, 32)
	for i := range captureSecret {
		captureSecret[i] = byte(i*3 + 1)
	}
	wts := uint32(5)
	startFakeCore(t, ln, []byte(secret), captureSecret, &wts, 1234,
		`\\.\pipe\xnc-desktop-rt-77`, 7, "")

	c, err := Dial(name, []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pid, pipe, sec, gen, err := c.StartCapture(wts)
	if err != nil {
		t.Fatal(err)
	}
	if pid != 1234 || pipe != `\\.\pipe\xnc-desktop-rt-77` || gen != 7 {
		t.Fatalf("StartCapture = pid %d pipe %q gen %d", pid, pipe, gen)
	}
	if !bytes.Equal(sec, captureSecret) {
		t.Fatalf("secret mismatch: %x", sec)
	}
	if err := c.StopCapture(); err != nil {
		t.Fatalf("StopCapture: %v", err)
	}
}

// TestStartCaptureErrorFrame:FlagError 响应的 payload 文本必须进错误
// (如旧核心的 NOT_IMPLEMENTED / 新核心的 SPAWN_FAILED 码)。
func TestStartCaptureErrorFrame(t *testing.T) {
	name, ln := listen(t)
	const secret = "test-pipe-secret"
	startFakeCore(t, ln, []byte(secret), nil, nil, 0, "", 0, "SPAWN_FAILED")
	c, err := Dial(name, []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _, _, _, err = c.StartCapture(1)
	if err == nil || !strings.Contains(err.Error(), "SPAWN_FAILED") {
		t.Fatalf("want SPAWN_FAILED error, got %v", err)
	}
}

// TestConcurrentRequests:多 goroutine 并发 Ping/StartCapture/StopCapture,
// 全部按 RequestID 正确配对(请求路径 goroutine 安全)。
func TestConcurrentRequests(t *testing.T) {
	name, ln := listen(t)
	const secret = "test-pipe-secret"
	captureSecret := make([]byte, 32)
	for i := range captureSecret {
		captureSecret[i] = byte(0xA0 + i)
	}
	startFakeCore(t, ln, []byte(secret), captureSecret, nil, 99,
		`\\.\pipe\xnc-desktop-rt-99`, 1, "")

	c, err := Dial(name, []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 8; i++ {
				var err error
				switch (g + i) % 3 {
				case 0:
					_, err = c.Ping()
				case 1:
					var pid uint32
					var pipe string
					var sec []byte
					var gen uint32
					pid, pipe, sec, gen, err = c.StartCapture(3)
					if err == nil && (pid != 99 || pipe != `\\.\pipe\xnc-desktop-rt-99` || gen != 1 || !bytes.Equal(sec, captureSecret)) {
						err = errBadValues
					}
				case 2:
					err = c.StopCapture()
				}
				if err != nil {
					errs <- err
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

var errBadValues = &badValuesError{}

type badValuesError struct{}

func (*badValuesError) Error() string { return "StartCapture returned mismatched values" }
