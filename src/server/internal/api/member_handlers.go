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
func (h *handlers) addMember(w http.ResponseWriter, r *http.Request) {
	c, apiErr := h.authorizeClusterOwner(r, chi.URLParam(r, "id"))
	if apiErr != nil {
		respondError(w, apiErr)
		return
	}
	u := auth.UserFrom(r.Context())
	var req struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" {
		respondError(w, proto.Err(400, proto.CodeInternal, "user_id and role required"))
		return
	}
	if !validRoles[req.Role] {
		respondError(w, proto.Err(400, proto.CodeInternal, "role must be owner, operator or viewer"))
		return
	}
	uid, err := uuid.Parse(req.UserID)
	if err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "invalid user_id"))
		return
	}
	if _, err := h.st.Q().GetUserByID(r.Context(), uid); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "user not found"))
		return
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

// removeMember 处理 DELETE /api/clusters/{id}/members/{userId}：仅 owner；
// 目标是最后一个 owner → 400（cluster 不能无 owner）；幂等（非成员删除 → 204）；
// 审计 cluster.member.remove {user_id}。
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
	if role, err := h.st.Q().GetMemberRole(r.Context(), sqlc.GetMemberRoleParams{
		ClusterID: c.ID, UserID: uid,
	}); err == nil && role == "owner" {
		if n, nerr := h.st.Q().CountClusterOwners(r.Context(), c.ID); nerr == nil && n <= 1 {
			respondError(w, proto.Err(400, proto.CodeInternal, "cannot remove the last owner"))
			return
		}
	}
	if err := h.st.Q().RemoveMember(r.Context(), sqlc.RemoveMemberParams{
		ClusterID: c.ID, UserID: uid,
	}); err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "remove member"))
		return
	}
	h.auditMember(r, u.ID, c.ID, "cluster.member.remove",
		map[string]string{"user_id": uid.String()})
	w.WriteHeader(http.StatusNoContent)
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
