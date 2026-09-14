package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/bootstrap"
	"xnc/server/internal/db/sqlc"
)

// admin 判定走 users.is_admin（0005 起）：auth middleware 每请求全行查库，
// u.IsAdmin 即当前值；此前"任一 cluster owner 即 admin"的派生谓词在人人拥有
// 默认 cluster 后会让所有用户变成平台 admin，故废弃。

// errLastAdmin：UpdateUserIsAdmin 触发最后 admin 保护（0 行）的哨兵。
var errLastAdmin = errors.New("last admin")

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
		dto := newUserDTO(usr.ID.String(), usr.Email, usr.DisplayName)
		dto.IsAdmin = usr.IsAdmin
		dto.LastLoginAt = tsPtr(usr.LastLoginAt)
		out = append(out, dto)
	}
	respondJSON(w, http.StatusOK, out)
}

// tsPtr pgtype.Timestamptz → *time.Time（Invalid = NULL = 从未登录）。
func tsPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

// updateUser 处理 PATCH /api/users/{id}：admin-only。可改 display_name、
// is_admin（授/撤，撤带最后 admin 保护 → 400 LAST_ADMIN——EnsureAdmin 只在
// 零用户时自愈，清光 admin 后没有 API 出路）与 password（管理员重置，无
// 须原密码——自助改密走 /api/auth/password）。审计 user.update /
// user.admin.toggle / user.password.reset。
func (h *handlers) updateUser(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !u.IsAdmin {
		respondError(w, proto.Err(403, proto.CodeForbidden, "admin required"))
		return
	}
	uid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeUserNotFound, "user not found"))
		return
	}
	target, err := h.st.Q().GetUserByID(r.Context(), uid)
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeUserNotFound, "user not found"))
		return
	}
	var req struct {
		DisplayName *string `json:"display_name"`
		IsAdmin     *bool   `json:"is_admin"`
		Password    *string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		(req.DisplayName == nil && req.IsAdmin == nil && req.Password == nil) {
		respondError(w, proto.Err(400, proto.CodeInternal, "nothing to update"))
		return
	}
	if req.DisplayName != nil && *req.DisplayName != target.DisplayName {
		if _, err := h.st.Q().UpdateUserDisplayName(r.Context(), sqlc.UpdateUserDisplayNameParams{
			ID: uid, DisplayName: *req.DisplayName,
		}); err != nil {
			respondError(w, proto.Err(500, proto.CodeInternal, "update display name"))
			return
		}
		target.DisplayName = *req.DisplayName
		h.auditUser(u.ID, "user.update",
			map[string]string{"user_id": uid.String()})
	}
	if req.Password != nil {
		if *req.Password == "" {
			respondError(w, proto.Err(400, proto.CodeInternal, "password must not be empty"))
			return
		}
		hash, err := auth.HashPassword(*req.Password)
		if err != nil {
			respondError(w, proto.Err(500, proto.CodeInternal, "hash password"))
			return
		}
		if _, err := h.st.Q().UpdateUserPassword(r.Context(), sqlc.UpdateUserPasswordParams{
			ID: uid, PasswordHash: hash,
		}); err != nil {
			respondError(w, proto.Err(500, proto.CodeInternal, "update password"))
			return
		}
		h.auditUser(u.ID, "user.password.reset",
			map[string]string{"user_id": uid.String()})
	}
	if req.IsAdmin != nil && *req.IsAdmin != target.IsAdmin {
		// 最后 admin 守卫的并发双撤同样有旧快照竞态，经全局 advisory 锁串行。
		err := func() error {
			tx, terr := h.st.Pool().Begin(r.Context())
			if terr != nil {
				return terr
			}
			defer tx.Rollback(r.Context())
			if _, terr := tx.Exec(r.Context(),
				`SELECT pg_advisory_xact_lock(hashtextextended('users:is_admin', 0))`); terr != nil {
				return terr
			}
			n, terr := sqlc.New(tx).UpdateUserIsAdmin(r.Context(), sqlc.UpdateUserIsAdminParams{
				ID: uid, IsAdmin: *req.IsAdmin,
			})
			if terr != nil {
				return terr
			}
			if n == 0 {
				return errLastAdmin
			}
			return tx.Commit(r.Context())
		}()
		if errors.Is(err, errLastAdmin) {
			respondError(w, proto.Err(400, proto.CodeLastAdmin,
				"cannot demote the last admin"))
			return
		}
		if err != nil {
			respondError(w, proto.Err(500, proto.CodeInternal, "update is_admin"))
			return
		}
		target.IsAdmin = *req.IsAdmin
		h.auditUser(u.ID, "user.admin.toggle",
			map[string]string{"user_id": uid.String(), "is_admin": strconv.FormatBool(*req.IsAdmin)})
	}
	dto := newUserDTO(target.ID.String(), target.Email, target.DisplayName)
	dto.IsAdmin = target.IsAdmin
	dto.LastLoginAt = tsPtr(target.LastLoginAt)
	respondJSON(w, http.StatusOK, dto)
}

// deleteUser 处理 DELETE /api/users/{id}：admin-only 硬删除。前置：
// 最后 admin 不可删（同 is_admin 撤销的守卫语义）；用户名下集群仍挂节点 →
// 409 USER_OWNS_NODES（节点是资产，出路 = 先 move/删除）。成功 = 同事务清掉
// 名下（此时必然空的）集群（membership/token 级联）后删用户行（membership
// 级联、审计行保留 actor 置空——0006 FK 语义）。审计 user.delete {email}。
func (h *handlers) deleteUser(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !u.IsAdmin {
		respondError(w, proto.Err(403, proto.CodeForbidden, "admin required"))
		return
	}
	uid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeUserNotFound, "user not found"))
		return
	}
	target, err := h.st.Q().GetUserByID(r.Context(), uid)
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeUserNotFound, "user not found"))
		return
	}
	err = func() error {
		tx, terr := h.st.Pool().Begin(r.Context())
		if terr != nil {
			return terr
		}
		defer tx.Rollback(r.Context())
		q := sqlc.New(tx)
		// 最后 admin 保护与 is_admin 撤销共用串行域（同 advisory 锁）。
		if _, terr := tx.Exec(r.Context(),
			`SELECT pg_advisory_xact_lock(hashtextextended('users:is_admin', 0))`); terr != nil {
			return terr
		}
		if target.IsAdmin {
			var admins int64
			if terr := tx.QueryRow(r.Context(),
				`SELECT count(*) FROM users WHERE is_admin`).Scan(&admins); terr != nil {
				return terr
			}
			if admins <= 1 {
				return errLastAdmin
			}
		}
		n, terr := q.CountNodesInOwnedClusters(r.Context(), uid)
		if terr != nil {
			return terr
		}
		if n > 0 {
			return &userOwnsNodesErr{}
		}
		if _, terr := q.DeleteOwnedClusters(r.Context(), uid); terr != nil {
			return terr
		}
		if _, terr := q.DeleteUser(r.Context(), uid); terr != nil {
			return terr
		}
		return tx.Commit(r.Context())
	}()
	switch {
	case err == nil:
	case errors.Is(err, errLastAdmin):
		respondError(w, proto.Err(400, proto.CodeLastAdmin, "cannot delete the last admin"))
		return
	default:
		var own *userOwnsNodesErr
		if errors.As(err, &own) {
			respondError(w, proto.Err(409, proto.CodeUserOwnsNodes,
				"user still owns clusters containing nodes; move or remove those nodes first"))
			return
		}
		respondError(w, proto.Err(500, proto.CodeInternal, "delete user"))
		return
	}
	h.auditUser(u.ID, "user.delete", map[string]string{"email": target.Email})
	w.WriteHeader(http.StatusNoContent)
}

// userOwnsNodesErr：删除前置检查的哨兵（409 USER_OWNS_NODES 分流用）。
type userOwnsNodesErr struct{}

func (*userOwnsNodesErr) Error() string { return "user owns nodes" }

// auditUser 用户维度审计（无 cluster 关联），沿用独立 background ctx 模式。
func (h *handlers) auditUser(actor uuid.UUID, action string, meta map[string]string) {
	actx, acancel := context.WithTimeout(context.Background(), auditInsertTimeout)
	defer acancel()
	_ = h.st.Q().InsertAuditLog(actx, sqlc.InsertAuditLogParams{
		UserID: pgUUID(actor), Action: action, Metadata: mustJSON(meta),
	})
}
