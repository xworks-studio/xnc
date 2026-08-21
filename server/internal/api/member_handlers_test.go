package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createUserViaAPI 走 T1 的 admin 端点建用户并登录拿 token（返回 userID + token）。
func createUserViaAPI(t *testing.T, srvURL, adminToken, email, name string) (string, string) {
	t.Helper()
	resp := doJSON(t, srvURL, "POST", "/api/users", adminToken,
		`{"email":"`+email+`","display_name":"`+name+`","password":"pw-654321"}`)
	require.Equal(t, 201, resp.StatusCode)
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, decodeJSON(resp.Body, &created))

	resp2, err := http.Post(srvURL+"/api/auth/login", "application/json",
		strings.NewReader(`{"email":"`+email+`","password":"pw-654321"}`))
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, 200, resp2.StatusCode)
	var login struct {
		Token string `json:"token"`
	}
	require.NoError(t, decodeJSON(resp2.Body, &login))
	require.NotEmpty(t, login.Token)
	return created.ID, login.Token
}

// defaultClusterID 经 GET /api/clusters 取 "default" cluster 的 UUID。
func defaultClusterID(t *testing.T, srvURL, adminToken string) string {
	t.Helper()
	resp := doJSON(t, srvURL, "GET", "/api/clusters", adminToken, "")
	require.Equal(t, 200, resp.StatusCode)
	var list []map[string]any
	require.NoError(t, decodeJSON(resp.Body, &list))
	for _, c := range list {
		if c["name"] == "default" {
			return c["id"].(string)
		}
	}
	t.Fatal("default cluster not found")
	return ""
}

func TestMemberManagement(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)
	cluster := defaultClusterID(t, srv.URL, admin)
	opID, opTok := createUserViaAPI(t, srv.URL, admin, "op@t.local", "Op")
	viewID, viewTok := createUserViaAPI(t, srv.URL, admin, "view@t.local", "View")
	extraID, extraTok := createUserViaAPI(t, srv.URL, admin, "extra@t.local", "Extra")
	adminID := env.AdminUUID(t).String()

	// 未认证 → 401
	assert.Equal(t, 401, doJSON(t, srv.URL, "GET", "/api/clusters/"+cluster+"/members", "", "").StatusCode)

	// 非 member 列表 → 404（不泄漏 cluster 存在性）
	assert.Equal(t, 404, doJSON(t, srv.URL, "GET",
		"/api/clusters/"+cluster+"/members", opTok, "").StatusCode)

	// owner（admin）列表 → 含 admin，role=owner
	resp := doJSON(t, srv.URL, "GET", "/api/clusters/"+cluster+"/members", admin, "")
	require.Equal(t, 200, resp.StatusCode)
	var members []map[string]any
	require.NoError(t, decodeJSON(resp.Body, &members))
	require.Len(t, members, 1)
	assert.Equal(t, adminID, members[0]["user_id"])
	assert.Equal(t, "admin@t.local", members[0]["email"])
	assert.Equal(t, "owner", members[0]["role"])

	// 非 owner（普通用户）添加 → 403
	resp2 := doJSON(t, srv.URL, "POST", "/api/clusters/"+cluster+"/members", opTok,
		`{"user_id":"`+viewID+`","role":"viewer"}`)
	assert.Equal(t, 403, resp2.StatusCode)

	// owner 添加 operator → 201
	resp3 := doJSON(t, srv.URL, "POST", "/api/clusters/"+cluster+"/members", admin,
		`{"user_id":"`+opID+`","role":"operator"}`)
	assert.Equal(t, 201, resp3.StatusCode)

	// owner 添加 viewer → 201
	resp4 := doJSON(t, srv.URL, "POST", "/api/clusters/"+cluster+"/members", admin,
		`{"user_id":"`+viewID+`","role":"viewer"}`)
	assert.Equal(t, 201, resp4.StatusCode)

	// 重复添加 → 409
	resp5 := doJSON(t, srv.URL, "POST", "/api/clusters/"+cluster+"/members", admin,
		`{"user_id":"`+opID+`","role":"viewer"}`)
	assert.Equal(t, 409, resp5.StatusCode)

	// 非法 role → 400
	resp6 := doJSON(t, srv.URL, "POST", "/api/clusters/"+cluster+"/members", admin,
		`{"user_id":"`+extraID+`","role":"root"}`)
	assert.Equal(t, 400, resp6.StatusCode)

	// 非法 user_id → 400
	resp7 := doJSON(t, srv.URL, "POST", "/api/clusters/"+cluster+"/members", admin,
		`{"user_id":"not-a-uuid","role":"viewer"}`)
	assert.Equal(t, 400, resp7.StatusCode)

	// 列表（成员即可，viewer 亦允许）→ 三人，role 正确
	resp8 := doJSON(t, srv.URL, "GET", "/api/clusters/"+cluster+"/members", viewTok, "")
	require.Equal(t, 200, resp8.StatusCode)
	var members2 []map[string]any
	require.NoError(t, decodeJSON(resp8.Body, &members2))
	assert.Len(t, members2, 3)
	roles := map[string]string{}
	for _, m := range members2 {
		roles[m["email"].(string)] = m["role"].(string)
	}
	assert.Equal(t, "owner", roles["admin@t.local"])
	assert.Equal(t, "operator", roles["op@t.local"])
	assert.Equal(t, "viewer", roles["view@t.local"])

	// 非 owner 删除 → 403
	resp9 := doJSON(t, srv.URL, "DELETE",
		"/api/clusters/"+cluster+"/members/"+viewID, opTok, "")
	assert.Equal(t, 403, resp9.StatusCode)

	// 移除唯一 owner → 400
	resp10 := doJSON(t, srv.URL, "DELETE",
		"/api/clusters/"+cluster+"/members/"+adminID, admin, "")
	assert.Equal(t, 400, resp10.StatusCode)

	// owner 移除 viewer → 204
	resp11 := doJSON(t, srv.URL, "DELETE",
		"/api/clusters/"+cluster+"/members/"+viewID, admin, "")
	assert.Equal(t, 204, resp11.StatusCode)

	// 移除后列表只剩 owner+operator
	resp12 := doJSON(t, srv.URL, "GET", "/api/clusters/"+cluster+"/members", opTok, "")
	require.Equal(t, 200, resp12.StatusCode)
	var members3 []map[string]any
	require.NoError(t, decodeJSON(resp12.Body, &members3))
	assert.Len(t, members3, 2)

	// 加回第二个 owner 后可移除（非唯一 owner）
	resp13 := doJSON(t, srv.URL, "POST", "/api/clusters/"+cluster+"/members", admin,
		`{"user_id":"`+extraID+`","role":"owner"}`)
	assert.Equal(t, 201, resp13.StatusCode)
	resp14 := doJSON(t, srv.URL, "DELETE",
		"/api/clusters/"+cluster+"/members/"+adminID, extraTok, "")
	assert.Equal(t, 204, resp14.StatusCode)

	// 审计：cluster.member.add（op operator / view viewer / extra owner）与 remove
	rows, err := env.Store.Pool().Query(t.Context(),
		`SELECT action, metadata::text FROM audit_logs
		 WHERE action IN ('cluster.member.add','cluster.member.remove') ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	var adds, removes int
	for rows.Next() {
		var action, meta string
		require.NoError(t, rows.Scan(&action, &meta))
		switch action {
		case "cluster.member.add":
			adds++
			assert.Contains(t, meta, `"role"`)
		case "cluster.member.remove":
			removes++
		}
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, 3, adds)
	assert.GreaterOrEqual(t, removes, 1)
}
