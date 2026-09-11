package api

// relay_session_test.go — 会话数据面 relay 路由（Stage B，2026-09-11）集成
// 测试：伪 relay 注册（allowlist 即 active）→ exec 会话 → 202 与
// SESSION_OPEN 的双腿 URL 均指向 relay 的 sdata 端点。

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
	"xnc/server/internal/config"
)

func mustJSONMsg(m proto.Message) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

// TestExecRoutedViaRelay：池内有宣告 sdata 端点的 relay → exec 会话双腿
// URL 指向 relay（agent WsURL + 客户端 websocketUrl），且 SESSION_OPEN 如
// 此下发；MarkRelayRouted 后 Opening TTL 不收割（活跃靠 ActiveSids 续）。
func TestExecRoutedViaRelay(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pubHex := hex.EncodeToString(pub)
	env := newTestEnvWithCfg(t, func(c *config.Config) {
		c.RTVRelayAllowlist = []string{pubHex}
	})
	nodeID := env.EnrollNode(t, "WEB-RL1", "mid-rl1")
	ctrl := dialControl(t, env, nodeID)

	relayID := fakeRelayWithKey(t, env, priv, pubHex, []proto.EndpointDesc{
		{Transport: "sdata", Host: "relay.test", Port: 443},
	})
	require.NotEmpty(t, relayID)

	// 等 pool 消化注册（DB 落库 + conn 入表 + 心跳）。
	time.Sleep(300 * time.Millisecond)

	// 开 exec 会话。
	body := strings.NewReader(`{"command":"echo hi"}`)
	req, _ := http.NewRequest(http.MethodPost, env.srv.URL+"/api/nodes/"+nodeID+"/exec", body)
	req.Header.Set("Authorization", "Bearer "+env.AdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	var created struct {
		SessionID    string `json:"sessionId"`
		WebsocketURL string `json:"websocketUrl"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	assert.True(t, strings.HasPrefix(created.WebsocketURL, "wss://relay.test:443/api/session/"),
		"client leg must point at relay, got %s", created.WebsocketURL)
	assert.Contains(t, created.WebsocketURL, "?token=")

	// agent 侧 SESSION_OPEN 的 WsURL 同样指向 relay 数据腿。
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, data, err := ctrl.Read(rctx)
	rcancel()
	require.NoError(t, err)
	var openMsg proto.Message
	require.NoError(t, json.Unmarshal(data, &openMsg))
	require.Equal(t, proto.TypeSessionOpen, openMsg.Type)
	var so proto.SessionOpen
	require.NoError(t, openMsg.Decode(&so))
	assert.True(t, strings.HasPrefix(so.WsURL, "wss://relay.test:443/api/agent/session?token="),
		"agent leg must point at relay, got %s", so.WsURL)

	// relayRouted 会话不被 Opening TTL 收割（60s 太久——等 200ms 验证仍在表）。
	time.Sleep(200 * time.Millisecond)
	assert.NotEmpty(t, env.Sess.SessionsOf(mustUUID(nodeID), proto.KindExec))

	// 模拟 relay 终局上报：NotifyClose 路径清理。
	env.Sess.NotifyClose(created.SessionID, "peer-disconnect")
	require.Eventually(t, func() bool {
		return len(env.Sess.SessionsOf(mustUUID(nodeID), proto.KindExec)) == 0
	}, 3*time.Second, 50*time.Millisecond, "relay-routed session must close via NotifyClose")
}

// fakeRelayWithKey 与 fakeRelay 同，但身份密钥由调用方生成（allowlist 需
// 在 router 构造前注入公钥）。
func fakeRelayWithKey(t *testing.T, env *TestEnv, priv ed25519.PrivateKey, pubHex string,
	endpoints []proto.EndpointDesc) string {
	t.Helper()
	c, _, err := websocket.Dial(context.Background(), "ws"+env.srv.URL[4:]+"/api/relay/connect", nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.CloseNow() })
	c.SetReadLimit(64 * 1024)

	reg, err := proto.NewMsg(proto.TypeRelayRegister, proto.RelayRegister{
		PublicKey: pubHex, Endpoints: endpoints, ClockUnix: time.Now().Unix(),
	})
	require.NoError(t, err)
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	require.NoError(t, c.Write(wctx, websocket.MessageText, mustJSONMsg(reg)))
	wcancel()

	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, data, err := c.Read(rctx)
	rcancel()
	require.NoError(t, err)
	var msg proto.Message
	require.NoError(t, json.Unmarshal(data, &msg))
	require.Equal(t, proto.TypeChallenge, msg.Type)
	var ch proto.Challenge
	require.NoError(t, msg.Decode(&ch))

	cr, err := proto.NewMsg(proto.TypeRelayChallengeResponse, proto.RelayChallengeResponse{
		RelayID: ch.RelayID, Signature: ed25519.Sign(priv, []byte(ch.Nonce)),
	})
	require.NoError(t, err)
	wctx2, wcancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	require.NoError(t, c.Write(wctx2, websocket.MessageText, mustJSONMsg(cr)))
	wcancel2()

	hb, _ := proto.NewMsg(proto.TypeRelayHeartbeat, proto.RelayHeartbeat{ClockUnix: time.Now().Unix()})
	wctx3, wcancel3 := context.WithTimeout(context.Background(), 5*time.Second)
	require.NoError(t, c.Write(wctx3, websocket.MessageText, mustJSONMsg(hb)))
	wcancel3()
	return ch.RelayID
}
