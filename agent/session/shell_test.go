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

// runShell 起 httptest WS 让 Shell 作为客户端拨入；返回服务侧连接与 Handle
// 返回（清理已落定）时关闭的 done 信号。拨号错误经 errCh 回报（goroutine
// 内不 require）。
func runShell(t *testing.T, host ShellHost, params proto.ShellParams) (*websocket.Conn, <-chan struct{}) {
	t.Helper()
	up := make(chan *websocket.Conn, 1)
	errCh := make(chan error, 1)
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
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, _, err := websocket.Dial(context.Background(), "ws"+srv.URL[4:], nil)
		if err != nil {
			errCh <- err
			return
		}
		(&Shell{Log: testLogger(), Host: host}).Handle(context.Background(), c, "sess-shell", raw)
	}()
	select {
	case c := <-up:
		t.Cleanup(func() { c.CloseNow() })
		return c, done
	case err := <-errCh:
		t.Fatalf("shell dial failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no ws")
	}
	return nil, done
}

// collectUntil 从 ws 读帧直到 match 返回 true（跳过不匹配的帧），超时 fail。
func collectUntil(t *testing.T, ws *websocket.Conn, match func(kind string, data []byte) bool, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			t.Fatal("collectUntil: timeout waiting for match")
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		typ, data, err := ws.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		kind := "binary"
		if typ == websocket.MessageText {
			kind = "text"
		}
		if match(kind, data) {
			return data
		}
	}
}

// writeBin 发送 binary 帧（client 键入 / pty 输入字节）。
func writeBin(t *testing.T, ws *websocket.Conn, b []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, ws.Write(ctx, websocket.MessageBinary, b))
}

// writeText 发送 text 帧（控制消息，如 SHELL_RESIZE）。
func writeText(t *testing.T, ws *websocket.Conn, b []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, ws.Write(ctx, websocket.MessageText, b))
}

// mustShellMsg 构造 kind 私有 text 帧原文（proto.NewMsg → marshal）。
func mustShellMsg(t *testing.T, typ string, payload any) []byte {
	t.Helper()
	m, err := proto.NewMsg(typ, payload)
	require.NoError(t, err)
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

// TestShellBeginEchoResizeKill:经 fake host 全链:SHELL_BEGIN(实际
// profile)、pty 双向、SHELL_RESIZE 透传(Resize 字节)、断开 → Kill。
func TestShellBeginEchoResizeKill(t *testing.T) {
	host := &fakeHost{build: func(spec ShellSpec) *fakeProc {
		p := &fakeProc{spec: spec, profile: "PWSH",
			streamCh: make(chan ShellStream, 16), exitCh: make(chan uint32, 1)}
		p.onKill = func(fp *fakeProc) {
			select {
			case fp.exitCh <- 1:
			default:
			}
		}
		return p
	}}
	ws, done := runShell(t, host, proto.ShellParams{Cols: 80, Rows: 25, Shell: "pwsh", System: true})

	// 1) SHELL_BEGIN 先行,报告实际 profile
	begin := collectUntil(t, ws, func(k string, d []byte) bool {
		if k != "text" {
			return false
		}
		var m proto.Message
		return json.Unmarshal(d, &m) == nil && m.Type == "SHELL_BEGIN"
	}, 10*time.Second)
	var sb proto.ShellBegin
	var m proto.Message
	require.NoError(t, json.Unmarshal(begin, &m))
	require.NoError(t, m.Decode(&sb))
	assert.Equal(t, "PWSH", sb.Shell)

	// 2) 键入 → stdin 泵(fake host 记录)
	writeBin(t, ws, []byte("echo sh-echo\r"))
	proc := host.lastProc()
	require.NotNil(t, proc)
	require.Eventually(t, func() bool { return len(proc.Stdin()) > 0 }, 5*time.Second, 50*time.Millisecond)
	assert.Equal(t, "echo sh-echo\r", string(proc.Stdin()))

	// 3) resize 字节透传
	writeText(t, ws, mustShellMsg(t, "SHELL_RESIZE", proto.ShellResize{Cols: 100, Rows: 40}))
	require.Eventually(t, func() bool {
		rs := proc.Resizes()
		return len(rs) == 1 && rs[0] == [2]int{100, 40}
	}, 5*time.Second, 50*time.Millisecond)

	// 4) pty 输出 → ws binary
	proc.emit(ShellStream{Bytes: []byte("sh-out-payload")})
	collectUntil(t, ws, func(k string, d []byte) bool {
		return k == "binary" && string(d) == "sh-out-payload"
	}, 10*time.Second)

	// 5) spec 透传:interactive + cols/rows + system
	specs := host.specs()
	require.Len(t, specs, 1)
	assert.True(t, specs[0].Interactive)
	assert.Equal(t, 80, specs[0].Cols)
	assert.Equal(t, 25, specs[0].Rows)
	assert.Equal(t, "PWSH", specs[0].Profile)
	assert.True(t, specs[0].System)

	// 6) 对端断开 → Kill(杀树)后 Handle 返回
	_ = ws.CloseNow()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Handle did not return after disconnect")
	}
	assert.GreaterOrEqual(t, proc.Killed(), 1, "disconnect must kill the shell tree")
}

// TestShellExitEndsSession:shell 自然退出(exit)→ Handle 返回。
func TestShellExitEndsSession(t *testing.T) {
	host := &fakeHost{}
	ws, done := runShell(t, host, proto.ShellParams{Cols: 80, Rows: 25})
	collectUntil(t, ws, func(k string, d []byte) bool {
		return k == "text" && json.Unmarshal(d, &proto.Message{}) == nil
	}, 10*time.Second)
	host.lastProc().exitCh <- 0
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Handle did not return after shell exit")
	}
}

// TestShellStartRejected:创建拒绝(NO_ACTIVE_SESSION)→ ERROR
// SHELL_START_FAILED,message 携带稳定码。
func TestShellStartRejected(t *testing.T) {
	host := &fakeHost{reject: &ShellHostError{Code: CodeNoActiveSession}}
	ws, _ := runShell(t, host, proto.ShellParams{Cols: 80, Rows: 25})
	errFrame := collectUntil(t, ws, func(k string, d []byte) bool {
		if k != "text" {
			return false
		}
		var m proto.Message
		return json.Unmarshal(d, &m) == nil && m.Type == proto.TypeError
	}, 10*time.Second)
	var m proto.Message
	require.NoError(t, json.Unmarshal(errFrame, &m))
	var ep proto.ErrorPayload
	require.NoError(t, m.Decode(&ep))
	assert.Equal(t, proto.CodeShellStartFailed, ep.Code)
	assert.Equal(t, "NO_ACTIVE_SESSION", ep.Message)
}

// TestShellCoreUnavailable:无 host 凭据 → ERROR,code CORE_UNAVAILABLE。
func TestShellCoreUnavailable(t *testing.T) {
	ws, _ := runShell(t, nil, proto.ShellParams{Cols: 80, Rows: 25})
	errFrame := collectUntil(t, ws, func(k string, d []byte) bool {
		if k != "text" {
			return false
		}
		var m proto.Message
		return json.Unmarshal(d, &m) == nil && m.Type == proto.TypeError
	}, 10*time.Second)
	var m proto.Message
	require.NoError(t, json.Unmarshal(errFrame, &m))
	var ep proto.ErrorPayload
	require.NoError(t, m.Decode(&ep))
	assert.Equal(t, "CORE_UNAVAILABLE", ep.Message)
}
