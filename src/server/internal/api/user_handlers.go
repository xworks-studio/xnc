package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/bootstrap"
	"xnc/server/internal/db/sqlc"
)

// admin 判定走 users.is_admin（0005 起）：auth middleware 每请求全行查库，
// u.IsAdmin 即当前值；此前"任一 cluster owner 即 admin"的派生谓词在人人拥有
// 默认 cluster 后会让所有用户变成平台 admin，故废弃。

type createUserReq struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
}

// createUser 处理 POST /api/users：admin-only 用户创建（无自注册）。bcrypt 落库
// 绝不回传；UNIQUE email 冲突 → 409；同一事务内建个人默认 cluster + owner
// 成员（0005：保证新用户 register 有 cluster 可选）；审计 user.create
// {email, default_cluster_id}。
func (h *handlers) createUser(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !u.IsAdmin {
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
	// 建号 + 默认 cluster 同事务：任一失败整体回滚，不留无 cluster 的半成品。
	tx, err := h.st.Pool().Begin(r.Context())
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "tx"))
		return
	}
	defer tx.Rollback(r.Context())
	q := h.st.Q().WithTx(tx)
	created, err := q.CreateUserByEmail(r.Context(), sqlc.CreateUserByEmailParams{
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
	pc, err := bootstrap.EnsurePersonalCluster(r.Context(), tx, created)
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "default cluster"))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "commit"))
		return
	}
	// 审计沿用 startSession 模式：独立 background ctx，不受客户端断连影响。
	actx, acancel := context.WithTimeout(context.Background(), auditInsertTimeout)
	defer acancel()
	_ = h.st.Q().InsertAuditLog(actx, sqlc.InsertAuditLogParams{
		UserID: pgUUID(u.ID), Action: "user.create",
		Metadata: mustJSON(map[string]string{
			"email": created.Email, "default_cluster_id": pc.ID.String(),
		}),
	})
	respondJSON(w, http.StatusCreated,
		newUserDTO(created.ID.String(), created.Email, created.DisplayName))
}

// listUsers 处理 GET /api/users：admin-only，返回脱敏列表（无 password_hash）。
func (h *handlers) listUsers(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !u.IsAdmin {
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
