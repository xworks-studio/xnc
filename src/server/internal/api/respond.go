package api

import (
	"encoding/json"
	"net/http"

	"xnc/proto"
)

func respondJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func respondError(w http.ResponseWriter, e *proto.APIError) {
	respondJSON(w, e.Status, map[string]any{"error": e})
}
