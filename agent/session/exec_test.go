package session

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	ws, _ := runExecIn(t, "sess-test", "", params)
	return ws
}

// runExecIn 为 runExec 的注入变体：自定义 sessionID 与脚本临时目录
// （空 = os.TempDir）。除服务侧连接外，还返回 Handle 返回（其 defer 清理
// 已执行完毕）时关闭的 done 信号——脚本生命周期用例据此在断言目录前等待
// 临时文件删除落定：EXEC_RESULT 帧先于 Handle 返回到达。
func runExecIn(t *testing.T, sessionID, tmpDir string, params proto.ExecParams) (*websocket.Conn, <-chan struct{}) {
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
		(&Exec{Log: testLogger(), TmpDir: tmpDir}).Handle(context.Background(), c, sessionID, raw)
	}()
	select {
	case c := <-up:
		// LIFO：先于 srv.Close 关闭服务侧连接，解除 Exec 关闭握手与
		// srv.Close 等待 handler 退出之间的互等。
		t.Cleanup(func() { c.CloseNow() })
		return c, done
	case err := <-errCh:
		t.Fatalf("exec dial/handle failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no ws connection")
	}
	return nil, done
}

// waitHandleDone 等 Exec.Handle 完全返回（defer 的临时文件删除已执行）。
// 先 CloseNow 服务侧连接，令 agent 侧的关闭握手立即终止而非等超时。
func waitHandleDone(t *testing.T, ws *websocket.Conn, done <-chan struct{}) {
	t.Helper()
	ws.CloseNow()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("exec Handle did not return")
	}
}

// assertDirEmpty 断言目录下无任何残留（临时脚本已删/未落盘）。
func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
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

func TestExecScriptLifecycle(t *testing.T) {
	dir := t.TempDir()
	script := "Write-Output from-script\nexit 3"
	if !isWindows() {
		script = "echo from-script\nexit 3"
	}
	ws, done := runExecIn(t, "sess-script", dir, proto.ExecParams{Script: script, TimeoutSec: 30})
	stdout, _, res := collectExec(t, ws)
	assert.Contains(t, stdout, "from-script")
	require.NotNil(t, res.ExitCode)
	assert.Equal(t, 3, *res.ExitCode)

	// 成功路径：临时脚本已删除（等 Handle 返回，defer 删除落定）。
	waitHandleDone(t, ws, done)
	assertDirEmpty(t, dir)
}

func TestExecScriptCleanupOnFailure(t *testing.T) {
	dir := t.TempDir()
	ws, done := runExecIn(t, "sess-fail", dir, proto.ExecParams{Script: "exit 9", TimeoutSec: 30})
	_, _, res := collectExec(t, ws)
	require.NotNil(t, res.ExitCode)
	require.Equal(t, 9, *res.ExitCode)

	waitHandleDone(t, ws, done)
	assertDirEmpty(t, dir) // temp script must be removed on failure path too
}

func TestExecOversizeScriptRefused(t *testing.T) {
	dir := t.TempDir()
	huge := strings.Repeat("a", 1024*1024+1)
	ws, _ := runExecIn(t, "sess-huge", dir, proto.ExecParams{Script: huge, TimeoutSec: 5})

	kind, data := readFrame(t, ws)
	require.Equal(t, "text", kind)
	var m proto.Message
	require.NoError(t, json.Unmarshal(data, &m))
	require.Equal(t, "EXEC_RESULT", m.Type)
	var r proto.ExecResult
	require.NoError(t, m.Decode(&r))
	assert.Nil(t, r.ExitCode)

	assertDirEmpty(t, dir) // no temp file written for oversize script
}

// TestExecHostileSessionIDRefused 路径穿越形态的 sessionID（T6 评审加固）：
// 整会话拒绝、null exitCode，且不在 TmpDir（乃至其逃逸目标）落任何文件。
func TestExecHostileSessionIDRefused(t *testing.T) {
	dir := t.TempDir()
	script := "Write-Output pwned"
	if !isWindows() {
		script = "echo pwned"
	}
	ws, _ := runExecIn(t, "../../evil", dir, proto.ExecParams{Script: script, TimeoutSec: 5})

	kind, data := readFrame(t, ws)
	require.Equal(t, "text", kind)
	var m proto.Message
	require.NoError(t, json.Unmarshal(data, &m))
	require.Equal(t, "EXEC_RESULT", m.Type)
	var r proto.ExecResult
	require.NoError(t, m.Decode(&r))
	assert.Nil(t, r.ExitCode)

	assertDirEmpty(t, dir)
	// 无守卫时 filepath.Join 会把 "xnc-../../evil.ps1" 清洗到 dir 之上——
	// 逃逸目标同样不得出现文件。
	_, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.ps1"))
	assert.True(t, os.IsNotExist(err), "script must not escape TmpDir via traversal")
}
