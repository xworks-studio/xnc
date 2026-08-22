// Package session 实现统一会话生命周期：创建（双侧 token）、双侧 attach、
// Opening TTL、关闭与清理。WS 帧粘合在 pump.go。
package session

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"xnc/proto"
	"xnc/server/internal/registry"
)

const (
	openingTTL = 60 * time.Second
	// expiredRetention：Opening 超时的会话保留一段时间再从表中回收，
	// 让迟到的 attach 得到 410（SESSION_EXPIRED）而非 404。
	expiredRetention = 5 * time.Minute

	// janitorDefaultInterval janitor 默认扫表周期（测试经 setJanitorInterval 缩短）。
	janitorDefaultInterval = 30 * time.Second
)

type Manager struct {
	reg *registry.Registry
	log *slog.Logger

	mu       sync.Mutex
	sessions map[string]*session

	// —— shell 会话治理（Phase 3）。New 设默认值，router 构造后按 config 覆写；
	// 0 值 = 不限。会话创建时快照到 session（janitor 只读会话内副本）。
	ShellPerNode     int
	ShellIdleTimeout time.Duration
	ShellMaxLifetime time.Duration

	// —— desktop 会话治理（M1-Slice2）：每节点并发会话上限（默认 4，对齐
	// agent host pipe 的 max_subs=4——多 viewer 各自独立会话，T6 门 ③；
	// agent 引用计数语义见 T4 报告）+ idle 无信令活动关闭（pump 每帧刷新
	// lastActivity；5min 无任何方向的信令帧 = 视为无订阅者，janitor 关闭）。
	DesktopPerNode     int
	DesktopIdleTimeout time.Duration

	janitorInterval time.Duration // mu 保护；janitorLoop 每轮重读
	stopJanitor     chan struct{}
	kickJanitor     chan struct{} // interval 变更后立即重臂定时器
	stopOnce        sync.Once
}

// session 内部完整状态。ID/Kind/NodeID/UserID/Params 与只读视图 Session
// 字段同名同型（供 (*Session)(s) / (*session)(s) 双向转换）；连接与 token 私有。
type session struct {
	ID     string
	Kind   string
	NodeID uuid.UUID
	UserID uuid.UUID
	Params json.RawMessage

	agentToken  string
	clientToken string
	// 单用途 token 消耗后留存原值：区分重放（401）与该侧已无可 attach（404）。
	usedAgentToken  string
	usedClientToken string
	expiresAt       time.Time

	agentWS  *websocket.Conn
	clientWS *websocket.Conn
	glued    bool
	ttl      *time.Timer

	finish      func(reason string)
	notifyAgent func(sc proto.SessionClose) error
	closeOnce   sync.Once

	// —— shell 治理（Phase 3）：创建时快照 manager 配置，janitor 只读此处。
	startedAt    time.Time
	lastActivity atomic.Int64 // unixnano；pump 每帧与 touch 更新（免锁）
	idleTimeout  time.Duration
	maxLifetime  time.Duration
}

// Session 是对外只读视图（ID/Kind/NodeID/UserID/Params）。
type Session session

type CreateResult struct {
	Session     *Session
	AgentToken  string // 仅进 SESSION_OPEN，绝不进 REST 响应
	ClientToken string // REST 响应的 token
	ExpiresAt   time.Time
	ClientPath  string // "/api/session/<id>"
}

func New(reg *registry.Registry, log *slog.Logger) *Manager {
	m := &Manager{
		reg: reg, log: log, sessions: map[string]*session{},
		ShellPerNode: 10, ShellIdleTimeout: 30 * time.Minute, ShellMaxLifetime: 8 * time.Hour,
		DesktopPerNode: 4, DesktopIdleTimeout: 5 * time.Minute,
		janitorInterval: janitorDefaultInterval,
		stopJanitor:     make(chan struct{}),
		kickJanitor:     make(chan struct{}, 1),
	}
	go m.janitorLoop()
	return m
}

// Close 幂等停止 janitor（测试用；生产随进程退出）。
func (m *Manager) Close() {
	m.stopOnce.Do(func() { close(m.stopJanitor) })
}

func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (m *Manager) Create(nodeID, userID uuid.UUID, kind string, params json.RawMessage) (*CreateResult, *proto.APIError) {
	if !m.reg.Online(nodeID.String()) {
		return nil, proto.Err(409, proto.CodeNodeOffline, "node is offline")
	}
	s := &session{
		ID: uuid.NewString(), Kind: kind, NodeID: nodeID, UserID: userID,
		Params: params, agentToken: newToken(), clientToken: newToken(),
		expiresAt: time.Now().Add(openingTTL),
	}
	switch kind {
	case proto.KindShell:
		// 治理参数创建时快照：janitor 只读会话内副本，免与配置覆写竞争。
		now := time.Now()
		s.startedAt = now
		s.lastActivity.Store(now.UnixNano())
		s.idleTimeout = m.ShellIdleTimeout
		s.maxLifetime = m.ShellMaxLifetime
	case proto.KindDesktop:
		// desktop 只有 idle 治理（无寿命上限：长时间观看合法，输入 = Slice3）。
		now := time.Now()
		s.startedAt = now
		s.lastActivity.Store(now.UnixNano())
		s.idleTimeout = m.DesktopIdleTimeout
	}
	m.mu.Lock()
	if kind == proto.KindShell {
		// 限额判定与表插入同锁：并发抢最后一个名额时不超发。
		if m.ShellPerNode > 0 && countByNodeLocked(m.sessions, nodeID, proto.KindShell) >= m.ShellPerNode {
			m.mu.Unlock()
			return nil, proto.Err(409, proto.CodeSessionLimited, "shell session limit reached")
		}
	}
	if kind == proto.KindDesktop {
		// 每节点并发上限（默认 4，对齐 agent host max_subs）：desktop 采集
		// host 在节点侧按订阅名额引用计数，超出上限的会话只会被 agent 拒绝
		// attach；同样在锁内判定防超发。
		if m.DesktopPerNode > 0 && countByNodeLocked(m.sessions, nodeID, proto.KindDesktop) >= m.DesktopPerNode {
			m.mu.Unlock()
			return nil, proto.Err(409, proto.CodeSessionLimited, "desktop session limit reached")
		}
	}
	m.sessions[s.ID] = s
	m.mu.Unlock()
	s.ttl = time.AfterFunc(openingTTL, func() { m.expire(s) })
	return &CreateResult{
		Session: (*Session)(s), AgentToken: s.agentToken, ClientToken: s.clientToken,
		ExpiresAt: s.expiresAt, ClientPath: "/api/session/" + s.ID,
	}, nil
}

// expire Opening 超时终局：未粘合即关闭。expiresAt 压到过去让 attach 的
// 过期检查返回 410；表项保留 expiredRetention 后延迟回收。
func (m *Manager) expire(s *session) {
	m.mu.Lock()
	if s.glued {
		m.mu.Unlock()
		return
	}
	s.expiresAt = time.Now()
	m.mu.Unlock()
	m.close(s, "opening-timeout", false)
	time.AfterFunc(expiredRetention, func() {
		m.mu.Lock()
		delete(m.sessions, s.ID)
		m.mu.Unlock()
	})
}

// shortenOpeningTTL 仅供测试缩短 Opening TTL。
func (m *Manager) shortenOpeningTTL(s *Session, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	(*session)(s).ttl.Reset(d)
}

// sessionByAgentToken 以 agent token 反查会话 ID。agent 侧 WS 的 URL（SESSION_OPEN
// 的 WsURL）只携带 token——会话 ID 对 agent 透明；Opening 态会话数极少且 token
// 为 256 位随机值，线性扫描无碰撞与枚举面。
func (m *Manager) sessionByAgentToken(token string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		if s.agentToken != "" && token == s.agentToken {
			return id
		}
	}
	return ""
}

func (m *Manager) attach(side, sessionID, token string, ws *websocket.Conn) *proto.APIError {
	if sessionID == "" {
		// agent 侧端点（/api/agent/session?token=）无路径 ID：按 token 定位。
		sessionID = m.sessionByAgentToken(token)
		if sessionID == "" {
			return proto.Err(404, proto.CodeSessionNotFound, "session not found")
		}
	}
	m.mu.Lock()
	s, ok := m.sessions[sessionID]
	if !ok {
		m.mu.Unlock()
		return proto.Err(404, proto.CodeSessionNotFound, "session not found")
	}
	if time.Now().After(s.expiresAt) {
		m.mu.Unlock()
		return proto.Err(410, proto.CodeSessionExpired, "session expired")
	}
	var want, used *string
	switch side {
	case "agent":
		want, used = &s.agentToken, &s.usedAgentToken
	case "client":
		want, used = &s.clientToken, &s.usedClientToken
	default:
		m.mu.Unlock()
		return proto.Err(500, proto.CodeInternal, "unknown attach side")
	}
	if *want == "" {
		// 该侧已 attach（token 已消耗）：重放原 token → 401；
		// 其余 token 视同不存在可 attach 的会话侧 → 404。
		if token != "" && token == *used {
			m.mu.Unlock()
			return proto.Err(401, proto.CodeUnauthorized, "session token already used")
		}
		m.mu.Unlock()
		return proto.Err(404, proto.CodeSessionNotFound, "session side already attached")
	}
	if token != *want { // 常量时间比较对一次性随机 token 非必要（非常量 Secret 固定值）
		m.mu.Unlock()
		return proto.Err(401, proto.CodeUnauthorized, "invalid session token")
	}
	// 单用途：用后即焚（原值留存于 used 供重放识别）
	*used = *want
	*want = ""
	if side == "agent" {
		s.agentWS = ws
	} else {
		s.clientWS = ws
	}
	both := s.agentWS != nil && s.clientWS != nil
	if both && !s.glued {
		s.glued = true
		if s.ttl != nil {
			s.ttl.Stop()
		}
	}
	m.mu.Unlock()
	if both {
		m.pump(s) // T3 实现帧粘合
	}
	return nil
}

func (m *Manager) AttachAgent(sessionID, token string, ws *websocket.Conn) *proto.APIError {
	return m.attach("agent", sessionID, token, ws)
}

func (m *Manager) AttachClient(sessionID, token string, ws *websocket.Conn) *proto.APIError {
	return m.attach("client", sessionID, token, ws)
}

// NotifyClose 幂等关闭：关双侧连接、清表、回调 finish、通知 agent。
// api 层在控制连接发送 SESSION_CLOSE 失败时兜底清理。
func (m *Manager) NotifyClose(sessionID, reason string) {
	m.mu.Lock()
	s, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if ok {
		m.close(s, reason, true)
	}
}

// close 幂等终态收敛。remove=true（NotifyClose 路径）立即清表，后续 attach → 404；
// 过期路径 remove=false 保留表项换取 410 语义，由 expire 的延迟回收清理。
func (m *Manager) close(s *session, reason string, remove bool) {
	s.closeOnce.Do(func() {
		m.mu.Lock()
		if s.ttl != nil {
			s.ttl.Stop()
		}
		agentWS, clientWS := s.agentWS, s.clientWS
		finish, notify := s.finish, s.notifyAgent
		if remove {
			delete(m.sessions, s.ID)
		}
		m.mu.Unlock()
		if agentWS != nil {
			_ = agentWS.CloseNow()
		}
		if clientWS != nil {
			_ = clientWS.CloseNow()
		}
		if notify != nil {
			_ = notify(proto.SessionClose{SessionID: s.ID, Reason: reason})
		}
		if finish != nil {
			finish(reason)
		}
	})
}

func (m *Manager) SetFinishFn(s *Session, fn func(reason string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	(*session)(s).finish = fn
}

func (m *Manager) SetNotifyFn(s *Session, fn func(sc proto.SessionClose) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	(*session)(s).notifyAgent = fn
}

// —— shell 会话治理：janitor 与查询助手 ——

// setJanitorInterval 仅供测试缩短扫表周期（构造后调用即生效：kick 通道让
// janitor 立即按新周期重臂定时器，不必等旧周期走完）。
func (m *Manager) setJanitorInterval(d time.Duration) {
	m.mu.Lock()
	m.janitorInterval = d
	m.mu.Unlock()
	select {
	case m.kickJanitor <- struct{}{}:
	default: // 已有 pending kick
	}
}

// janitorLoop 周期扫表回收 idle/超寿 shell 会话。每轮从 mu 下重读周期再重臂
// 定时器（支持运行中调整）；stopJanitor 关闭即退出。
func (m *Manager) janitorLoop() {
	for {
		m.mu.Lock()
		d := m.janitorInterval
		m.mu.Unlock()
		if d <= 0 {
			d = janitorDefaultInterval
		}
		t := time.NewTimer(d)
		select {
		case <-m.stopJanitor:
			t.Stop()
			return
		case <-m.kickJanitor:
			t.Stop()
		case now := <-t.C:
			m.sweep(now)
		}
	}
}

// sweep 单轮回收：锁内判定到期（idle 优先级低于寿命），锁外 NotifyClose
// （幂等；避免持锁回调）。shell = max-lifetime + idle；desktop 仅 idle
// （无订阅活动的会话回收，长观看不受寿命约束）。
func (m *Manager) sweep(now time.Time) {
	type expiration struct {
		id     string
		reason string
	}
	var expire []expiration
	m.mu.Lock()
	for id, s := range m.sessions {
		switch s.Kind {
		case proto.KindShell:
			if s.maxLifetime > 0 && now.Sub(s.startedAt) > s.maxLifetime {
				expire = append(expire, expiration{id, "max-lifetime"})
				continue
			}
			if s.idleTimeout > 0 && now.Sub(time.Unix(0, s.lastActivity.Load())) > s.idleTimeout {
				expire = append(expire, expiration{id, "idle-timeout"})
			}
		case proto.KindDesktop:
			if s.idleTimeout > 0 && now.Sub(time.Unix(0, s.lastActivity.Load())) > s.idleTimeout {
				expire = append(expire, expiration{id, "idle-timeout"})
			}
		}
	}
	m.mu.Unlock()
	for _, e := range expire {
		m.NotifyClose(e.id, e.reason)
	}
}

func countByNodeLocked(sessions map[string]*session, nodeID uuid.UUID, kind string) int {
	n := 0
	for _, s := range sessions {
		if s.NodeID == nodeID && s.Kind == kind {
			n++
		}
	}
	return n
}

// CountByNode 返回某节点指定 kind 的活跃会话数（含 Opening 态）。
func (m *Manager) CountByNode(nodeID uuid.UUID, kind string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return countByNodeLocked(m.sessions, nodeID, kind)
}

// SessionsOf 返回某节点指定 kind 的会话只读视图快照。
func (m *Manager) SessionsOf(nodeID uuid.UUID, kind string) []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Session
	for _, s := range m.sessions {
		if s.NodeID == nodeID && s.Kind == kind {
			out = append(out, (*Session)(s))
		}
	}
	return out
}

// touch 更新会话活跃时间（测试/内部助手；pump 的每帧更新走同一 atomic 字段）。
func (m *Manager) touch(s *Session, at time.Time) {
	(*session)(s).lastActivity.Store(at.UnixNano())
}
