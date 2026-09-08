// screen_test.go — RTV 重构后的 ScreenHandler 用例:流式与快照均已退役,
// 分别回稳定码 SCREEN_STREAM_RETIRED / SCREEN_SNAPSHOT_UNSUPPORTED。
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

// fakeSnapshot 记录请求参数并返回预设 JPEG / 错误。
type fakeSnapshot struct {
	jpeg    []byte
	err     error
	gotMaxW uint32
	calls   int
}

func (f *fakeSnapshot) Snapshot(maxWidth uint32) ([]byte, error) {
	f.calls++
	f.gotMaxW = maxWidth
	return f.jpeg, f.err
}

// runScreenSession 起 httptest WS server 并承载一个 screen 会话,返回
// 客户端侧连接(与服务端同参;快照 handler 自行收线)。
func runScreenSession(t *testing.T, snap SnapshotProvider, sessionID string, params any) *websocket.Conn {
	t.Helper()
	up := make(chan *websocket.Conn, 1)
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
			return
		}
		(&ScreenHandler{Snap: snap, Log: testLogger()}).Handle(context.Background(), c, sessionID, raw)
		c.CloseNow()
	}()
	select {
	case c := <-up:
		t.Cleanup(func() { c.CloseNow() })
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("no ws connection")
		return nil
	}
}

func readScreenText(t *testing.T, ws *websocket.Conn) proto.Message {
	t.Helper()
	kind, data := readFrame(t, ws)
	require.Equal(t, "text", kind)
	var msg proto.Message
	require.NoError(t, json.Unmarshal(data, &msg))
	return msg
}

// RTV 重构(2026-09-08):快照成功路径已退役——snapshot=true 一律回
// SCREEN_SNAPSHOT_UNSUPPORTED,不触发快照调用(恢复为后续 PATCH)。
func TestScreenSnapshotRetired(t *testing.T) {
	fake := &fakeSnapshot{jpeg: []byte{0xFF}}
	ws := runScreenSession(t, fake, "s1", proto.ScreenParams{Snapshot: true, MaxWidth: 1280})

	msg := readScreenText(t, ws)
	require.Equal(t, proto.TypeError, msg.Type)
	var ep proto.ErrorPayload
	require.NoError(t, json.Unmarshal(msg.Payload, &ep))
	assert.Equal(t, CodeSnapshotUnsupported, ep.Code)
	assert.Contains(t, ep.Message, "retired")
	assert.Zero(t, fake.calls)
}

// TestScreenStreamRetired 流式请求(无 snapshot)→ 稳定码
// SCREEN_STREAM_RETIRED;不触发快照调用。
func TestScreenStreamRetired(t *testing.T) {
	fake := &fakeSnapshot{jpeg: []byte{0xFF}}
	ws := runScreenSession(t, fake, "s1", proto.ScreenParams{})

	msg := readScreenText(t, ws)
	require.Equal(t, proto.TypeError, msg.Type)
	var ep proto.ErrorPayload
	require.NoError(t, json.Unmarshal(msg.Payload, &ep))
	assert.Equal(t, CodeScreenStreamRetired, ep.Code)
	assert.Contains(t, ep.Message, "desktop session")
	assert.Zero(t, fake.calls)
}
