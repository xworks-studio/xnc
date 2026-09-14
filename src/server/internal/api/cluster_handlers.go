package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/db/sqlc"
)

func (h *handlers) listClusters(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	rows, err := h.st.Q().ListClustersForUser(r.Context(), u.ID)
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "list clusters"))
		return
	}
	// role 供 Web/CLI 区分"我的角色"；personal 标记系统自动建的个人默认
	// cluster（徽标用）。已删 cluster 已在查询层过滤。
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		out = append(out, map[string]any{
			"id": c.ID, "name": c.Name, "personal": c.Personal, "role": c.Role,
		})
	}
	respondJSON(w, 200, out)
}

// maxClusterLimit 兜底（config 零值 = 默认 20）。
func (h *handlers) clusterLimit() int {
	if h.cfg.MaxClustersPerUser > 0 {
		return h.cfg.MaxClustersPerUser
	}
	return 20
}

func (h *handlers) createCluster(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	var req struct{ Name string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		respondError(w, proto.Err(400, proto.CodeInternal, "name required"))
		return
	}
	// 每用户限额（我任 owner 的存活 cluster）：防刷。个人默认 cluster 计入。
	if n, err := h.st.Q().CountOwnedLiveClusters(r.Context(), u.ID); err == nil &&
		int(n) >= h.clusterLimit() {
		respondError(w, proto.Err(400, proto.CodeInternal, "cluster limit reached"))
		return
	}
	c, err := h.st.Q().CreateCluster(r.Context(), sqlc.CreateClusterParams{
		ID: uuid.New(), Name: req.Name, OwnerID: u.ID, Personal: false,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			respondError(w, proto.Err(400, proto.CodeInternal, "cluster name exists"))
			return
		}
		respondError(w, proto.Err(500, proto.CodeInternal, "create cluster"))
		return
	}
	if err := h.st.Q().AddMembership(r.Context(), sqlc.AddMembershipParams{
		ClusterID: c.ID, UserID: u.ID, Role: "owner",
	}); err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "membership"))
		return
	}
	respondJSON(w, 201, map[string]any{"id": c.ID, "name": c.Name, "personal": false})
}
