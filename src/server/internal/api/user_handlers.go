package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/db"
	"xnc/server/internal/db/sqlc"
)

// isAdminUser 判定用户是否 admin：是任一 cluster 的 owner 即视为 admin（简化
// 判定，Phase 5 无独立 isAdmin 字段；bootstrap admin 天然是 default cluster 的
// owner）。查询失败按非 admin 处理（fail closed）。
func isAdminUser(ctx context.Context, st *db.Store, userID uuid.UUID) bool {
	var is bool
	err := st.Pool().QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM cluster_members WHERE user_id=$1 AND role='owner')`,
		userID).Scan(&is)
	return err == nil && is
}

type createUserReq struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
}

// createUser 处理 POST /api/users：admin-only 用户创建（无自注册）。bcrypt 落库
// 绝不回传；UNIQUE email 冲突 → 409；审计 user.create {email}。
func (h *handlers) createUser(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !isAdminUser(r.Context(), h.st, u.ID) {
		respondError(w, proto.Err(403, proto.CodeForbidden, "admin required"))
		return
	}
	var req createUserReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		req.Email == "" || req.Password == "" {
		respondError(w, proto.Err(400, proto.CodeInternal, "email and password required"))
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "hash password"))
		return
	}
	// 显式预检给出稳定的 409；插入侧仍兜底 UNIQUE 冲突（并发窗口内）。
	if n, err := h.st.Q().CountUsersByEmail(r.Context(), req.Email); err == nil && n > 0 {
		respondError(w, proto.Err(409, proto.CodeInternal, "email already exists"))
		return
	}
	created, err := h.st.Q().CreateUserByEmail(r.Context(), sqlc.CreateUserByEmailParams{
		Email: req.Email, DisplayName: req.DisplayName, PasswordHash: hash,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			respondError(w, proto.Err(409, proto.CodeInternal, "email already exists"))
			return
		}
		respondError(w, proto.Err(500, proto.CodeInternal, "create user"))
		return
	}
	// 审计沿用 startSession 模式：独立 background ctx，不受客户端断连影响。
	actx, acancel := context.WithTimeout(context.Background(), auditInsertTimeout)
	defer acancel()
	_ = h.st.Q().InsertAuditLog(actx, sqlc.InsertAuditLogParams{
		UserID: pgUUID(u.ID), Action: "user.create",
		Metadata: mustJSON(map[string]string{"email": created.Email}),
	})
	respondJSON(w, http.StatusCreated,
		newUserDTO(created.ID.String(), created.Email, created.DisplayName))
}

// listUsers 处理 GET /api/users：admin-only，返回脱敏列表（无 password_hash）。
func (h *handlers) listUsers(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !isAdminUser(r.Context(), h.st, u.ID) {
		respondError(w, proto.Err(403, proto.CodeForbidden, "admin required"))
		return
	}
	users, err := h.st.Q().ListUsers(r.Context())
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "list users"))
		return
	}
	out := make([]userDTO, 0, len(users))
	for _, usr := range users {
		out = append(out, newUserDTO(usr.ID.String(), usr.Email, usr.DisplayName))
	}
	respondJSON(w, http.StatusOK, out)
}
