package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// blockingHandler 阻塞在自身 ctx.Done 上，用于验证引擎的 ctx 所有权与会话表。
type blockingHandler struct {
	started chan struct{}
	done    chan struct{}
}

func (h *blockingHandler) Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage) {
	close(h.started)
	defer close(h.done)
	<-ctx.Done()
}

func TestEngineRefusesUnknownKind(t *testing.T) {
	refused := make(chan proto.Message, 1)
	e := NewEngine(testLogger(), func(m proto.Message) error {
		refused <- m
		return nil
	})
	e.HandleSessionOpen(context.Background(), proto.SessionOpen{
		SessionID: "s-refuse",
		Kind:      "nope",
		WsURL:     "ws://127.0.0.1:1/nowhere", // 不应被拨号
	})
	select {
	case m := <-refused:
		require.Equal(t, proto.TypeSessionRefused, m.Type)
		var r proto.SessionRefused
		require.NoError(t, m.Decode(&r))
		assert.Equal(t, "s-refuse", r.SessionID)
		assert.Equal(t, proto.CodeKindUnsupported, r.Code)
	case <-time.After(3 * time.Second):
		t.Fatal("no SESSION_REFUSED sent")
	}
}

func TestEngineSessionCloseCancelsHandler(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		<-r.Context().Done()
		c.CloseNow()
	}))
	t.Cleanup(srv.Close)

	// 分发器（connect.Client.drain）的 ctx 随连接死亡而取消——引擎必须
	// 忽略它、以自持的 Background 派生 ctx 驱动会话。
	dispatchCtx, dispatchCancel := context.WithCancel(context.Background())
	defer dispatchCancel()

	h := &blockingHandler{started: make(chan struct{}), done: make(chan struct{})}
	e := NewEngine(testLogger(), func(proto.Message) error { return nil })
	e.Register(proto.KindExec, h)

	e.HandleSessionOpen(dispatchCtx, proto.SessionOpen{
		SessionID: "s-close",
		Kind:      proto.KindExec,
		WsURL:     "ws" + srv.URL[4:],
	})
	<-h.started

	dispatchCancel() // 模拟控制连接死亡
	select {
	case <-h.done:
		t.Fatal("handler cancelled by dispatcher ctx; engine must own session ctx")
	case <-time.After(300 * time.Millisecond):
		// 分发 ctx 死亡后会话仍存活——符合预期
	}

	e.HandleSessionClose(dispatchCtx, proto.SessionClose{SessionID: "s-close", Reason: "test"})
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("SESSION_CLOSE did not cancel session handler")
	}
}
