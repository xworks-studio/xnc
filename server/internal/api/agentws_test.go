package api

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

func dialAgentWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.Dial(context.Background(), url, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func readMsg(t *testing.T, c *websocket.Conn) proto.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	require.NoError(t, err)
	var m proto.Message
	require.NoError(t, json.Unmarshal(data, &m))
	return m
}

func writeMsg(t *testing.T, c *websocket.Conn, m proto.Message) {
	t.Helper()
	b, err := json.Marshal(m)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, c.Write(ctx, websocket.MessageText, b))
}

func TestControlConnection(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-01", "mid-ws") // Step 3 助手：注册并返回 (nodeID, priv)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()

	c := dialAgentWS(t, "ws"+srv.URL[4:]+"/api/agent/connect")

	// 1. 挑战
	m := readMsg(t, c)
	require.Equal(t, proto.TypeChallenge, m.Type)
	var ch proto.Challenge
	require.NoError(t, m.Decode(&ch))
	require.NotEmpty(t, ch.Nonce)

	// （错误签名路径在独立子测试 TestControlConnectionBadSignature 验证）

	// 3. 正确签名 + HELLO
	sig := ed25519.Sign(env.Priv(t, nodeID), []byte(ch.Nonce))
	resp, _ := proto.NewMsg(proto.TypeChallengeResponse,
		proto.ChallengeResponse{NodeID: nodeID, Signature: sig})
	writeMsg(t, c, resp)

	hello, _ := proto.NewMsg(proto.TypeHello, proto.Hello{
		NodeID: nodeID, Hostname: "WEB-01", AgentVersion: "0.1.0", ShellType: "pwsh",
	})
	writeMsg(t, c, hello)
	ack := readMsg(t, c)
	require.Equal(t, proto.TypeHelloAck, ack.Type)

	// 4. 心跳
	hb, _ := proto.NewMsg(proto.TypeHeartbeat, struct{}{})
	writeMsg(t, c, hb)
	got := readMsg(t, c)
	assert.Equal(t, proto.TypeHeartbeatAck, got.Type)

	// 5. 节点在线
	nodes := env.ListNodes(t)
	require.Len(t, nodes, 1)
	assert.Equal(t, "online", nodes[0]["status"])
	assert.Equal(t, "pwsh", nodes[0]["shell_type"])

	// 6. 断开 → offline
	_ = c.CloseNow()
	require.Eventually(t, func() bool {
		return env.ListNodes(t)[0]["status"] == "offline"
	}, 5*time.Second, 200*time.Millisecond, "node should go offline after disconnect")
}

func TestControlConnectionBadSignature(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-02", "mid-ws2")
	srv := httptest.NewServer(env.Router)
	defer srv.Close()

	c := dialAgentWS(t, "ws"+srv.URL[4:]+"/api/agent/connect")
	m := readMsg(t, c)
	var ch proto.Challenge
	require.NoError(t, m.Decode(&ch))

	_, wrongPriv, _ := ed25519.GenerateKey(nil)
	sig := ed25519.Sign(wrongPriv, []byte(ch.Nonce))
	resp, _ := proto.NewMsg(proto.TypeChallengeResponse,
		proto.ChallengeResponse{NodeID: nodeID, Signature: sig})
	writeMsg(t, c, resp)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	assert.Error(t, err, "server must close connection on bad signature")
}
