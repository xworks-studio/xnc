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

// desktopOpenResp 是 REST 响应形态（RTV：wtUrl/wsUrl + lease；无 turn）。
type desktopOpenResp struct {
	SessionID string `json:"sessionId"`
	Token     string `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	WTURL     string `json:"wtUrl"`
	WSURL     string `json:"wsUrl"`
	Lease     *desktopLeaseResp `json:"lease"`
}

// desktopLeaseResp：202 响应的 lease 判定（RTV 后 lease 为 server 侧簿记，
// relay 的 input 门控键）。
type desktopLeaseResp struct {
	Granted bool   `json:"granted"`
	LeaseID string `json:"leaseId"`
}

// TestDesktopSessionEndToEnd：202 → SESSION_OPEN 携带 KindDesktop + 服务端
// RTV endpoint + Hub 签发的 HostToken + 白名单后的客户端字段；REST 响应
// 携带 token/wtUrl/wsUrl/lease；审计 desktop.open。
// 关键安全断言：客户端提交的 streamEndpoint / hostToken / 任意未知字段绝
// 不透传（hostToken 是 relay 注册凭据，泄漏 = 会话劫持）。
func TestDesktopSessionEndToEnd(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-DT1", "mid-dt1")
	ctrl := dialControl(t, env, nodeID)
	openCh := captureSessionOpen(t, ctrl)

	resp := desktopPost(t, env, nodeID,
		`{"wtsSession":0,"streamEndpoint":"evil.example:6666",`+
			`"hostToken":"evil-token","iceTransportPolicy":"all"}`)
	defer resp.Body.Close()
	require.Equal(t, 202, resp.StatusCode)

	var body desktopOpenResp
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.NotEmpty(t, body.SessionID)
	assert.NotEmpty(t, body.Token)
	assert.Contains(t, body.WTURL, "/wt")
	assert.Contains(t, body.WSURL, "/ws")

	select {
	case so := <-openCh:
		require.Equal(t, proto.KindDesktop, so.Kind)
		var p proto.DesktopParams
		require.NoError(t, jsonUnmarshal(so.Params, &p))
		// endpoint 只来自 server config；hostToken 只来自 relay Hub（64 hex）。
		assert.Equal(t, env.Cfg.RTVStreamEndpoint, p.StreamEndpoint)
		assert.Regexp(t, "^[0-9a-f]{64}$", p.HostToken)
		// 白名单剥离：客户端的 endpoint/token 绝不透传。
		assert.NotContains(t, string(so.Params), "evil.example")
		assert.NotContains(t, string(so.Params), "evil-token")
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

// TestDesktopHostTokenStablePerNode：HostToken 按节点签发且跨会话稳定——
// core StartCapture 幂等复用运行中的 host，第二会话的 cfg 不会送达 host，
// token 必须可重放；不同节点 token 不同。
func TestDesktopHostTokenStablePerNode(t *testing.T) {
	env := NewTestEnv(t)
	nodeA := env.EnrollNode(t, "WEB-DT1B", "mid-dt1b")
	nodeB := env.EnrollNode(t, "WEB-DT1C", "mid-dt1c")
	ctrl := dialControl(t, env, nodeA)
	opens := captureManySessionOpens(t, ctrl, 2)

	tokOf := func(node string) string {
		resp := desktopPost(t, env, node, `{}`)
		defer resp.Body.Close()
		require.Equal(t, 202, resp.StatusCode)
		select {
		case so := <-opens:
			require.Equal(t, proto.KindDesktop, so.Kind)
			var p proto.DesktopParams
			require.NoError(t, jsonUnmarshal(so.Params, &p))
			return p.HostToken
		case <-time.After(3 * time.Second):
			t.Fatal("no SESSION_OPEN")
			return ""
		}
	}
	assert.Equal(t, tokOf(nodeA), tokOf(nodeA), "same node must reuse the host token")

	ctrlB := dialControl(t, env, nodeB)
	openB := captureSessionOpen(t, ctrlB)
	resp := desktopPost(t, env, nodeB, `{}`)
	defer resp.Body.Close()
	require.Equal(t, 202, resp.StatusCode)
	select {
	case so := <-openB:
		var p proto.DesktopParams
		require.NoError(t, jsonUnmarshal(so.Params, &p))
		// 不同节点 token 不同（B 在 Hub 未签发过——惰性 mint）。
		assert.NotEmpty(t, p.HostToken)
	case <-time.After(3 * time.Second):
		t.Fatal("no SESSION_OPEN (node B)")
	}
}

// TestDesktopDefaultsAndValidation：空体 = 缺省；坏 JSON → 400。
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
		assert.Equal(t, env.Cfg.RTVStreamEndpoint, p.StreamEndpoint)
		assert.NotEmpty(t, p.HostToken)
	case <-time.After(3 * time.Second):
		t.Fatal("no SESSION_OPEN")
	}

	r := desktopPost(t, env, nodeID, `{not json`)
	_ = r.Body.Close()
	assert.Equal(t, 400, r.StatusCode)
}

// TestDesktopPerNodeLimit：desktop 每节点并发默认 4（多 viewer）——第 2 个
// 会话 202（第二 viewer），第 5 个 409 SESSION_LIMIT_EXCEEDED；关闭后
// 名额归还。
func TestDesktopPerNodeLimit(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-DT3", "mid-dt3")
	_ = dialControl(t, env, nodeID)

	// 默认 8（含 manager.New 零值兜底）：前 8 发均 202
	for i := 0; i < 8; i++ {
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

// TestDesktopRtvUnconfigured：无 RTV endpoint 配置的服务器 → 503
// RTV_UNCONFIGURED（拒绝开会话优于开一个必死的会话）。
func TestDesktopRtvUnconfigured(t *testing.T) {
	env := newTestEnvWithCfg(t, func(c *config.Config) {
		c.RTVStreamEndpoint = ""
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
	assert.Equal(t, proto.CodeRtvUnconfigured, e.Error.Code)
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

// TestDesktopLease（RTV 后语义）：① 首会话 202 + lease{granted:true,
// leaseId}（server 侧簿记，relay input 门控键——不再嵌入 agent params）；
// ② 第二会话（并发上限内）202 + granted:false（view-only）；③ viewer
// 403（RBAC 层拒绝）；④ 持有者关闭 → 约 → 新会话授予（仲裁移交闭环）。
func TestDesktopLease(t *testing.T) {
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

	// ① owner（非 admin 用户）：授予。
	body, code := postDesk(f.tokens["owner"])
	require.Equal(t, 202, code)
	require.NotNil(t, body.Lease)
	assert.True(t, body.Lease.Granted)
	assert.Len(t, body.Lease.LeaseID, 16)
	select {
	case so := <-openCh:
		// params 绝不携带 leaseId/capability（agent 不感知输入权限；
		// relay 门控按 manager 的 desktopLeases 表判定）。
		assert.NotContains(t, string(so.Params), "leaseId")
		assert.NotContains(t, string(so.Params), "capabilities")
	case <-time.After(3 * time.Second):
		t.Fatal("no SESSION_OPEN (owner)")
	}

	// ② operator（并发上限内第二 viewer）：view-only。
	body2, code := postDesk(f.tokens["operator"])
	require.Equal(t, 202, code)
	require.NotNil(t, body2.Lease)
	assert.False(t, body2.Lease.Granted, "second concurrent session must be view-only")

	// ③ viewer：RBAC 拒绝（403）。
	_, code = postDesk(f.tokens["viewer"])
	assert.Equal(t, 403, code)

	// 持有者关闭 → 约 → 新会话授予。SessionsOf 是 map 序遍历（随机序），
	// 全部关闭使断言与遍历顺序无关。
	for _, s := range f.env.Sess.SessionsOf(mustUUID(f.nodeID), proto.KindDesktop) {
		f.env.Sess.NotifyClose(s.ID, "test")
	}
	body3, code := postDesk(f.tokens["owner"])
	require.Equal(t, 202, code)
	require.NotNil(t, body3.Lease)
	assert.True(t, body3.Lease.Granted, "lease must be re-grantable after holder close")
}
