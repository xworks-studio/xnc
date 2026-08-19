package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

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
