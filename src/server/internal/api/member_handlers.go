package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/db/sqlc"
)

// validRoles 与 cluster_members 的 CHECK 约束一致（spec §6 角色矩阵）。
var validRoles = map[string]bool{"owner": true, "operator": true, "viewer": true}

type memberDTO struct {
	UserID      string `json:"user_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// resolveCluster 按 UUID 或 name 解析 cluster（与 enroll_handlers 的
// authorizeClusterOwner 同一解析规则，仅不含 owner 检查——列表端点成员即可）。
func (h *handlers) resolveCluster(r *http.Request, idOrName string) (sqlc.Cluster, *proto.APIError) {
	var c sqlc.Cluster
	var err error
	if pid, perr := uuid.Parse(idOrName); perr == nil {
		c, err = h.st.Q().GetClusterByID(r.Context(), pid)
	} else {
		c, err = h.st.Q().GetClusterByName(r.Context(), idOrName)
	}
	if err != nil {
		return c, proto.Err(404, proto.CodeClusterNotFound, "cluster not found")
	}
	return c, nil
}

// listMembers 处理 GET /api/clusters/{id}/members：任一成员可列（viewer 亦可）；
// 非 member → 404（不泄漏 cluster 存在性，与节点侧 GetNodeForUser 行为一致）。
func (h *handlers) listMembers(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	c, apiErr := h.resolveCluster(r, chi.URLParam(r, "id"))
	if apiErr != nil {
		respondError(w, apiErr)
		return
	}
	if _, err := h.st.Q().GetMemberRole(r.Context(), sqlc.GetMemberRoleParams{
		ClusterID: c.ID, UserID: u.ID,
	}); err != nil {
		respondError(w, proto.Err(404, proto.CodeClusterNotFound, "cluster not found"))
		return
	}
	rows, err := h.st.Q().ListMembers(r.Context(), c.ID)
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "list members"))
		return
	}
	out := make([]memberDTO, 0, len(rows))
	for _, m := range rows {
		out = append(out, memberDTO{
			UserID: m.UserID.String(), Email: m.Email,
			DisplayName: m.DisplayName, Role: m.Role,
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// addMember 处理 POST /api/clusters/{id}/members：仅 owner；role 必须 ∈
// {owner,operator,viewer}；已是成员 → 409；审计 cluster.member.add {user_id, role}。
// 请求体 {email} 或 {user_id}（email 优先——GET /api/users 是 admin-only，
// 普通 owner 只有 email 可用；二者给一，都不给 → 400）。email 未注册 →
// 404 USER_NOT_FOUND。
func (h *handlers) addMember(w http.ResponseWriter, r *http.Request) {
	c, apiErr := h.authorizeClusterOwner(r, chi.URLParam(r, "id"))
	if apiErr != nil {
		respondError(w, apiErr)
		return
	}
	u := auth.UserFrom(r.Context())
	var req struct {
		Email  string `json:"email"`
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		(req.Email == "" && req.UserID == "") {
		respondError(w, proto.Err(400, proto.CodeInternal, "email (or user_id) and role required"))
		return
	}
	if !validRoles[req.Role] {
		respondError(w, proto.Err(400, proto.CodeInternal, "role must be owner, operator or viewer"))
		return
	}
	var uid uuid.UUID
	switch {
	case req.Email != "":
		target, err := h.st.Q().GetUserByEmail(r.Context(), req.Email)
		if err != nil {
			respondError(w, proto.Err(404, proto.CodeUserNotFound, "user not found"))
			return
		}
		uid = target.ID
	default:
		var err error
		uid, err = uuid.Parse(req.UserID)
		if err != nil {
			respondError(w, proto.Err(400, proto.CodeInternal, "invalid user_id"))
			return
		}
		if _, err := h.st.Q().GetUserByID(r.Context(), uid); err != nil {
			respondError(w, proto.Err(404, proto.CodeUserNotFound, "user not found"))
			return
		}
	}
	if err := h.st.Q().AddMember(r.Context(), sqlc.AddMemberParams{
		ClusterID: c.ID, UserID: uid, Role: req.Role,
	}); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			respondError(w, proto.Err(409, proto.CodeInternal, "already a member"))
			return
		}
		respondError(w, proto.Err(500, proto.CodeInternal, "add member"))
		return
	}
	h.auditMember(r, u.ID, c.ID, "cluster.member.add",
		map[string]string{"user_id": uid.String(), "role": req.Role})
	respondJSON(w, http.StatusCreated, map[string]string{
		"user_id": uid.String(), "role": req.Role,
	})
}

// withMembershipLock 在事务内持 cluster 级 advisory 锁后执行 fn——成员
// 变更（删除/降级）的最后 owner 守卫是"条件子查询"判定，READ COMMITTED 下
// 两个并发变更各自读旧快照可同时通过 count>1（清光 owner）。同 cluster 的
// 守卫变更经此锁串行化后判定才是真正的原子。
func (h *handlers) withMembershipLock(ctx context.Context, clusterID uuid.UUID,
	fn func(q *sqlc.Queries) error,
) error {
	tx, err := h.st.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('cluster-members:'||$1, 0))`,
		clusterID.String()); err != nil {
		return err
	}
	if err := fn(sqlc.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// removeMember 处理 DELETE /api/clusters/{id}/members/{userId}：仅 owner；
// 最后一个 owner 不可删（条件删除 + membership 锁串行，防并发双删清光
// owner——0005 顺手修掉原查-删分离窗口）；非成员删除幂等 204；审计
// cluster.member.remove {user_id}。
func (h *handlers) removeMember(w http.ResponseWriter, r *http.Request) {
	c, apiErr := h.authorizeClusterOwner(r, chi.URLParam(r, "id"))
	if apiErr != nil {
		respondError(w, apiErr)
		return
	}
	u := auth.UserFrom(r.Context())
	uid, err := uuid.Parse(chi.URLParam(r, "userId"))
	if err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "invalid user id"))
		return
	}
	var n int64
	err = h.withMembershipLock(r.Context(), c.ID, func(q *sqlc.Queries) error {
		var derr error
		n, derr = q.RemoveMemberGuarded(r.Context(), sqlc.RemoveMemberGuardedParams{
			ClusterID: c.ID, UserID: uid,
		})
		return derr
	})
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "remove member"))
		return
	}
	if n == 0 {
		// 区分：仍是成员 = 最后 owner 保护；非成员 = 幂等成功。
		if role, rerr := h.st.Q().GetMemberRole(r.Context(), sqlc.GetMemberRoleParams{
			ClusterID: c.ID, UserID: uid,
		}); rerr == nil && role == "owner" {
			respondError(w, proto.Err(400, proto.CodeInternal, "cannot remove the last owner"))
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.auditMember(r, u.ID, c.ID, "cluster.member.remove",
		map[string]string{"user_id": uid.String()})
	w.WriteHeader(http.StatusNoContent)
}

// updateMemberRole 处理 PATCH /api/clusters/{id}/members/{userId}：仅 owner
// 改 role（提升为 owner 合法，多 owner 允许）；最后 owner 降级 → 400
// （条件更新 + membership 锁串行，防并发双降级清光 owner）；非成员 404；
// 审计 cluster.member.role {user_id, from, to}。
func (h *handlers) updateMemberRole(w http.ResponseWriter, r *http.Request) {
	c, apiErr := h.authorizeClusterOwner(r, chi.URLParam(r, "id"))
	if apiErr != nil {
		respondError(w, apiErr)
		return
	}
	u := auth.UserFrom(r.Context())
	uid, err := uuid.Parse(chi.URLParam(r, "userId"))
	if err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "invalid user id"))
		return
	}
	var req struct{ Role string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !validRoles[req.Role] {
		respondError(w, proto.Err(400, proto.CodeInternal, "role must be owner, operator or viewer"))
		return
	}
	old, err := h.st.Q().GetMemberRole(r.Context(), sqlc.GetMemberRoleParams{
		ClusterID: c.ID, UserID: uid,
	})
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeClusterNotFound, "not a member"))
		return
	}
	var n int64
	err = h.withMembershipLock(r.Context(), c.ID, func(q *sqlc.Queries) error {
		var uerr error
		n, uerr = q.UpdateMemberRole(r.Context(), sqlc.UpdateMemberRoleParams{
			ClusterID: c.ID, UserID: uid, Role: req.Role,
		})
		return uerr
	})
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "update member role"))
		return
	}
	if n == 0 {
		respondError(w, proto.Err(400, proto.CodeInternal, "cannot demote the last owner"))
		return
	}
	h.auditMember(r, u.ID, c.ID, "cluster.member.role",
		map[string]string{"user_id": uid.String(), "from": old, "to": req.Role})
	respondJSON(w, 200, map[string]string{"user_id": uid.String(), "role": req.Role})
}

// auditMember 沿用 startSession 审计模式：独立 background ctx + actor/cluster 维度。
func (h *handlers) auditMember(r *http.Request, actor, cluster uuid.UUID, action string, meta map[string]string) {
	actx, acancel := context.WithTimeout(context.Background(), auditInsertTimeout)
	defer acancel()
	_ = h.st.Q().InsertAuditLog(actx, sqlc.InsertAuditLogParams{
		UserID: pgUUID(actor), ClusterID: pgUUID(cluster),
		Action: action, Metadata: mustJSON(meta),
	})
}
