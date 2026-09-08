package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/db/sqlc"
)

// relayBodyMaxBytes：relay 管理端点请求体上限（状态流转一个字段）。
const relayBodyMaxBytes = 4 * 1024

// relayPool 池管理器接线面（rtvpool 注入；nil = 无在线通知——T5 前的
// 过渡形态，状态变更对在线 relay 的推送由池管理器承担）。
type relayPool interface {
	RelayStatusChanged(id, status string)
}

// relayStatusLegal 状态流转合法性：pending→active（审批）/pending→retired
// （拒绝）；active↔draining（摘除/回纳）；*→retired；retired→active
// （重新启用）。pending 不得直接 draining（未准入者无存量会话可摘）。
func relayStatusLegal(from, to string) bool {
	if to != "active" && to != "draining" && to != "retired" {
		return false
	}
	switch from {
	case "pending":
		return to == "active" || to == "retired"
	case "active", "draining", "retired":
		return true
	}
	return false
}

// adminListRelays 处理 GET /api/admin/relays：池清单（admin-only）。行
// 含注册画像与状态；在线/负载画像由池管理器（rtvpool）注入 live 字段。
func (h *handlers) adminListRelays(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !isAdminUser(r.Context(), h.st, u.ID) {
		respondError(w, proto.Err(403, proto.CodeForbidden, "admin only"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := h.st.Q().ListRelays(ctx)
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "list relays"))
		return
	}
	out := []map[string]any{}
	for _, row := range rows {
		out = append(out, relayRowJSON(row))
	}
	respondJSON(w, http.StatusOK, out)
}

// adminSetRelayStatus 处理 PATCH /api/admin/relays/{id}：状态流转
// （审批 pending→active 是主路径）。relay id 是文本（rl-<8hex>）非 UUID。
func (h *handlers) adminSetRelayStatus(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !isAdminUser(r.Context(), h.st, u.ID) {
		respondError(w, proto.Err(403, proto.CodeForbidden, "admin only"))
		return
	}
	id := chi.URLParam(r, "id")
	if id == "" || len(id) > 32 {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "relay not found"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, relayBodyMaxBytes)
	var req struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	row, err := h.st.Q().GetRelay(ctx, id)
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "relay not found"))
		return
	}
	if !relayStatusLegal(row.Status, req.Status) {
		respondError(w, proto.Err(409, proto.CodeInternal,
			"illegal relay status transition "+row.Status+" -> "+req.Status))
		return
	}
	n, err := h.st.Q().SetRelayStatus(ctx, sqlc.SetRelayStatusParams{ID: id, Status: req.Status})
	if err != nil || n == 0 {
		respondError(w, proto.Err(500, proto.CodeInternal, "update relay"))
		return
	}
	from := row.Status
	row.Status = req.Status
	// 在线通知（池管理器接线后生效；P1 无 drain 编排，仅同步内存画像）。
	if h.pool != nil {
		h.pool.RelayStatusChanged(id, req.Status)
	}
	// 审计走独立 background ctx：客户端断连不丢审计行（仓内惯例）。
	actx, acancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer acancel()
	_ = h.st.Q().InsertAuditLog(actx, sqlc.InsertAuditLogParams{
		Action:   "relay.status",
		Metadata: mustJSON(map[string]string{"relayId": id, "from": from, "to": req.Status}),
	})
	respondJSON(w, http.StatusOK, relayRowJSON(row))
}

// relayRowJSON DB 行 → JSON 视图（endpoints 原样透传 proto.EndpointDesc 数组）。
func relayRowJSON(row sqlc.Relay) map[string]any {
	var endpoints any
	_ = json.Unmarshal(row.Endpoints, &endpoints)
	return map[string]any{
		"id": row.ID, "pubkey": row.Pubkey, "region": row.Region,
		"endpoints": endpoints, "maxSessions": row.MaxSessions,
		"maxMbpsOut": row.MaxMbpsOut, "version": row.Version,
		"status":    row.Status,
		"createdAt": row.CreatedAt, "lastSeenAt": row.LastSeenAt,
	}
}
