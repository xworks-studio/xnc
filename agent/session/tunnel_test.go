// tunnel_test.go — Tunnel 处理器用例：TCP 双向中继、目标不可达回
// ERROR{RDP_NOT_AVAILABLE}。tunnel 的 SESSION_OPEN params 是服务端白名单展开的
// {"target","host","port"} map，而非 proto.TunnelParams。
package session

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// runSessionTunnel 同 runSessionFile 模式，但 params 为任意可 marshal 值
// （原始 JSON）且用 NewTunnel 处理。
func runSessionTunnel(t *testing.T, params any) *websocket.Conn {
	t.Helper()
	up := make(chan *websocket.Conn, 1)
	errCh := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		up <- c
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	raw, _ := json.Marshal(params)
	go func() {
		c, _, err := websocket.Dial(context.Background(), "ws"+srv.URL[4:], nil)
		if err != nil {
			errCh <- err
			return
		}
		NewTunnel(testLogger()).Handle(context.Background(), c, "sess-tunnel", raw)
	}()
	select {
	case c := <-up:
		t.Cleanup(func() { c.CloseNow() })
		return c
	case err := <-errCh:
		t.Fatalf("tunnel dial failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no ws connection")
	}
	return nil
}

// readBinaryInto 读一帧 binary 并拷入 buf，返回字节数。
func readBinaryInto(t *testing.T, ws *websocket.Conn, buf []byte) int {
	t.Helper()
	kind, data := readFrame(t, ws)
	require.Equal(t, "binary", kind)
	return copy(buf, data)
}

func TestTunnelRelaysTCPTraffic(t *testing.T) {
	// 本地 TCP echo server
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(c, c) // echo
				_ = c.Close()
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	ws := runSessionTunnel(t, map[string]any{"target": "test", "host": "127.0.0.1", "port": port})
	sendBin(t, ws, []byte("ping"))
	buf := make([]byte, 4)
	n := readBinaryInto(t, ws, buf)
	assert.Equal(t, "ping", string(buf[:n]))
}

func TestTunnelUnreachableSendsError(t *testing.T) {
	// 端口 1 几乎必然连不上（无监听 → 拒绝连接）
	ws := runSessionTunnel(t, map[string]any{"target": "test", "host": "127.0.0.1", "port": 1})
	kind, data := readFrame(t, ws)
	require.Equal(t, "text", kind)
	var m proto.Message
	require.NoError(t, json.Unmarshal(data, &m))
	require.Equal(t, proto.TypeError, m.Type)
	var ep proto.ErrorPayload
	require.NoError(t, m.Decode(&ep))
	assert.Equal(t, proto.CodeRdpNotAvailable, ep.Code)
}
