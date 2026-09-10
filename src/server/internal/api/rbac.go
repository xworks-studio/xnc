package api

import (
	"net/http"

	"github.com/google/uuid"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/db/sqlc"
)

// roleRank：RBAC 层级 owner > operator > viewer（spec §5x）。DB 侧
// cluster_members.role 的 CHECK 约束保证只出现这三个值。
var roleRank = map[string]int{"viewer": 0, "operator": 1, "owner": 2}

// requireMinRole 校验请求用户对 nodeID 所属 cluster 的 membership 且
// role ≥ minRole，附带节点 disabled 检查（spec §51）：
//
//	minRole = "viewer"    → 任一成员（等同原 GetNodeForUser 的 membership 检查）
//	minRole = "operator"  → operator 或 owner
//	minRole = "owner"     → 仅 owner
//
// 失败路径（已写响应，调用方直接 return）：
//   - 节点不存在或请求者非成员 → 404 NODE_NOT_FOUND（不泄漏节点存在性）
//   - 角色不足 → 403 FORBIDDEN
//   - 节点已 disable → 403 NODE_DISABLED
//
// 成功返回节点行（含 cluster_id）与 true。检查顺序：存在性/成员 → 角色 →
// disabled——非成员与不存在的节点同响应，角色先于 disabled 使 viewer 对
// disabled 节点也只见到 FORBIDDEN。
func (h *handlers) requireMinRole(w http.ResponseWriter, r *http.Request,
	nodeID uuid.UUID, minRole string,
) (*sqlc.Node, bool) {
	return h.authorizeNodeRole(w, r, nodeID, minRole, false)
}

// requireMinRoleIgnoreDisabled：requireMinRole 的管理端点变体——跳过 disabled
// 检查。node disable/enable 的 owner 必须能作用于已 disabled 的节点（否则
// enable 永远 403 NODE_DISABLED，disable 也无法幂等）；disabled 拒绝的语义
// 只针对会话创建（exec/shell/file/tunnel），管理面不适用。
func (h *handlers) requireMinRoleIgnoreDisabled(w http.ResponseWriter, r *http.Request,
	nodeID uuid.UUID, minRole string,
) (*sqlc.Node, bool) {
	return h.authorizeNodeRole(w, r, nodeID, minRole, true)
}

func (h *handlers) authorizeNodeRole(w http.ResponseWriter, r *http.Request,
	nodeID uuid.UUID, minRole string, ignoreDisabled bool,
) (*sqlc.Node, bool) {
	// 任意节点（不限成员可见）：存在性在角色判定前，非成员统一 404。
	node, err := h.st.Q().GetNodeByID(r.Context(), nodeID)
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return nil, false
	}
	u := auth.UserFrom(r.Context())
	role, err := h.st.Q().GetMemberRole(r.Context(), sqlc.GetMemberRoleParams{
		ClusterID: node.ClusterID, UserID: u.ID,
	})
	if err != nil {
		// 无行（非成员）与其他查询错误同响应，避免成员资格可探测。
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return nil, false
	}
	if roleRank[role] < roleRank[minRole] {
		respondError(w, proto.Err(403, proto.CodeForbidden, "insufficient role"))
		return nil, false
	}
	if !ignoreDisabled && node.Status == "disabled" {
		respondError(w, proto.Err(403, proto.CodeNodeDisabled, "node is disabled"))
		return nil, false
	}
	return &node, true
}
