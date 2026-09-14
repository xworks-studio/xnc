package api

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// TestAdminNodeDisableEnable：node disable/enable 全生命周期。
// owner disable → 会话创建 403 NODE_DISABLED → owner enable → exec 恢复 202；
// 管理动作本身 owner-only（operator/viewer 403、非成员 404）；审计行成对。
func TestAdminNodeDisableEnable(t *testing.T) {
	f := newRBACFixture(t)
	disable := "/api/nodes/" + f.nodeID + "/disable"
	enable := "/api/nodes/" + f.nodeID + "/enable"

	// 未认证 → 401
	assert.Equal(t, 401, doJSON(t, f.srv.URL, "POST", disable, "", "").StatusCode)

	// operator/viewer → 403 FORBIDDEN（owner-only，与角色矩阵一致）
	code, errCode := f.post(t, f.tokens["operator"], disable, "")
	assert.Equal(t, 403, code)
	assert.Equal(t, proto.CodeForbidden, errCode)
	code, errCode = f.post(t, f.tokens["viewer"], disable, "")
	assert.Equal(t, 403, code)
	assert.Equal(t, proto.CodeForbidden, errCode)

	// 非成员 → 404（不泄漏节点存在性）；非法 UUID 同为 404
	code, errCode = f.post(t, f.tokens["outsider"], disable, "")
	assert.Equal(t, 404, code)
	assert.Equal(t, proto.CodeNodeNotFound, errCode)
	code, errCode = f.post(t, f.tokens["owner"],
		"/api/nodes/not-a-uuid/disable", "")
	assert.Equal(t, 404, code)
	assert.Equal(t, proto.CodeNodeNotFound, errCode)

	// owner disable → 204，节点状态翻转 disabled
	assert.Equal(t, 204, doJSON(t, f.srv.URL, "POST", disable, f.tokens["owner"], "").StatusCode)
	assert.Equal(t, "disabled", nodeStatus(t, f, f.nodeID))

	// disabled 期间：owner/operator 会话创建全被 403 NODE_DISABLED 拒绝
	//（requireMinRole 的 disabled 分支，控制连接在线也无效）
	for _, user := range []string{"owner", "operator"} {
		code, errCode = f.post(t, f.tokens[user],
			"/api/nodes/"+f.nodeID+"/exec", `{"command":"hostname"}`)
		assert.Equal(t, 403, code, user)
		assert.Equal(t, proto.CodeNodeDisabled, errCode, user)
	}

	// operator enable → 仍 403（enable 同为 owner-only）
	code, errCode = f.post(t, f.tokens["operator"], enable, "")
	assert.Equal(t, 403, code)
	assert.Equal(t, proto.CodeForbidden, errCode)

	// owner enable → 204，状态回 offline（不直跳 online——那由 agent 心跳驱动）
	assert.Equal(t, 204, doJSON(t, f.srv.URL, "POST", enable, f.tokens["owner"], "").StatusCode)
	assert.Equal(t, "offline", nodeStatus(t, f, f.nodeID))

	// enable 后会话恢复：控制连接在线 → exec 202
	code, _ = f.post(t, f.tokens["operator"],
		"/api/nodes/"+f.nodeID+"/exec", `{"command":"hostname"}`)
	assert.Equal(t, 202, code)

	// 审计：node.disable / node.enable 各一行，actor/cluster/node 维度齐全
	rows, err := f.env.Store.Pool().Query(t.Context(),
		`SELECT a.action, u.email, a.cluster_id::text, a.node_id::text
		 FROM audit_logs a JOIN users u ON u.id = a.user_id
		 WHERE a.action IN ('node.disable','node.enable') ORDER BY a.id`)
	require.NoError(t, err)
	defer rows.Close()
	var got []string
	for rows.Next() {
		var action, actor, cluster, node string
		require.NoError(t, rows.Scan(&action, &actor, &cluster, &node))
		assert.Equal(t, "rbac-owner@t.local", actor, action)
		assert.NotEmpty(t, cluster, action)
		assert.Equal(t, f.nodeID, node, action)
		got = append(got, action)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"node.disable", "node.enable"}, got)
}

// nodeStatus 经 GET /api/nodes/{id} 读取节点当前 status。
func nodeStatus(t *testing.T, f *rbacFixture, nodeID string) string {
	t.Helper()
	resp := doJSON(t, f.srv.URL, "GET", "/api/nodes/"+nodeID, f.tokens["owner"], "")
	require.Equal(t, 200, resp.StatusCode)
	var node struct {
		Status string `json:"status"`
	}
	require.NoError(t, decodeJSON(resp.Body, &node))
	return node.Status
}

// TestAdminClusterDelete：软删除语义（0005 起 = 打 deleted_at，原名保留、
// 行保留、原名经存活行部分唯一索引可复用）；有节点 → 409
// CLUSTER_NOT_EMPTY；非 owner → 403；审计 cluster.delete。
func TestAdminClusterDelete(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)

	// default cluster 有已注册节点 → owner 删除 → 409 CLUSTER_NOT_EMPTY
	env.EnrollNode(t, "DEL-1", "mid-del-1")
	resp := doJSON(t, srv.URL, "DELETE", "/api/clusters/default", admin, "")
	require.Equal(t, 409, resp.StatusCode)
	var e struct {
		Error proto.APIError `json:"error"`
	}
	require.NoError(t, decodeJSON(resp.Body, &e))
	assert.Equal(t, proto.CodeClusterNotEmpty, e.Error.Code)

	// 新建空 cluster；非 owner（普通用户，未加入）删除 → 403
	resp2 := doJSON(t, srv.URL, "POST", "/api/clusters", admin, `{"name":"del-me"}`)
	require.Equal(t, 201, resp2.StatusCode)
	_, plainTok := createUserViaAPI(t, srv.URL, admin, "del-view@t.local", "Del")
	assert.Equal(t, 403, doJSON(t, srv.URL, "DELETE",
		"/api/clusters/del-me", plainTok, "").StatusCode)
	// 不存在的 cluster → 404
	assert.Equal(t, 404, doJSON(t, srv.URL, "DELETE",
		"/api/clusters/no-such", admin, "").StatusCode)

	// owner 删除空 cluster → 204；行保留原名 + deleted_at 打标（不再改名）
	assert.Equal(t, 204, doJSON(t, srv.URL, "DELETE",
		"/api/clusters/del-me", admin, "").StatusCode)
	var deletedAt string
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT deleted_at::text FROM clusters WHERE name='del-me'`).Scan(&deletedAt))
	assert.NotEmpty(t, deletedAt, "row tombstoned in place with original name")
	// 已删 cluster 按 name 解析 → 404；同名重建 → 201（部分唯一索引）。
	assert.Equal(t, 404, doJSON(t, srv.URL, "DELETE",
		"/api/clusters/del-me", admin, "").StatusCode)
	assert.Equal(t, 201, doJSON(t, srv.URL, "POST", "/api/clusters", admin,
		`{"name":"del-me"}`).StatusCode)

	// 审计：cluster.delete，metadata 含原名
	var action, meta string
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT action, metadata::text FROM audit_logs WHERE action='cluster.delete'`).
		Scan(&action, &meta))
	assert.Equal(t, "cluster.delete", action)
	assert.Contains(t, meta, "del-me")
}
