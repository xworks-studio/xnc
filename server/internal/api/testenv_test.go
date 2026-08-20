package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"xnc/server/internal/auth"
	"xnc/server/internal/bootstrap"
	"xnc/server/internal/config"
	"xnc/server/internal/db"
	"xnc/server/internal/registry"
	"xnc/server/internal/session"
)

// TestEnv 起一个真实 PG 容器 + bootstrap admin + 完整 router，供 api 层集成测试复用。
type TestEnv struct {
	Router http.Handler
	srv    *httptest.Server
	Store  *db.Store
	Cfg    config.Config
	reg    *registry.Registry
	Sess   *session.Manager
	nodes  map[string]ed25519.PrivateKey
}

func NewTestEnv(t *testing.T) *TestEnv {
	t.Helper()
	st := db.OpenTestStore(t)
	cfg := config.Config{
		JWTSecret:        []byte("test-secret-test-secret-test!!"),
		AdminEmail:       "admin@t.local",
		AdminPassword:    "pw-123456",
		HeartbeatTimeout: 5 * time.Second,
		// 零值 TTL 会让 token 立即过期（config.Load 的默认值只在生产路径生效），测试环境须显式给出。
		EnrollTokenTTL: 30 * time.Minute,
	}
	require.NoError(t, bootstrap.EnsureAdmin(context.Background(), st, cfg))
	reg := registry.New()
	// 测试注入共享 manager（NewRouterWithSession），env.Sess 供测试直接驱动会话生命周期。
	sess := session.New(reg, slog.Default())
	h := NewRouterWithSession(st, cfg, reg, sess)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &TestEnv{
		Router: h, srv: srv, Store: st, Cfg: cfg, reg: reg, Sess: sess,
		nodes: map[string]ed25519.PrivateKey{},
	}
}

// AdminUUID 返回 bootstrap admin 的 UUID 形态（Create 入参用）。
func (e *TestEnv) AdminUUID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(e.adminID(t))
	require.NoError(t, err)
	return id
}

// dialControl 走真实 WS 完成挑战→验签→HELLO 全流程，返回保持打开的控制连接
// （复用 agentws_test.go 的 dialAgentWS/agentHandshake 助手）。
func dialControl(t *testing.T, env *TestEnv, nodeID string) *websocket.Conn {
	t.Helper()
	c := dialAgentWS(t, "ws"+env.srv.URL[4:]+"/api/agent/connect")
	agentHandshake(t, env, c, nodeID)
	return c
}

func (e *TestEnv) AdminToken(t *testing.T) string {
	t.Helper()
	tok, err := auth.MakeToken(e.Cfg.JWTSecret, e.adminID(t), time.Hour)
	require.NoError(t, err)
	return tok
}

func (e *TestEnv) adminID(t *testing.T) string {
	t.Helper()
	u, err := e.Store.Q().GetUserByEmail(context.Background(), e.Cfg.AdminEmail)
	require.NoError(t, err)
	return u.ID.String()
}

func decodeJSON(r io.Reader, v any) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

type enrolledNode struct {
	ID   string
	Priv ed25519.PrivateKey
}

// EnrollNode 生成新密钥对并走真实 enroll 端点注册节点，返回 nodeID（私钥存入 e.nodes）。
func (e *TestEnv) EnrollNode(t *testing.T, hostname, machineID string) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	tok := e.CreateEnrollToken(t)
	body := fmt.Sprintf(`{"token":%q,"hostname":%q,"machineId":%q,
		"osVersion":"Windows","agentVersion":"0.1.0","publicKey":%q}`,
		tok, hostname, machineID, base64.StdEncoding.EncodeToString(pub))
	resp, err := http.Post(e.srv.URL+"/api/agent/enroll", "application/json",
		strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 201, resp.StatusCode)
	var out struct {
		NodeID string `json:"nodeId"`
	}
	require.NoError(t, decodeJSON(resp.Body, &out))
	e.nodes[out.NodeID] = priv
	return out.NodeID
}

func (e *TestEnv) CreateEnrollToken(t *testing.T) string {
	t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/clusters/default/enrollment-tokens",
		strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+e.AdminToken(t))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 201, resp.StatusCode)
	var out struct {
		Token string `json:"token"`
	}
	require.NoError(t, decodeJSON(resp.Body, &out))
	return out.Token
}

func (e *TestEnv) Priv(t *testing.T, nodeID string) ed25519.PrivateKey {
	t.Helper()
	return e.nodes[nodeID]
}

func (e *TestEnv) listNodes(t *testing.T, withToken bool) ([]map[string]any, int) {
	t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/nodes", nil)
	if withToken {
		req.Header.Set("Authorization", "Bearer "+e.AdminToken(t))
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, resp.StatusCode
	}
	var out []map[string]any
	require.NoError(t, decodeJSON(resp.Body, &out))
	return out, 200
}

func (e *TestEnv) ListNodes(t *testing.T) []map[string]any {
	t.Helper()
	nodes, code := e.listNodes(t, true)
	require.Equal(t, 200, code)
	return nodes
}

func (e *TestEnv) ListNodesStatus(t *testing.T, _ string) ([]map[string]any, int) {
	t.Helper()
	return e.listNodes(t, false)
}
