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

// agentHandshake 完成挑战→验签→HELLO 流程，读到 HELLO_ACK 为止。
func agentHandshake(t *testing.T, env *TestEnv, c *websocket.Conn, nodeID string) {
	t.Helper()
	m := readMsg(t, c)
	require.Equal(t, proto.TypeChallenge, m.Type)
	var ch proto.Challenge
	require.NoError(t, m.Decode(&ch))
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
}

// 顶替清理竞态（fix round 1）：连接 B 顶替 A 后，A 的延迟清理不得把节点翻成
// offline，也不得删掉 B 在 registry 中的表项；最后一条连接断开才落 offline。
func TestControlConnectionReplaceKeepsOnline(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-R", "mid-replace")
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	url := "ws" + srv.URL[4:] + "/api/agent/connect"

	// 连接 A：完整握手 → online
	a := dialAgentWS(t, url)
	agentHandshake(t, env, a, nodeID)
	nodes := env.ListNodes(t)
	require.Len(t, nodes, 1)
	require.Equal(t, "online", nodes[0]["status"])

	// 连接 B：同节点再次握手 → 顶替 A（A 的 ctx 被 cancel，清理 defer 随后触发）
	b := dialAgentWS(t, url)
	agentHandshake(t, env, b, nodeID)

	// 给 A 的清理 defer 足够时间触发：节点必须仍在线、registry 仍持有新连接
	time.Sleep(1 * time.Second)
	nodes = env.ListNodes(t)
	require.Len(t, nodes, 1)
	assert.Equal(t, "online", nodes[0]["status"], "replaced conn must not flip node offline")
	assert.Equal(t, 1, env.reg.Count(), "registry must still hold the new conn")

	// 关闭 B（最后一条连接）→ offline
	_ = b.CloseNow()
	require.Eventually(t, func() bool {
		return env.ListNodes(t)[0]["status"] == "offline"
	}, 5*time.Second, 200*time.Millisecond, "node should go offline after final disconnect")
}

// 协议绑定（design §2.1）：控制连接收到二进制帧必须立即断开，内容合法也不行。
func TestControlConnectionBinaryFrameRejected(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-BIN", "mid-bin")
	srv := httptest.NewServer(env.Router)
	defer srv.Close()

	c := dialAgentWS(t, "ws"+srv.URL[4:]+"/api/agent/connect")
	m := readMsg(t, c)
	require.Equal(t, proto.TypeChallenge, m.Type)
	var ch proto.Challenge
	require.NoError(t, m.Decode(&ch))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// 内容是完全合法的 CHALLENGE_RESPONSE，但以二进制帧发送 → 必须被拒
	sig := ed25519.Sign(env.Priv(t, nodeID), []byte(ch.Nonce))
	resp, _ := proto.NewMsg(proto.TypeChallengeResponse,
		proto.ChallengeResponse{NodeID: nodeID, Signature: sig})
	b, err := json.Marshal(resp)
	require.NoError(t, err)
	require.NoError(t, c.Write(ctx, websocket.MessageBinary, b))

	// 再补发合法文本 HELLO：修复前服务器会误收二进制 CR 并回复 HELLO_ACK
	//（读取成功 → 本测试红）；修复后连接已断开 → 读取报错且无 HELLO_ACK。
	hello, _ := proto.NewMsg(proto.TypeHello, proto.Hello{
		NodeID: nodeID, Hostname: "WEB-BIN", AgentVersion: "0.1.0", ShellType: "pwsh",
	})
	hb, _ := json.Marshal(hello)
	_ = c.Write(ctx, websocket.MessageText, hb) // 修复后连接已关，写失败属预期

	_, _, err = c.Read(ctx)
	assert.Error(t, err, "server must close connection on binary frame; no HELLO_ACK")
}
