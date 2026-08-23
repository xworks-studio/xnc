// screen_test.go — M2-Slice3 Task 3 换轨后的 ScreenHandler 用例:
// snapshot=true → fake 快照源 → SCREEN_BEGIN + 0x03 JPEG binary 后收线;
// 流式(无 snapshot)→ 稳定码 SCREEN_STREAM_RETIRED;快照失败 →
// SNAPSHOT_FAILED 且错误文本透传;maxWidth 缺省夹紧 1920。
package session

import (
	"context"
	"encoding/json"
	"errors"
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

// TestScreenSnapshotReturnsJPEG 快照成功:SCREEN_BEGIN{jpeg} → 0x03 前缀
// binary JPEG → 会话收线。
func TestScreenSnapshotReturnsJPEG(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3}
	fake := &fakeSnapshot{jpeg: jpeg}
	ws := runScreenSession(t, fake, "s1", proto.ScreenParams{Snapshot: true})

	msg := readScreenText(t, ws)
	require.Equal(t, typeScreenBegin, msg.Type)
	var begin proto.ScreenBegin
	require.NoError(t, json.Unmarshal(msg.Payload, &begin))
	assert.Equal(t, "jpeg", begin.Codec)
	assert.Equal(t, "capturing", begin.State)

	kind, data := readFrame(t, ws)
	require.Equal(t, "binary", kind)
	assert.Equal(t, append([]byte{proto.ScreenBinJPEG}, jpeg...), data)
	assert.Equal(t, 1, fake.calls)
}

// TestScreenSnapshotDefaultMaxWidth maxWidth 缺省(0)→ 夹紧 1920。
func TestScreenSnapshotDefaultMaxWidth(t *testing.T) {
	fake := &fakeSnapshot{jpeg: []byte{0xFF, 0xD8}}
	ws := runScreenSession(t, fake, "s1", proto.ScreenParams{Snapshot: true, MaxWidth: 0})
	readScreenText(t, ws)   // SCREEN_BEGIN
	_, _ = readFrame(t, ws) // binary
	assert.Equal(t, uint32(1920), fake.gotMaxW)
	_ = ws
}

// TestScreenSnapshotMaxWidthForwarded 显式 maxWidth 原样转发。
func TestScreenSnapshotMaxWidthForwarded(t *testing.T) {
	fake := &fakeSnapshot{jpeg: []byte{0xFF, 0xD8}}
	ws := runScreenSession(t, fake, "s1", proto.ScreenParams{Snapshot: true, MaxWidth: 1280})
	readScreenText(t, ws)
	_, _ = readFrame(t, ws)
	assert.Equal(t, uint32(1280), fake.gotMaxW)
	_ = ws
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

// TestScreenSnapshotFailed 快照失败 → SNAPSHOT_FAILED + 错误文本。
func TestScreenSnapshotFailed(t *testing.T) {
	fake := &fakeSnapshot{err: errors.New("coreclient: snapshot rejected: SPAWN_FAILED")}
	ws := runScreenSession(t, fake, "s1", proto.ScreenParams{Snapshot: true})

	msg := readScreenText(t, ws)
	require.Equal(t, proto.TypeError, msg.Type)
	var ep proto.ErrorPayload
	require.NoError(t, json.Unmarshal(msg.Payload, &ep))
	assert.Equal(t, CodeSnapshotFailed, ep.Code)
	assert.Contains(t, ep.Message, "SPAWN_FAILED")
}
