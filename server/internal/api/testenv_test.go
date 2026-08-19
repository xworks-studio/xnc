package api

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"xnc/server/internal/auth"
	"xnc/server/internal/bootstrap"
	"xnc/server/internal/config"
	"xnc/server/internal/db"
	"xnc/server/internal/registry"
)

// TestEnv 起一个真实 PG 容器 + bootstrap admin + 完整 router，供 api 层集成测试复用。
type TestEnv struct {
	Router http.Handler
	srv    *httptest.Server
	Store  *db.Store
	Cfg    config.Config
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
	}
	require.NoError(t, bootstrap.EnsureAdmin(context.Background(), st, cfg))
	h := NewRouter(st, cfg, registry.New())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &TestEnv{
		Router: h, srv: srv, Store: st, Cfg: cfg,
		nodes: map[string]ed25519.PrivateKey{},
	}
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
