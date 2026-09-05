package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/server/internal/config"
)

func TestLoginFlow(t *testing.T) {
	env := NewTestEnv(t) // 起 PG + bootstrap admin + router
	srv := httptest.NewServer(env.Router)
	defer srv.Close()

	// 错误密码 → 401 UNAUTHORIZED
	resp, err := http.Post(srv.URL+"/api/auth/login", "application/json",
		bytes.NewBufferString(`{"email":"admin@t.local","password":"wrong"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 401, resp.StatusCode)

	// 正确登录
	resp2, err := http.Post(srv.URL+"/api/auth/login", "application/json",
		bytes.NewBufferString(`{"email":"admin@t.local","password":"pw-123456"}`))
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, 200, resp2.StatusCode)
	var body struct {
		Token string `json:"token"`
	}
	require.NoError(t, decodeJSON(resp2.Body, &body))
	require.NotEmpty(t, body.Token)

	// me 未认证 → 401
	resp3, err := http.Get(srv.URL + "/api/auth/me")
	require.NoError(t, err)
	defer resp3.Body.Close()
	assert.Equal(t, 401, resp3.StatusCode)

	// me 带token → 200
	req, _ := http.NewRequest("GET", srv.URL+"/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+body.Token)
	resp4, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp4.Body.Close()
	assert.Equal(t, 200, resp4.StatusCode)
	_ = config.Config{}
}

// TestProfileUpdateAndChangePassword — 自助档案端点（设计 §3.3）：
// display_name 自助修改（TrimSpace/长度/清除/鉴权）与改密（current 校验、
// 强度、生效后新旧密码翻转、审计行）。
func TestProfileUpdateAndChangePassword(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t) // bootstrap admin，初始密码 pw-123456

	// PATCH /api/auth/me：TrimSpace 生效。
	resp := doJSON(t, srv.URL, "PATCH", "/api/auth/me", admin,
		`{"display_name":"  Boss  "}`)
	require.Equal(t, 200, resp.StatusCode)
	var out struct {
		User struct {
			Email       string `json:"email"`
			DisplayName string `json:"display_name"`
		} `json:"user"`
	}
	require.NoError(t, decodeJSON(resp.Body, &out))
	assert.Equal(t, "Boss", out.User.DisplayName)

	// >64 字符 → 400。
	resp = doJSON(t, srv.URL, "PATCH", "/api/auth/me", admin,
		`{"display_name":"`+strings.Repeat("x", 65)+`"}`)
	assert.Equal(t, 400, resp.StatusCode)

	// 空 display_name 合法（清除显示名）。
	resp = doJSON(t, srv.URL, "PATCH", "/api/auth/me", admin, `{"display_name":""}`)
	require.Equal(t, 200, resp.StatusCode)

	// 未认证 → 401。
	resp = doJSON(t, srv.URL, "PATCH", "/api/auth/me", "", `{"display_name":"x"}`)
	assert.Equal(t, 401, resp.StatusCode)

	// POST /api/auth/password：current 错 → 401（与 login 同文案）。
	resp = doJSON(t, srv.URL, "POST", "/api/auth/password", admin,
		`{"current_password":"wrong-password","new_password":"pw-654321"}`)
	assert.Equal(t, 401, resp.StatusCode)

	// 新密码 <8 → 400。
	resp = doJSON(t, srv.URL, "POST", "/api/auth/password", admin,
		`{"current_password":"pw-123456","new_password":"short"}`)
	assert.Equal(t, 400, resp.StatusCode)

	// 缺字段 → 400。
	resp = doJSON(t, srv.URL, "POST", "/api/auth/password", admin,
		`{"current_password":"pw-123456"}`)
	assert.Equal(t, 400, resp.StatusCode)

	// 正确 current → 204；旧密码 login 401、新密码 login 200。
	resp = doJSON(t, srv.URL, "POST", "/api/auth/password", admin,
		`{"current_password":"pw-123456","new_password":"pw-654321"}`)
	require.Equal(t, 204, resp.StatusCode)

	respOld, err := http.Post(srv.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"email":"admin@t.local","password":"pw-123456"}`))
	require.NoError(t, err)
	defer respOld.Body.Close()
	assert.Equal(t, 401, respOld.StatusCode)

	respNew, err := http.Post(srv.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"email":"admin@t.local","password":"pw-654321"}`))
	require.NoError(t, err)
	defer respNew.Body.Close()
	assert.Equal(t, 200, respNew.StatusCode)

	// 审计行（background-ctx 写入，稍候轮询到即可）。
	require.Eventually(t, func() bool {
		var n int
		err := env.Store.Pool().QueryRow(context.Background(),
			`SELECT count(*) FROM audit_logs WHERE action='user.password_change'`).Scan(&n)
		return err == nil && n == 1
	}, 5*time.Second, 100*time.Millisecond)
}
