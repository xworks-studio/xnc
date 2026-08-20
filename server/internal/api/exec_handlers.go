package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/db/sqlc"
	"xnc/server/internal/session"
)

const (
	execScriptMaxBytes = 256 * 1024
	execTimeoutDefault = 300
	execTimeoutMax     = 86400
	// execBodyMaxBytes：请求体上限 512KB——覆盖 256KB script 加 JSON 结构与
	// 其余字段的转义开销，先于解码生效，杜绝无限缓冲。
	execBodyMaxBytes = 512 * 1024
	// auditInsertTimeout：审计写入用独立 background ctx，不用 r.Context()。
	// exec.start 写入发生在 SESSION_OPEN 已下发之后——客户端此刻断连即取消
	// r.Context()，start 行会丢；exec.finish 更是在 POST 返回很久之后才触发。
	auditInsertTimeout = 5 * time.Second
)

// execReq 的 TimeoutSec 为指针：缺省（nil）→ 默认 300；显式给出则必须
// 落在 [1, 86400]——显式 0 视为非法（客户端想用默认值应省略字段）。
type execReq struct {
	Command    string `json:"command"`
	Script     string `json:"script"`
	TimeoutSec *int   `json:"timeoutSec"`
	Cwd        string `json:"cwd"`
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// execStart 处理 POST /api/nodes/{id}/exec：鉴权（router 中间件）+ membership
// → 校验 → 委托 startSession（Create（双侧 token）→ 装配 finish/notify 钩子 →
// 经控制连接下发 SESSION_OPEN → 审计 exec.start → 202 统一异步响应）。
func (h *handlers) execStart(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, execBodyMaxBytes)

	var req execReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}
	timeout := execTimeoutDefault
	if req.TimeoutSec != nil {
		timeout = *req.TimeoutSec
	}
	switch {
	case req.Command == "" && req.Script == "":
		respondError(w, proto.Err(400, proto.CodeInternal, "command or script required"))
		return
	case req.Command != "" && req.Script != "":
		respondError(w, proto.Err(400, proto.CodeInternal, "command and script are exclusive"))
		return
	case len(req.Script) > execScriptMaxBytes:
		respondError(w, proto.Err(400, proto.CodeFileTooLarge, "script exceeds 256KB"))
		return
	case timeout < 1 || timeout > execTimeoutMax:
		respondError(w, proto.Err(400, proto.CodeInternal, "timeoutSec out of range"))
		return
	}

	params, err := json.Marshal(proto.ExecParams{
		Command: req.Command, Script: req.Script, TimeoutSec: timeout, Cwd: req.Cwd,
	})
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "encode params"))
		return
	}
	h.startSession(w, r, proto.KindExec, params, "exec.start", "exec.finish")
}

// startSession 是 exec/shell 共享的会话创建路径：membership → Create（双侧
// token）→ 装配 finish/notify 钩子 → 经控制连接下发 SESSION_OPEN → 审计
// openAction → 202。返回 (result, true) 表示已写 202；false 表示已写错误响应。
func (h *handlers) startSession(w http.ResponseWriter, r *http.Request,
	kind string, params json.RawMessage, openAction, closeAction string,
) (*session.CreateResult, bool) {
	u := auth.UserFrom(r.Context())
	nodeID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return nil, false
	}
	if _, err := h.st.Q().GetNodeForUser(r.Context(), sqlc.GetNodeForUserParams{
		UserID: u.ID, ID: nodeID,
	}); err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return nil, false
	}

	// Create 内部检查 reg.Online：节点无控制连接 → 409 NODE_OFFLINE。
	res, apiErr := h.sess.Create(nodeID, u.ID, kind, params)
	if apiErr != nil {
		respondError(w, apiErr)
		return nil, false
	}

	// 钩子在 Create 后立即装配（早于任何可触发 NotifyClose 的路径）：
	// Opening 超时（60s）、pump 断连、SESSION_REFUSED 都可能在 handler
	// 返回后异步关闭会话，届时 finish/notify 必须已就位。
	// finish 不含会话内容（命令/VT），仅 reason/kind/sessionId。
	h.sess.SetFinishFn(res.Session, func(reason string) {
		ctx, cancel := context.WithTimeout(context.Background(), auditInsertTimeout)
		defer cancel()
		_ = h.st.Q().InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
			UserID: pgUUID(u.ID), NodeID: pgUUID(nodeID), Action: closeAction,
			Metadata: mustJSON(map[string]string{
				"reason": reason, "kind": kind, "sessionId": res.Session.ID,
			}),
		})
	})
	// NotifyClose → SESSION_CLOSE 经控制连接下发；连接已失则跳过
	// （manager 的 close 路径已完成双侧清理）。
	h.sess.SetNotifyFn(res.Session, func(sc proto.SessionClose) error {
		if nc := h.reg.Get(nodeID.String()); nc != nil && nc.Send != nil {
			msg, err := proto.NewMsg(proto.TypeSessionClose, sc)
			if err != nil {
				return err
			}
			return nc.Send(msg)
		}
		return nil
	})

	// 控制连接下发 SESSION_OPEN；失败 → 兜底清理会话 + 409 NODE_OFFLINE。
	nodeConn := h.reg.Get(nodeID.String())
	if nodeConn == nil || nodeConn.Send == nil {
		h.sess.NotifyClose(res.Session.ID, "node-offline")
		respondError(w, proto.Err(409, proto.CodeNodeOffline, "node is offline"))
		return nil, false
	}
	openMsg, err := proto.NewMsg(proto.TypeSessionOpen, proto.SessionOpen{
		SessionID: res.Session.ID, Kind: kind, Params: params,
		AgentToken: res.AgentToken,
		WsURL:      wsBaseURL(r) + "/api/agent/session?token=" + res.AgentToken,
		ExpiresAt:  res.ExpiresAt,
	})
	if err != nil {
		h.sess.NotifyClose(res.Session.ID, "internal")
		respondError(w, proto.Err(500, proto.CodeInternal, "encode session open"))
		return nil, false
	}
	if err := nodeConn.Send(openMsg); err != nil {
		h.sess.NotifyClose(res.Session.ID, "node-offline")
		respondError(w, proto.Err(409, proto.CodeNodeOffline, "node is offline"))
		return nil, false
	}

	// 独立 background ctx：此刻 SESSION_OPEN 已下发，客户端随时可能断连
	// 取消 r.Context()，审计 open 行必须不受影响。
	actx, acancel := context.WithTimeout(context.Background(), auditInsertTimeout)
	defer acancel()
	_ = h.st.Q().InsertAuditLog(actx, sqlc.InsertAuditLogParams{
		UserID: pgUUID(u.ID), NodeID: pgUUID(nodeID), Action: openAction,
		Metadata: mustJSON(map[string]string{"kind": kind, "sessionId": res.Session.ID}),
	})
	// AgentToken 绝不进 REST 响应；client 拿到的 token 是一次性 ClientToken。
	respondJSON(w, 202, map[string]any{
		"sessionId":    res.Session.ID,
		"token":        res.ClientToken,
		"expiresAt":    res.ExpiresAt,
		"websocketUrl": "/api/session/" + res.Session.ID + "?token=" + res.ClientToken,
	})
	return res, true
}
