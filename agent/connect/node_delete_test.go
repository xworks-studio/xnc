package connect

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/agent/identity"
	"xnc/agent/machineinfo"
	"xnc/proto"
)

// deleteTestServer 起一个假控制面：挑战认证（验签）后等待 NODE_DELETE，
// act 在收到后驱动服务端行为（默认：直接关闭 = 删除确认）。
func deleteTestServer(t *testing.T, pub ed25519.PublicKey, act func(c *websocket.Conn, m proto.Message)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		write := func(typ string, p any) {
			m, _ := proto.NewMsg(typ, p)
			b, _ := json.Marshal(m)
			_ = c.Write(ctx, websocket.MessageText, b)
		}
		write(proto.TypeChallenge, proto.Challenge{Nonce: "nonce-del"})
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var m proto.Message
		require.NoError(t, json.Unmarshal(data, &m))
		var cr proto.ChallengeResponse
		require.NoError(t, m.Decode(&cr))
		require.Equal(t, "node-del", cr.NodeID)
		require.True(t, ed25519.Verify(pub, []byte("nonce-del"), cr.Signature))
		// HELLO（内容不校验，假面足够）
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		write(proto.TypeHelloAck, struct{}{})
		// 等待 NODE_DELETE
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m proto.Message
			require.NoError(t, json.Unmarshal(data, &m))
			if m.Type == proto.TypeHeartbeat {
				write(proto.TypeHeartbeatAck, struct{}{})
				continue
			}
			if m.Type != proto.TypeNodeDelete {
				continue
			}
			if act != nil {
				act(c, m)
			}
			_ = c.Close(websocket.StatusNormalClosure, "node deleted")
			return
		}
	}))
}

func deleteClient(srv *httptest.Server, priv ed25519.PrivateKey) *Client {
	return NewClient(srv.URL, &identity.Key{NodeID: "node-del", Priv: priv},
		machineinfo.Info{Hostname: "h", AgentVersion: "t"})
}

// DeleteNode：挑战认证 → NODE_DELETE → 服务端关闭即确认，返回 nil。
func TestDeleteNode(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	var got atomic.Bool
	srv := deleteTestServer(t, pub, func(_ *websocket.Conn, _ proto.Message) { got.Store(true) })
	defer srv.Close()

	c := deleteClient(srv, priv)
	require.NoError(t, c.DeleteNode(t.Context()))
	assert.True(t, got.Load(), "server must receive NODE_DELETE")
}

// DeleteNode 失败路径：服务端回 ERROR 帧再关闭 → 返回带错误码的错误。
func TestDeleteNodeServerError(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	srv := deleteTestServer(t, pub, func(c *websocket.Conn, _ proto.Message) {
		m, _ := proto.NewMsg(proto.TypeError, proto.ErrorPayload{Code: "INTERNAL", Message: "db down"})
		b, _ := json.Marshal(m)
		_ = c.Write(context.Background(), websocket.MessageText, b)
	})
	defer srv.Close()

	c := deleteClient(srv, priv)
	err := c.DeleteNode(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "INTERNAL")
	assert.Contains(t, err.Error(), "db down")
}

// Connected 反映当前控制连接是否就绪：握手完成后 true，连接终止/未连接 false。
func TestConnected(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	ready := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		write := func(typ string, p any) {
			m, _ := proto.NewMsg(typ, p)
			b, _ := json.Marshal(m)
			_ = c.Write(ctx, websocket.MessageText, b)
		}
		write(proto.TypeChallenge, proto.Challenge{Nonce: "n"})
		for {
			_, _, err := c.Read(ctx)
			if err != nil {
				return
			}
			select {
			case <-ready:
			default:
			}
			write(proto.TypeHelloAck, struct{}{})
			write(proto.TypeHeartbeatAck, struct{}{})
		}
	}))
	defer srv.Close()

	c := deleteClient(srv, priv)
	assert.False(t, c.Connected(), "not connected before handshake")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	require.Eventually(t, func() bool { return c.Connected() }, 5*time.Second, 50*time.Millisecond)
	cancel()
	require.Eventually(t, func() bool { return !c.Connected() }, 5*time.Second, 50*time.Millisecond,
		"Connected must drop after connection teardown")
}

// shortenAckTimeout 把删除确认等待缩短到测试量级（生产 30s）。
func shortenAckTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := nodeDeleteAckTimeout
	nodeDeleteAckTimeout = d
	t.Cleanup(func() { nodeDeleteAckTimeout = old })
}

// 服务端挂起（收到 NODE_DELETE 后不关闭、不应答）：读取超时绝不能被当成
// 删除确认——必须报错，agent 据此保留 binding（防孤儿节点）。
func TestDeleteNodeServerHang(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	srv := deleteTestServer(t, pub, func(c *websocket.Conn, _ proto.Message) {
		// 不关闭：模拟服务端删除处理挂起。
		select {}
	})
	defer srv.Close()
	shortenAckTimeout(t, 200*time.Millisecond)

	c := deleteClient(srv, priv)
	err := c.DeleteNode(t.Context())
	require.Error(t, err, "timeout must not be treated as delete confirmation")
	assert.Contains(t, err.Error(), "no close confirmation")
}

// 服务端硬关闭（CloseNow，无 close 帧）：仍是对端关闭，视为删除确认。
func TestDeleteNodeAbruptClose(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	srv := deleteTestServer(t, pub, func(c *websocket.Conn, _ proto.Message) {
		c.CloseNow()
	})
	defer srv.Close()

	c := deleteClient(srv, priv)
	require.NoError(t, c.DeleteNode(t.Context()))
}
