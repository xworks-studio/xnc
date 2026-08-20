package api

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"xnc/server/internal/auth"
	"xnc/server/internal/config"
	"xnc/server/internal/db"
	"xnc/server/internal/registry"
	"xnc/server/internal/session"
)

type handlers struct {
	st   *db.Store
	cfg  config.Config
	reg  *registry.Registry
	sess *session.Manager
}

func NewRouter(st *db.Store, cfg config.Config, reg *registry.Registry) http.Handler {
	return NewRouterWithSession(st, cfg, reg, nil)
}

// NewRouterWithSession 允许注入共享的 session manager（测试经 TestEnv.Sess 直接
// 驱动会话生命周期）；sess 为 nil 时自建并按 config 覆写 shell 治理参数
// （NewRouter 即此生产路径；Manager.New 的默认值仅零值兜底）。
func NewRouterWithSession(st *db.Store, cfg config.Config, reg *registry.Registry, sess *session.Manager) http.Handler {
	if sess == nil {
		sess = session.New(reg, slog.Default())
		sess.ShellPerNode = cfg.ShellPerNode
		sess.ShellIdleTimeout = cfg.ShellIdleTimeout
		sess.ShellMaxLifetime = cfg.ShellMaxLifetime
	}
	h := &handlers{st: st, cfg: cfg, reg: reg, sess: sess}
	r := chi.NewRouter()

	r.Get("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		respondJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": "0.1.0"})
	})

	r.Route("/api/auth", func(ar chi.Router) {
		ar.Post("/login", h.login)
		ar.Group(func(g chi.Router) {
			g.Use(auth.Middleware(cfg.JWTSecret, st))
			g.Get("/me", h.me)
		})
	})

	r.Route("/api/clusters", func(cr chi.Router) {
		cr.Use(auth.Middleware(cfg.JWTSecret, st))
		cr.Get("/", h.listClusters)
		cr.Post("/", h.createCluster)
	})

	r.Route("/api/clusters/{id}/enrollment-tokens", func(tr chi.Router) {
		tr.Use(auth.Middleware(cfg.JWTSecret, st))
		tr.Post("/", h.createEnrollToken)
	})
	r.Post("/api/agent/enroll", h.agentEnroll)
	r.Get("/api/agent/connect", h.agentConnect)
	// 会话 WS（两侧均 token 即凭证，不走 JWT）
	r.Get("/api/session/{id}", h.clientSessionWS)
	r.Get("/api/agent/session", h.agentSessionWS)
	r.Route("/api/nodes", func(nr chi.Router) {
		nr.Use(auth.Middleware(cfg.JWTSecret, st))
		nr.Get("/", h.listNodes)
		nr.Get("/{id}", h.getNode)
		// exec：鉴权 + membership 通过后创建会话并下发 SESSION_OPEN，202 异步语义
		nr.Post("/{id}/exec", h.execStart)
		// shell：同一会话创建路径（startSession），kind=shell，ConPTY 交互式终端
		nr.Post("/{id}/shell", h.shellStart)
	})
	return r
}
