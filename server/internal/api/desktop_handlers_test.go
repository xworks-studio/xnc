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
	SessionID     string                   `json:"sessionId"`
	Token         string                   `json:"token"`
	ExpiresAt     time.Time                `json:"expiresAt"`
	WebsocketURL  string                   `json:"websocketUrl"`
	Turn          *proto.DesktopTurnConfig `json:"turn"`
	Lease         *desktopLeaseResp        `json:"lease"`
	MediaProtocol string                   `json:"mediaProtocol"`
}

// desktopLeaseResp：202 响应的 lease 判定（M2-Slice3 Task 4）。
type desktopLeaseResp struct {
	Granted bool   `json:"granted"`
	LeaseID string `json:"leaseId"`
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

// TestDesktopPerNodeLimit：desktop 每节点并发默认 4（多 viewer）——第 2 个
// 会话 202（T6 门 ③ 的第二 viewer），第 5 个 409 SESSION_LIMIT_EXCEEDED；
// 关闭后名额归还。
func TestDesktopPerNodeLimit(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-DT3", "mid-dt3")
	_ = dialControl(t, env, nodeID)

	// 默认 4（含 manager.New 零值兜底，对齐 agent host max_subs=4）：前 4 发均 202
	for i := 0; i < 4; i++ {
		resp := desktopPost(t, env, nodeID, `{}`)
		_ = resp.Body.Close()
		require.Equal(t, 202, resp.StatusCode, "viewer %d", i+1)
	}

	r5 := desktopPost(t, env, nodeID, `{}`)
	defer r5.Body.Close()
	var e struct {
		Error proto.APIError `json:"error"`
	}
	require.Equal(t, 409, r5.StatusCode)
	require.NoError(t, json.NewDecoder(r5.Body).Decode(&e))
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
// 非成员 → 404 NODE_NOT_FOUND。desktop 每节点并发有限（默认 4）：每个 202
// 后关闭会话再试下一个角色，避免名额挤占混入断言。
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

// TestDesktopLeaseAndCapabilities（M2-Slice3 Task 4）：
// ① 首会话 202 + lease{granted:true,leaseId} 且 SESSION_OPEN params 嵌入
//
//	同一 leaseId（REST/agent 同源）；
//
// ② 第二会话（并发上限内）202 + granted:false（view-only），params 无
//
//	leaseId；
//
// ③ capability 集按角色下发：owner 含 input.secure_attention/shell.system，
//
//	operator 含 input.mouse/keyboard 无 SAS；viewer 403（RBAC 层拒绝，
//	capability 只对已建会话生效）。
func TestDesktopLeaseAndCapabilities(t *testing.T) {
	f := newRBACFixture(t)
	ctrl := dialControl(t, f.env, f.nodeID)
	openCh := captureManySessionOpens(t, ctrl, 3)
	path := "/api/nodes/" + f.nodeID + "/desktop"

	postDesk := func(tok string) (*desktopOpenResp, int) {
		resp := doJSON(t, f.srv.URL, "POST", path, tok, `{}`)
		code := resp.StatusCode
		var body desktopOpenResp
		if code == 202 {
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
		}
		resp.Body.Close()
		return &body, code
	}

	// ① owner（非 admin 用户）：授予 + capability 全集。
	body, code := postDesk(f.tokens["owner"])
	require.Equal(t, 202, code)
	require.NotNil(t, body.Lease)
	assert.True(t, body.Lease.Granted)
	assert.Len(t, body.Lease.LeaseID, 16)
	select {
	case so := <-openCh:
		var p proto.DesktopParams
		require.NoError(t, jsonUnmarshal(so.Params, &p))
		assert.Equal(t, body.Lease.LeaseID, p.LeaseID, "SESSION_OPEN params must embed the REST leaseId")
		require.ElementsMatch(t, []string{
			proto.CapScreenView, proto.CapInputMouse, proto.CapInputKeyboard,
			proto.CapInputSecureAttn, proto.CapShellSystem,
		}, p.Capabilities)
	case <-time.After(3 * time.Second):
		t.Fatal("no SESSION_OPEN (owner)")
	}

	// ② operator（并发上限内第二 viewer）：view-only + operator 集。
	body2, code := postDesk(f.tokens["operator"])
	require.Equal(t, 202, code)
	require.NotNil(t, body2.Lease)
	assert.False(t, body2.Lease.Granted, "second concurrent session must be view-only")
	select {
	case so := <-openCh:
		var p proto.DesktopParams
		require.NoError(t, jsonUnmarshal(so.Params, &p))
		assert.Empty(t, p.LeaseID)
		require.ElementsMatch(t, []string{
			proto.CapScreenView, proto.CapInputMouse, proto.CapInputKeyboard,
		}, p.Capabilities)
	case <-time.After(3 * time.Second):
		t.Fatal("no SESSION_OPEN (operator)")
	}

	// ③ viewer：RBAC 拒绝（403）——capability 集只对已建会话有意义。
	_, code = postDesk(f.tokens["viewer"])
	assert.Equal(t, 403, code)

	// 持有者关闭 → 约 → 新会话授予（server 仲裁移交闭环）。
	// SessionsOf 是 map 序遍历（随机序），sess[0] 不保证是持有者——并发上限内
	// 还有 operator 的 view-only 会话，误关它会留下未释放的约导致断言抖动。
	// 全部关闭使「持有者关闭释放约」断言与遍历顺序无关。
	for _, s := range f.env.Sess.SessionsOf(mustUUID(f.nodeID), proto.KindDesktop) {
		f.env.Sess.NotifyClose(s.ID, "test")
	}
	body3, code := postDesk(f.tokens["owner"])
	require.Equal(t, 202, code)
	require.NotNil(t, body3.Lease)
	assert.True(t, body3.Lease.Granted, "lease must be re-grantable after holder close")
}

// openDesktopForMedia 起一套 env（可变异 config）+ 节点 + 控制连接，开一个
// desktop 会话并返回（REST 响应体, SESSION_OPEN params, env, nodeID）。
// M4 Task 4 的 mediaProtocol 回环共用。
func openDesktopForMedia(t *testing.T, mutate func(*config.Config), body string) (*desktopOpenResp, json.RawMessage, *TestEnv, string) {
	t.Helper()
	env := newTestEnvWithCfg(t, func(c *config.Config) {
		// desktop 会话需要 TURN（NewTestEnv 同款注入；openDesktopForMedia
		// 不配置 TURN 时 503 TURN_UNCONFIGURED 先于媒体断言）。
		c.TurnURLs = []string{"turn:test-turn:3478?transport=tcp", "turn:test-turn:3478"}
		c.TurnUsername = "testuser"
		c.TurnCredential = "testcred"
		if mutate != nil {
			mutate(c)
		}
	})
	nodeID := env.EnrollNode(t, "WEB-MEDIA", "mid-media")
	ctrl := dialControl(t, env, nodeID)
	openCh := captureSessionOpen(t, ctrl)

	resp := desktopPost(t, env, nodeID, body)
	defer resp.Body.Close()
	require.Equal(t, 202, resp.StatusCode)
	var body202 desktopOpenResp
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body202))

	select {
	case so := <-openCh:
		require.Equal(t, proto.KindDesktop, so.Kind)
		return &body202, so.Params, env, nodeID
	case <-time.After(3 * time.Second):
		t.Fatal("no SESSION_OPEN")
		return nil, nil, nil, ""
	}
}

// TestDesktopMediaProtocolRollout（M4 Task 4）：百分比 100 → 一切新会话选
// v2：SESSION_OPEN params、REST 202、manager 快照三处同源；客户端提交的
// mediaProtocol 被白名单剥离（server 独占控制点）；审计行携带选定值。
func TestDesktopMediaProtocolRollout(t *testing.T) {
	body, openParams, env, nodeID := openDesktopForMedia(t, func(c *config.Config) {
		c.DesktopMediaV2Percent = 100
	}, `{"mediaProtocol":"v1"}`) // 客户端试图倒退回 v1——必须被忽略

	assert.Equal(t, proto.MediaProtocolV2, body.MediaProtocol,
		"REST body must report the server-selected protocol")

	var p proto.DesktopParams
	require.NoError(t, jsonUnmarshal(openParams, &p))
	assert.Equal(t, proto.MediaProtocolV2, p.MediaProtocol,
		"SESSION_OPEN params must embed the server selection (client value stripped)")

	// 快照语义：manager 存的 params 与 SESSION_OPEN 下发的逐字节一致——
	// 选择在会话打开时定死，live 会话没有重选路径。
	sess := env.Sess.SessionsOf(mustUUID(nodeID), proto.KindDesktop)
	require.NotEmpty(t, sess)
	assert.JSONEq(t, string(openParams), string(sess[0].Params),
		"stored session params must equal SESSION_OPEN params (selection snapshotted at open)")

	// 审计：desktop.open 行携带 mediaProtocol（canary 运维证据）。
	require.Eventually(t, func() bool {
		var n int
		require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
			`SELECT count(*) FROM audit_logs WHERE action = 'desktop.open'`+
				` AND metadata ->> 'mediaProtocol' = 'v2'`).Scan(&n))
		return n >= 1
	}, 5*time.Second, 200*time.Millisecond)
}

// TestDesktopMediaProtocolDefaultAndRollback（M4 Task 4）：未配置（百分比 0）
// → fail closed 到 v1；回滚开关开着时即使百分比 100 也一切新会话 v1。
func TestDesktopMediaProtocolDefaultAndRollback(t *testing.T) {
	// 缺省：未配置 rollout → v1。
	body, openParams, _, _ := openDesktopForMedia(t, nil, `{}`)
	assert.Equal(t, proto.MediaProtocolV1, body.MediaProtocol)
	var p proto.DesktopParams
	require.NoError(t, jsonUnmarshal(openParams, &p))
	assert.Equal(t, proto.MediaProtocolV1, p.MediaProtocol)

	// 回滚：百分比 100 + 回滚开关 → 新会话仍 v1（allowlist 胜百分比、
	// 回滚胜一切的纯单测见 desktop_media_select_test.go）。
	body2, openParams2, _, _ := openDesktopForMedia(t, func(c *config.Config) {
		c.DesktopMediaV2Percent = 100
		c.DesktopMediaV2Rollback = true
	}, `{}`)
	assert.Equal(t, proto.MediaProtocolV1, body2.MediaProtocol, "rollback must pin all new sessions to v1")
	var p2 proto.DesktopParams
	require.NoError(t, jsonUnmarshal(openParams2, &p2))
	assert.Equal(t, proto.MediaProtocolV1, p2.MediaProtocol)
}
