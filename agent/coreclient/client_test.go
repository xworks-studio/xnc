//go:build windows

// client_test.go — coreclient 用例:进程内 winio pipe 假服务端走完整
// 握手 + Ping 往返;secret 不一致时 Dial 必须失败;服务端不回 Pong 时
// Ping 必须超时。依赖 named pipe,故仅 Windows 构建(agent/session 先例)。
package coreclient

import (
	"net"
	"testing"

	"github.com/Microsoft/go-winio"
	"xnc/proto/ipc"
)

func listen(t *testing.T) (string, net.Listener) {
	t.Helper()
	ln, err := winio.ListenPipe(`\\.\pipe\xnc-coreclient-test-`+t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String(), ln
}

func TestHandshakeAndPing(t *testing.T) {
	name, ln := listen(t)
	const secret = "test-pipe-secret"
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if err := ServerHandshake(conn, []byte(secret)); err != nil {
			t.Errorf("server handshake: %v", err)
			return
		}
		f, err := ipc.ReadFrame(conn) // Ping
		if err != nil || f.MessageType != ipc.MsgPing {
			t.Errorf("ping read: %v %+v", err, f)
			return
		}
		_ = ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagResponse, MessageType: ipc.MsgPong, RequestID: f.RequestID})
	}()
	c, err := Dial(name, []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Ping(); err != nil {
		t.Fatal(err)
	}
}

func TestHandshakeRejectsWrongSecret(t *testing.T) {
	name, ln := listen(t)
	go func() {
		conn, _ := ln.Accept()
		defer conn.Close()
		if err := ServerHandshake(conn, []byte("server-secret")); err != nil {
			return // 客户端证明失败即断
		}
	}()
	_, err := Dial(name, []byte("client-secret"))
	if err == nil {
		t.Fatal("Dial must fail when secrets differ")
	}
}

func TestPingTimeout(t *testing.T) {
	c := &Client{conn: pipeConn(t)} // 注入不回包连接
	// 用真实 pipe 但服务端不回 Pong:
	if _, err := c.Ping(); err == nil {
		t.Fatal("Ping must time out without Pong")
	}
}

// pipeConn 返回 net.Pipe 的一侧;对端 goroutine 只 ReadFrame 消费、
// 永不回包,模拟死掉的服务端(Client.Ping 内部设 2s 读 deadline)。
func pipeConn(t *testing.T) net.Conn {
	t.Helper()
	conn, peer := net.Pipe()
	t.Cleanup(func() {
		conn.Close()
		peer.Close()
	})
	go func() {
		for {
			if _, err := ipc.ReadFrame(peer); err != nil {
				return
			}
		}
	}()
	return conn
}
