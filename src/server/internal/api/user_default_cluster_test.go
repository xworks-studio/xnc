package api

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/server/internal/config"
)

// TestBootstrapAdminFlag：全新库首启的 admin 显式 is_admin=true、default
// cluster 标 personal（防 EnsureAdmin 漏设回归——0005 只回填存量库，全新库
// 不经迁移路径）。
func TestBootstrapAdminFlag(t *testing.T) {
	env := NewTestEnv(t)
	var isAdmin, personal bool
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT u.is_admin, c.personal FROM users u
		 JOIN clusters c ON c.name='default'
		 WHERE u.email=$1`, env.Cfg.AdminEmail).Scan(&isAdmin, &personal))
	assert.True(t, isAdmin, "bootstrap admin must be is_admin")
	assert.True(t, personal, "bootstrap default cluster is personal")
}

// TestCreateUserDefaultCluster：admin 建用户 → 同事务得个人默认 cluster
// （personal=true、owner 成员、命名 <localpart>-default）；并发同 localpart
// 不同域建号走 23505 重试后缀；新用户 register 前提成立（cluster 列表非空）。
func TestCreateUserDefaultCluster(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)

	_, tok := createUserViaAPI(t, srv.URL, admin, "john@a.local", "John")
	resp := doJSON(t, srv.URL, "GET", "/api/clusters", tok, "")
	require.Equal(t, 200, resp.StatusCode)
	var clusters []map[string]any
	require.NoError(t, decodeJSON(resp.Body, &clusters))
	require.Len(t, clusters, 1)
	assert.Equal(t, "john-default", clusters[0]["name"])
	assert.Equal(t, true, clusters[0]["personal"])
	assert.Equal(t, "owner", clusters[0]["role"])

	// 同 localpart 不同域：基名撞唯一索引 → 后缀重试成功（建号不失败）。
	_, tok2 := createUserViaAPI(t, srv.URL, admin, "john@b.local", "John2")
	resp2 := doJSON(t, srv.URL, "GET", "/api/clusters", tok2, "")
	require.Equal(t, 200, resp2.StatusCode)
	var clusters2 []map[string]any
	require.NoError(t, decodeJSON(resp2.Body, &clusters2))
	require.Len(t, clusters2, 1)
	assert.Equal(t, "john-default-2", clusters2[0]["name"])
}

// TestCreateClusterDoesNotGrantAdmin：非 admin 用户自建 cluster 成为 owner 后
// 仍非平台 admin（listUsers 403）——0005 最大的行为变化回归。
func TestCreateClusterDoesNotGrantAdmin(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)

	_, tok := createUserViaAPI(t, srv.URL, admin, "selfserve@t.local", "Self")
	assert.Equal(t, 201, doJSON(t, srv.URL, "POST", "/api/clusters", tok,
		`{"name":"mine"}`).StatusCode)
	assert.Equal(t, 403, doJSON(t, srv.URL, "GET", "/api/users", tok, "").StatusCode)
	assert.Equal(t, 403, doJSON(t, srv.URL, "POST", "/api/users", tok,
		`{"email":"x@t.local","password":"pw-123456"}`).StatusCode)
}

// TestClusterCreateLimit：每用户存活自有 cluster 限额（含个人默认 cluster）。
func TestClusterCreateLimit(t *testing.T) {
	env := newTestEnvWithCfg(t, func(c *config.Config) {
		c.MaxClustersPerUser = 2
	})
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)

	_, tok := createUserViaAPI(t, srv.URL, admin, "capped@t.local", "Capped")
	// 个人默认已占 1/2：再建 1 个成功、第 2 个 400。
	assert.Equal(t, 201, doJSON(t, srv.URL, "POST", "/api/clusters", tok,
		`{"name":"ok"}`).StatusCode)
	resp := doJSON(t, srv.URL, "POST", "/api/clusters", tok, `{"name":"over"}`)
	assert.Equal(t, 400, resp.StatusCode)
}
