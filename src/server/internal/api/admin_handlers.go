package api

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/db/sqlc"
)

// nodeDisable 处理 POST /api/nodes/{id}/disable：仅 owner；节点置 disabled，
// 一切会话创建随即 403 NODE_DISABLED（requireMinRole 的 disabled 分支）。
// 幂等：对已 disabled 节点重复 disable 仍 204（管理端点跳过 disabled 检查）。
// 审计 node.disable。
func (h *handlers) nodeDisable(w http.ResponseWriter, r *http.Request) {
	h.setNodeStatus(w, r, "disabled", "node.disable")
}

// nodeEnable 处理 POST /api/nodes/{id}/enable：仅 owner；disabled → offline
// （不直接置 online——online 由 agent 心跳/控制连接驱动，enable 只解除封锁）。
// 审计 node.enable。
func (h *handlers) nodeEnable(w http.ResponseWriter, r *http.Request) {
	h.setNodeStatus(w, r, "offline", "node.enable")
}

// setNodeStatus 是 disable/enable 共享路径：解析 nodeID（非法 UUID 与不存在
// 同为 404，不泄漏存在性）→ owner 检查（忽略 disabled，见
// requireMinRoleIgnoreDisabled）→ SetNodeStatus → 审计。
func (h *handlers) setNodeStatus(w http.ResponseWriter, r *http.Request,
	status, action string,
) {
	nodeID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return
	}
	node, ok := h.requireMinRoleIgnoreDisabled(w, r, nodeID, "owner")
	if !ok {
		return
	}
	if err := h.st.Q().SetNodeStatus(r.Context(), sqlc.SetNodeStatusParams{
		ID: node.ID, Status: status,
	}); err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "update node status"))
		return
	}
	u := auth.UserFrom(r.Context())
	h.auditNode(r, u.ID, node.ClusterID, node.ID, action,
		map[string]string{"name": node.Name})
	w.WriteHeader(http.StatusNoContent)
}

// deleteCluster 处理 DELETE /api/clusters/{id}：仅 owner；仍有节点 → 409
// CLUSTER_NOT_EMPTY（先 disable/清理节点，或把节点 move 到别的 cluster）；
// 软删除 = 打 deleted_at（0005 起；恢复 = 清 deleted_at，原名经存活行
// 部分唯一索引自动可复用，不再需要改名释放）。审计 cluster.delete。
func (h *handlers) deleteCluster(w http.ResponseWriter, r *http.Request) {
	c, apiErr := h.authorizeClusterOwner(r, chi.URLParam(r, "id"))
	if apiErr != nil {
		respondError(w, apiErr)
		return
	}
	if n, err := h.st.Q().CountNodesInCluster(r.Context(), c.ID); err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "count nodes"))
		return
	} else if n > 0 {
		respondError(w, proto.Err(409, proto.CodeClusterNotEmpty,
			"cluster still has nodes; disable or remove them first"))
		return
	}
	if _, err := h.st.Q().SoftDeleteCluster(r.Context(), c.ID); err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "delete cluster"))
		return
	}
	u := auth.UserFrom(r.Context())
	h.auditMember(r, u.ID, c.ID, "cluster.delete", map[string]string{"name": c.Name})
	w.WriteHeader(http.StatusNoContent)
}

// adminDeleteNode 处理 DELETE /api/nodes/{id}：admin-only（isAdminUser，与
// adminDeleteRelease 同款判定）硬删除节点行——管理端清理残留注册项（跨
// cluster machineId 冲突须先删后注册的出口）。controller 评审确认采用平台级
// admin 谓词而非 deleteCluster 的 per-cluster owner：跨 cluster/无 owner 的
// 孤儿节点只有平台 admin 能清理。落库复用 WS NODE_DELETE 的
// DeleteNode；节点在线则逐出其控制连接（evictNodeConn：registry 移除 +
// Cancel）；审计 node_delete {userId, nodeId}。非 admin 403；未知/非法 id
// 404；成功 200（空 body）。
func (h *handlers) adminDeleteNode(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !u.IsAdmin {
		respondError(w, proto.Err(403, proto.CodeForbidden, "admin required"))
		return
	}
	nodeID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return
	}
	// 先查后删：审计需要 cluster 维度与节点名，行删除后不可再取。
	node, err := h.st.Q().GetNodeByID(r.Context(), nodeID)
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return
	}
	if _, err := h.st.Q().DeleteNode(r.Context(), node.ID); err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "delete node"))
		return
	}
	h.auditNode(r, u.ID, node.ClusterID, node.ID, "node_delete",
		map[string]string{"name": node.Name})
	h.evictNodeConn(node.ID)
	w.WriteHeader(http.StatusOK)
}

// auditNode 沿用 startSession 审计模式（独立 background ctx，不受客户端断连
// 影响），附带 node 维度——disable/enable 均在写响应前同步落审计行。
func (h *handlers) auditNode(_ *http.Request, actor, cluster, node uuid.UUID,
	action string, meta map[string]string,
) {
	actx, acancel := context.WithTimeout(context.Background(), auditInsertTimeout)
	defer acancel()
	_ = h.st.Q().InsertAuditLog(actx, sqlc.InsertAuditLogParams{
		UserID: pgUUID(actor), ClusterID: pgUUID(cluster), NodeID: pgUUID(node),
		Action: action, Metadata: mustJSON(meta),
	})
}
