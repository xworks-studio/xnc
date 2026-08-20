package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// dialWS 拨一条真实 WS 连到 url（测试侧持有）。
func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

// gluedSession 建立一条真实粘合会话：Create 后起本地 httptest WS 端点，
// agent/client 先后拨入完成 attach（后者触发 pump）。端点 handler 在 attach
// 后立即返回——与 api 层修复后的所有权语义一致（hijacked conn 交 manager/pump，
// handler 不挂起、不关连接）。onFinish 在粘合前装好（读超时类用例需要），
// 返回 (res, agent 侧, client 侧) 两条测试持有的连接。
func gluedSession(t *testing.T, onFinish func(reason string)) (*CreateResult, *websocket.Conn, *websocket.Conn) {
	t.Helper()
	nodeID, m := onlineMgr(t)
	res, apiErr := m.Create(nodeID, uuid.New(), proto.KindExec, []byte(`{}`))
	require.Nil(t, apiErr)
	if onFinish != nil {
		m.SetFinishFn(res.Session, onFinish)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		if strings.HasSuffix(r.URL.Path, "/agent") {
			_ = m.AttachAgent("", res.AgentToken, c)
		} else {
			_ = m.AttachClient(res.Session.ID, res.ClientToken, c)
		}
		// attach 后即返回：连接所有权归 manager/pump。
	}))
	t.Cleanup(srv.Close)

	agent := dialWS(t, "ws"+srv.URL[4:]+"/agent")
	client := dialWS(t, "ws"+srv.URL[4:]+"/client")
	return res, agent, client
}

// TestPumpReadTimeoutTearsDownSilentSide（机制验证）：pumpReadTimeout 注入
// 100ms 时，client 侧静默超过该时限 → relay 读错误 → NotifyClose 拆会话
// （双侧连接被 manager 关闭）。
func TestPumpReadTimeoutTearsDownSilentSide(t *testing.T) {
	orig := pumpReadTimeout
	pumpReadTimeout = 100 * time.Millisecond
	t.Cleanup(func() { pumpReadTimeout = orig })

	finished := make(chan string, 1)
	_, agent, _ := gluedSession(t, func(reason string) { finished <- reason })

	select {
	case reason := <-finished:
		assert.Equal(t, "peer-disconnect", reason)
	case <-time.After(5 * time.Second):
		t.Fatal("injected read timeout did not tear down the silent session")
	}

	// 双侧连接被 manager CloseNow：agent 侧读应得错误
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, _, err := agent.Read(ctx)
	assert.Error(t, err, "agent side conn must be closed after teardown")
}

// TestPumpDefaultReadSideSurvivesSilence（默认契约）：pumpReadTimeout 默认 0
// （读侧 background ctx）——client 静默 250ms 不得拆会话，agent 随后发的帧
// client 仍能收到。会话超时所有权在 agent，server 不因方向静默代拆。
func TestPumpDefaultReadSideSurvivesSilence(t *testing.T) {
	require.Zero(t, pumpReadTimeout, "default read timeout must be disabled (background ctx)")

	_, agent, client := gluedSession(t, nil)
	time.Sleep(250 * time.Millisecond) // client 侧全程静默

	wctx, wcancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer wcancel()
	require.NoError(t, agent.Write(wctx, websocket.MessageBinary, []byte("still-alive")))

	typ, data, err := client.Read(wctx)
	require.NoError(t, err, "session must survive client silence and relay agent frames")
	require.Equal(t, websocket.MessageBinary, typ)
	assert.Equal(t, "still-alive", string(data))
}

// TestPumpWriteTimeoutDoesNotBindReadSide（读写分离回归）：写侧超时缩短到
// 100ms 而读侧保持禁用时，client 静默 250ms 不得拆会话——写超时不得泄漏到
// 读方向（修复前读写共用同一 ctx，慢写时限会拆掉单向静默的健康会话）。
func TestPumpWriteTimeoutDoesNotBindReadSide(t *testing.T) {
	require.Zero(t, pumpReadTimeout)
	orig := pumpWriteTimeout
	pumpWriteTimeout = 100 * time.Millisecond
	t.Cleanup(func() { pumpWriteTimeout = orig })

	_, agent, client := gluedSession(t, nil)
	time.Sleep(250 * time.Millisecond) // 静默超过注入的写超时：不得拆会话

	wctx, wcancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer wcancel()
	require.NoError(t, agent.Write(wctx, websocket.MessageBinary, []byte("immune")))
	typ, data, err := client.Read(wctx)
	require.NoError(t, err, "write timeout must not tear down a silent-but-healthy direction")
	require.Equal(t, websocket.MessageBinary, typ)
	assert.Equal(t, "immune", string(data))
}
