package proto

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionOpenRoundtrip(t *testing.T) {
	params, _ := json.Marshal(ExecParams{Command: "Get-Service", TimeoutSec: 300})
	m, err := NewMsg(TypeSessionOpen, SessionOpen{
		SessionID: "s-1", Kind: KindExec, Params: params,
		AgentToken: "at", WsURL: "wss://x/api/agent/session?token=at",
	})
	require.NoError(t, err)
	b, err := json.Marshal(m)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"SESSION_OPEN","payload":{"sessionId":"s-1","kind":"exec",
		"params":{"command":"Get-Service","timeoutSec":300},"agentToken":"at",
		"wsUrl":"wss://x/api/agent/session?token=at","expiresAt":"0001-01-01T00:00:00Z"}}`, string(b))

	var got Message
	require.NoError(t, json.Unmarshal(b, &got))
	var so SessionOpen
	require.NoError(t, got.Decode(&so))
	assert.Equal(t, KindExec, so.Kind)
	var p ExecParams
	require.NoError(t, json.Unmarshal(so.Params, &p))
	assert.Equal(t, "Get-Service", p.Command)
}

func TestExecResultNullExitCode(t *testing.T) {
	b, err := json.Marshal(ExecResult{ExitCode: nil, TimedOut: true, DurationMs: 1001})
	require.NoError(t, err)
	assert.JSONEq(t, `{"exitCode":null,"timedOut":true,"durationMs":1001}`, string(b))

	var r ExecResult
	require.NoError(t, json.Unmarshal([]byte(`{"exitCode":7,"timedOut":false,"durationMs":9}`), &r))
	require.NotNil(t, r.ExitCode)
	assert.Equal(t, 7, *r.ExitCode)

	seven := 7
	b2, _ := json.Marshal(ExecResult{ExitCode: &seven})
	assert.Contains(t, string(b2), `"exitCode":7`)
}

func TestSessionCloseRefused(t *testing.T) {
	m, _ := NewMsg(TypeSessionClose, SessionClose{SessionID: "s-1", Reason: "client-gone"})
	b, _ := json.Marshal(m)
	assert.JSONEq(t, `{"type":"SESSION_CLOSE","payload":{"sessionId":"s-1","reason":"client-gone"}}`, string(b))
	m2, _ := NewMsg(TypeSessionRefused, SessionRefused{SessionID: "s-2", Code: CodeKindUnsupported, Message: "kind"})
	b2, _ := json.Marshal(m2)
	assert.Contains(t, string(b2), `"SESSION_REFUSED"`)
}

func TestShellVocabulary(t *testing.T) {
	m, err := NewMsg(TypeSessionOpen, SessionOpen{
		SessionID: "s1", Kind: KindShell,
		Params: mustRaw(t, ShellParams{Cols: 120, Rows: 30}),
	})
	require.NoError(t, err)
	b, err := json.Marshal(m)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"SESSION_OPEN","payload":{"sessionId":"s1","kind":"shell",
		"params":{"cols":120,"rows":30},"agentToken":"","wsUrl":"",
		"expiresAt":"0001-01-01T00:00:00Z"}}`, string(b))

	bb, _ := json.Marshal(ShellBegin{Shell: "pwsh"})
	assert.JSONEq(t, `{"shell":"pwsh"}`, string(bb))

	rb, _ := NewMsg("SHELL_RESIZE", ShellResize{Cols: 100, Rows: 40})
	// SHELL_RESIZE 是会话内 text 帧，type 字面量属 kind 私有词汇：
	// 不进 proto 常量（与 EXEC_RESULT 同策略），测试用字面量验证 payload。
	mr, err := NewMsg("SHELL_RESIZE", ShellResize{Cols: 100, Rows: 40})
	require.NoError(t, err)
	br, _ := json.Marshal(mr)
	assert.Contains(t, string(br), `"cols":100`)
	assert.Contains(t, string(br), `"rows":40`)

	assert.Equal(t, "SESSION_LIMIT_EXCEEDED", CodeSessionLimited)
	_ = rb
}

func TestFileVocabulary(t *testing.T) {
	fp := FileParams{Direction: "upload", Path: `C:\temp\f.zip`, Size: 1024, Sha256: "abc"}
	b, _ := json.Marshal(fp)
	assert.JSONEq(t, `{"direction":"upload","path":"C:\\temp\\f.zip","size":1024,"sha256":"abc"}`, string(b))

	dp := FileParams{Direction: "download", Path: `C:\temp\f.zip`}
	b2, _ := json.Marshal(dp)
	assert.JSONEq(t, `{"direction":"download","path":"C:\\temp\\f.zip"}`, string(b2))

	fb, _ := json.Marshal(FileBegin{Direction: "upload", Path: "p", Size: 1, Sha256: "s"})
	assert.JSONEq(t, `{"direction":"upload","path":"p","size":1,"sha256":"s"}`, string(fb))

	fr, _ := json.Marshal(FileResult{Bytes: 42, Sha256: "h", Ok: true})
	assert.JSONEq(t, `{"bytes":42,"sha256":"h","ok":true}`, string(fr))

	fe, _ := json.Marshal(FileError{Code: CodeFileNotFound})
	assert.JSONEq(t, `{"code":"FILE_NOT_FOUND"}`, string(fe))

	tp, _ := json.Marshal(TunnelParams{Target: "rdp"})
	assert.JSONEq(t, `{"target":"rdp"}`, string(tp))
}

// TestScreenVocabulary：ScreenParams 全零值 omitempty（server 端补默认）+
// 非零值字段写出；ScreenBegin/ScreenState 无 omitempty，字段全量序列化，
// 并做 Marshal→Unmarshal 往返断言（Phase 6 SCREEN_BEGIN/SCREEN_STATE 帧）。
func TestScreenVocabulary(t *testing.T) {
	empty, _ := json.Marshal(ScreenParams{})
	assert.JSONEq(t, `{}`, string(empty))

	sp := ScreenParams{Fps: 30, Quality: 80, MaxWidth: 1280}
	b, _ := json.Marshal(sp)
	assert.JSONEq(t, `{"fps":30,"quality":80,"maxWidth":1280}`, string(b))
	var back ScreenParams
	require.NoError(t, json.Unmarshal(b, &back))
	assert.Equal(t, sp, back)

	sb := ScreenBegin{Width: 1920, Height: 1080, State: "capturing", Codec: "h264"}
	bb, _ := json.Marshal(sb)
	assert.JSONEq(t, `{"width":1920,"height":1080,"state":"capturing","codec":"h264"}`, string(bb))
	var backB ScreenBegin
	require.NoError(t, json.Unmarshal(bb, &backB))
	assert.Equal(t, sb, backB)

	st := ScreenState{State: "locked"}
	bs, _ := json.Marshal(st)
	assert.JSONEq(t, `{"state":"locked"}`, string(bs))
	var backS ScreenState
	require.NoError(t, json.Unmarshal(bs, &backS))
	assert.Equal(t, st, backS)
}

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
