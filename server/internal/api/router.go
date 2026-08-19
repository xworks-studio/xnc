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
	return r
}
