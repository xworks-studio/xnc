package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/db/sqlc"
)

// moveNode 处理 POST /api/nodes/{id}/move {"target": "<clusterId或name>"}：
// 把节点搬到另一 cluster（机器单归属下的正规换组路径，nodeId/公钥/状态/
// 在线控制连接全部保留——连接身份是公钥挑战-应答，registry 不缓存 cluster）。
//
// 权限 = 源 ∧ 目标 cluster 双 owner（把机器移出旧组、放入新组都需要资产方
// 同意；单人多 cluster 即自己）。目标内重名自动 `<name>-2` 递增（enroll 同
// 款）；名字竞态与 machine 全局唯一由 DB 约束兜底，23505 按约束名分流
// （machine → 409 MACHINE_ID_CONFLICT，name → 换后缀重试，savepoint 保事务
// 可继续——PG 语句失败会中止事务）。乐观并发：UPDATE 带 cluster_id=源 条
// 件，0 行 = 节点已被并发 move/删除 → 409 请重试。审计 node.move。
func (h *handlers) moveNode(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	nodeID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return
	}
	node, err := h.st.Q().GetNodeByID(r.Context(), nodeID)
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return
	}
	var req struct{ Target string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Target == "" {
		respondError(w, proto.Err(400, proto.CodeInternal, "target required"))
		return
	}
	target, apiErr := h.resolveCluster(r, req.Target)
	if apiErr != nil {
		respondError(w, apiErr)
		return
	}
	if target.ID == node.ClusterID {
		respondError(w, proto.Err(400, proto.CodeInternal, "node already in target cluster"))
		return
	}
	// 双 owner：源（忽略 disabled，与 disable/enable 同款信任）与目标。
	for _, cid := range [2]uuid.UUID{node.ClusterID, target.ID} {
		if role, err := h.st.Q().GetMemberRole(r.Context(), sqlc.GetMemberRoleParams{
			ClusterID: cid, UserID: u.ID,
		}); err != nil || role != "owner" {
			respondError(w, proto.Err(403, proto.CodeForbidden, "owner role required in both clusters"))
			return
		}
	}
	// 前置友好检查（message 带占用 cluster 名；唯一索引兜底并发窗口）。
	if name, err := h.st.Q().GetMachineIDConflictCluster(r.Context(),
		sqlc.GetMachineIDConflictClusterParams{
			MachineID: node.MachineID, ClusterID: node.ClusterID,
		}); err == nil {
		respondError(w, proto.Err(409, proto.CodeMachineIDConflict,
			"machineId already registered in cluster "+strconv.Quote(name)))
		return
	}

	ctx := r.Context()
	tx, err := h.st.Pool().Begin(ctx)
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "tx"))
		return
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)

	// 目标内名字唯一化（enroll 同款 hostname-2 递增）。
	name := node.Name
	for i := 2; ; i++ {
		if _, err := q.GetNodeByNameInCluster(ctx, sqlc.GetNodeByNameInClusterParams{
			ClusterID: target.ID, Name: name,
		}); err != nil {
			break
		}
		name = node.Name + "-" + strconv.Itoa(i)
	}

	for attempt := 0; ; attempt++ {
		if _, err := tx.Exec(ctx, "SAVEPOINT node_move"); err != nil {
			respondError(w, proto.Err(500, proto.CodeInternal, "savepoint"))
			return
		}
		n, err := q.MoveNodeCluster(ctx, sqlc.MoveNodeClusterParams{
			ID: node.ID, ClusterID: target.ID, Name: name, ClusterID_2: node.ClusterID,
		})
		if err == nil {
			if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT node_move"); err != nil {
				respondError(w, proto.Err(500, proto.CodeInternal, "release savepoint"))
				return
			}
			if n == 0 {
				// 并发 move/删除改掉了源 cluster——整体回滚请重试。
				respondError(w, proto.Err(409, proto.CodeInternal,
					"node changed concurrently; retry"))
				return
			}
			if err := q.InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
				UserID:    pgUUID(u.ID),
				ClusterID: pgUUID(target.ID),
				NodeID:    pgUUID(node.ID),
				Action:    "node.move",
				Metadata: mustJSON(map[string]string{
					"from_cluster": node.ClusterID.String(),
					"to_cluster":   target.ID.String(),
					"old_name":     node.Name,
					"new_name":     name,
				}),
			}); err != nil {
				respondError(w, proto.Err(500, proto.CodeInternal, "audit"))
				return
			}
			if err := tx.Commit(ctx); err != nil {
				respondError(w, proto.Err(500, proto.CodeInternal, "commit"))
				return
			}
			respondJSON(w, 200, map[string]string{
				"nodeId": node.ID.String(), "clusterId": target.ID.String(), "name": name,
			})
			return
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			respondError(w, proto.Err(500, proto.CodeInternal, "move node"))
			return
		}
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT node_move"); err != nil {
			respondError(w, proto.Err(500, proto.CodeInternal, "rollback savepoint"))
			return
		}
		switch pgErr.ConstraintName {
		case "nodes_machine_id_key":
			// 并发窗口内 machine 已进其他 cluster（全局唯一索引兜底）。
			respondError(w, proto.Err(409, proto.CodeMachineIDConflict,
				"machineId already registered in another cluster"))
			return
		case "nodes_cluster_id_name_key":
			// 名字唯一化竞态：换下一后缀重试（≤3 次后放弃）。
			if attempt >= 3 {
				respondError(w, proto.Err(500, proto.CodeInternal, "move node name conflict"))
				return
			}
			name = node.Name + "-" + strconv.Itoa(attempt+2)
		default:
			respondError(w, proto.Err(500, proto.CodeInternal, "move node"))
			return
		}
	}
}
