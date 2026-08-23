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
	Command    string   `json:"command"`
	Script     string   `json:"script"`
	TimeoutSec *int     `json:"timeoutSec"`
	Cwd        string   `json:"cwd"`
	Shell      string   `json:"shell"`
	Env        []string `json:"env"`
	// System(M2-Slice2):SYSTEM 令牌显式请求,owner-only RBAC +
	// 审计 system=true;缺省 false = 用户令牌(spec §8.4 不隐式提权)。
	System bool `json:"system"`
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// execStart 处理 POST /api/nodes/{id}/exec：鉴权（router 中间件）+ RBAC
// （operator 及以上；system=true 收紧为 owner-only，M2-Slice2）→ 校验 →
// 委托 startSession（Create（双侧 token）→ 装配 finish/notify 钩子 →
// 经控制连接下发 SESSION_OPEN → 审计 exec.start（system=true 时携带）
// → 202 统一异步响应）。
func (h *handlers) execStart(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, execBodyMaxBytes)

	var req execReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}
	// system 令牌 = owner-only(Slice3 换票据 capability 前的最低线,
	// 裁决记录):operator 403 由 requireMinRole 统一写出。
	if req.System && !h.requireSystemRole(w, r) {
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
		Shell: req.Shell, Env: req.Env, System: req.System,
	})
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "encode params"))
		return
	}
	var audit map[string]string
	if req.System {
		audit = map[string]string{"system": "true"}
	}
	h.startSession(w, r, proto.KindExec, params, "exec.start", "exec.finish", nil, audit, nil)
}

// requireSystemRole 校验 system 令牌请求的 owner 身份(403 已写出时
// 返回 false;节点不存在统一 404,不泄漏存在性)。
func (h *handlers) requireSystemRole(w http.ResponseWriter, r *http.Request) bool {
	nodeID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return false
	}
	_, ok := h.requireMinRole(w, r, nodeID, "owner")
	return ok
}

// startSession 是 exec/shell/file/tunnel/screen/desktop 共享的会话创建路径：
// RBAC（requireMinRole "operator"）→ Create（双侧 token）→ 装配 finish/notify
// 钩子 → 经控制连接下发 SESSION_OPEN → 审计 openAction → 202。
// extra（可 nil）并入 202 响应体（desktop 的 turn 配置等 kind 特有字段；
// 绝不入审计——TURN 凭据不得落审计行）。auditExtra（可 nil）并入 open
// 审计 metadata（exec/shell 的 system=true 标记）。
// extraFn（可 nil，M2-Slice3 Task 4）：Create 之后按结果追加响应字段
// （desktop 的 lease 判定 {granted,leaseId}——授予与否只有 Create 后可知）。
// 返回 (result, true) 表示已写 202；false 表示已写错误响应。
func (h *handlers) startSession(w http.ResponseWriter, r *http.Request,
	kind string, params json.RawMessage, openAction, closeAction string,
	extra map[string]any, auditExtra map[string]string,
	extraFn func(res *session.CreateResult) map[string]any,
) (*session.CreateResult, bool) {
	u := auth.UserFrom(r.Context())
	nodeID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return nil, false
	}
	// RBAC：operator 及以上方可开会话（exec/shell/file/tunnel 全经此路径）；
	// 非成员 404、viewer 403 由 requireMinRole 统一写出。
	node, ok := h.requireMinRole(w, r, nodeID, "operator")
	if !ok {
		return nil, false
	}
	// desktop（M2-Slice3 Task 4）：按请求者角色计算 capability 集并入 params
	//（客户端提交值已在 handler 白名单剥离——这里只来自 server RBAC）。
	if kind == proto.KindDesktop {
		params = withDesktopCapabilities(r.Context(), h, node, u.ID, params)
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
	// SESSION_OPEN 用 manager 侧 params（desktop 授予时 leaseId 已嵌入；
	// 与 REST 响应/agent 看到的同一份）。
	openMsg, err := proto.NewMsg(proto.TypeSessionOpen, proto.SessionOpen{
		SessionID: res.Session.ID, Kind: kind, Params: res.Session.Params,
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
	meta := map[string]string{"kind": kind, "sessionId": res.Session.ID}
	for k, v := range auditExtra {
		meta[k] = v
	}
	_ = h.st.Q().InsertAuditLog(actx, sqlc.InsertAuditLogParams{
		UserID: pgUUID(u.ID), NodeID: pgUUID(nodeID), Action: openAction,
		Metadata: mustJSON(meta),
	})
	// AgentToken 绝不进 REST 响应；client 拿到的 token 是一次性 ClientToken。
	body := map[string]any{
		"sessionId":    res.Session.ID,
		"token":        res.ClientToken,
		"expiresAt":    res.ExpiresAt,
		"websocketUrl": "/api/session/" + res.Session.ID + "?token=" + res.ClientToken,
	}
	for k, v := range extra {
		body[k] = v
	}
	if extraFn != nil {
		for k, v := range extraFn(res) {
			body[k] = v
		}
	}
	respondJSON(w, 202, body)
	return res, true
}
