package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/db/sqlc"
)

func (h *handlers) nodeDTO(id, name, cluster, hostname, osv, agentv, shell, status string,
	lastSeen any) map[string]any {
	return map[string]any{
		"id": id, "name": name, "cluster": cluster, "hostname": hostname,
		"os_version": osv, "agent_version": agentv, "shell_type": shell,
		"status": status, "last_seen_at": lastSeen,
	}
}

// tsOrNull：nodes.last_seen_at 可空，sqlc 生成 pgtype.Timestamptz；
// 未上线过的节点序列化为 JSON null，否则输出时间戳。
func tsOrNull(ts pgtype.Timestamptz) any {
	if !ts.Valid {
		return nil
	}
	return ts.Time
}

func (h *handlers) listNodes(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	rows, err := h.st.Q().ListNodesForUser(r.Context(), u.ID)
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "list nodes"))
		return
	}
	clusterFilter, statusFilter := r.URL.Query().Get("clusterId"), r.URL.Query().Get("status")
	out := []map[string]any{}
	for _, n := range rows {
		if clusterFilter != "" && clusterFilter != n.ClusterName && clusterFilter != n.ClusterID.String() {
			continue
		}
		status := n.Status
		if status == "online" && !h.reg.Online(n.ID.String()) {
			status = "offline"
		}
		if statusFilter != "" && statusFilter != status {
			continue
		}
		out = append(out, h.nodeDTO(n.ID.String(), n.Name, n.ClusterName, n.Hostname,
			n.OsVersion, n.AgentVersion, n.ShellType, status, tsOrNull(n.LastSeenAt)))
	}
	respondJSON(w, 200, out)
}

func (h *handlers) getNode(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return
	}
	n, err := h.st.Q().GetNodeForUser(r.Context(), sqlc.GetNodeForUserParams{
		UserID: u.ID, ID: id,
	})
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return
	}
	status := n.Status
	if status == "online" && !h.reg.Online(n.ID.String()) {
		status = "offline"
	}
	respondJSON(w, 200, h.nodeDTO(n.ID.String(), n.Name, n.ClusterName, n.Hostname,
		n.OsVersion, n.AgentVersion, n.ShellType, status, tsOrNull(n.LastSeenAt)))
}
