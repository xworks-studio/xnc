package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/db/sqlc"
	"xnc/server/internal/tokens"
)

// authorizeClusterOwner 校验用户是目标 cluster（UUID 或 name）的 owner。
func (h *handlers) authorizeClusterOwner(r *http.Request, idOrName string) (sqlc.Cluster, *proto.APIError) {
	u := auth.UserFrom(r.Context())
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
	member, merr := h.st.Pool().Query(r.Context(),
		`SELECT 1 FROM cluster_members WHERE cluster_id=$1 AND user_id=$2 AND role='owner'`,
		c.ID, u.ID)
	if merr != nil || !member.Next() {
		member.Close()
		return c, proto.Err(403, proto.CodeForbidden, "owner role required")
	}
	member.Close()
	return c, nil
}

func (h *handlers) createEnrollToken(w http.ResponseWriter, r *http.Request) {
	c, apiErr := h.authorizeClusterOwner(r, chi.URLParam(r, "id"))
	if apiErr != nil {
		respondError(w, apiErr)
		return
	}
	var req struct {
		TTL     string `json:"ttl"`
		MaxUses int32  `json:"maxUses"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	ttl := h.cfg.EnrollTokenTTL
	if req.TTL != "" {
		if p, err := time.ParseDuration(req.TTL); err == nil {
			ttl = p
		}
	}
	maxUses := int32(1)
	if req.MaxUses > 0 {
		maxUses = req.MaxUses
	}
	pt, hash, err := tokens.Generate()
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "token gen"))
		return
	}
	t, err := h.st.Q().CreateEnrollmentToken(r.Context(), sqlc.CreateEnrollmentTokenParams{
		ID: uuid.New(), ClusterID: c.ID, TokenHash: hash,
		ExpiresAt: time.Now().Add(ttl), MaxUses: maxUses,
		CreatedBy: auth.UserFrom(r.Context()).ID,
	})
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "token save"))
		return
	}
	respondJSON(w, 201, map[string]any{
		"id": t.ID, "token": pt, "expiresAt": t.ExpiresAt,
	})
}

type enrollReq struct {
	Token        string `json:"token"`
	Hostname     string `json:"hostname"`
	MachineID    string `json:"machineId"`
	OSVersion    string `json:"osVersion"`
	AgentVersion string `json:"agentVersion"`
	PublicKey    string `json:"publicKey"`
}

// enrollNode 共用注册落库事务（spec §6.4：JWT 注册路径"等价于服务端铸造并
// 消费一次性 enrollment token"——两条路径只差授权，落库语义同一）：
//
//	tx 开始 → 幂等：同 (cluster, machineId, publicKey) 复用既有节点（不烧
//	token，不重复审计）→ 同 machineId 异 key：allowAdopt 时 adopt（重绑公钥、
//	刷新机器字段复用既有节点行 + 双审计），否则 409 NODE_ALREADY_ENROLLED →
//	tokenID 非 nil 时消费一次性 token → 节点名唯一化（hostname、hostname-2…）
//	→ CreateNode → 审计 → commit。
//
// tokenID 由 token 授权路径（/api/agent/enroll）传入；用户 JWT 路径传 nil。
// 审计 action 与 actor 由调用方给出（"node.enroll"/token 创建者 vs
// "register"/调用用户）。allowAdopt 仅用户 JWT register 路径开启（controller
// 批准的 adopt 语义）；token 路径异 key 维持 409（token 授权域不做身份接
// 管）。req.Token 字段在此路径被忽略。
func (h *handlers) enrollNode(ctx context.Context, clusterID uuid.UUID, req enrollReq,
	tokenID *uuid.UUID, allowAdopt bool, auditAction string, auditActor uuid.UUID,
) (sqlc.Node, *proto.APIError) {
	tx, err := h.st.Pool().Begin(ctx)
	if err != nil {
		return sqlc.Node{}, proto.Err(500, proto.CodeInternal, "tx")
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)

	// 幂等：同 (cluster, machineId, publicKey) 复用既有节点。
	if existing, err := q.GetNodeByIdentity(ctx, sqlc.GetNodeByIdentityParams{
		ClusterID: clusterID, MachineID: req.MachineID}); err == nil {
		if existing.PublicKey == req.PublicKey {
			_ = tx.Commit(ctx)
			return existing, nil
		}
		// 同 machineId 异 key：adopt（重绑公钥/刷新机器字段复用既有节点行）或
		// 409。adopt 双审计（同事务）：常规注册审计（如常）+ node_adopt（显式
		// 可查的身份重绑记录）。
		if !allowAdopt {
			return sqlc.Node{}, proto.Err(409, proto.CodeNodeAlreadyEnrolled,
				"machine already enrolled with a different key")
		}
		node, err := q.AdoptNodeIdentity(ctx, sqlc.AdoptNodeIdentityParams{
			PublicKey: req.PublicKey, Hostname: req.Hostname,
			OsVersion: req.OSVersion, AgentVersion: req.AgentVersion, ID: existing.ID,
		})
		if err != nil {
			return sqlc.Node{}, proto.Err(500, proto.CodeInternal, "adopt node")
		}
		for _, action := range []string{auditAction, "node_adopt"} {
			if err := q.InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
				UserID:    pgtype.UUID{Bytes: auditActor, Valid: true},
				ClusterID: pgtype.UUID{Bytes: clusterID, Valid: true},
				NodeID:    pgtype.UUID{Bytes: node.ID, Valid: true},
				Action:    action, Metadata: []byte("{}"),
			}); err != nil {
				return sqlc.Node{}, proto.Err(500, proto.CodeInternal, "audit")
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return sqlc.Node{}, proto.Err(500, proto.CodeInternal, "commit")
		}
		return node, nil
	}

	if tokenID != nil {
		if _, err := q.ConsumeEnrollmentToken(ctx, *tokenID); err != nil {
			return sqlc.Node{}, proto.Err(401, proto.CodeEnrollmentTokenInvalid, "token exhausted")
		}
	}

	name := req.Hostname
	for i := 2; ; i++ {
		if _, err := q.GetNodeByNameInCluster(ctx, sqlc.GetNodeByNameInClusterParams{
			ClusterID: clusterID, Name: name}); err != nil {
			break
		}
		name = req.Hostname + "-" + strconv.Itoa(i)
	}

	node, err := q.CreateNode(ctx, sqlc.CreateNodeParams{
		ID: uuid.New(), ClusterID: clusterID, Name: name, MachineID: req.MachineID,
		Hostname: req.Hostname, OsVersion: req.OSVersion,
		AgentVersion: req.AgentVersion, PublicKey: req.PublicKey,
	})
	if err != nil {
		return sqlc.Node{}, proto.Err(500, proto.CodeInternal, "create node")
	}
	// audit_logs 的 user_id/cluster_id/node_id 为可空列，sqlc 生成 pgtype.UUID（见 Task 6 评审注记）。
	if err := q.InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
		UserID:    pgtype.UUID{Bytes: auditActor, Valid: true},
		ClusterID: pgtype.UUID{Bytes: clusterID, Valid: true},
		NodeID:    pgtype.UUID{Bytes: node.ID, Valid: true},
		Action:    auditAction, Metadata: []byte("{}"),
	}); err != nil {
		return sqlc.Node{}, proto.Err(500, proto.CodeInternal, "audit")
	}
	if err := tx.Commit(ctx); err != nil {
		return sqlc.Node{}, proto.Err(500, proto.CodeInternal, "commit")
	}
	return node, nil
}

// respondEnrolled 统一两条注册路径的成功响应。
func respondEnrolled(w http.ResponseWriter, node sqlc.Node) {
	respondJSON(w, 201, map[string]string{
		"nodeId": node.ID.String(), "clusterId": node.ClusterID.String(), "name": node.Name})
}

// agentEnroll 处理 POST /api/agent/enroll：一次性 enrollment token 授权的
// 节点注册——token 查验/过期判定后走共用落库事务 enrollNode（token 在事务内
// 幂等检查之后消费，幂等复用不烧 token）。
func (h *handlers) agentEnroll(w http.ResponseWriter, r *http.Request) {
	var req enrollReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}
	tok, err := h.st.Q().GetEnrollmentTokenByHash(r.Context(), tokens.Hash(req.Token))
	if err != nil {
		respondError(w, proto.Err(401, proto.CodeEnrollmentTokenInvalid, "invalid token"))
		return
	}
	if time.Now().After(tok.ExpiresAt) {
		respondError(w, proto.Err(410, proto.CodeEnrollmentTokenExpired, "token expired"))
		return
	}
	pub, err := base64.StdEncoding.DecodeString(req.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad public key"))
		return
	}
	node, apiErr := h.enrollNode(r.Context(), tok.ClusterID, req, &tok.ID, false,
		"node.enroll", tok.CreatedBy)
	if apiErr != nil {
		respondError(w, apiErr)
		return
	}
	respondEnrolled(w, node)
}
