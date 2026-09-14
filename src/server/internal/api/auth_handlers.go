package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/db/sqlc"
)

type userDTO struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	// IsAdmin 仅 admin 视角的端点（listUsers/updateUser）填充；omitempty
	// 保持 login/me 等既有响应形态不变。
	IsAdmin bool `json:"is_admin,omitempty"`
	// LastLoginAt 同为 admin 视角字段（0006）；nil = 从未登录。
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
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
	// 上次登录时间（0006，Users 页独立列）：best-effort，失败不阻断登录。
	_ = h.st.Q().TouchUserLogin(r.Context(), u.ID)
	respondJSON(w, 200, map[string]any{
		"token": tok,
		"user":  newUserDTO(u.ID.String(), u.Email, u.DisplayName),
	})
}

func (h *handlers) me(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	dto := newUserDTO(u.ID.String(), u.Email, u.DisplayName)
	dto.IsAdmin = u.IsAdmin // 0006 起 Web 管理弹窗的删除按钮等 admin 视图判断
	respondJSON(w, 200, map[string]any{
		"user": dto,
	})
}

// updateMe — PATCH /api/auth/me：自助修改 display_name（email 只读，设计
// §3.3）。TrimSpace 后 ≤64 rune；空串合法（清除显示名）。
func (h *handlers) updateMe(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	var req struct {
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}
	name := strings.TrimSpace(req.DisplayName)
	if utf8.RuneCountInString(name) > 64 {
		respondError(w, proto.Err(400, proto.CodeInternal, "display_name too long (max 64)"))
		return
	}
	if _, err := h.st.Q().UpdateUserDisplayName(r.Context(), sqlc.UpdateUserDisplayNameParams{
		ID: u.ID, DisplayName: name,
	}); err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "update profile"))
		return
	}
	respondJSON(w, 200, map[string]any{"user": newUserDTO(u.ID.String(), u.Email, name)})
}

// changePassword — POST /api/auth/password：current 校验 → 新密码强度 →
// bcrypt 落库。current 不符 401（与 login 同文案，防枚举）；JWT 无状态、
// 存量 token 保持有效（spec §2 决策）。审计 user.password_change。
func (h *handlers) changePassword(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context()) // sqlc.User 全行，含 PasswordHash
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		req.CurrentPassword == "" || req.NewPassword == "" {
		respondError(w, proto.Err(400, proto.CodeInternal, "current_password and new_password required"))
		return
	}
	if !auth.VerifyPassword(u.PasswordHash, req.CurrentPassword) {
		respondError(w, proto.Err(401, proto.CodeUnauthorized, "invalid credentials"))
		return
	}
	if len(req.NewPassword) < 8 {
		respondError(w, proto.Err(400, proto.CodeInternal, "new password must be at least 8 characters"))
		return
	}
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "hash password"))
		return
	}
	if _, err := h.st.Q().UpdateUserPassword(r.Context(), sqlc.UpdateUserPasswordParams{
		ID: u.ID, PasswordHash: hash,
	}); err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "update password"))
		return
	}
	// 审计沿用 user.create 的 background-ctx 模式（不受客户端断连影响）。
	actx, acancel := context.WithTimeout(context.Background(), auditInsertTimeout)
	defer acancel()
	_ = h.st.Q().InsertAuditLog(actx, sqlc.InsertAuditLogParams{
		UserID: pgUUID(u.ID), Action: "user.password_change", Metadata: []byte("{}"),
	})
	w.WriteHeader(http.StatusNoContent)
}
