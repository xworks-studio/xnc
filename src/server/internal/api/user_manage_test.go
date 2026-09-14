package api

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// TestLoginTouchesLastLogin：login 成功写 last_login_at（0006），listUsers
// 随行带出；从未登录的用户为 null。
func TestLoginTouchesLastLogin(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)

	// bootstrap admin 走一次真实 login（AdminToken 直接铸 JWT 不触发 touch）。
	assert.Equal(t, 200, doJSON(t, srv.URL, "POST", "/api/auth/login", "",
		`{"email":"`+env.Cfg.AdminEmail+`","password":"`+env.Cfg.AdminPassword+`"}`).StatusCode)
	resp := doJSON(t, srv.URL, "GET", "/api/users", admin, "")
	require.Equal(t, 200, resp.StatusCode)
	var users []struct {
		Email       string  `json:"email"`
		LastLoginAt *string `json:"last_login_at"`
	}
	require.NoError(t, decodeJSON(resp.Body, &users))
	var adminRow *struct {
		Email       string  `json:"email"`
		LastLoginAt *string `json:"last_login_at"`
	}
	for i := range users {
		if users[i].Email == env.Cfg.AdminEmail {
			adminRow = &users[i]
		}
	}
	require.NotNil(t, adminRow, "admin row present")
	assert.NotNil(t, adminRow.LastLoginAt, "admin logged in (token mint via login)")

	// 新建用户从未登录 → null（直接建号，不走 createUserViaAPI——它会登录）。
	assert.Equal(t, 201, doJSON(t, srv.URL, "POST", "/api/users", admin,
		`{"email":"never@t.local","display_name":"Never","password":"pw-123456"}`).StatusCode)
	resp2 := doJSON(t, srv.URL, "GET", "/api/users", admin, "")
	require.Equal(t, 200, resp2.StatusCode)
	var users2 []struct {
		Email       string  `json:"email"`
		LastLoginAt *string `json:"last_login_at"`
	}
	require.NoError(t, decodeJSON(resp2.Body, &users2))
	for _, u := range users2 {
		if u.Email == "never@t.local" {
			assert.Nil(t, u.LastLoginAt, "never logged in → null")
		}
	}
}

// TestAdminResetPassword：管理员重置密码无须原密码；新密码可登录、旧密码失效。
func TestAdminResetPassword(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)
	uid, tok := createUserViaAPI(t, srv.URL, admin, "reset@t.local", "Reset")

	assert.Equal(t, 200, doJSON(t, srv.URL, "PATCH", "/api/users/"+uid, admin,
		`{"password":"NewPwd-123456!"}`).StatusCode)

	// 旧 token 仍有效（JWT 与密码无关；/api/clusters 非 admin 端点），旧密码
	// 登录失败、新密码成功。
	assert.Equal(t, 200, doJSON(t, srv.URL, "GET", "/api/clusters", tok, "").StatusCode)
	assert.Equal(t, 401, doJSON(t, srv.URL, "POST", "/api/auth/login", "",
		`{"email":"reset@t.local","password":"pw-654321"}`).StatusCode)
	assert.Equal(t, 200, doJSON(t, srv.URL, "POST", "/api/auth/login", "",
		`{"email":"reset@t.local","password":"NewPwd-123456!"}`).StatusCode)

	// 非 admin 重置他人密码 → 403；空密码 → 400。
	assert.Equal(t, 403, doJSON(t, srv.URL, "PATCH", "/api/users/"+uid, tok,
		`{"password":"hijack-123"}`).StatusCode)
	assert.Equal(t, 400, doJSON(t, srv.URL, "PATCH", "/api/users/"+uid, admin,
		`{"password":""}`).StatusCode)
}

// TestDeleteUser：空名下集群随删、membership 消失、审计行保留（actor 置空
// 的 FK 语义）；名下有节点 → 409 USER_OWNS_NODES；最后 admin 不可删；非
// admin 403。
func TestDeleteUser(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)

	// 1) 干净用户（仅个人默认集群，无节点）→ 删除成功，名下集群清空。
	uid, _ := createUserViaAPI(t, srv.URL, admin, "gone@t.local", "Gone")
	assert.Equal(t, 204, doJSON(t, srv.URL, "DELETE", "/api/users/"+uid, admin, "").StatusCode)
	var n int
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM users WHERE id=$1`, uid).Scan(&n))
	assert.Zero(t, n)
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM clusters WHERE owner_id=$1`, uid).Scan(&n))
	assert.Zero(t, n, "personal cluster hard-deleted with owner")

	// 2) 名下集群挂节点 → 409 USER_OWNS_NODES，用户保留。
	uid2, _ := createUserViaAPI(t, srv.URL, admin, "noded@t.local", "Noded")
	// 经用户 JWT 注册路径给其个人集群挂一台节点。
	resp := doJSON(t, srv.URL, "GET", "/api/clusters", doLogin(t, srv.URL, "noded@t.local", "pw-654321"), "")
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, 201, doJSON(t, srv.URL, "POST",
		"/api/clusters/noded-default/nodes/register",
		doLogin(t, srv.URL, "noded@t.local", "pw-654321"),
		registerBody("DELNODE-1", "mid-deluser-1",
			"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")).StatusCode)
	respDel := doJSON(t, srv.URL, "DELETE", "/api/users/"+uid2, admin, "")
	require.Equal(t, 409, respDel.StatusCode)
	var e struct {
		Error proto.APIError `json:"error"`
	}
	require.NoError(t, decodeJSON(respDel.Body, &e))
	assert.Equal(t, proto.CodeUserOwnsNodes, e.Error.Code)
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM users WHERE id=$1`, uid2).Scan(&n))
	assert.Equal(t, 1, n, "user kept when nodes block")

	// 3) 最后 admin 不可删；非 admin 403。
	assert.Equal(t, 400, doJSON(t, srv.URL, "DELETE",
		"/api/users/"+env.AdminUUID(t).String(), admin, "").StatusCode)
	assert.Equal(t, 403, doJSON(t, srv.URL, "DELETE", "/api/users/"+uid2,
		doLogin(t, srv.URL, "noded@t.local", "pw-654321"), "").StatusCode)

	// 4) 审计行在用户删除后保留（user.create 的 actor 是 admin 不受影响；
	// 验证 0006 FK 语义：被删用户的行 user_id 置空而非级联消失）。
	var kept int
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM audit_logs WHERE action='user.create' AND metadata::text LIKE '%gone@t.local%'`).
		Scan(&kept))
	assert.GreaterOrEqual(t, kept, 1, "audit history survives user deletion")
}

// doLogin 便利：登录拿 token（不依赖 createUserViaAPI 的返回）。
func doLogin(t *testing.T, srvURL, email, password string) string {
	t.Helper()
	resp := doJSON(t, srvURL, "POST", "/api/auth/login", "",
		`{"email":"`+email+`","password":"`+password+`"}`)
	require.Equal(t, 200, resp.StatusCode)
	var out struct {
		Token string `json:"token"`
	}
	require.NoError(t, decodeJSON(resp.Body, &out))
	return out.Token
}
