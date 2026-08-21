// screen_test.go — ScreenStreamManager / FrameHub / ScreenHandler 用例：
// net.Pipe 模拟 helper 的 named pipe（跨平台），验证订阅即启动、SCREEN_BEGIN、
// 缓存 I 帧即时出画面、多观众广播、状态变化 text 帧、退订归零停管线、
// 启动失败回 no_session。
package session

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// screenMockPipe 模拟 helper：starter 注入 net.Pipe 客户端，测试经 write
// 推帧。
type screenMockPipe struct {
	t *testing.T
	c net.Conn // 服务端侧（测试持有）
}

func (p *screenMockPipe) write(typ byte, data []byte) {
	p.t.Helper()
	frame := make([]byte, 5, 5+len(data))
	frame[0] = typ
	binary.LittleEndian.PutUint32(frame[1:5], uint32(len(data)))
	frame = append(frame, data...)
	_, err := p.c.Write(frame)
	require.NoError(p.t, err)
}

// newMockScreenManager 构造注入 mock pipe 的管理器；starter 在首次
// Subscribe 时被调用（与真实 helper 生命周期一致）。
func newMockScreenManager(t *testing.T) (*ScreenStreamManager, *screenMockPipe) {
	t.Helper()
	mock := &screenMockPipe{t: t}
	m := newScreenStreamManager("xnc-agent.exe", testLogger())
	m.starter = func(*ScreenStreamManager) (net.Conn, *helperProc, error) {
		client, server := net.Pipe()
		t.Cleanup(func() { _ = server.Close() })
		mock.c = server
		return client, nil, nil
	}
	return m, mock
}

// runScreenSession 起 httptest WS server 并以注入管理器的 ScreenHandler
// 承载一个会话，返回客户端侧连接。
func runScreenSession(t *testing.T, m *ScreenStreamManager, sessionID string, params any) *websocket.Conn {
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
		(&ScreenHandler{Manager: m}).Handle(context.Background(), c, sessionID, raw)
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

// readScreenText 读一帧 text 并解出 proto.Message。
func readScreenText(t *testing.T, ws *websocket.Conn) proto.Message {
	t.Helper()
	kind, data := readFrame(t, ws)
	require.Equal(t, "text", kind)
	var msg proto.Message
	require.NoError(t, json.Unmarshal(data, &msg))
	return msg
}

// nalu 构造带 3B start code 的 NALU。
func nalu(hdr byte, payload ...byte) []byte {
	b := []byte{0, 0, 1, hdr}
	return append(b, payload...)
}

var (
	mockSPS = nalu(0x67, 0xAA, 0xBB)
	mockPPS = nalu(0x68, 0xCC)
	mockIDR = nalu(0x65, 0x01, 0x02, 0x03)
	mockP   = nalu(0x41, 0x09, 0x08)
)

// pipeDims 打包 0x04 分辨率 payload。
func pipeDims(w, h int) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b[0:4], uint32(w))
	binary.LittleEndian.PutUint32(b[4:8], uint32(h))
	return b
}

// TestScreenViewerReceivesBeginKeyAndDelta 一个观众：SCREEN_BEGIN → I 帧
// （缓存 SPS/PPS + IDR 原序）→ P 帧 binary。
func TestScreenViewerReceivesBeginKeyAndDelta(t *testing.T) {
	m, mock := newMockScreenManager(t)
	ws := runScreenSession(t, m, "s1", proto.ScreenParams{})

	time.Sleep(150 * time.Millisecond) // 等 handler 完成 Subscribe
	require.True(t, m.Running())

	mock.write(0x04, pipeDims(1920, 1080))
	mock.write(0x03, []byte("capturing"))
	iframe := append(append(append([]byte{}, mockSPS...), mockPPS...), mockIDR...)
	mock.write(0x01, iframe)
	mock.write(0x02, mockP)

	// SCREEN_BEGIN（可能在 0x03 帧之前或之后发出，state 二者皆合法）。
	msg := readScreenText(t, ws)
	require.Equal(t, typeScreenBegin, msg.Type)
	var begin proto.ScreenBegin
	require.NoError(t, json.Unmarshal(msg.Payload, &begin))
	assert.Equal(t, "h264", begin.Codec)
	assert.Contains(t, []string{"no_session", "capturing"}, begin.State)

	// 0x03 状态帧先于 I 帧到达 → SCREEN_STATE text。
	msg = readScreenText(t, ws)
	require.Equal(t, typeScreenState, msg.Type)
	var st proto.ScreenState
	require.NoError(t, json.Unmarshal(msg.Payload, &st))
	assert.Equal(t, "capturing", st.State)

	// I 帧 binary：SPS/PPS/IDR 原序。
	kind, data := readFrame(t, ws)
	require.Equal(t, "binary", kind)
	assert.Equal(t, iframe, data)

	// P 帧 binary。
	kind, data = readFrame(t, ws)
	require.Equal(t, "binary", kind)
	assert.Equal(t, mockP, data)

	w, h, state := m.State()
	assert.Equal(t, 1920, w)
	assert.Equal(t, 1080, h)
	assert.Equal(t, "capturing", state)
}

// TestScreenSecondViewerGetsCachedKeyFrame 第二个观众加入即收 SCREEN_BEGIN
// {capturing, 分辨率} + 缓存 SPS/PPS+I 帧（无需等下一个 I 帧）。
func TestScreenSecondViewerGetsCachedKeyFrame(t *testing.T) {
	m, mock := newMockScreenManager(t)
	ws1 := runScreenSession(t, m, "s1", proto.ScreenParams{})
	time.Sleep(150 * time.Millisecond)

	mock.write(0x04, pipeDims(1280, 720))
	mock.write(0x03, []byte("capturing"))
	iframe := append(append(append([]byte{}, mockSPS...), mockPPS...), mockIDR...)
	mock.write(0x01, iframe)
	readFrame(t, ws1) // drain：text begin
	readFrame(t, ws1) // drain：binary I 帧
	kind, data := readFrame(t, ws1)
	require.Equal(t, "binary", kind)
	_ = data

	ws2 := runScreenSession(t, m, "s2", proto.ScreenParams{})
	msg := readScreenText(t, ws2)
	require.Equal(t, typeScreenBegin, msg.Type)
	var begin proto.ScreenBegin
	require.NoError(t, json.Unmarshal(msg.Payload, &begin))
	assert.Equal(t, "capturing", begin.State)
	assert.Equal(t, 1280, begin.Width)
	assert.Equal(t, 720, begin.Height)

	kind, data = readFrame(t, ws2)
	require.Equal(t, "binary", kind)
	assert.Equal(t, iframe, data) // 缓存立即推送
}

// TestScreenBroadcastTwoViewers 两个观众收到相同帧。
func TestScreenBroadcastTwoViewers(t *testing.T) {
	m, mock := newMockScreenManager(t)
	ws1 := runScreenSession(t, m, "s1", proto.ScreenParams{})
	ws2 := runScreenSession(t, m, "s2", proto.ScreenParams{})
	time.Sleep(150 * time.Millisecond)

	mock.write(0x02, mockP)
	for _, ws := range []*websocket.Conn{ws1, ws2} {
		// 先消费各自的 SCREEN_BEGIN（text），再收广播的 P 帧 binary。
		msg := readScreenText(t, ws)
		require.Equal(t, typeScreenBegin, msg.Type)
		kind, data := readFrame(t, ws)
		require.Equal(t, "binary", kind)
		assert.Equal(t, mockP, data)
	}
}

// TestScreenStateChangeTextFrame 状态变化 → SCREEN_STATE text 帧。
func TestScreenStateChangeTextFrame(t *testing.T) {
	m, mock := newMockScreenManager(t)
	ws := runScreenSession(t, m, "s1", proto.ScreenParams{})
	time.Sleep(150 * time.Millisecond)

	mock.write(0x03, []byte("locked"))
	// 先到的可能是 SCREEN_BEGIN（若它晚于 0x03），跳过之。
	msg := readScreenText(t, ws)
	if msg.Type == typeScreenBegin {
		msg = readScreenText(t, ws)
	}
	require.Equal(t, typeScreenState, msg.Type)
	var st proto.ScreenState
	require.NoError(t, json.Unmarshal(msg.Payload, &st))
	assert.Equal(t, "locked", st.State)
}

// TestScreenUnsubscribeStopsPipeline 退订归零 → 管线停止（stopCh 关闭）。
func TestScreenUnsubscribeStopsPipeline(t *testing.T) {
	m, _ := newMockScreenManager(t)
	ch := m.Subscribe("solo", proto.ScreenParams{})
	require.True(t, m.Running())
	m.Unsubscribe("solo")
	require.False(t, m.Running())
	select {
	case <-m.stopCh:
	case <-time.After(time.Second):
		t.Fatal("stopCh not closed")
	}
	_, ok := <-ch
	assert.False(t, ok, "subscriber channel closed on stop")
}

// TestScreenResubscribeRestartsPipeline 停止后再次订阅能重新启动。
func TestScreenResubscribeRestartsPipeline(t *testing.T) {
	m, mock := newMockScreenManager(t)
	_ = m.Subscribe("a", proto.ScreenParams{})
	m.Unsubscribe("a")
	require.False(t, m.Running())

	_ = m.Subscribe("b", proto.ScreenParams{})
	require.True(t, m.Running())
	mock.write(0x03, []byte("capturing"))
	require.Eventually(t, func() bool {
		_, _, s := m.State()
		return s == "capturing"
	}, 2*time.Second, 20*time.Millisecond)
	m.Unsubscribe("b")
}

// TestScreenHandlerHelperStartFailed helper 启动失败 → SCREEN_BEGIN
// {no_session} 后会话即收线。
func TestScreenHandlerHelperStartFailed(t *testing.T) {
	m := newScreenStreamManager("xnc-agent.exe", testLogger())
	m.starter = func(*ScreenStreamManager) (net.Conn, *helperProc, error) {
		return nil, nil, errors.New("mock helper unavailable")
	}
	ws := runScreenSession(t, m, "s1", proto.ScreenParams{})

	msg := readScreenText(t, ws)
	require.Equal(t, typeScreenBegin, msg.Type)
	var begin proto.ScreenBegin
	require.NoError(t, json.Unmarshal(msg.Payload, &begin))
	assert.Equal(t, "no_session", begin.State)
	assert.False(t, m.Running())

	// 会话收线：后续读失败（服务端已返回）。
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, _, err := ws.Read(ctx)
	assert.Error(t, err)
}

// TestScreenSplitSPSPPS NALU 拆分：SPS/PPS 入缓存桶，其余保持原序。
func TestScreenSplitSPSPPS(t *testing.T) {
	iframe := append(append(append([]byte{}, mockSPS...), mockPPS...), mockIDR...)
	spspps, rest := splitSPSPPS(iframe)
	assert.Equal(t, append(append([]byte{}, mockSPS...), mockPPS...), spspps)
	assert.Equal(t, mockIDR, rest)

	// 无 start code：整体视为 I 帧（两桶皆空）。
	raw := []byte{0xAA, 0xBB, 0xCC}
	spspps, rest = splitSPSPPS(raw)
	assert.Empty(t, spspps)
	assert.Empty(t, rest)
}

// TestScreenPipeDeathStopsPipeline helper（pipe）死亡 → 管线自动停止。
func TestScreenPipeDeathStopsPipeline(t *testing.T) {
	m, mock := newMockScreenManager(t)
	_ = m.Subscribe("s1", proto.ScreenParams{})
	require.True(t, m.Running())
	_ = mock.c.Close() // 模拟 helper 退出
	require.Eventually(t, func() bool { return !m.Running() }, 2*time.Second, 20*time.Millisecond)
}

// TestScreenHelperPathFromExe helper 与 agent 同目录、固定名
// xnc-screen-helper(.exe)——与 bin/ 产物及 deploy 脚本的拷贝目标一致。
func TestScreenHelperPathFromExe(t *testing.T) {
	assert.Equal(t, "xnc-screen-helper", helperPathFromExe(""))
	dir := t.TempDir()
	exe := filepath.Join(dir, "xnc-agent.exe")
	name := "xnc-screen-helper"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	assert.Equal(t, filepath.Join(dir, name), helperPathFromExe(exe))
}

// TestScreenHelperArgs M1：首个订阅者的捕获参数转发为 helper 命令行（0 值
// 省略，helper 侧 flag 自带默认值）。
func TestScreenHelperArgs(t *testing.T) {
	assert.Equal(t, []string{"--pipe", "p"}, helperArgs("p", proto.ScreenParams{}))
	assert.Equal(t,
		[]string{"--pipe", "p", "--fps", "10", "--quality", "80", "--max-width", "1280"},
		helperArgs("p", proto.ScreenParams{Fps: 10, Quality: 80, MaxWidth: 1280}))
}
