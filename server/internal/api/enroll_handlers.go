package api

import (
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

	ctx := r.Context()
	tx, err := h.st.Pool().Begin(ctx)
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "tx"))
		return
	}
	defer tx.Rollback(ctx)
	q := sqlc.New(tx)

	// 幂等：同 (cluster, machineId, publicKey) 复用既有节点。
	if existing, err := q.GetNodeByIdentity(ctx, sqlc.GetNodeByIdentityParams{
		ClusterID: tok.ClusterID, MachineID: req.MachineID}); err == nil {
		if existing.PublicKey == req.PublicKey {
			_ = tx.Commit(ctx)
			respondJSON(w, 201, map[string]string{
				"nodeId": existing.ID.String(), "clusterId": existing.ClusterID.String(),
				"name": existing.Name})
			return
		}
		respondError(w, proto.Err(409, proto.CodeNodeAlreadyEnrolled,
			"machine already enrolled with a different key"))
		return
	}

	if _, err := q.ConsumeEnrollmentToken(ctx, tok.ID); err != nil {
		respondError(w, proto.Err(401, proto.CodeEnrollmentTokenInvalid, "token exhausted"))
		return
	}

	name := req.Hostname
	for i := 2; ; i++ {
		if _, err := q.GetNodeByNameInCluster(ctx, sqlc.GetNodeByNameInClusterParams{
			ClusterID: tok.ClusterID, Name: name}); err != nil {
			break
		}
		name = req.Hostname + "-" + strconv.Itoa(i)
	}

	node, err := q.CreateNode(ctx, sqlc.CreateNodeParams{
		ID: uuid.New(), ClusterID: tok.ClusterID, Name: name, MachineID: req.MachineID,
		Hostname: req.Hostname, OsVersion: req.OSVersion,
		AgentVersion: req.AgentVersion, PublicKey: req.PublicKey,
	})
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "create node"))
		return
	}
	// audit_logs 的 user_id/cluster_id/node_id 为可空列，sqlc 生成 pgtype.UUID（见 Task 6 评审注记）。
	if err := q.InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
		UserID:    pgtype.UUID{Bytes: tok.CreatedBy, Valid: true},
		ClusterID: pgtype.UUID{Bytes: tok.ClusterID, Valid: true},
		NodeID:    pgtype.UUID{Bytes: node.ID, Valid: true},
		Action:    "node.enroll", Metadata: []byte("{}"),
	}); err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "audit"))
		return
	}
	if err := tx.Commit(ctx); err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "commit"))
		return
	}
	respondJSON(w, 201, map[string]string{
		"nodeId": node.ID.String(), "clusterId": node.ClusterID.String(), "name": node.Name})
}
