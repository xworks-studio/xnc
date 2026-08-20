package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// fakeAgentSession 在 control WS 上等 SESSION_OPEN，按其中 WsURL 拨 agent 会话 WS，
// 随后执行 act 与 client 侧交互；返回收到的 SessionOpen。
// （readMsg/writeMsg 复用 agentws_test.go 的同包助手。）
func fakeAgentSession(t *testing.T, ctrlWS *websocket.Conn, act func(ws *websocket.Conn)) proto.SessionOpen {
	t.Helper()
	m := readMsg(t, ctrlWS)
	require.Equal(t, proto.TypeSessionOpen, m.Type)
	var so proto.SessionOpen
	require.NoError(t, m.Decode(&so))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, so.WsURL, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ws.CloseNow() })
	if act != nil {
		act(ws)
	}
	return so
}

// dialClientSession 以 CreateResult 的 token 拨 client 侧会话 WS。
func dialClientSession(t *testing.T, base, path, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+base[4:]+path+"?token="+token, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ws.CloseNow() })
	return ws
}

// wsWriteText / wsWriteBinary：带 5s 超时的写助手。
func wsWriteText(t *testing.T, ws *websocket.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, ws.Write(ctx, websocket.MessageText, b))
}

func wsWriteBinary(t *testing.T, ws *websocket.Conn, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, ws.Write(ctx, websocket.MessageBinary, data))
}

// mustMsg 构造 EXEC_RESULT 形态的会话 text 帧消息（server 不解析会话帧，
// "EXEC_RESULT" 字面量属 kind 私有词汇，仅允许出现在测试与 agent 实现中）。
func mustMsg(t *testing.T, v any) proto.Message {
	t.Helper()
	m, err := proto.NewMsg("EXEC_RESULT", v)
	require.NoError(t, err)
	return m
}

// readBin 读一帧并断言为 binary（会话 stdout 通道形态）。
func readBin(t *testing.T, ws *websocket.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	typ, data, err := ws.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, websocket.MessageBinary, typ)
	return data
}

// captureSessionOpen 在控制连接上后台读取一条 SESSION_OPEN，断言类型并解码后
// 送入 channel（captureOnly 变体：不拨 agent 会话 WS，测试只关心下发的 params）。
// goroutine 内用 Errorf 而非 require——FailNow 只能在测试主 goroutine 调用。
func captureSessionOpen(t *testing.T, ctrlWS *websocket.Conn) chan proto.SessionOpen {
	t.Helper()
	ch := make(chan proto.SessionOpen, 1)
	go func() {
		m := readMsg(t, ctrlWS)
		if m.Type != proto.TypeSessionOpen {
			t.Errorf("expected %s on control conn, got %s", proto.TypeSessionOpen, m.Type)
			return
		}
		var so proto.SessionOpen
		if err := m.Decode(&so); err != nil {
			t.Errorf("decode %s: %v", proto.TypeSessionOpen, err)
			return
		}
		ch <- so
	}()
	return ch
}

// mustUUID 解析节点 UUID（EnrollNode 返回字符串形态）。
func mustUUID(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		panic(err)
	}
	return id
}

// readRaw 读一帧返回 ("text"|"binary", data)，不断言帧型（exec 终态帧是 text）。
func readRaw(t *testing.T, ws *websocket.Conn) (string, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	typ, data, err := ws.Read(ctx)
	require.NoError(t, err)
	if typ == websocket.MessageText {
		return "text", data
	}
	return "binary", data
}

// jsonUnmarshal 是 json.Unmarshal 的别名（测试正文用它保持简洁）。
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
