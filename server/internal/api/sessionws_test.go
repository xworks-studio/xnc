package api

import (
	"context"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
	"xnc/server/internal/session"
)

// sendSessionOpen 扮演 T4 exec handler 的职责：经控制连接的串行化发送器
// （NodeConn.Send）下发 SESSION_OPEN，WsURL 指向 agent 会话端点绝对地址。
// T3 尚无 REST 入口触发它，测试手工发送以驱动 manager 级粘合。
func sendSessionOpen(t *testing.T, env *TestEnv, nodeID string, res *session.CreateResult) {
	t.Helper()
	m, err := proto.NewMsg(proto.TypeSessionOpen, proto.SessionOpen{
		SessionID: res.Session.ID, Kind: proto.KindExec, Params: []byte(`{}`),
		AgentToken: res.AgentToken,
		WsURL:      "ws" + env.srv.URL[4:] + "/api/agent/session?token=" + res.AgentToken,
		ExpiresAt:  res.ExpiresAt,
	})
	require.NoError(t, err)
	conn := env.reg.Get(nodeID)
	require.NotNil(t, conn, "control conn must be registered")
	require.NoError(t, conn.Send(m))
}

// 直接驱动 manager（绕过 REST，REST 属 T4）：Create → 两侧拨号 → 帧透传。
func TestSessionGlueRelaysFrames(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-S1", "mid-s1")
	ctrl := dialControl(t, env, nodeID)

	res, apiErr := env.Sess.Create(mustUUID(nodeID), env.AdminUUID(t), proto.KindExec, []byte(`{}`))
	require.Nil(t, apiErr)

	// 手工下发 SESSION_OPEN（T4 exec handler 职责，见 sendSessionOpen 注释），
	// 随后 fakeAgentSession 在控制连接上收到它并拨 agent 会话 WS。
	go fakeAgentSession(t, ctrl, func(aws *websocket.Conn) {
		wsWriteBinary(t, aws, append([]byte{0x01}, []byte("hello-stdout")...))
		wsWriteText(t, aws, mustMsg(t, proto.ExecResult{TimedOut: false, DurationMs: 1}))
	})
	sendSessionOpen(t, env, nodeID, res)

	cl := dialClientSession(t, env.srv.URL, res.ClientPath, res.ClientToken)
	got := readBin(t, cl)
	assert.Equal(t, append([]byte{0x01}, []byte("hello-stdout")...), got)
}

// TestSessionClientDisconnectCloses：client 关闭 → agent 侧读错误 → NotifyClose →
// 控制连接收到 SESSION_CLOSE{reason:"peer-disconnect"}。
func TestSessionClientDisconnectCloses(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-S2", "mid-s2")
	ctrl := dialControl(t, env, nodeID)

	res, apiErr := env.Sess.Create(mustUUID(nodeID), env.AdminUUID(t), proto.KindExec, []byte(`{}`))
	require.Nil(t, apiErr)
	// 会话关闭通知通路（T4 exec handler 的接线职责，此处手工补上）：
	// NotifyClose → SESSION_CLOSE 经控制连接串行化发送器下发。
	env.Sess.SetNotifyFn(res.Session, func(sc proto.SessionClose) error {
		m, err := proto.NewMsg(proto.TypeSessionClose, sc)
		require.NoError(t, err)
		return env.reg.Get(nodeID).Send(m)
	})

	go fakeAgentSession(t, ctrl, func(aws *websocket.Conn) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		_, _, err := aws.Read(ctx) // 等 client 断开经 pump 传导为读错误
		require.Error(t, err)
	})
	sendSessionOpen(t, env, nodeID, res)

	cl := dialClientSession(t, env.srv.URL, res.ClientPath, res.ClientToken)
	_ = cl.CloseNow()

	// 控制连接上应收到 SESSION_CLOSE
	m := readMsg(t, ctrl)
	require.Equal(t, proto.TypeSessionClose, m.Type)
	var sc proto.SessionClose
	require.NoError(t, m.Decode(&sc))
	assert.Equal(t, res.Session.ID, sc.SessionID)
	assert.NotEmpty(t, sc.Reason)
}

// TestSessionRefusedNotifiesClose：agent 不支持 kind → SESSION_REFUSED 消费 →
// NotifyClose 关会话（后续 attach → 404），且控制连接心跳循环不被打断。
func TestSessionRefusedNotifiesClose(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-SR", "mid-sr")
	ctrl := dialControl(t, env, nodeID)

	res, apiErr := env.Sess.Create(mustUUID(nodeID), env.AdminUUID(t), proto.KindExec, []byte(`{}`))
	require.Nil(t, apiErr)

	refused, err := proto.NewMsg(proto.TypeSessionRefused, proto.SessionRefused{
		SessionID: res.Session.ID, Code: proto.CodeKindUnsupported, Message: "kind unsupported",
	})
	require.NoError(t, err)
	writeMsg(t, ctrl, refused)

	// 会话被消费性关闭：后续 attach 得 404（而非 410/401）
	require.Eventually(t, func() bool {
		e := env.Sess.AttachClient(res.Session.ID, res.ClientToken, nil)
		return e != nil && e.Status == 404
	}, 5*time.Second, 100*time.Millisecond)

	// 控制连接心跳循环不受影响：HEARTBEAT 仍得 ACK（经串行化发送器）
	hb, err := proto.NewMsg(proto.TypeHeartbeat, struct{}{})
	require.NoError(t, err)
	writeMsg(t, ctrl, hb)
	ack := readMsg(t, ctrl)
	assert.Equal(t, proto.TypeHeartbeatAck, ack.Type)
}

// wsBaseURL：X-Forwarded-Proto（caddy 注入）> r.TLS > 默认 ws。
func TestWsBaseURL(t *testing.T) {
	r := httptest.NewRequest("GET", "http://xnc.local/api/session/x", nil)
	assert.Equal(t, "ws://xnc.local", wsBaseURL(r))

	r = httptest.NewRequest("GET", "https://xnc.local/api/session/x", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	assert.Equal(t, "wss://xnc.local", wsBaseURL(r))

	r = httptest.NewRequest("GET", "http://xnc.local/api/session/x", nil)
	r.Header.Set("X-Forwarded-Proto", "http") // 非 https 不升级
	assert.Equal(t, "ws://xnc.local", wsBaseURL(r))

	// https target 即原生 TLS（NewRequest 会置非 nil 的 dummy TLS）
	r = httptest.NewRequest("GET", "https://xnc.local/api/session/x", nil)
	require.NotNil(t, r.TLS)
	assert.Equal(t, "wss://xnc.local", wsBaseURL(r))
}

// readSessionOpen 读控制连接直到 SESSION_OPEN（跳过测试自身心跳的 ACK）。
// 用于 goroutine 泄漏回归的循环驱动。
func readSessionOpen(t *testing.T, ctrl *websocket.Conn) proto.SessionOpen {
	t.Helper()
	for {
		m := readMsg(t, ctrl)
		if m.Type == proto.TypeHeartbeatAck {
			continue
		}
		require.Equal(t, proto.TypeSessionOpen, m.Type)
		var so proto.SessionOpen
		require.NoError(t, m.Decode(&so))
		return so
	}
}

// TestSessionHandlersDoNotLeakGoroutines（回归：sessionWS handler 泄漏）：
// websocket.Accept hijack 连接后 net/http 永不 cancel 请求 ctx，旧实现的
// <-r.Context().Done() 挂起永不返回——每个会话泄漏 2 个永久驻留的 handler
// goroutine。修复后 attach 成功即 return。N=30 个完整会话（创建→agent
// attach→client attach→帧到达→双侧关闭）走真实端点，全部拆解后
// runtime.NumGoroutine() 必须回到基线（±10 容差吸收运行时噪声）。
func TestSessionHandlersDoNotLeakGoroutines(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-GL", "mid-goroutine-leak")
	ctrl := dialControl(t, env, nodeID)

	// 基线在环境/控制连接 goroutine 全部就位后采样
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	baseline := runtime.NumGoroutine()

	const n = 30
	frame := append([]byte{0x01}, []byte("leak-check")...)
	for i := 0; i < n; i++ {
		// 心跳防 ctrl 读超时（HeartbeatTimeout 5s）判节点 offline；
		// 其 ACK 由下一轮 readSessionOpen 跳过。
		if i > 0 {
			hb, err := proto.NewMsg(proto.TypeHeartbeat, struct{}{})
			require.NoError(t, err)
			writeMsg(t, ctrl, hb)
		}

		res, apiErr := env.Sess.Create(mustUUID(nodeID), env.AdminUUID(t), proto.KindExec, []byte(`{}`))
		require.Nil(t, apiErr, "iteration %d", i)

		agentDone := make(chan struct{})
		go func() {
			defer close(agentDone)
			so := readSessionOpen(t, ctrl)
			dctx, dcancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer dcancel()
			aws, _, err := websocket.Dial(dctx, so.WsURL, nil)
			if err != nil {
				t.Errorf("iter %d: agent dial: %v", i, err)
				return
			}
			defer func() { _ = aws.CloseNow() }()

			wctx, wcancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer wcancel()
			if err := aws.Write(wctx, websocket.MessageBinary, frame); err != nil {
				t.Errorf("iter %d: agent write: %v", i, err)
				return
			}
			// 等 client 断开经 pump 传导为读错误
			_, _, err = aws.Read(wctx)
			if err == nil {
				t.Errorf("iter %d: expected agent read error after client close", i)
			}
		}()
		sendSessionOpen(t, env, nodeID, res)

		cl := dialClientSession(t, env.srv.URL, res.ClientPath, res.ClientToken)
		require.Equal(t, frame, readBin(t, cl), "iteration %d", i)
		_ = cl.CloseNow()

		select {
		case <-agentDone:
		case <-time.After(10 * time.Second):
			t.Fatalf("iteration %d: agent side never observed teardown", i)
		}
	}

	// 全部会话拆解后，驻留 goroutine 应回到基线（修复前每会话泄漏 2 个
	// 挂起的 sessionWS handler，30 轮即 +60）。
	require.Eventually(t, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= baseline+10
	}, 15*time.Second, 200*time.Millisecond,
		"goroutines leaked: baseline %d, now %d", baseline, runtime.NumGoroutine())
}
