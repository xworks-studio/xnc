package api

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/db/sqlc"
)

// registerReq：POST /api/clusters/{id}/nodes/register 请求体——与
// /api/agent/enroll 同形但不含 token（授权是调用者的用户 JWT）。Token 用指针
// 区分"字段存在"与"空值"：携带 token 字段（含空串）一律 400，防止两条注册
// 路径混用。
type registerReq struct {
	Token        *string `json:"token"`
	Hostname     string  `json:"hostname"`
	MachineID    string  `json:"machineId"`
	OSVersion    string  `json:"osVersion"`
	AgentVersion string  `json:"agentVersion"`
	PublicKey    string  `json:"publicKey"`
}

// userRegisterNode 处理 POST /api/clusters/{id}/nodes/register（spec §6.4）：
// 用户 JWT + cluster 成员关系（任一角色，与 listMembers 的成员判定同一查询）
// 授权的节点注册——语义等价于服务端在同一事务内"铸造并消费"一次性 enrollment
// token：复用 enrollNode 落库路径（machineId 幂等去重、公钥绑定、节点名唯一
// 化、NodeID 分配），audit 记 register {userId, clusterId}（取代 token 创建+
// 使用两条审计的拼接）。
//
// 同 cluster 同 machineId 异 key → adopt（controller 批准设计）：201 复用既有
// 节点行（同 nodeId、name 不变），重绑 public_key 并刷新机器字段，双审计
// register + node_adopt。安全边界：同 cluster 成员即可 adopt——与注册新节点
// 同一信任域（machineId 冲突即说明是同一台机器重装后换 key）；跨 cluster
// 冲突不走 adopt（不同信任域），须管理端先删除原注册项（见 409 分支）。
//
//	成员（viewer 亦可）→ 201 {nodeId, clusterId, name}（幂等/adopt 复用同款 201）
//	非成员 → 403 FORBIDDEN（spec 明示 403，不走 404 隐藏 cluster 存在性）
//	404 CLUSTER_NOT_FOUND / 400 缺参、坏公钥或携带 token 字段
//	409 MACHINE_ID_CONFLICT：machineId 已注册于其他 cluster（message 含冲突
//	cluster 名，CLI 据此提示 --force 或管理端处理）
func (h *handlers) userRegisterNode(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	c, apiErr := h.resolveCluster(r, chi.URLParam(r, "id"))
	if apiErr != nil {
		respondError(w, apiErr)
		return
	}
	// 与 listMembers 同一成员判定（GetMemberRole）：任一角色可通过；无行即非成员。
	if _, err := h.st.Q().GetMemberRole(r.Context(), sqlc.GetMemberRoleParams{
		ClusterID: c.ID, UserID: u.ID}); err != nil {
		respondError(w, proto.Err(403, proto.CodeForbidden, "cluster membership required"))
		return
	}
	var req registerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}
	if req.Token != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "token field not accepted"))
		return
	}
	if req.Hostname == "" || req.MachineID == "" {
		respondError(w, proto.Err(400, proto.CodeInternal, "hostname and machineId required"))
		return
	}
	pub, err := base64.StdEncoding.DecodeString(req.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad public key"))
		return
	}
	// 跨 cluster 冲突（本 cluster 内的幂等/异 key 由 enrollNode 事务内处理）：
	// machineId 已属其他 cluster → 409，message 带冲突 cluster 名。
	if name, err := h.st.Q().GetMachineIDConflictCluster(r.Context(),
		sqlc.GetMachineIDConflictClusterParams{
			MachineID: req.MachineID, ClusterID: c.ID}); err == nil {
		respondError(w, proto.Err(409, proto.CodeMachineIDConflict,
			"machineId already registered in cluster "+strconv.Quote(name)))
		return
	}
	// allowAdopt=true：同 cluster 异 key → adopt（见上方注释；token 路径不开启）。
	node, apiErr := h.enrollNode(r.Context(), c.ID, enrollReq{
		Hostname: req.Hostname, MachineID: req.MachineID,
		OSVersion: req.OSVersion, AgentVersion: req.AgentVersion,
		PublicKey: req.PublicKey,
	}, nil, true, "register", u.ID)
	if apiErr != nil {
		respondError(w, apiErr)
		return
	}
	respondEnrolled(w, node)
}
