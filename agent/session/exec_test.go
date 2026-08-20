package session

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func isWindows() bool { return runtime.GOOS == "windows" }

// runExec 起 httptest WS 服务端并让 Exec 作为客户端拨入；返回服务侧连接。
// 注意：dial 错误经 errCh 回报（goroutine 内禁用 require）。
func runExec(t *testing.T, params proto.ExecParams) *websocket.Conn {
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
	go func() {
		c, _, err := websocket.Dial(context.Background(), "ws"+srv.URL[4:], nil)
		if err != nil {
			errCh <- err
			return
		}
		NewExec(testLogger()).Handle(context.Background(), c, "sess-test", raw)
		errCh <- nil
	}()
	select {
	case c := <-up:
		// LIFO：先于 srv.Close 关闭服务侧连接，解除 Exec 关闭握手与
		// srv.Close 等待 handler 退出之间的互等。
		t.Cleanup(func() { c.CloseNow() })
		return c
	case err := <-errCh:
		t.Fatalf("exec dial/handle failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no ws connection")
	}
	return nil
}

func readFrame(t *testing.T, ws *websocket.Conn) (string, []byte) {
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

// collectExec 消费会话帧直至 EXEC_RESULT：binary 帧按前缀归入 stdout/stderr，
// text 帧解码终态。windows 与非 windows 用例共用以消除断言重复。
func collectExec(t *testing.T, ws *websocket.Conn) (string, string, *proto.ExecResult) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	var res *proto.ExecResult
	for res == nil {
		kind, data := readFrame(t, ws)
		switch kind {
		case "binary":
			if data[0] == 0x01 {
				stdout.Write(data[1:])
			} else {
				stderr.Write(data[1:])
			}
		case "text":
			var m proto.Message
			require.NoError(t, json.Unmarshal(data, &m))
			require.Equal(t, "EXEC_RESULT", m.Type)
			var r proto.ExecResult
			require.NoError(t, m.Decode(&r))
			res = &r
		}
	}
	return stdout.String(), stderr.String(), res
}

func TestExecCommandStreamsAndExits(t *testing.T) {
	cmd := `Write-Output hello; Write-Error boom; exit 7`
	if !isWindows() {
		cmd = `echo hello; echo boom 1>&2; exit 7`
	}
	ws := runExec(t, proto.ExecParams{Command: cmd, TimeoutSec: 30})
	stdout, stderr, res := collectExec(t, ws)
	assert.Contains(t, stdout, "hello")
	assert.Contains(t, stderr, "boom")
	require.NotNil(t, res.ExitCode)
	assert.Equal(t, 7, *res.ExitCode)
	assert.False(t, res.TimedOut)
	assert.True(t, res.DurationMs >= 0)
}

func TestExecTimeoutKillsAndReports(t *testing.T) {
	cmd := `Start-Sleep 60`
	if !isWindows() {
		cmd = `sleep 60`
	}
	start := time.Now()
	ws := runExec(t, proto.ExecParams{Command: cmd, TimeoutSec: 1})
	_, _, res := collectExec(t, ws)
	assert.True(t, res.TimedOut)
	assert.Nil(t, res.ExitCode)
	assert.Less(t, time.Since(start), 15*time.Second) // 秒级杀掉，非等满 60s
}
