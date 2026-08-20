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
