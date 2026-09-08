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
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"xnc/proto/ipc"
)

// encodeStartCaptureRespT 是测试侧服务端编码器(布局 = C++ core, RTV):
// [u32 pid][u32 gen]。
func encodeStartCaptureRespT(pid uint32, gen uint32) []byte {
	p := make([]byte, 8)
	binary.LittleEndian.PutUint32(p, pid)
	binary.LittleEndian.PutUint32(p[4:], gen)
	return p
}

// startFakeCore accepts one connection, handshakes as the server, then
// answers requests until EOF:
//   - MsgStartCapture: asserts [u32 wts][u16 cfgLen][cfg], first injects one
//     unsolicited event frame (the client pump must drop it, not mispair),
//     then responds with the capture descriptor (or a FlagError frame when
//     startErr != "").
//   - MsgStopCapture: responds FlagResponse with empty payload.
//   - MsgPing: responds Pong.
//
// wantWts == nil disables the wts assertion (any value accepted).
func startFakeCore(t *testing.T, ln net.Listener, secret []byte,
	wantWts *uint32, pid uint32, gen uint32, startErr string) {
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
					cfgLen := 0
					if len(f.Payload) >= 6 {
						cfgLen = int(binary.LittleEndian.Uint16(f.Payload[4:6]))
					}
					if len(f.Payload) != 6+cfgLen || cfgLen == 0 ||
						binary.LittleEndian.Uint32(f.Payload) != *wantWts {
						t.Errorf("start_capture payload = %x, want [u32 %d][u16 cfgLen][cfg]", f.Payload, *wantWts)
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
					Payload:   encodeStartCaptureRespT(pid, gen),
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

// TestStartCaptureRoundTrip 覆盖:请求 payload 布局([u32 wts][u16 cfgLen]
// [cfg])、响应解码([u32 pid][u32 gen])、事件帧不串扰、StopCapture 空
// 响应即成功。
func TestStartCaptureRoundTrip(t *testing.T) {
	name, ln := listen(t)
	const secret = "test-pipe-secret"
	wts := uint32(5)
	startFakeCore(t, ln, []byte(secret), &wts, 1234, 7, "")

	c, err := Dial(name, []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cfg := []byte(`{"endpoint":"xnc.app:4433","nodeId":"n1","token":"t"}`)
	pid, gen, err := c.StartCapture(wts, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if pid != 1234 || gen != 7 {
		t.Fatalf("StartCapture = pid %d gen %d", pid, gen)
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
	startFakeCore(t, ln, []byte(secret), nil, 0, 0, "SPAWN_FAILED")
	c, err := Dial(name, []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _, err = c.StartCapture(1, []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "SPAWN_FAILED") {
		t.Fatalf("want SPAWN_FAILED error, got %v", err)
	}
}

// TestConcurrentRequests:多 goroutine 并发 Ping/StartCapture/StopCapture,
// 全部按 RequestID 正确配对(请求路径 goroutine 安全)。
func TestConcurrentRequests(t *testing.T) {
	name, ln := listen(t)
	const secret = "test-pipe-secret"
	startFakeCore(t, ln, []byte(secret), nil, 99, 1, "")

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
					var pid, gen uint32
					pid, gen, err = c.StartCapture(3, []byte(`{}`))
					if err == nil && (pid != 99 || gen != 1) {
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

// startSasFakeCore answers Ping + MsgSas(0x0110;M2-Slice1 Task 4/5 镜像):
// 断言 payload 恰为 24 字节 NUL 填充 reason 字段,按 sasErr 空/非空回
// ok [u32 hr] / FlagError(稳定 ASCII 码)。收到的 reason 原样记录。
func startSasFakeCore(t *testing.T, ln net.Listener, secret []byte, hr uint32, sasErr string,
	reasons *[]string) {
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
			case ipc.MsgPing:
				_ = ipc.WriteFrame(conn, &ipc.Frame{
					Flags: ipc.FlagResponse, MessageType: ipc.MsgPong, RequestID: f.RequestID,
				})
			case MsgSas:
				if len(f.Payload) != sasReasonLen {
					t.Errorf("sas payload %d bytes, want %d", len(f.Payload), sasReasonLen)
					return
				}
				if i := bytes.IndexByte(f.Payload, 0); i >= 0 {
					for _, b := range f.Payload[i:] {
						if b != 0 {
							t.Errorf("sas payload not NUL-padded: %x", f.Payload)
							return
						}
					}
				}
				*reasons = append(*reasons, strings.TrimRight(string(f.Payload), "\x00"))
				if sasErr != "" {
					_ = ipc.WriteFrame(conn, &ipc.Frame{
						Flags: ipc.FlagResponse | ipc.FlagError, MessageType: MsgSas,
						RequestID: f.RequestID, Payload: []byte(sasErr),
					})
					continue
				}
				p := make([]byte, 4)
				binary.LittleEndian.PutUint32(p, hr)
				_ = ipc.WriteFrame(conn, &ipc.Frame{
					Flags: ipc.FlagResponse, MessageType: MsgSas,
					RequestID: f.RequestID, Payload: p,
				})
			default:
				t.Errorf("sas fake core: unexpected message type %#x", f.MessageType)
				return
			}
		}
	}()
}

// TestSasRoundTrip:请求布局([char reason[24]] NUL 填充,长 reason 截断
// 到 23)、ok 响应解码 [u32 hr](非零 hr 亦是成功——合成 HRESULT 语义)、
// 门控拒绝的稳定码进错误。
func TestSasRoundTrip(t *testing.T) {
	const secret = "test-pipe-secret"
	var reasons []string
	// 长 reason(>23 字节)验证截断:编码侧必须保证 24 字节定长。
	long := strings.Repeat("v", 40)
	hrSent := uint32(0xC0000022)

	t.Run("ok", func(t *testing.T) {
		name, ln := listen(t)
		startSasFakeCore(t, ln, []byte(secret), hrSent, "", &reasons)
		c, err := Dial(name, []byte(secret))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		hr, err := c.SendSAS(long)
		if err != nil {
			t.Fatal(err)
		}
		if hr != hrSent {
			t.Fatalf("SendSAS hr = %#x, want %#x", hr, hrSent)
		}
		want := strings.Repeat("v", sasReasonLen-1)
		if len(reasons) != 1 || reasons[0] != want {
			t.Fatalf("core saw reason %q (len %d), want %d-byte truncation", reasons, len(reasons), sasReasonLen-1)
		}
	})
	t.Run("denied", func(t *testing.T) {
		name, ln := listen(t)
		startSasFakeCore(t, ln, []byte(secret), 0, "SAS_DENIED", &reasons)
		c, err := Dial(name, []byte(secret))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, err = c.SendSAS("viewer")
		if err == nil || !strings.Contains(err.Error(), "SAS_DENIED") {
			t.Fatalf("want SAS_DENIED error, got %v", err)
		}
		var rej *RejectedError
		if !errors.As(err, &rej) || rej.Code != "SAS_DENIED" {
			t.Fatalf("error not a RejectedError with the stable code: %v", err)
		}
	})
	t.Run("bad-response-len", func(t *testing.T) {
		name, ln := listen(t)
		// 服务端手写一条长度不符的 ok 响应,验证解码拒绝。
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			if err := ServerHandshake(conn, []byte(secret)); err != nil {
				return
			}
			f, err := ipc.ReadFrame(conn)
			if err != nil {
				return
			}
			_ = ipc.WriteFrame(conn, &ipc.Frame{
				Flags: ipc.FlagResponse, MessageType: MsgSas,
				RequestID: f.RequestID, Payload: []byte{1, 2},
			})
		}()
		c, err := Dial(name, []byte(secret))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if _, err := c.SendSAS("viewer"); err == nil {
			t.Fatal("2-byte sas response must decode-fail")
		}
	})
}

var errBadValues = &badValuesError{}

type badValuesError struct{}

func (*badValuesError) Error() string { return "StartCapture returned mismatched values" }

// TestSnapshotRespCodec — 0x0111 响应编解码(布局 [u32 len][jpeg bytes];
// 长度不自洽即协议错误)。请求侧为纯小端打包,核心侧 golden 覆盖,此处

