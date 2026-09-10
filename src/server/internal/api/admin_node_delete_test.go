package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAdminDeleteNode：管理端 DELETE /api/nodes/{id}（admin-only，与
// adminDeleteRelease 同款 isAdminUser 判定）：删除节点行（复用 WS NODE_DELETE
// 的 DeleteNode 落库）+ 审计 node_delete {userId, nodeId} + 逐出在线控制连接
// （registry 移除 + Cancel，被逐连接读到关闭）；非 admin（cluster 成员但非
// owner）403；未认证 401；未知/非法 id 404；重复删除 404（行已不存在）。
func TestAdminDeleteNode(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)
	adminID := env.AdminUUID(t)

	del := func(token, id string) *http.Response {
		t.Helper()
		return doJSON(t, srv.URL, "DELETE", "/api/nodes/"+id, token, "")
	}

	// viewer：default cluster 成员但非任何 cluster owner → 非 admin。
	viewID, viewTok := createUserViaAPI(t, srv.URL, admin, "viewer-del@t.local", "Viewer")
	require.Equal(t, 201, doJSON(t, srv.URL, "POST", "/api/clusters/default/members",
		admin, `{"user_id":"`+viewID+`","role":"viewer"}`).StatusCode)

	nodeID := env.EnrollNode(t, "WEB-ADM-DEL", "mid-adm-del")

	// 1. 未认证 → 401（JWT 中间件）
	assert.Equal(t, 401, del("", nodeID).StatusCode)
	// 2. 非 admin → 403（isAdminUser：任一 cluster owner 即 admin；viewer 不是）
	assert.Equal(t, 403, del(viewTok, nodeID).StatusCode)
	// 3. 未知/非法 id → 404（不泄漏存在性差异）
	assert.Equal(t, 404, del(admin, uuid.New().String()).StatusCode)
	assert.Equal(t, 404, del(admin, "not-a-uuid").StatusCode)

	// 4. 在线控制连接（模拟在线节点；删除后应被服务端逐出关闭）。
	live := dialControl(t, env, nodeID)

	// 5. admin 删除 → 200，节点行删除
	require.Equal(t, 200, del(admin, nodeID).StatusCode)
	_, err := env.Store.Q().GetNodeByID(context.Background(), mustUUID(nodeID))
	require.Error(t, err, "node row must be deleted")

	// 6. 审计 node_delete：actor=admin、target=node
	var action, actor string
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT action, user_id::text FROM audit_logs
		 WHERE node_id=$1 AND action='node_delete'`, mustUUID(nodeID)).Scan(&action, &actor))
	assert.Equal(t, "node_delete", action)
	assert.Equal(t, adminID.String(), actor)

	// 7. 在线连接被逐出（服务端 Cancel → 读错误）+ registry 表项清除
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_, _, err := live.Read(ctx)
		return err != nil
	}, 5*time.Second, 100*time.Millisecond, "live control connection must be closed after admin delete")
	assert.Nil(t, env.reg.Get(nodeID), "registry entry must be removed")

	// 8. 重复删除 → 404（行已不存在；管理端删除不按幂等成功处理——与 WS
	// NODE_DELETE 的幂等语义不同，HTTP 侧 404 更利于排障）
	assert.Equal(t, 404, del(admin, nodeID).StatusCode)
}
