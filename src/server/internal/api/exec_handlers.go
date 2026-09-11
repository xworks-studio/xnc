package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
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
	// sessionTicketTTL（Stage B）：会话数据腿 sdata 票据有效期——只在双侧
	// 拨号窗口消费（会话建立后票据不再校验），1h 覆盖重试/晚拨余量。
	sessionTicketTTL = time.Hour
)

// relaySessionEndpoint 为节点选 relay 数据腿端点（sdata 传输候选）。同一
// pool.Assign 的节点粘性语义与桌面媒体一致（node→relay 全 kind 统一）。
// 无 = 主站旧路径。
func (h *handlers) relaySessionEndpoint(nodeID string) (ep, relayID string, ok bool) {
	if h.pool == nil {
		return "", "", false
	}
	a, ok := h.pool.AssignData(nodeID) // 仅含宣告 sdata 的 relay（粘性独立于媒体）
	if !ok {
		return "", "", false
	}
	for _, e := range a.Endpoints {
		if e.Transport == "sdata" && e.Host != "" && e.Port > 0 {
			return net.JoinHostPort(e.Host, strconv.Itoa(e.Port)), a.RelayID, true
		}
	}
	return "", "", false
}

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
	h.startSession(w, r, proto.KindExec, params, "exec.start", "exec.finish", nil, audit, nil, nil)
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

// startSession 是 exec/shell/file/tunnel/desktop 共享的会话创建路径：
// RBAC（requireMinRole "operator"）→ Create（双侧 token）→ 装配 finish/notify
// 钩子 → 经控制连接下发 SESSION_OPEN → 审计 openAction → 202。
// extra（可 nil）并入 202 响应体（desktop 的 relay 端点/票据等 kind 特有
// 字段；绝不入审计——票据不得落审计行）。auditExtra（可 nil）并入 open
// 审计 metadata（exec/shell 的 system=true 标记）。
// extraFn（可 nil，M2-Slice3 Task 4）：Create 之后按结果追加响应字段
// （desktop 的 lease 判定 {granted,leaseId}——授予与否只有 Create 后可知）。
// 返回 (result, true) 表示已写 202；false 表示已写错误响应。
func (h *handlers) startSession(w http.ResponseWriter, r *http.Request,
	kind string, params json.RawMessage, openAction, closeAction string,
	extra map[string]any, auditExtra map[string]string,
	extraFn func(res *session.CreateResult) map[string]any,
	finishExtra func(sessionID, reason string),
) (*session.CreateResult, bool) {
	u := auth.UserFrom(r.Context())
	nodeID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return nil, false
	}
	// RBAC：operator 及以上方可开会话（exec/shell/file/tunnel/desktop 全经
	// 此路径）；非成员 404、viewer 403 由 requireMinRole 统一写出。
	if _, ok := h.requireMinRole(w, r, nodeID, "operator"); !ok {
		return nil, false
	}
	// Create 内部检查 reg.Online：节点无控制连接 → 409 NODE_OFFLINE。
	res, apiErr := h.sess.Create(nodeID, u.ID, kind, params)
	if apiErr != nil {
		respondError(w, apiErr)
		return nil, false
	}

	// relay 数据面（2026-09-11 Stage B）：exec/shell/file/tunnel 的会话
	// 双腿改经外置 relay 的 session router（sdata 票据即凭证，路径与主站
	// 同形——agent/CLI 对 URL 无假设，web toWsUrl 透传绝对地址，存量端
	// 零改动）。desktop 除外（RTV 专腿）。无可用的 sdata 端点（relay 未
	// 宣告域名数据面/池空）= 主站旧路径，行为不变。
	var relayFinish func()
	agentWSURL := wsBaseURL(r) + "/api/agent/session?token=" + res.AgentToken
	clientWSURL := "/api/session/" + res.Session.ID + "?token=" + res.ClientToken
	if kind != proto.KindDesktop {
		if ep, rid, ok := h.relaySessionEndpoint(nodeID.String()); ok {
			atok, aerr := h.rtvSign.SessionDataTicket(res.Session.ID, nodeID.String(), rid, "agent", sessionTicketTTL)
			ctok, cerr := h.rtvSign.SessionDataTicket(res.Session.ID, nodeID.String(), rid, "client", sessionTicketTTL)
			if aerr == nil && cerr == nil {
				agentWSURL = "wss://" + ep + "/api/agent/session?token=" + atok
				clientWSURL = "wss://" + ep + "/api/session/" + res.Session.ID + "?token=" + ctok
				h.sess.MarkRelayRouted(res.Session.ID)
				relayFinish = func() { // 终局撤销：relay 关腿 + 墓碑（票据不可复活）
					h.pool.SessionKill(rid, nodeID.String(), res.Session.ID, "session-end")
				}
				slog.Info("session routed via relay", "kind", kind, "session", res.Session.ID, "relay", rid)
			} else {
				slog.Error("relay: session ticket mint failed; falling back to main-site legs", "err", errors.Join(aerr, cerr))
			}
		}
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
		if relayFinish != nil {
			relayFinish()
		}
		if finishExtra != nil {
			finishExtra(res.Session.ID, reason)
		}
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
	// 与 REST 响应/agent 看到的同一份）。WsURL = relay 数据腿（Stage B）
	// 或主站旧路径。
	openMsg, err := proto.NewMsg(proto.TypeSessionOpen, proto.SessionOpen{
		SessionID: res.Session.ID, Kind: kind, Params: res.Session.Params,
		AgentToken: res.AgentToken,
		WsURL:      agentWSURL,
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
	// AgentToken 绝不进 REST 响应；client 拿到的 token 是一次性 ClientToken
	// （relay 路径下改随 URL 的 sdata 张票，token 字段保留主站旧值仅为
	// 兼容旧客户端解析——它不会被消费）。
	body := map[string]any{
		"sessionId":    res.Session.ID,
		"token":        res.ClientToken,
		"expiresAt":    res.ExpiresAt,
		"websocketUrl": clientWSURL,
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
