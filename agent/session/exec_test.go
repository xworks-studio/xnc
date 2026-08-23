package session

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, nil))
}

// runExec 起 httptest WS 服务端并让 Exec 作为客户端拨入；返回服务侧连接。
// 注意：dial 错误经 errCh 回报（goroutine 内禁用 require）。
func runExec(t *testing.T, host ShellHost, params proto.ExecParams) *websocket.Conn {
	t.Helper()
	ws, _ := runExecIn(t, host, "sess-test", "", params)
	return ws
}

// runExecIn 为 runExec 的注入变体：自定义 sessionID 与脚本临时目录
// （空 = os.TempDir）。除服务侧连接外，还返回 Handle 返回（其 defer 清理
// 已执行完毕）时关闭的 done 信号——脚本生命周期用例据此在断言目录前等待
// 临时文件删除落定：EXEC_RESULT 帧先于 Handle 返回到达。
func runExecIn(t *testing.T, host ShellHost, sessionID, tmpDir string, params proto.ExecParams) (*websocket.Conn, <-chan struct{}) {
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
		(&Exec{Log: testLogger(), Host: host, TmpDir: tmpDir}).Handle(context.Background(), c, sessionID, raw)
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
// text 帧解码终态。
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

// TestExecCommandStreamsAndExits:fake host 服务 canned stdout/stderr +
// exit 7;spec 透传 command/profile/system。
func TestExecCommandStreamsAndExits(t *testing.T) {
	host := &fakeHost{build: func(spec ShellSpec) *fakeProc {
		p := &fakeProc{spec: spec, profile: spec.Profile,
			streamCh: make(chan ShellStream, 16), exitCh: make(chan uint32, 1)}
		go func() {
			p.emit(ShellStream{Bytes: []byte("hello")})
			p.emit(ShellStream{Stderr: true, Bytes: []byte("boom")})
			p.exitCh <- 7
			close(p.streamCh)
		}()
		return p
	}}
	ws := runExec(t, host, proto.ExecParams{Command: "whoami", TimeoutSec: 30, Shell: "cmd", System: true})
	stdout, stderr, res := collectExec(t, ws)
	assert.Contains(t, stdout, "hello")
	assert.Contains(t, stderr, "boom")
	require.NotNil(t, res.ExitCode)
	assert.Equal(t, 7, *res.ExitCode)
	assert.False(t, res.TimedOut)
	assert.Empty(t, res.Code)

	specs := host.specs()
	require.Len(t, specs, 1)
	assert.Equal(t, "whoami", specs[0].Command)
	assert.Equal(t, "CMD", specs[0].Profile)
	assert.True(t, specs[0].System, "system flag must reach the shell host")
	assert.False(t, specs[0].Interactive)
	assert.EqualValues(t, 30, specs[0].TimeoutSec)
}

// TestExecDefaultUserToken:System 缺省 false(不隐式提权,spec §8.4)。
func TestExecDefaultUserToken(t *testing.T) {
	host := &fakeHost{build: func(spec ShellSpec) *fakeProc {
		p := &fakeProc{spec: spec, profile: spec.Profile,
			streamCh: make(chan ShellStream, 16), exitCh: make(chan uint32, 1)}
		p.exitCh <- 0
		close(p.streamCh)
		return p
	}}
	ws := runExec(t, host, proto.ExecParams{Command: "whoami", Shell: "powershell"})
	_, _, res := collectExec(t, ws)
	require.NotNil(t, res.ExitCode)
	specs := host.specs()
	require.Len(t, specs, 1)
	assert.False(t, specs[0].System)
	assert.Equal(t, "POWERSHELL", specs[0].Profile)
}

// TestExecTimeoutKills:超时 → Kill(杀树)+ TimedOut=true + ExitCode=null。
func TestExecTimeoutKillsAndReports(t *testing.T) {
	host := &fakeHost{build: func(spec ShellSpec) *fakeProc {
		p := &fakeProc{spec: spec, profile: spec.Profile,
			streamCh: make(chan ShellStream, 16), exitCh: make(chan uint32, 1)}
		p.onKill = func(fp *fakeProc) {
			select {
			case fp.exitCh <- 1:
			default:
			}
			close(fp.streamCh)
		}
		return p
	}}
	start := time.Now()
	ws := runExec(t, host, proto.ExecParams{Command: "sleep", TimeoutSec: 1, Shell: "cmd"})
	_, _, res := collectExec(t, ws)
	assert.True(t, res.TimedOut)
	assert.Nil(t, res.ExitCode)
	assert.Less(t, time.Since(start), 15*time.Second)
	assert.Equal(t, 1, host.lastProc().Killed(), "timeout must kill via the shell host")
}

// TestExecNoActiveSessionPropagates:用户令牌 + 无活动会话 → EXEC_RESULT
// 携带 NO_ACTIVE_SESSION 稳定码(不隐式提权)。
func TestExecNoActiveSessionPropagates(t *testing.T) {
	host := &fakeHost{reject: &ShellHostError{Code: CodeNoActiveSession}}
	ws := runExec(t, host, proto.ExecParams{Command: "whoami", Shell: "cmd"})
	_, _, res := collectExec(t, ws)
	assert.Nil(t, res.ExitCode)
	assert.Equal(t, "NO_ACTIVE_SESSION", res.Code)
}

// TestExecCoreUnavailable:无 host 凭据(dev 无 xnc-core)→ CORE_UNAVAILABLE。
func TestExecCoreUnavailable(t *testing.T) {
	ws := runExec(t, nil, proto.ExecParams{Command: "whoami", Shell: "cmd"})
	_, _, res := collectExec(t, ws)
	assert.Nil(t, res.ExitCode)
	assert.Equal(t, "CORE_UNAVAILABLE", res.Code)
}

func TestExecScriptLifecycle(t *testing.T) {
	dir := t.TempDir()
	host := &fakeHost{build: func(spec ShellSpec) *fakeProc {
		p := &fakeProc{spec: spec, profile: spec.Profile,
			streamCh: make(chan ShellStream, 16), exitCh: make(chan uint32, 1)}
		go func() {
			p.emit(ShellStream{Bytes: []byte("from-script")})
			p.exitCh <- 3
			close(p.streamCh)
		}()
		return p
	}}
	ws, done := runExecIn(t, host, "sess-script", dir, proto.ExecParams{Script: "Write-Output from-script\nexit 3", TimeoutSec: 30, Shell: "powershell"})
	stdout, _, res := collectExec(t, ws)
	assert.Contains(t, stdout, "from-script")
	require.NotNil(t, res.ExitCode)
	assert.Equal(t, 3, *res.ExitCode)

	// 成功路径：临时脚本已删除（等 Handle 返回，defer 删除落定）。
	waitHandleDone(t, ws, done)
	assertDirEmpty(t, dir)

	// 脚本模式命令 = 临时文件调用串(& '<path>')。
	specs := host.specs()
	require.Len(t, specs, 1)
	assert.Contains(t, specs[0].Command, "sess-script")
	assert.Equal(t, "POWERSHELL", specs[0].Profile)
}

func TestExecScriptCleanupOnFailure(t *testing.T) {
	dir := t.TempDir()
	host := &fakeHost{build: func(spec ShellSpec) *fakeProc {
		p := &fakeProc{spec: spec, profile: spec.Profile,
			streamCh: make(chan ShellStream, 16), exitCh: make(chan uint32, 1)}
		p.exitCh <- 9
		close(p.streamCh)
		return p
	}}
	ws, done := runExecIn(t, host, "sess-fail", dir, proto.ExecParams{Script: "exit 9", TimeoutSec: 30})
	_, _, res := collectExec(t, ws)
	require.NotNil(t, res.ExitCode)
	require.Equal(t, 9, *res.ExitCode)

	waitHandleDone(t, ws, done)
	assertDirEmpty(t, dir) // temp script must be removed on failure path too
}

func TestExecOversizeScriptRefused(t *testing.T) {
	dir := t.TempDir()
	host := &fakeHost{}
	huge := strings.Repeat("a", 1024*1024+1)
	ws, _ := runExecIn(t, host, "sess-huge", dir, proto.ExecParams{Script: huge, TimeoutSec: 5})

	kind, data := readFrame(t, ws)
	require.Equal(t, "text", kind)
	var m proto.Message
	require.NoError(t, json.Unmarshal(data, &m))
	require.Equal(t, "EXEC_RESULT", m.Type)
	var r proto.ExecResult
	require.NoError(t, m.Decode(&r))
	assert.Nil(t, r.ExitCode)

	assertDirEmpty(t, dir) // no temp file written for oversize script
	assert.Empty(t, host.specs())
}

// TestExecHostileSessionIDRefused 路径穿越形态的 sessionID（T6 评审加固）：
// 整会话拒绝、null exitCode，且不在 TmpDir（乃至其逃逸目标）落任何文件。
func TestExecHostileSessionIDRefused(t *testing.T) {
	dir := t.TempDir()
	host := &fakeHost{}
	ws, _ := runExecIn(t, host, "../../evil", dir, proto.ExecParams{Script: "Write-Output pwned", TimeoutSec: 5})

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
	assert.Empty(t, host.specs())
}

// TestBuildExecCommandMapping:auto 解析为具体白名单 profile;显式 shell
// 映射;非法 shell 拒绝;script → 调用串。
func TestBuildExecCommandMapping(t *testing.T) {
	cmd, profile, err := buildExecCommand(proto.ExecParams{Command: "dir", Shell: "bash"}, "")
	require.NoError(t, err)
	assert.Equal(t, "dir", cmd)
	assert.Equal(t, "BASH", profile)

	cmd, profile, err = buildExecCommand(proto.ExecParams{Command: "dir", Shell: "pwsh"}, "")
	require.NoError(t, err)
	assert.Equal(t, "PWSH", profile)

	_, profile, err = buildExecCommand(proto.ExecParams{Command: "dir", Shell: "auto"}, "")
	require.NoError(t, err)
	assert.Contains(t, []string{"BASH", "PWSH", "POWERSHELL", "CMD"}, profile)

	_, _, err = buildExecCommand(proto.ExecParams{Command: "dir", Shell: "zsh"}, "")
	assert.Error(t, err)

	cmd, _, err = buildExecCommand(proto.ExecParams{Script: "x", Shell: "powershell"}, `C:\t\xnc-s.ps1`)
	require.NoError(t, err)
	assert.Equal(t, `& 'C:\t\xnc-s.ps1'`, cmd)

	_, _, err = buildExecCommand(proto.ExecParams{Shell: "cmd"}, "")
	assert.Error(t, err, "command or script required")
}

// TestExecTruncatedFlag:Dropped()>0(超预算丢弃)→ EXEC_RESULT.Truncated
// =true(T5 截断诚实化;marker 行由 shellpipe 层负责,此处钉终态位)。
func TestExecTruncatedFlag(t *testing.T) {
	host := &fakeHost{build: func(spec ShellSpec) *fakeProc {
		p := &fakeProc{spec: spec, profile: spec.Profile,
			streamCh: make(chan ShellStream, 16), exitCh: make(chan uint32, 1)}
		p.droppedVal = 4096
		go func() {
			p.emit(ShellStream{Bytes: []byte("partial")})
			p.exitCh <- 0
			close(p.streamCh)
		}()
		return p
	}}
	ws := runExec(t, host, proto.ExecParams{Command: "big", TimeoutSec: 30, Shell: "cmd"})
	_, _, res := collectExec(t, ws)
	assert.True(t, res.Truncated)
}

// TestExecNotTruncatedWhenNoDrop:Dropped()==0 → Truncated 缺省 false。
func TestExecNotTruncatedWhenNoDrop(t *testing.T) {
	host := &fakeHost{build: func(spec ShellSpec) *fakeProc {
		p := &fakeProc{spec: spec, profile: spec.Profile,
			streamCh: make(chan ShellStream, 16), exitCh: make(chan uint32, 1)}
		p.exitCh <- 0
		close(p.streamCh)
		return p
	}}
	ws := runExec(t, host, proto.ExecParams{Command: "whoami", Shell: "cmd"})
	_, _, res := collectExec(t, ws)
	assert.False(t, res.Truncated)
}
