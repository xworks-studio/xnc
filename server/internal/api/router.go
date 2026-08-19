package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"xnc/server/internal/config"
	"xnc/server/internal/db"
	"xnc/server/internal/registry"
)

func NewRouter(st *db.Store, cfg config.Config, reg *registry.Registry) http.Handler {
	r := chi.NewRouter()
	r.Get("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		respondJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": "0.1.0"})
	})
	return r
}
