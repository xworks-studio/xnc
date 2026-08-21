package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// doJSON 发送带认证头的 JSON 请求并返回响应（不关 Body，由调用方 defer）。
func doJSON(t *testing.T, srvURL, method, path, token, body string) *http.Response {
	t.Helper()
	var rd *bytes.Reader
	if body == "" {
		rd = bytes.NewReader(nil)
	} else {
		rd = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, srvURL+path, rd)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestUserCreateAndList(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t) // bootstrap admin：default cluster 的 owner → is admin

	// admin 创建用户 → 201 {id,email,display_name}
	resp := doJSON(t, srv.URL, "POST", "/api/users", admin,
		`{"email":"op@t.local","display_name":"Operator","password":"pw-654321"}`)
	require.Equal(t, 201, resp.StatusCode)
	var created struct {
		ID          string `json:"id"`
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
	}
	require.NoError(t, decodeJSON(resp.Body, &created))
	assert.NotEmpty(t, created.ID)
	assert.Equal(t, "op@t.local", created.Email)
	assert.Equal(t, "Operator", created.DisplayName)

	// 重复 email → 409
	resp2 := doJSON(t, srv.URL, "POST", "/api/users", admin,
		`{"email":"op@t.local","display_name":"Dup","password":"pw-654321"}`)
	assert.Equal(t, 409, resp2.StatusCode)

	// admin 列表 → 含 admin 与新用户，且不含 password_hash
	resp3 := doJSON(t, srv.URL, "GET", "/api/users", admin, "")
	require.Equal(t, 200, resp3.StatusCode)
	var users []map[string]any
	require.NoError(t, decodeJSON(resp3.Body, &users))
	assert.Len(t, users, 2)
	emails := map[string]bool{}
	for _, u := range users {
		emails[u["email"].(string)] = true
		assert.NotEmpty(t, u["id"])
		_, hasHash := u["password_hash"]
		assert.False(t, hasHash, "password hash must not leak")
	}
	assert.True(t, emails["admin@t.local"])
	assert.True(t, emails["op@t.local"])

	// 新用户密码有效（bcrypt hash 落库正确）：能登录
	resp4, err := http.Post(srv.URL+"/api/auth/login", "application/json",
		bytes.NewBufferString(`{"email":"op@t.local","password":"pw-654321"}`))
	require.NoError(t, err)
	defer resp4.Body.Close()
	assert.Equal(t, 200, resp4.StatusCode)
	var login struct {
		Token string `json:"token"`
	}
	require.NoError(t, decodeJSON(resp4.Body, &login))
	require.NotEmpty(t, login.Token)

	// 非 admin（无任何 cluster membership）→ POST/GET 均 403
	resp5 := doJSON(t, srv.URL, "POST", "/api/users", login.Token,
		`{"email":"x@t.local","display_name":"X","password":"pw-123456"}`)
	assert.Equal(t, 403, resp5.StatusCode)
	resp6 := doJSON(t, srv.URL, "GET", "/api/users", login.Token, "")
	assert.Equal(t, 403, resp6.StatusCode)

	// 未认证 → 401
	resp7 := doJSON(t, srv.URL, "GET", "/api/users", "", "")
	assert.Equal(t, 401, resp7.StatusCode)

	// 缺字段 → 400
	resp8 := doJSON(t, srv.URL, "POST", "/api/users", admin,
		`{"email":"no-pw@t.local","display_name":"NP"}`)
	assert.Equal(t, 400, resp8.StatusCode)

	// 审计行：user.create，actor=admin，metadata 带 email
	var action, meta string
	var actor string
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT a.action, a.metadata::text, u.email FROM audit_logs a
		 JOIN users u ON u.id = a.user_id WHERE a.action='user.create'`).Scan(&action, &meta, &actor))
	assert.Equal(t, "user.create", action)
	assert.Equal(t, "admin@t.local", actor)
	assert.Contains(t, meta, "op@t.local")
}
