package api

import (
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// userIDByEmail 经 DB 查用户 id（测试布景用，避免重复走 API）。
func userIDByEmail(t *testing.T, f *rbacFixture, email string) string {
	t.Helper()
	var id string
	require.NoError(t, f.env.Store.Pool().QueryRow(t.Context(),
		`SELECT id::text FROM users WHERE email=$1`, email).Scan(&id))
	return id
}

// auditRows 拉取 /api/audit 并断言 200。
func auditRows(t *testing.T, f *rbacFixture, tok, query string) []map[string]any {
	t.Helper()
	resp := doJSON(t, f.srv.URL, "GET", "/api/audit"+query, tok, "")
	require.Equal(t, 200, resp.StatusCode, query)
	var rows []map[string]any
	require.NoError(t, decodeJSON(resp.Body, &rows), query)
	return rows
}

// TestAuditQuery：admin-only 审计查询——可选过滤（nodeId/userId/action/since）
// 可组合、created_at DESC、LIMIT/OFFSET 分页、非法参数 400。
// 布景复用 rbacFixture：其 owner（任一 cluster owner）即 admin，operator/
// viewer 非 admin；disable/enable 再补两行带 node 维度的审计。
func TestAuditQuery(t *testing.T) {
	f := newRBACFixture(t)
	assert.Equal(t, 204, doJSON(t, f.srv.URL, "POST",
		"/api/nodes/"+f.nodeID+"/disable", f.tokens["owner"], "").StatusCode)
	assert.Equal(t, 204, doJSON(t, f.srv.URL, "POST",
		"/api/nodes/"+f.nodeID+"/enable", f.tokens["owner"], "").StatusCode)
	ownerID := userIDByEmail(t, f, "rbac-owner@t.local")

	// 未认证 → 401；非 admin（operator/viewer）→ 403
	assert.Equal(t, 401, doJSON(t, f.srv.URL, "GET", "/api/audit", "", "").StatusCode)
	for _, tok := range []string{f.tokens["operator"], f.tokens["viewer"]} {
		assert.Equal(t, 403, doJSON(t, f.srv.URL, "GET", "/api/audit", tok, "").StatusCode)
	}

	// admin 全量：非空、created_at 严格 DESC、行形态齐全（metadata 为对象）
	all := auditRows(t, f, f.tokens["owner"], "")
	require.Greater(t, len(all), 5)
	for i := 1; i < len(all); i++ {
		prev, _ := time.Parse(time.RFC3339Nano, all[i-1]["created_at"].(string))
		cur, _ := time.Parse(time.RFC3339Nano, all[i]["created_at"].(string))
		assert.False(t, cur.After(prev), "created_at must be DESC")
	}
	assert.NotZero(t, all[0]["id"])
	assert.IsType(t, map[string]any{}, all[0]["metadata"], "metadata must be an object")

	// action 过滤：只余 node.disable
	byAction := auditRows(t, f, f.tokens["owner"], "?action=node.disable")
	require.Greater(t, len(byAction), 0)
	for _, row := range byAction {
		assert.Equal(t, "node.disable", row["action"])
	}

	// nodeId 过滤：全部挂在该节点，含 node.enroll 与 node.disable
	byNode := auditRows(t, f, f.tokens["owner"], "?nodeId="+f.nodeID)
	require.Greater(t, len(byNode), 0)
	actions := map[string]bool{}
	for _, row := range byNode {
		assert.Equal(t, f.nodeID, row["node_id"])
		actions[row["action"].(string)] = true
	}
	assert.True(t, actions["node.enroll"])
	assert.True(t, actions["node.disable"])

	// userId 过滤：全部为该 actor；组合 nodeId+action 进一步收窄
	byUser := auditRows(t, f, f.tokens["owner"], "?userId="+ownerID)
	require.Greater(t, len(byUser), 0)
	for _, row := range byUser {
		assert.Equal(t, ownerID, row["user_id"])
	}
	combo := auditRows(t, f, f.tokens["owner"],
		"?nodeId="+f.nodeID+"&action=node.enable")
	require.Len(t, combo, 1)
	assert.Equal(t, f.nodeID, combo[0]["node_id"])

	// since：相对时长（1h/7d）命中；RFC3339 未来时刻 → 空
	assert.NotEmpty(t, auditRows(t, f, f.tokens["owner"], "?since=1h"))
	assert.NotEmpty(t, auditRows(t, f, f.tokens["owner"], "?since=7d"))
	// since：相对时长（1h/7d）命中；RFC3339 未来时刻 → 空（QueryEscape——
	// 本机时区 +08:00 的 + 在 query 里原样会被解码成空格）
	future := url.QueryEscape(time.Now().Add(time.Hour).Format(time.RFC3339))
	assert.Empty(t, auditRows(t, f, f.tokens["owner"], "?since="+future))

	// 非法参数 → 400（不静默吞）
	for _, q := range []string{
		"?nodeId=not-a-uuid", "?userId=not-a-uuid", "?since=nonsense",
		"?since=-1h", "?limit=abc", "?limit=0", "?offset=-1",
	} {
		resp := doJSON(t, f.srv.URL, "GET", "/api/audit"+q, f.tokens["owner"], "")
		assert.Equal(t, 400, resp.StatusCode, q)
	}
	// 超上限 limit 不报错（钳制到 200）
	assert.NotEmpty(t, auditRows(t, f, f.tokens["owner"], "?limit=1000"))

	// 分页：limit=2 两页与全量前缀一致且不相交
	page1 := auditRows(t, f, f.tokens["owner"], "?limit=2&offset=0")
	page2 := auditRows(t, f, f.tokens["owner"], "?limit=2&offset=2")
	require.Len(t, page1, 2)
	require.Len(t, page2, 2)
	for i := range page1 {
		assert.Equal(t, all[i]["id"], page1[i]["id"])
		assert.Equal(t, all[i+2]["id"], page2[i]["id"])
		assert.NotEqual(t, page1[i]["id"], page2[i]["id"])
	}
}
