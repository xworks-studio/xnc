package api

import (
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

// renameNode 处理 PATCH /api/nodes/{id} {name}：owner-only 节点改名（管理
// 弹窗；忽略 disabled，与 disable/enable 同款信任域）。cluster 内重名 →
// 409（(cluster_id,name) 唯一约束）；审计 node.rename {from, to}。
func (h *handlers) renameNode(w http.ResponseWriter, r *http.Request) {
	nodeID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return
	}
	node, ok := h.requireMinRoleIgnoreDisabled(w, r, nodeID, "owner")
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		respondError(w, proto.Err(400, proto.CodeInternal, "name required"))
		return
	}
	updated, err := h.st.Q().RenameNode(r.Context(), sqlc.RenameNodeParams{
		ID: node.ID, Name: req.Name,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			respondError(w, proto.Err(409, proto.CodeInternal,
				"node name already taken in this cluster"))
			return
		}
		respondError(w, proto.Err(500, proto.CodeInternal, "rename node"))
		return
	}
	u := auth.UserFrom(r.Context())
	h.auditNode(r, u.ID, node.ClusterID, node.ID, "node.rename",
		map[string]string{"from": node.Name, "to": updated.Name})
	respondJSON(w, 200, map[string]string{
		"id": updated.ID.String(), "name": updated.Name,
	})
}
