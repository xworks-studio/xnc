package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
	"xnc/server/internal/config"
	"xnc/server/internal/registry"
)

// TestTurnStatus — GET /api/turn/status：默认 TestEnv（TurnURLs 注入）=
// urls 模式；池注入 = pool 模式带健康快照；清空 = unconfigured；
// 未认证 401；desktop 会话计数生效；凭据字段在场（探测用）。
func TestTurnStatus(t *testing.T) {
	env := NewTestEnv(t)
	token := env.AdminToken(t)

	// 未认证 → 401（getTurnStatus：本文件 helper，见下）。
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	resp := doJSON(t, srv.URL, "GET", "/api/turn/status", "", "")
	assert.Equal(t, 401, resp.StatusCode)

	// 默认（TurnURLs 配置、无池）→ urls 模式。
	s := getTurnStatus(t, token, env)
	assert.Equal(t, "urls", s["mode"])
	assert.Equal(t, "relay", s["icePolicy"])
	assert.NotEmpty(t, s["fallbackUrls"])
	assert.Empty(t, s["pool"])
	assert.Equal(t, "testuser", s["username"])   // TestEnv 注入值
	assert.Equal(t, "testcred", s["credential"]) // 与会话下发同源
}

// getTurnStatus — helper：一次性 httptest server 包 env.Router，带 token
// GET /api/turn/status 并解码为 map（勿改 testenv）。
func getTurnStatus(t *testing.T, token string, env *TestEnv) map[string]any {
	t.Helper()
	srv := httptest.NewServer(env.Router)
	t.Cleanup(srv.Close)
	resp := doJSON(t, srv.URL, "GET", "/api/turn/status", token, "")
	require.Equal(t, 200, resp.StatusCode)
	var s map[string]any
	require.NoError(t, decodeJSON(resp.Body, &s))
	return s
}

// TestTurnStatusModes — pool 模式（池注入带健康快照）与 unconfigured
// （TurnURLs 清空且无池）两种形态。
func TestTurnStatusModes(t *testing.T) {
	// pool 模式：注入池（127.0.0.1 无 coturn 也无害——初始 healthy=true，
	// 2 次探测失败才会翻转，测试窗口内不会）。
	poolEnv := newTestEnvWithCfg(t, func(c *config.Config) {
		c.TurnPool = []string{"127.0.0.1:3478"}
		c.TurnUsername = "u1"
		c.TurnCredential = "p1"
	})
	s := getTurnStatus(t, poolEnv.AdminToken(t), poolEnv)
	require.Equal(t, "pool", s["mode"])
	pool := s["pool"].([]any)
	require.Len(t, pool, 1)
	m := pool[0].(map[string]any)
	assert.Equal(t, "127.0.0.1", m["ip"])
	assert.Equal(t, float64(3478), m["port"])
	assert.Equal(t, true, m["healthy"])
	assert.Equal(t, "u1", s["username"])

	// unconfigured：清空 TurnURLs 且无池。
	offEnv := newTestEnvWithCfg(t, func(c *config.Config) {
		c.TurnURLs = nil
	})
	s = getTurnStatus(t, offEnv.AdminToken(t), offEnv)
	assert.Equal(t, "unconfigured", s["mode"])
	assert.Equal(t, "", s["username"])
}

// TestTurnStatusCountsDesktopSessions — desktop 会话计数生效（env.Sess 直建，
// 端点读同一共享 manager；节点须在 registry 在线，Create 的 Online 检查才过）。
func TestTurnStatusCountsDesktopSessions(t *testing.T) {
	env := NewTestEnv(t)
	node := uuid.New()
	env.reg.Add(&registry.NodeConn{NodeID: node.String(), LastBeat: time.Now()})
	if _, err := env.Sess.Create(node, uuid.New(), proto.KindDesktop, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("create desktop: %v", err)
	}
	s := getTurnStatus(t, env.AdminToken(t), env)
	assert.Equal(t, float64(1), s["activeDesktopSessions"])
}
