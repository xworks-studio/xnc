package api

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registerBody 构造 POST /api/clusters/{id}/nodes/register 请求体（与
// /api/agent/enroll 同形、无 token 字段）。
func registerBody(hostname, machineID, pub string) string {
	return `{"hostname":"` + hostname + `","machineId":"` + machineID +
		`","osVersion":"Windows Server 2022","agentVersion":"0.1.0","publicKey":"` + pub + `"}`
}

// TestUserNodeRegister 覆盖 spec §6.4：成员注册成功 + audit register
// {userId, clusterId}；viewer 亦可（任一成员，非 admin-only）；非成员 403；
// 未知 cluster 404；未认证 401；token 字段 400；缺参/坏公钥 400；machineId
// 已属其他 cluster → 409 MACHINE_ID_CONFLICT（响应含冲突 cluster 名）；
// 同 cluster 异 key → 409 NODE_ALREADY_ENROLLED（与跨 cluster 冲突可区分）。
func TestUserNodeRegister(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)
	adminID := env.AdminUUID(t)
	cluster := defaultClusterID(t, srv.URL, admin)
	register := func(token, clusterID, body string) *http.Response {
		t.Helper()
		return doJSON(t, srv.URL, "POST", "/api/clusters/"+clusterID+"/nodes/register", token, body)
	}

	pub, _, _ := ed25519.GenerateKey(nil)
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	// 1. 未认证 → 401（JWT 中间件）
	assert.Equal(t, 401, register("", cluster, registerBody("H", "reg-mid-1", pubB64)).StatusCode)

	// 2. 未知 cluster → 404 CLUSTER_NOT_FOUND
	resp := register(admin, uuid.New().String(), registerBody("H", "reg-mid-1", pubB64))
	require.Equal(t, 404, resp.StatusCode)
	var nf struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, decodeJSON(resp.Body, &nf))
	assert.Equal(t, "CLUSTER_NOT_FOUND", nf.Error.Code)

	// 3. 非成员 → 403 FORBIDDEN（spec §6.4 明示 403，不走 404 隐藏）
	_, outsiderTok := createUserViaAPI(t, srv.URL, admin, "outsider@t.local", "Outsider")
	resp = register(outsiderTok, cluster, registerBody("OUT-01", "reg-mid-out", pubB64))
	require.Equal(t, 403, resp.StatusCode)
	var fb struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, decodeJSON(resp.Body, &fb))
	assert.Equal(t, "FORBIDDEN", fb.Error.Code)

	// 4. 成员注册成功 → 201 {nodeId, clusterId, name}
	resp = register(admin, cluster, registerBody("WEB-01", "reg-mid-1", pubB64))
	require.Equal(t, 201, resp.StatusCode)
	var node struct {
		NodeID    string `json:"nodeId"`
		ClusterID string `json:"clusterId"`
		Name      string `json:"name"`
	}
	require.NoError(t, decodeJSON(resp.Body, &node))
	require.NotEmpty(t, node.NodeID)
	assert.Equal(t, cluster, node.ClusterID)
	assert.Equal(t, "WEB-01", node.Name)

	// 5. audit register：user_id=调用者、cluster_id、node_id（spec：取代
	// token 创建+使用两条审计的拼接）
	var actor, cid, nid string
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT user_id::text, cluster_id::text, node_id::text FROM audit_logs
		 WHERE action='register' AND node_id::text=$1`, node.NodeID).Scan(&actor, &cid, &nid))
	assert.Equal(t, adminID.String(), actor)
	assert.Equal(t, cluster, cid)
	assert.Equal(t, node.NodeID, nid)

	// 6. 幂等：同 machineId + 同 key → 同 nodeId（不新增 audit）
	resp = register(admin, cluster, registerBody("WEB-01", "reg-mid-1", pubB64))
	require.Equal(t, 201, resp.StatusCode)
	var node2 struct {
		NodeID string `json:"nodeId"`
	}
	require.NoError(t, decodeJSON(resp.Body, &node2))
	assert.Equal(t, node.NodeID, node2.NodeID)
	var nReg int
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM audit_logs WHERE action='register'`).Scan(&nReg))
	assert.Equal(t, 1, nReg)

	// 7. viewer 成员亦可注册（任一成员，非 admin-only）
	viewID, viewTok := createUserViaAPI(t, srv.URL, admin, "viewer@t.local", "Viewer")
	require.Equal(t, 201, doJSON(t, srv.URL, "POST", "/api/clusters/"+cluster+"/members",
		admin, `{"user_id":"`+viewID+`","role":"viewer"}`).StatusCode)
	assert.Equal(t, 201, register(viewTok, cluster,
		registerBody("VIEW-01", "reg-mid-view", pubB64)).StatusCode)

	// 8. 请求体含 token 字段 → 400（两条注册路径不得混用；空 token 同拒）
	assert.Equal(t, 400, register(admin, cluster,
		`{"token":"anything","hostname":"H","machineId":"reg-mid-tok",
			"osVersion":"Windows","agentVersion":"0.1.0","publicKey":"`+pubB64+`"}`).StatusCode)
	assert.Equal(t, 400, register(admin, cluster,
		`{"token":"","hostname":"H","machineId":"reg-mid-tok",
			"osVersion":"Windows","agentVersion":"0.1.0","publicKey":"`+pubB64+`"}`).StatusCode)

	// 9. 参数缺失 / 公钥非法 → 400
	assert.Equal(t, 400, register(admin, cluster,
		registerBody("H", "", pubB64)).StatusCode)
	assert.Equal(t, 400, register(admin, cluster,
		registerBody("H", "reg-mid-bad", "not-base64!!")).StatusCode)

	// 10. machineId 已属其他 cluster → 409 MACHINE_ID_CONFLICT，响应含冲突
	// cluster 名（CLI 据此提示 --force 或管理端处理）
	resp = doJSON(t, srv.URL, "POST", "/api/clusters", admin, `{"name":"second"}`)
	require.Equal(t, 201, resp.StatusCode)
	var second struct {
		ID string `json:"id"`
	}
	require.NoError(t, decodeJSON(resp.Body, &second))
	pub2, _, _ := ed25519.GenerateKey(nil)
	resp = register(admin, second.ID, registerBody("WEB-02", "reg-mid-1",
		base64.StdEncoding.EncodeToString(pub2)))
	require.Equal(t, 409, resp.StatusCode)
	var conflict struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, decodeJSON(resp.Body, &conflict))
	assert.Equal(t, "MACHINE_ID_CONFLICT", conflict.Error.Code)
	assert.Contains(t, conflict.Error.Message, `"default"`)

	// 11. 同 cluster 同 machineId 异 key → 409 NODE_ALREADY_ENROLLED（共用
	// enroll 落库路径，与跨 cluster 冲突码可区分）
	resp = register(admin, cluster, registerBody("WEB-03", "reg-mid-1",
		base64.StdEncoding.EncodeToString(pub2)))
	require.Equal(t, 409, resp.StatusCode)
	var dup struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, decodeJSON(resp.Body, &dup))
	assert.Equal(t, "NODE_ALREADY_ENROLLED", dup.Error.Code)
}
