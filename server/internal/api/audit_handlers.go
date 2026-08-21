package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/db/sqlc"
)

const (
	auditDefaultLimit = 50
	// auditMaxLimit：单页上限，防 admin 误操作全表拉取。
	auditMaxLimit = 200
)

// auditDTO：审计行对外形态。可空维度（user/cluster/node）NULL 序列化为 null。
type auditDTO struct {
	ID        int64           `json:"id"`
	UserID    *string         `json:"user_id"`
	ClusterID *string         `json:"cluster_id"`
	NodeID    *string         `json:"node_id"`
	Action    string          `json:"action"`
	SessionID string          `json:"session_id"`
	Metadata  json.RawMessage `json:"metadata"`
	CreatedAt time.Time       `json:"created_at"`
}

// listAudit 处理 GET /api/audit?nodeId=&userId=&action=&since=&limit=&offset=：
// admin-only（isAdminUser）。过滤全部可选、可组合；since 为时长（24h/7d，d
// 后缀=24h 的倍数）或 RFC3339 绝对时间；created_at DESC + LIMIT/OFFSET
// （默认 50，上限 200）。非法参数一律 400，不静默吞。
func (h *handlers) listAudit(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !isAdminUser(r.Context(), h.st, u.ID) {
		respondError(w, proto.Err(403, proto.CodeForbidden, "admin required"))
		return
	}
	q := r.URL.Query()
	var p sqlc.QueryAuditLogParams
	p.Limit = auditDefaultLimit

	if s := q.Get("nodeId"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			respondError(w, proto.Err(400, proto.CodeInternal, "invalid nodeId"))
			return
		}
		p.NodeID = pgUUID(id)
	}
	if s := q.Get("userId"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			respondError(w, proto.Err(400, proto.CodeInternal, "invalid userId"))
			return
		}
		p.UserID = pgUUID(id)
	}
	if s := q.Get("action"); s != "" {
		p.Action = pgtype.Text{String: s, Valid: true}
	}
	if s := q.Get("since"); s != "" {
		t, err := parseAuditSince(s)
		if err != nil {
			respondError(w, proto.Err(400, proto.CodeInternal,
				"since must be a duration (24h, 7d) or RFC3339 timestamp"))
			return
		}
		p.Since = pgtype.Timestamptz{Time: t, Valid: true}
	}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			respondError(w, proto.Err(400, proto.CodeInternal, "invalid limit"))
			return
		}
		if n > auditMaxLimit {
			n = auditMaxLimit
		}
		p.Limit = int32(n)
	}
	if s := q.Get("offset"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			respondError(w, proto.Err(400, proto.CodeInternal, "invalid offset"))
			return
		}
		p.Offset = int32(n)
	}

	rows, err := h.st.Q().QueryAuditLog(r.Context(), p)
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "query audit"))
		return
	}
	out := make([]auditDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, auditDTO{
			ID: row.ID, UserID: uuidPtr(row.UserID), ClusterID: uuidPtr(row.ClusterID),
			NodeID: uuidPtr(row.NodeID), Action: row.Action,
			SessionID: row.SessionID, Metadata: json.RawMessage(row.Metadata),
			CreatedAt: row.CreatedAt,
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// parseAuditSince 支持 (1) Go 时长（24h/30m/10s）；(2) Nd 天数简写（7d——
// time.ParseDuration 不识别 d 后缀，CLI 契约 cli.md §57 用 7d）；(3) RFC3339
// 绝对时间。时长换算为 now−d；负时长拒绝（since 指向未来无意义）。
func parseAuditSince(s string) (time.Time, error) {
	if d, ok := strings.CutSuffix(s, "d"); ok {
		if days, err := strconv.Atoi(d); err == nil && days > 0 {
			return time.Now().Add(-time.Duration(days) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return time.Time{}, errors.New("negative since")
		}
		return time.Now().Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, errors.New("invalid since")
}

// uuidPtr：pgtype.UUID → *string（NULL 维度序列化为 JSON null）。
func uuidPtr(v pgtype.UUID) *string {
	if !v.Valid {
		return nil
	}
	s := uuid.UUID(v.Bytes).String()
	return &s
}
