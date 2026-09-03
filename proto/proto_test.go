package proto

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageRoundtrip(t *testing.T) {
	tests := []struct {
		name    string
		typ     string
		payload any
		want    string
	}{
		{"challenge", TypeChallenge, Challenge{Nonce: "abc123"}, `{"type":"CHALLENGE","payload":{"nonce":"abc123"}}`},
		{"challenge_response", TypeChallengeResponse, ChallengeResponse{NodeID: "n1", Signature: []byte{1, 2}}, `{"type":"CHALLENGE_RESPONSE","payload":{"nodeId":"n1","signature":"AQI="}}`},
		{"hello", TypeHello, Hello{NodeID: "n1", Hostname: "WEB-01", AgentVersion: "0.1.0", ShellType: "pwsh"}, `{"type":"HELLO","payload":{"nodeId":"n1","hostname":"WEB-01","agentVersion":"0.1.0","shellType":"pwsh"}}`},
		{"heartbeat", TypeHeartbeat, struct{}{}, `{"type":"HEARTBEAT","payload":{}}`},
		{"error", TypeError, ErrorPayload{Code: CodeNodeOffline, Message: "Node is offline."}, `{"type":"ERROR","payload":{"code":"NODE_OFFLINE","message":"Node is offline."}}`},
		// Task 7（spec §9）：安装器更新编排的推送与审计。
		{"update_available", TypeUpdateAvailable, UpdateAvailable{Version: "0.6.2", URL: "/setup.exe?channel=stable", SHA256: "ab12"}, `{"type":"UPDATE_AVAILABLE","payload":{"version":"0.6.2","url":"/setup.exe?channel=stable","sha256":"ab12"}}`},
		{"update_audit_ok", TypeUpdateAudit, UpdateAudit{Event: UpdateEventOK, From: "0.6.1", To: "0.6.2"}, `{"type":"UPDATE_AUDIT","payload":{"event":"update_ok","from":"0.6.1","to":"0.6.2"}}`},
		{"update_audit_rollback", TypeUpdateAudit, UpdateAudit{Event: UpdateEventRollback, From: "0.6.1", To: "0.6.2", Reason: "watchdog deadline exceeded"}, `{"type":"UPDATE_AUDIT","payload":{"event":"update_rollback","from":"0.6.1","to":"0.6.2","reason":"watchdog deadline exceeded"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := NewMsg(tt.typ, tt.payload)
			require.NoError(t, err)
			b, err := json.Marshal(m)
			require.NoError(t, err)
			assert.JSONEq(t, tt.want, string(b))

			var got Message
			require.NoError(t, json.Unmarshal(b, &got))
			assert.Equal(t, tt.typ, got.Type)
		})
	}
}

func TestChallengeResponseDecode(t *testing.T) {
	raw := []byte(`{"type":"CHALLENGE_RESPONSE","payload":{"nodeId":"n9","signature":"AQIDBA=="}}`)
	var m Message
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Equal(t, TypeChallengeResponse, m.Type)
	var p ChallengeResponse
	require.NoError(t, m.Decode(&p))
	assert.Equal(t, "n9", p.NodeID)
	assert.Equal(t, []byte{1, 2, 3, 4}, p.Signature)
}

func TestAPIError(t *testing.T) {
	e := Err(401, CodeUnauthorized, "bad token")
	assert.Equal(t, 401, e.Status)
	assert.Contains(t, e.Error(), "UNAUTHORIZED")
}
