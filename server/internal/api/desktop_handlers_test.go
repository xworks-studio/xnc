package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
	"xnc/server/internal/config"
)

// desktopPost 发起 POST /api/nodes/{id}/desktop（admin Bearer）。
func desktopPost(t *testing.T, env *TestEnv, nodeID, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", env.srv.URL+"/api/nodes/"+nodeID+"/desktop",
		bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+env.AdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// desktopOpenResp 是 REST 响应形态（T4 e2eviewer server 模式同一契约）。
type desktopOpenResp struct {
	SessionID    string                   `json:"sessionId"`
	Token        string                   `json:"token"`
	ExpiresAt    time.Time                `json:"expiresAt"`
	WebsocketURL string                   `json:"websocketUrl"`
	Turn         *proto.DesktopTurnConfig `json:"turn"`
}

// TestDesktopSessionEndToEnd：202 → SESSION_OPEN 携带 KindDesktop + 服务端
// TURN 配置 + 白名单后的客户端字段；REST 响应携带 token/websocketUrl/turn；
// 审计 desktop.open。
// 关键安全断言：客户端提交的 turn / iceTransportPolicy / 任意未知字段绝不下发。
func TestDesktopSessionEndToEnd(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-DT1", "mid-dt1")
	ctrl := dialControl(t, env, nodeID)
	openCh := captureSessionOpen(t, ctrl)

	resp := desktopPost(t, env, nodeID,
		`{"signaling":"webrtc","wtsSession":0,"iceTransportPolicy":"all",`+
			`"turn":{"urls":["turn:evil.example:3478"],"username":"evil","credential":"evil"}}`)
	defer resp.Body.Close()
	require.Equal(t, 202, resp.StatusCode)

	var body desktopOpenResp
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.NotEmpty(t, body.SessionID)
	assert.NotEmpty(t, body.Token)
	assert.Contains(t, body.WebsocketURL, "/api/session/"+body.SessionID+"?token=")
	// REST turn = 服务端配置（testenv 注入），非客户端提交值
	require.NotNil(t, body.Turn)
	assert.Equal(t, env.Cfg.TurnURLs, body.Turn.URLs)
	assert.Equal(t, env.Cfg.TurnUsername, body.Turn.Username)
	assert.Equal(t, env.Cfg.TurnCredential, body.Turn.Credential)

	select {
	case so := <-openCh:
		require.Equal(t, proto.KindDesktop, so.Kind)
		var p proto.DesktopParams
		require.NoError(t, jsonUnmarshal(so.Params, &p))
		assert.Equal(t, "webrtc", p.Signaling)
		// 服务端 TURN 配置原样下发
		require.NotNil(t, p.Turn)
		assert.Equal(t, env.Cfg.TurnURLs, p.Turn.URLs)
		assert.Equal(t, env.Cfg.TurnUsername, p.Turn.Username)
		assert.Equal(t, env.Cfg.TurnCredential, p.Turn.Credential)
		// 白名单剥离：客户端的 iceTransportPolicy 绝不透传（缺省 = agent 侧
		// 强制 relay）；客户端 turn 配置被服务端配置覆盖。
		assert.Empty(t, p.IceTransportPolicy)
		assert.NotContains(t, string(so.Params), "evil.example")
	case <-time.After(3 * time.Second):
		t.Fatal("no SESSION_OPEN")
	}

	require.Eventually(t, func() bool {
		var n int
		require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
			`SELECT count(*) FROM audit_logs WHERE action = 'desktop.open'`).Scan(&n))
		return n >= 1
	}, 5*time.Second, 200*time.Millisecond)
}

// TestDesktopDefaultsAndValidation：空体 = 缺省 webrtc；非法 signaling → 400；
// 坏 JSON → 400。
func TestDesktopDefaultsAndValidation(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-DT2", "mid-dt2")
	ctrl := dialControl(t, env, nodeID)
	openCh := captureSessionOpen(t, ctrl)

	resp := desktopPost(t, env, nodeID, ``)
	defer resp.Body.Close()
	require.Equal(t, 202, resp.StatusCode)
	select {
	case so := <-openCh:
		var p proto.DesktopParams
		require.NoError(t, jsonUnmarshal(so.Params, &p))
		assert.Equal(t, "webrtc", p.Signaling)
		assert.NotNil(t, p.Turn)
		assert.Empty(t, p.IceTransportPolicy)
	case <-time.After(3 * time.Second):
		t.Fatal("no SESSION_OPEN")
	}

	for _, b := range []string{`{"signaling":"proprietary"}`, `{not json`} {
		r := desktopPost(t, env, nodeID, b)
		_ = r.Body.Close()
		assert.Equal(t, 400, r.StatusCode, b)
	}
}

// TestDesktopSingleSessionPerNode：desktop 每节点单会话——第二发 409
// SESSION_LIMITED；关闭后可再建。
func TestDesktopSingleSessionPerNode(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-DT3", "mid-dt3")
	_ = dialControl(t, env, nodeID)

	resp := desktopPost(t, env, nodeID, `{}`)
	defer resp.Body.Close()
	require.Equal(t, 202, resp.StatusCode)

	r2 := desktopPost(t, env, nodeID, `{}`)
	defer r2.Body.Close()
	var e struct {
		Error proto.APIError `json:"error"`
	}
	require.Equal(t, 409, r2.StatusCode)
	require.NoError(t, json.NewDecoder(r2.Body).Decode(&e))
	assert.Equal(t, proto.CodeSessionLimited, e.Error.Code)

	// 会话关闭后名额归还
	sess := env.Sess.SessionsOf(mustUUID(nodeID), proto.KindDesktop)
	require.NotEmpty(t, sess)
	env.Sess.NotifyClose(sess[0].ID, "test")
	require.Eventually(t, func() bool {
		r := desktopPost(t, env, nodeID, `{}`)
		_ = r.Body.Close()
		return r.StatusCode == 202
	}, 3*time.Second, 100*time.Millisecond)
}

// TestDesktopTurnUnconfigured：无 TURN 配置的服务器 → 503 TURN_UNCONFIGURED
// （desktop relay-only 无 TURN 不可用，拒绝开会话优于开一个必死的会话）。
func TestDesktopTurnUnconfigured(t *testing.T) {
	env := newTestEnvWithCfg(t, func(c *config.Config) {
		c.TurnURLs, c.TurnUsername, c.TurnCredential = nil, "", ""
	})
	nodeID := env.EnrollNode(t, "WEB-DT4", "mid-dt4")
	_ = dialControl(t, env, nodeID)

	resp := desktopPost(t, env, nodeID, `{}`)
	defer resp.Body.Close()
	var e struct {
		Error proto.APIError `json:"error"`
	}
	require.Equal(t, 503, resp.StatusCode)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&e))
	assert.Equal(t, "TURN_UNCONFIGURED", e.Error.Code)
}

// TestDesktopRBAC：Scenario F——owner/operator → 202；viewer → 403 FORBIDDEN；
// 非成员 → 404 NODE_NOT_FOUND。desktop 每节点单会话：每个 202 后关闭会话
// 再试下一个角色，避免名额挤占混入断言。
func TestDesktopRBAC(t *testing.T) {
	f := newRBACFixture(t)
	path := "/api/nodes/" + f.nodeID + "/desktop"
	for _, user := range []string{"owner", "operator", "viewer", "outsider"} {
		code, errCode := f.post(t, f.tokens[user], path, `{}`)
		var want int
		switch user {
		case "owner", "operator":
			want = 202
		case "viewer":
			want = 403
		default:
			want = 404
		}
		assert.Equal(t, want, code, user)
		switch want {
		case 202:
			for _, s := range f.env.Sess.SessionsOf(mustUUID(f.nodeID), proto.KindDesktop) {
				f.env.Sess.NotifyClose(s.ID, "rbac-test")
			}
		case 403:
			assert.Equal(t, proto.CodeForbidden, errCode, user)
		case 404:
			assert.Equal(t, proto.CodeNodeNotFound, errCode, user)
		}
	}
}
