package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"xnc/server/internal/auth"
	"xnc/server/internal/config"
	"xnc/server/internal/db"
	"xnc/server/internal/registry"
)

type handlers struct {
	st  *db.Store
	cfg config.Config
	reg *registry.Registry
}

func NewRouter(st *db.Store, cfg config.Config, reg *registry.Registry) http.Handler {
	h := &handlers{st: st, cfg: cfg, reg: reg}
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
	r.Route("/api/nodes", func(nr chi.Router) {
		nr.Use(auth.Middleware(cfg.JWTSecret, st))
		nr.Get("/", h.listNodes)
		nr.Get("/{id}", h.getNode)
	})
	return r
}
