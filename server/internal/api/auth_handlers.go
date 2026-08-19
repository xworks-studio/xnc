package api

import (
	"encoding/json"
	"net/http"
	"time"

	"xnc/proto"
	"xnc/server/internal/auth"
)

type userDTO struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

func newUserDTO(id, email, name string) userDTO {
	return userDTO{ID: id, Email: email, DisplayName: name}
}

func (h *handlers) login(w http.ResponseWriter, r *http.Request) {
	var req struct{ Email, Password string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}
	u, err := h.st.Q().GetUserByEmail(r.Context(), req.Email)
	if err != nil || !auth.VerifyPassword(u.PasswordHash, req.Password) {
		respondError(w, proto.Err(401, proto.CodeUnauthorized, "invalid credentials"))
		return
	}
	tok, err := auth.MakeToken(h.cfg.JWTSecret, u.ID.String(), 24*time.Hour)
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "token"))
		return
	}
	respondJSON(w, 200, map[string]any{
		"token": tok,
		"user":  newUserDTO(u.ID.String(), u.Email, u.DisplayName),
	})
}

func (h *handlers) me(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	respondJSON(w, 200, map[string]any{
		"user": newUserDTO(u.ID.String(), u.Email, u.DisplayName),
	})
}
