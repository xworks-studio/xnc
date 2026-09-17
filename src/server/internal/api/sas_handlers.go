package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/db/sqlc"
)

// SAS（secure attention sequence）REST 面（2026-09-17 安全桌面交互）：
// POST /api/nodes/{id}/desktop/sas —— web 工具栏"发送 Ctrl+Alt+Del"。
// 链路：RBAC(operator+，与桌面会话同门) → 控制连接 SAS_REQUEST → agent →
// XNCCore 0x0110(SendSAS) → SAS_RESULT 回执（reqId 关联）→ 200/502/504。
// agentws.go 的 TypeSasResult 分发投递到 sasWaiters。
//
// 能力语义：这是"操控安全桌面"级别的能力（可在登录界面输入凭据），门 =
// operator+ RBAC + 审计行（desktop.sas），与 desktop.open 一致。

// sasAckTimeout：等待 agent 回执的 HTTP 侧上限。agent 的 dial+RPC 均有界
// （handshakeTimeout/rpcTimeout，各秒级）；8s 覆盖 core 忙的余量。
// var 而非 const：测试注入短超时驱动 504 路径。
var sasAckTimeout = 8 * time.Second

// sasRegister 登记 reqId 等待方（带缓冲 1：超时后迟到的回执可无阻塞投递，
// 随 chan 一起被 GC——投递方 agentws 只在 map 命中时写）。
func (h *handlers) sasRegister(reqID string) chan proto.SasResult {
	ch := make(chan proto.SasResult, 1)
	h.sasMu.Lock()
	defer h.sasMu.Unlock()
	if h.sasWaiters == nil {
		h.sasWaiters = make(map[string]chan proto.SasResult)
	}
	h.sasWaiters[reqID] = ch
	return ch
}

// sasUnregister 摘除等待方（收到回执或超时；迟到的 agent 回执自然落空）。
func (h *handlers) sasUnregister(reqID string) {
	h.sasMu.Lock()
	defer h.sasMu.Unlock()
	delete(h.sasWaiters, reqID)
}

// sasDeliver agentws 收到 SAS_RESULT 时投递；无等待方（迟到/未知 reqId）
// 静默丢弃并记日志。
func (h *handlers) sasDeliver(res proto.SasResult) {
	h.sasMu.Lock()
	ch, ok := h.sasWaiters[res.ReqID]
	if ok {
		delete(h.sasWaiters, res.ReqID)
	}
	h.sasMu.Unlock()
	if !ok {
		slog.Warn("sas result without waiter", "reqId", res.ReqID)
		return
	}
	ch <- res // 缓冲 1 且刚摘除：绝不阻塞 agent 读循环
}

// nodeSas 处理 POST /api/nodes/{id}/desktop/sas。
func (h *handlers) nodeSas(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	nodeID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return
	}
	// RBAC：operator 及以上（viewer 403、非成员 404 由 requireMinRole 统一写出）。
	node, ok := h.requireMinRole(w, r, nodeID, "operator")
	if !ok {
		return
	}
	nodeConn := h.reg.Get(nodeID.String())
	if nodeConn == nil || nodeConn.Send == nil {
		respondError(w, proto.Err(409, proto.CodeNodeOffline, "node is offline"))
		return
	}

	reqID := uuid.NewString()
	// Reason = 触发者标识，落 core 侧 sas_audit 日志行（24 字节截断）。
	msg, err := proto.NewMsg(proto.TypeSasRequest, proto.SasRequest{ReqID: reqID, Reason: u.Email})
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "encode sas request"))
		return
	}
	ch := h.sasRegister(reqID)
	if err := nodeConn.Send(msg); err != nil {
		h.sasUnregister(reqID)
		respondError(w, proto.Err(502, proto.CodeInternal, "dispatch sas request failed"))
		return
	}

	var res proto.SasResult
	select {
	case res = <-ch:
	case <-time.After(sasAckTimeout):
		h.sasUnregister(reqID)
		h.sasAudit(node, u.ID, reqID, "timeout")
		respondError(w, proto.Err(504, proto.CodeInternal, "node did not answer sas request in time"))
		return
	}

	// 审计（背景 ctx：r.Context() 随响应写出可能被取消，审计不丢）。
	status := "ok"
	if !res.OK {
		status = "rejected"
	}
	h.sasAudit(node, u.ID, reqID, status)

	// OK 但 hr≠0：SendSAS 返回 VOID，hr 是 SEH 捕获的异常码（典型
	// C0000022=策略拒绝）——如实上报，由 web 提示。
	respondJSON(w, http.StatusOK, map[string]any{
		"ok": res.OK, "hr": res.HR, "code": res.Code, "err": res.Err,
	})
}

// sasAudit 写 desktop.sas 审计行（独立 background ctx，同 exec 审计纪律）。
func (h *handlers) sasAudit(node *sqlc.Node, userID uuid.UUID, reqID, status string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	meta, _ := json.Marshal(map[string]string{"reqId": reqID, "status": status})
	_ = h.st.Q().InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
		UserID: pgUUID(userID), NodeID: pgUUID(node.ID),
		ClusterID: pgtype.UUID{Bytes: node.ClusterID, Valid: true},
		Action:    "desktop.sas", Metadata: meta,
	})
}
