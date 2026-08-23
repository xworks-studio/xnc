package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSystemTokenRBACMatrix(M2-Slice2 Task 4):system 令牌请求的
// owner-only RBAC 矩阵——operator/viewer 403;owner 通过(节点在线时
// 202);缺省 system=false 时 operator 正常通过;审计 metadata 携带
// system=true;SESSION_OPEN params 透传 system。
func TestSystemTokenRBACMatrix(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)
	cluster := defaultClusterID(t, srv.URL, admin)
	opID, opTok := createUserViaAPI(t, srv.URL, admin, "sysop@t.local", "SysOp")
	viewID, viewTok := createUserViaAPI(t, srv.URL, admin, "sysview@t.local", "SysView")
	require.NotEmpty(t, opID)
	require.NotEmpty(t, viewID)
	resp := doJSON(t, srv.URL, "POST", "/api/clusters/"+cluster+"/members", admin,
		`{"user_id":"`+opID+`","role":"operator"}`)
	require.Equal(t, 201, resp.StatusCode)
	resp = doJSON(t, srv.URL, "POST", "/api/clusters/"+cluster+"/members", admin,
		`{"user_id":"`+viewID+`","role":"viewer"}`)
	require.Equal(t, 201, resp.StatusCode)

	nodeID := env.EnrollNode(t, "SYS-RBAC", "mid-sys")

	// operator + system → 403(exec 与 shell)。
	assert.Equal(t, 403, doJSON(t, srv.URL, "POST", "/api/nodes/"+nodeID+"/exec", opTok,
		`{"command":"whoami","system":true}`).StatusCode)
	assert.Equal(t, 403, doJSON(t, srv.URL, "POST", "/api/nodes/"+nodeID+"/shell", opTok,
		`{"cols":80,"rows":25,"system":true}`).StatusCode)
	// viewer + system → 403。
	assert.Equal(t, 403, doJSON(t, srv.URL, "POST", "/api/nodes/"+nodeID+"/exec", viewTok,
		`{"command":"whoami","system":true}`).StatusCode)
	// 非 member + system → 404(不泄漏存在性)。
	_, nobodyTok := createUserViaAPI(t, srv.URL, admin, "nobody@t.local", "Nobody")
	assert.Equal(t, 404, doJSON(t, srv.URL, "POST", "/api/nodes/"+nodeID+"/exec", nobodyTok,
		`{"command":"whoami","system":true}`).StatusCode)
	// owner + system + 节点离线 → 409 NODE_OFFLINE(RBAC 已过,非 403)。
	assert.Equal(t, 409, doJSON(t, srv.URL, "POST", "/api/nodes/"+nodeID+"/exec", admin,
		`{"command":"whoami","system":true}`).StatusCode)
	// 缺省 system=false:operator 通过 RBAC(离线 → 409,而非 403)。
	assert.Equal(t, 409, doJSON(t, srv.URL, "POST", "/api/nodes/"+nodeID+"/exec", opTok,
		`{"command":"whoami"}`).StatusCode)

	// owner + system + 在线 → 202;params 透传 system=true;审计 system=true。
	ctrl := dialControl(t, env, nodeID)
	defer func() { _ = ctrl.CloseNow() }()
	agentDone := make(chan struct{}, 1)
	go func() {
		fakeAgentSession(t, ctrl, func(aws *websocket.Conn) {
			_ = aws.Close(websocket.StatusNormalClosure, "")
		})
		agentDone <- struct{}{}
	}()
	resp2 := doJSON(t, srv.URL, "POST", "/api/nodes/"+nodeID+"/exec", admin,
		`{"command":"whoami","system":true}`)
	defer resp2.Body.Close()
	require.Equal(t, 202, resp2.StatusCode)

	require.Eventually(t, func() bool {
		var n int
		require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
			`SELECT count(*) FROM audit_logs WHERE action='exec.start' AND metadata->>'system' = 'true'`).Scan(&n))
		return n >= 1
	}, 5*time.Second, 200*time.Millisecond, "audit must carry system=true")

	select {
	case <-agentDone:
	case <-time.After(5 * time.Second):
		t.Fatal("agent session not opened")
	}
}
