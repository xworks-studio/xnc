// sessions.go — relay 侧会话数据面路由器（2026-09-11 Stage B）。
//
// exec/shell/file/tunnel 的会话数据腿从主站迁到 relay：两条腿挂本进程
// HTTP mux——agent 腿 /api/agent/session?token=（票据 sub=agent，sid 在
// 票据内）、客户端腿 /api/session/{sid}?token=（票据 sub=client）。粘合
// 后做帧不透明双向泵（复刻主站 pump.go 语义：不解析帧、8MiB 读限、任一
// 侧断开即双侧终局）。
//
// 准入 = sdata 票据离线验签（typ/rid/sid/sub 校验）；一次性消费由状态机
// 保证（每侧只许 attach 一次）。终局上报 RELAY_SESSION_CLOSED（server
// NotifyClose 等价源）；server 的 RELAY_SESSION_KILL 同时终结本路由器的
// 会话（双腿 + 墓碑由 rtv.Server 承担，此处只关腿）。活跃 sid 集并入
// STATS ActiveSids（server 代 Touch——idle 治理的数据源）。
package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"xnc/proto"
	"xnc/rtv"
)

// maxSessionFrameBytes 对齐主站会话 WS 读限（proto.MaxSessionFrameBytes；
// 此处独立常量避免 relay 耦合 proto 帧语义——读限是传输层防护）。
const maxSessionFrameBytes = 8 << 20

// attachWaitWindow 先到一侧等待另一侧的窗口（对齐主站 Opening TTL 的
// 宽容语义；超时单侧腿自行关闭，会话由 server 侧超时/撤销收尾）。
const attachWaitWindow = 60 * time.Second

// sessionRouter 会话数据腿粘合器。生命周期 = 两条腿的 WS 会话；进程内
// 表，无持久化（腿断即清，server 是会话事实源）。
type sessionRouter struct {
	verifier *rtv.Verifier
	relayID  func() string // 控制连接回填 rid 前为空（空 = 拒绝，与 host leg 一致）
	// originPatterns 客户端腿放行的页面 Origin（--allow-origin 同源配置；
	// 2026-09-14 修复：此前漏传，websocket.Accept 零值走同 Host 校验，
	// 浏览器（xnc.app 页面 → r*.xnc.app relay）永远 403，仅无 Origin 头的
	// CLI/agent 腿可用——web terminal 经 relay 从未通过过的根因）。
	originPatterns []string

	mu       sync.Mutex
	sessions map[string]*dataSession

	// closed 上报回调（控制连接注入；nil = 未连接期，终局直接丢——
	// server 的 Opening/idle 兜底会收）。
	closed func(proto.RelaySessionClosed)
}

type dataSession struct {
	sid, nodeID string
	agent, clnt *websocket.Conn
	glued       bool
	closeOnce   sync.Once
}

func newSessionRouter(verifier *rtv.Verifier, relayID func() string, originPatterns []string) *sessionRouter {
	return &sessionRouter{verifier: verifier, relayID: relayID, originPatterns: originPatterns, sessions: map[string]*dataSession{}}
}

// AgentLegHandler 挂 /api/agent/session（token 即凭证，sid 在票据内——
// 与主站同形：agent 引擎对 URL 形态无假设）。agent 腿无浏览器 Origin，
// AcceptOptions 仅保持与客户端腿同一构造（空 Origin 不受 patterns 约束）。
func (sr *sessionRouter) AgentLegHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tk, apiErr := sr.verifyLeg(r.URL.Query().Get("token"), "", "agent")
		if apiErr != nil {
			http.Error(w, apiErr.Message, apiErr.Status)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: sr.originPatterns})
		if err != nil {
			return
		}
		sr.attach("agent", tk, c)
	}
}

// ClientLegHandler 挂 /api/session/{sid}（路径 sid 必须与票据一致——
// 防票据挪用他sid）。浏览器腿带页面 Origin（xnc.app）而 Host 是 relay
// 域名（r*.xnc.app）——跨 host 必须经 OriginPatterns 放行（同
// --allow-origin 的媒体腿语义）。
func (sr *sessionRouter) ClientLegHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sid := r.PathValue("sid")
		tk, apiErr := sr.verifyLeg(r.URL.Query().Get("token"), sid, "client")
		if apiErr != nil {
			http.Error(w, apiErr.Message, apiErr.Status)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: sr.originPatterns})
		if err != nil {
			return
		}
		sr.attach("client", tk, c)
	}
}

// verifyLeg 校验 sdata 票据：typ/rid/sid（client 腿还查路径一致）/sub。
func (sr *sessionRouter) verifyLeg(token, pathSID, sub string) (rtv.Ticket, *proto.APIError) {
	rid := sr.relayID()
	if rid == "" {
		return rtv.Ticket{}, proto.Err(503, proto.CodeInternal, "relay not registered yet")
	}
	tk, err := sr.verifier.Verify(token)
	if err != nil {
		return rtv.Ticket{}, proto.Err(401, proto.CodeUnauthorized, "invalid session ticket")
	}
	if tk.Typ != rtv.TicketSessionData || tk.RelayID != rid || tk.SessionID == "" || tk.NodeID == "" {
		return rtv.Ticket{}, proto.Err(401, proto.CodeUnauthorized, "invalid session ticket")
	}
	if tk.Name != sub {
		return rtv.Ticket{}, proto.Err(401, proto.CodeUnauthorized, "ticket side mismatch")
	}
	if pathSID != "" && pathSID != tk.SessionID {
		return rtv.Ticket{}, proto.Err(401, proto.CodeUnauthorized, "session mismatch")
	}
	return tk, nil
}

// attach 单侧接入：粘合前登记/等待，双侧到齐即泵。
func (sr *sessionRouter) attach(side string, tk rtv.Ticket, c *websocket.Conn) {
	c.SetReadLimit(maxSessionFrameBytes)

	sr.mu.Lock()
	s, ok := sr.sessions[tk.SessionID]
	if !ok {
		s = &dataSession{sid: tk.SessionID, nodeID: tk.NodeID}
		sr.sessions[tk.SessionID] = s
		time.AfterFunc(attachWaitWindow, func() { sr.expireUnjoined(tk.SessionID) })
	}
	var peer **websocket.Conn
	if side == "agent" {
		peer = &s.agent
	} else {
		peer = &s.clnt
	}
	if *peer != nil { // 同侧二次接入（票据重放/重复拨）：拒绝并关新连接
		sr.mu.Unlock()
		slog.Warn("relay: session leg already attached", "sid", tk.SessionID, "side", side)
		c.Close(websocket.StatusPolicyViolation, "side already attached")
		return
	}
	*peer = c
	glue := s.agent != nil && s.clnt != nil && !s.glued
	if glue {
		s.glued = true
	}
	sr.mu.Unlock()

	if !glue {
		return // 等对侧；窗口由 AfterFunc 收
	}
	slog.Info("relay: session data legs glued", "sid", tk.SessionID, "node", tk.NodeID)
	sr.pump(s)
}

// expireUnjoined Opening 窗口到期仍未粘合：清掉只到一侧的会话。
func (sr *sessionRouter) expireUnjoined(sid string) {
	sr.mu.Lock()
	s, ok := sr.sessions[sid]
	if !ok || s.glued {
		sr.mu.Unlock()
		return
	}
	delete(sr.sessions, sid)
	agent, clnt := s.agent, s.clnt
	nodeID := s.nodeID
	sr.mu.Unlock()
	sr.reportClosed(sid, nodeID, "opening-timeout")
	if agent != nil {
		_ = agent.CloseNow()
	}
	if clnt != nil {
		_ = clnt.CloseNow()
	}
}

// pump 帧不透明双向泵（复刻主站 pump.go 语义）。任一方向退出即双侧终局
// + 表项清理 + RELAY_SESSION_CLOSED 上报（peer-disconnect 对齐主站泵）。
// 收线用 CloseNow（无握手——Close 的关闭握手会被不读帧的对端挂起，
// 卡住终局上报，真机测试踩过）。
func (sr *sessionRouter) pump(s *dataSession) {
	done := make(chan struct{})
	go func() {
		srw(s.agent, s.clnt)
		done <- struct{}{}
	}()
	go func() {
		srw(s.clnt, s.agent)
		done <- struct{}{}
	}()
	<-done // 首个方向退出（另一 goroutine 随关腿错误退出）
	s.closeOnce.Do(func() {
		sr.mu.Lock()
		if cur, ok := sr.sessions[s.sid]; ok && cur == s {
			delete(sr.sessions, s.sid)
		}
		sr.mu.Unlock()
		sr.reportClosed(s.sid, s.nodeID, "peer-disconnect")
		_ = s.agent.CloseNow()
		_ = s.clnt.CloseNow()
	})
}

// srw 流式转发 src→dst（text/binary 原样保序）。读不设 ctx 超时（长连
// 接 idle 合法，终局由对端/上层关腿触发）；写超时防对端僵死挂泵。
func srw(src, dst *websocket.Conn) {
	for {
		typ, r, err := src.Reader(context.Background())
		if err != nil {
			return
		}
		wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
		w, err := dst.Writer(wctx, typ)
		if err != nil {
			wcancel()
			return
		}
		if _, err := io.Copy(w, r); err != nil {
			wcancel()
			return
		}
		if err := w.Close(); err != nil {
			wcancel()
			return
		}
		wcancel()
	}
}

// Kill server 撤销（RELAY_SESSION_KILL）：关双腿（墓碑在 rtv.Server）。
// CloseNow（server 已知情，无需握手往返）。
func (sr *sessionRouter) Kill(sid, reason string) {
	sr.mu.Lock()
	s, ok := sr.sessions[sid]
	if ok {
		delete(sr.sessions, sid)
	}
	sr.mu.Unlock()
	if !ok {
		return
	}
	s.closeOnce.Do(func() {
		if s.agent != nil {
			_ = s.agent.CloseNow()
		}
		if s.clnt != nil {
			_ = s.clnt.CloseNow()
		}
	})
}

// ActiveSids 在服会话键集（STATS 上报 → server 代 Touch）。
func (sr *sessionRouter) ActiveSids() []string {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	out := make([]string, 0, len(sr.sessions))
	for sid, s := range sr.sessions {
		_ = s
		out = append(out, sid)
	}
	return out
}

// reportClosed 终局上报（回调未注入/控制连接断开 = 丢弃，server 侧
// Opening/idle 兜底收）。
func (sr *sessionRouter) reportClosed(sid, nodeID, reason string) {
	if sr.closed != nil {
		sr.closed(proto.RelaySessionClosed{SessionID: sid, NodeID: nodeID, Reason: reason})
	}
}
