// Package rtvpool — RTV 外部中继池管理器（relay-plane spec §3.4/§4.3，
// server 侧）。
//
// 职责：relay 控制连接（注册-挑战-应答准入 + 心跳 + 统计上报 + 对账）、
// host 腿健康探测（30s QUIC 探测，连败 2 次摘除/1 次恢复——模式承接
// TURN 池历史文档）、node-sticky 分配（合格集内负载最低者，命中即沿用）、
// 会话撤销下发（RELAY_SESSION_KILL）。relay-0（内嵌）不入池。
//
// 准入（spec §0）：注册按 pubkey 幂等入库；allowlist 命中 → active，
// 未命中 → pending 待管理端审批（PATCH /api/admin/relays/{id}）。
// 挑战-应答只证明持钥——准入由清单/审批决定，防投毒。
//
// 状态归属（spec §2.2）：本管理器全部内存软状态（连接/画像/sticky 表），
// server 重启后由 relay 重连重建；PG 只持久化 relays 注册表。
package rtvpool

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"xnc/proto"
	"xnc/rtv"
	"xnc/server/internal/config"
	"xnc/server/internal/db"
	"xnc/server/internal/db/sqlc"
)

const (
	heartbeatEvery = 30 * time.Second // 心跳间隔（与 agent 控制连接同参）
	heartbeatWait  = 90 * time.Second // 读超时 = 判死窗口
	probeEvery     = 30 * time.Second // host 腿探测间隔
	probeTimeout   = 3 * time.Second
	driftLimit     = 5 * time.Minute // 时钟漂移停配阈值（spec §3.1）
)

// relayConn 一台在线 relay 的控制连接 + 内存画像。
type relayConn struct {
	id        string
	status    string // DB status 的内存镜像（管理端流转时同步）
	region    string
	maxSess   int
	maxMbps   int
	endpoints []proto.EndpointDesc // 注册时上报（候选列表来源）

	sendMu sync.Mutex
	send   func(proto.Message) error

	lastBeat time.Time
	stats    proto.RelayStats
	liveSess int // 对账上报的在服会话数（观测）

	probeFails int // 连败计数（≥2 摘除分配；成功清零）
	drift      time.Duration
}

// Assignment 一次分配的产物（desktopStart 消费）。Region 随中继注册
// 画像（观测信息，会话响应 candidates 会带）。
type Assignment struct {
	RelayID   string
	Region    string
	Endpoints []proto.EndpointDesc
}

// Manager 池管理器。
type Manager struct {
	st    *db.Store
	cfg   config.Config
	log   *slog.Logger
	allow map[string]bool
	// pubkey server 当前票据签名公钥（hex）——认证通过即下发给 relay。
	pubkey string
	// touch 活跃会话代触碰（stats 的 ActiveSids → session manager：外部
	// relay 的 viewer 触碰不出进程，粘合/idle 治理经此旁路）。
	touch func(sessionID string)
	// notifyClose 会话数据腿终局（Stage B：RELAY_SESSION_CLOSED → session
	// manager NotifyClose，等价主站泵的 peer-disconnect）。
	notifyClose func(sessionID, reason string)

	mu         sync.Mutex
	conns      map[string]*relayConn // relayID → conn
	sticky     map[string]string     // nodeID → relayID（媒体分配粘性）
	dataSticky map[string]string     // nodeID → relayID（会话数据面粘性，
	                                  // 仅含宣告 sdata 的 relay，独立于媒体）

	stop chan struct{}
	once sync.Once
}

// New 构造池管理器；signingPubkey 为 server 票据签名公钥（hex），经
// RELAY_CONFIG 下发给已认证 relay。
func New(st *db.Store, cfg config.Config, log *slog.Logger, signingPubkey string,
	touch func(sessionID string), notifyClose func(sessionID, reason string)) *Manager {
	allow := map[string]bool{}
	for _, k := range cfg.RTVRelayAllowlist {
		allow[normalizeHex(k)] = true
	}
	return &Manager{
		st: st, cfg: cfg, log: log, allow: allow, pubkey: signingPubkey,
		touch: touch, notifyClose: notifyClose,
		conns:      map[string]*relayConn{}, sticky: map[string]string{},
		dataSticky: map[string]string{},
		stop:       make(chan struct{}),
	}
}

// Start 启动健康探测循环（生产经 NewApp 生命周期；测试可不启）。
func (m *Manager) Start() { go m.probeLoop() }

// Stop 停后台循环并断开全部控制连接。
func (m *Manager) Stop() {
	m.once.Do(func() { close(m.stop) })
}

// ---------------- 控制连接（/api/relay/connect） ----------------

// Handler 返回 relay 控制连接的 HTTP 入口（公开路由——身份凭据是
// 注册公钥的挑战-应答，无 JWT）。
func (m *Manager) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		m.serveRelay(c)
	}
}

// serveRelay 一个 relay 控制连接的全生命周期：
//  1. RELAY_REGISTER（pubkey 幂等入库 + 准入判定）
//  2. CHALLENGE / RELAY_CHALLENGE_RESPONSE（证明私钥持有）
//  3. 服务循环（HEARTBEAT/STATS/RECONCILE；心跳/统计刷新判活）
func (m *Manager) serveRelay(c *websocket.Conn) {
	defer c.CloseNow()
	c.SetReadLimit(64 * 1024)

	// ① 注册（首帧，60s 内到达）。
	reg, pub, row, fresh, err := m.register(c)
	if err != nil {
		m.log.Warn("rtvpool: register rejected", "err", err)
		return
	}
	status := row.Status
	if fresh {
		// 准入两路：allowlist 命中即 active；否则 pending 待审批。
		if m.allow[normalizeHex(reg.PublicKey)] {
			status = "active"
		} else {
			status = "pending"
		}
		if status != row.Status {
			ctxS, cancelS := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = m.st.Q().SetRelayStatus(ctxS, sqlc.SetRelayStatusParams{ID: row.ID, Status: status})
			cancelS()
		}
	}
	m.log.Info("rtvpool: relay registered", "id", row.ID, "status", status, "fresh", fresh)

	// ② 挑战-应答（agentws 同构：对 nonce b64 串签名）。
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return
	}
	nonceStr := base64.RawStdEncoding.EncodeToString(nonce)
	send := func(msg proto.Message) error {
		b, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer wcancel()
		return c.Write(wctx, websocket.MessageText, b)
	}
	// RelayID 随挑战回传：首注册时 relay 尚不知被分配的 id。
	if send(proto.Message{Type: proto.TypeChallenge,
		Payload: mustJSON(proto.Challenge{Nonce: nonceStr, RelayID: row.ID})}) != nil {
		return
	}
	if !m.challengeOK(c, row.ID, pub, nonceStr) {
		m.log.Warn("rtvpool: challenge failed", "id", row.ID)
		return
	}

	// ③ 注册连接 + 服务循环（pending 状态也维持连接——管理端审批后
	// 无需重连即可参与分配）。
	conn := &relayConn{
		id: row.ID, status: status, region: reg.Region,
		maxSess: reg.MaxSessions, maxMbps: reg.MaxMbpsOut,
		endpoints: reg.Endpoints, lastBeat: time.Now(),
		send: send,
	}
	// 下发音票公钥：relay 离线验票的信任根（relay-plane spec §3.4 的
	// RELAY_CONFIG；P1 认证后立即一次，轮换推送留 P2）。
	if m.pubkey != "" {
		_ = send(proto.Message{Type: proto.TypeRelayConfig,
			Payload: mustJSON(proto.RelayConfig{SigningPubkeys: []string{m.pubkey}})})
	}
	m.attach(conn)
	defer m.detach(conn)
	m.log.Info("rtvpool: relay authenticated", "id", row.ID)

	for {
		rctx, rcancel := context.WithTimeout(context.Background(), heartbeatWait)
		typ, data, err := c.Read(rctx)
		rcancel()
		if err != nil {
			m.log.Info("rtvpool: relay control conn ended", "id", conn.id, "err", err)
			return
		}
		if typ != websocket.MessageText {
			return
		}
		var msg proto.Message
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Type {
		case proto.TypeRelayHeartbeat:
			var hb proto.RelayHeartbeat
			if msg.Decode(&hb) == nil {
				conn.sendMu.Lock()
				conn.drift = time.Since(time.Unix(hb.ClockUnix, 0))
				conn.lastBeat = time.Now()
				conn.sendMu.Unlock()
				if d := conn.driftSnapshot(); d > driftLimit || d < -driftLimit {
					m.log.Warn("rtvpool: relay clock drift exceeds limit, skipping assignment",
						"id", conn.id, "drift", d.String())
				}
			}
			m.touchDB(conn.id)
			_ = send(proto.Message{Type: proto.TypeRelayHeartbeatAck})
		case proto.TypeRelayStats:
			var st proto.RelayStats
			if msg.Decode(&st) == nil {
				conn.sendMu.Lock()
				conn.stats = st
				conn.lastBeat = time.Now()
				conn.sendMu.Unlock()
				// 代触碰活跃会话（粘合 + idle 治理；10s 粒度 << 60s
				// opening / 5m idle，时序安全）。
				if m.touch != nil {
					for _, sid := range st.ActiveSids {
						m.touch(sid)
					}
				}
			}
		case proto.TypeRelayReconcile:
			var rc proto.RelayReconcile
			if msg.Decode(&rc) == nil {
				conn.sendMu.Lock()
				conn.liveSess = len(rc.Live)
				conn.lastBeat = time.Now()
				conn.sendMu.Unlock()
			}
		case proto.TypeRelaySessionClosed:
			// 会话数据腿终局（Stage B）：relay 泵的 peer-disconnect 等价
			// 信号 → session manager NotifyClose（幂等；rid 校验由票据/
			// conn 归属天然保证——只有持有该会话的 relay 会报）。
			var cl proto.RelaySessionClosed
			if msg.Decode(&cl) == nil && cl.SessionID != "" {
				conn.sendMu.Lock()
				conn.lastBeat = time.Now()
				conn.sendMu.Unlock()
				if m.notifyClose != nil {
					m.notifyClose(cl.SessionID, orDefault(cl.Reason, "peer-disconnect"))
				}
			}
		default:
			m.log.Debug("rtvpool: unknown relay msg", "type", msg.Type)
		}
	}
}

// orDefault 空串回退（闭环 reason 透传，空 = 对齐主站泵语义）。
func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// register 处理首帧 RELAY_REGISTER：校验 pubkey、幂等入库（新 relay 分配
// rl-<8hex>），返回（注册载荷、公钥、DB 行、是否首次）。
func (m *Manager) register(c *websocket.Conn) (proto.RelayRegister, ed25519.PublicKey, sqlc.Relay, bool, error) {
	var zero proto.RelayRegister
	ctxReg, cancelReg := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelReg()
	_, data, err := c.Read(ctxReg)
	if err != nil {
		return zero, nil, sqlc.Relay{}, false, err
	}
	var msg proto.Message
	if json.Unmarshal(data, &msg) != nil || msg.Type != proto.TypeRelayRegister {
		return zero, nil, sqlc.Relay{}, false, fmt.Errorf("first frame must be RELAY_REGISTER")
	}
	var reg proto.RelayRegister
	if msg.Decode(&reg) != nil {
		return zero, nil, sqlc.Relay{}, false, fmt.Errorf("bad register payload")
	}
	pub, err := decodePubkey(reg.PublicKey)
	if err != nil {
		return zero, nil, sqlc.Relay{}, false, err
	}

	ctxDB, cancelDB := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDB()
	pubHex := normalizeHex(reg.PublicKey)
	// 已注册（按 id 或 pubkey）→ 画像刷新、保持既有 status。
	if reg.RelayID != "" {
		if row, err := m.st.Q().GetRelay(ctxDB, reg.RelayID); err == nil {
			if row.Pubkey != pubHex {
				return zero, nil, sqlc.Relay{}, false, fmt.Errorf("relay id bound to another pubkey")
			}
			row, err := m.upsertRow(ctxDB, row.ID, reg)
			return reg, pub, row, false, err
		}
	}
	if row, err := m.st.Q().GetRelayByPubkey(ctxDB, pubHex); err == nil {
		row, err := m.upsertRow(ctxDB, row.ID, reg)
		return reg, pub, row, false, err
	}
	id, err := newRelayID()
	if err != nil {
		return zero, nil, sqlc.Relay{}, false, err
	}
	row, err := m.upsertRow(ctxDB, id, reg)
	return reg, pub, row, true, err
}

func (m *Manager) upsertRow(ctx context.Context, id string, reg proto.RelayRegister) (sqlc.Relay, error) {
	eps, _ := json.Marshal(reg.Endpoints)
	return m.st.Q().UpsertRelay(ctx, sqlc.UpsertRelayParams{
		ID: id, Pubkey: normalizeHex(reg.PublicKey), Region: reg.Region,
		Endpoints: eps, MaxSessions: int32(reg.MaxSessions), MaxMbpsOut: int32(reg.MaxMbpsOut),
		Version: reg.Version, Status: "pending",
	})
}

func (m *Manager) challengeOK(c *websocket.Conn, relayID string, pub ed25519.PublicKey, nonceStr string) bool {
	ctxCh, cancelCh := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelCh()
	_, data, err := c.Read(ctxCh)
	if err != nil {
		return false
	}
	var msg proto.Message
	var cr proto.RelayChallengeResponse
	if json.Unmarshal(data, &msg) != nil || msg.Type != proto.TypeRelayChallengeResponse ||
		msg.Decode(&cr) != nil || cr.RelayID != relayID {
		return false
	}
	return ed25519.Verify(pub, []byte(nonceStr), cr.Signature)
}

// attach/detach 连接表维护（同 relay 重连 = 顶替旧连接）。
func (m *Manager) attach(c *relayConn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old := m.conns[c.id]; old != nil {
		_ = old.sendRaw(proto.Message{Type: proto.TypeError,
			Payload: mustJSON(proto.ErrorPayload{Code: "REPLACED", Message: "new connection for this relay"})})
	}
	m.conns[c.id] = c
}

func (m *Manager) detach(c *relayConn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur := m.conns[c.id]; cur == c {
		delete(m.conns, c.id)
	}
}

func (c *relayConn) sendRaw(msg proto.Message) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.send(msg)
}

func (c *relayConn) driftSnapshot() time.Duration {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.drift
}

// RelayStatusChanged 管理端状态流转回调（api.relayPool 接口）。
func (m *Manager) RelayStatusChanged(id, status string) {
	m.mu.Lock()
	conn := m.conns[id]
	if status == "retired" {
		for node, rid := range m.sticky {
			if rid == id {
				delete(m.sticky, node)
			}
		}
	}
	m.mu.Unlock()
	if conn != nil {
		conn.sendMu.Lock()
		conn.status = status
		conn.sendMu.Unlock()
		if status == "retired" {
			_ = conn.sendRaw(proto.Message{Type: proto.TypeError,
				Payload: mustJSON(proto.ErrorPayload{Code: "RETIRED", Message: "relay retired by admin"})})
		}
	}
}

// SessionKill 会话撤销下发（desktopStart finish 钩子 → 归属 relay）。
func (m *Manager) SessionKill(relayID, node, sessionID, reason string) {
	m.mu.Lock()
	conn := m.conns[relayID]
	m.mu.Unlock()
	if conn == nil {
		return
	}
	_ = conn.sendRaw(proto.Message{Type: proto.TypeRelaySessionKill,
		Payload: mustJSON(proto.RelaySessionKill{SessionID: sessionID, NodeID: node, Reason: reason})})
}

// SessionGrant 被动首约下发（server → relay，2026-09-11 Stage A）：relay
// 在本地 Arbiter 执行 Grant（空闲时授予首约——外部会话与内嵌 relay-0 的
// "首个 viewer 免显式 takeControl" UX 对齐）。尽力而为：下发失败 viewer
// 仍可显式 takeControl。
func (m *Manager) SessionGrant(relayID, node, sessionID, holder string) {
	m.mu.Lock()
	conn := m.conns[relayID]
	m.mu.Unlock()
	if conn == nil {
		return
	}
	_ = conn.sendRaw(proto.Message{Type: proto.TypeRelaySessionGrant,
		Payload: mustJSON(proto.RelaySessionGrant{SessionID: sessionID, NodeID: node, Holder: holder})})
}

// ---------------- 分配（node-sticky + 负载打分） ----------------

// Assign 为节点选择中继：sticky 命中且仍合格 → 沿用（host 张票字节等值、
// core 不换血的前提）；否则在合格集内挑负载最低者。空 = 池无合格者
// （调用方回落 relay-0）。
func (m *Manager) Assign(nodeID string) (Assignment, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	eligible := make([]*relayConn, 0, len(m.conns))
	for _, c := range m.conns {
		if c.eligibleLocked(now) {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		return Assignment{}, false
	}
	if rid, ok := m.sticky[nodeID]; ok {
		for _, c := range eligible {
			if c.id == rid {
				return c.assignmentLocked(), true
			}
		}
	}
	best := eligible[0]
	bestLoad := best.loadNormLocked()
	for _, c := range eligible[1:] {
		if l := c.loadNormLocked(); l < bestLoad {
			best, bestLoad = c, l
		}
	}
	m.sticky[nodeID] = best.id
	return best.assignmentLocked(), true
}

// AssignData 为节点的会话数据面（exec/shell/file/tunnel，Stage B）选
// relay：仅考虑宣告了 sdata 端点的合格 relay；粘性独立于媒体分配（媒体
// 可用任意 relay，数据面必须有域名端点——两类可用集不同，混用会把 exec
// 锁死在无 sdata 的 sticky 上）。空 = 无可用（主站旧路径）。
func (m *Manager) AssignData(nodeID string) (Assignment, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	eligible := make([]*relayConn, 0, len(m.conns))
	for _, c := range m.conns {
		if c.eligibleLocked(now) && hasSdata(c.endpoints) {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		return Assignment{}, false
	}
	if rid, ok := m.dataSticky[nodeID]; ok {
		for _, c := range eligible {
			if c.id == rid {
				return c.assignmentLocked(), true
			}
		}
	}
	best := eligible[0]
	bestLoad := best.loadNormLocked()
	for _, c := range eligible[1:] {
		if l := c.loadNormLocked(); l < bestLoad {
			best, bestLoad = c, l
		}
	}
	m.dataSticky[nodeID] = best.id
	return best.assignmentLocked(), true
}

// hasSdata 端点表含可用的会话数据端点。
func hasSdata(eps []proto.EndpointDesc) bool {
	for _, e := range eps {
		if e.Transport == "sdata" && e.Host != "" && e.Port > 0 {
			return true
		}
	}
	return false
}

func (c *relayConn) assignmentLocked() Assignment {
	eps := make([]proto.EndpointDesc, len(c.endpoints))
	copy(eps, c.endpoints)
	c.sendMu.Lock()
	region := c.region
	c.sendMu.Unlock()
	return Assignment{RelayID: c.id, Region: region, Endpoints: eps}
}

// eligibleLocked 合格 = active + 判活窗口内有心跳 + 探测连败 <2 + 时钟
// 漂移在阈内。调用方持 Manager.mu（画像字段另有 sendMu，读近似即可——
// 状态翻转的瞬时不一致只影响一次分配选择，无害）。
func (c *relayConn) eligibleLocked(now time.Time) bool {
	if c.status != "active" || c.probeFails >= 2 {
		return false
	}
	if c.drift > driftLimit || c.drift < -driftLimit {
		return false
	}
	return now.Sub(c.lastBeat) < heartbeatWait
}

// loadNormLocked 负载归一化：容量声明缺省（0）= 不参与容量维度（0 负载）。
// 打分口径 = sessions/maxSessions（spec §6：mbpsOut 为二级维度，stats
// 上报稳定后再启用）。
func (c *relayConn) loadNormLocked() float64 {
	if c.maxSess > 0 {
		return float64(c.stats.Sessions) / float64(c.maxSess)
	}
	return 0
}

// ---------------- 健康探测 ----------------

func (m *Manager) probeLoop() {
	t := time.NewTicker(probeEvery)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.probeOnce()
		}
	}
}

func (m *Manager) probeOnce() {
	type target struct {
		id   string
		addr string
	}
	m.mu.Lock()
	targets := make([]target, 0, len(m.conns))
	for _, c := range m.conns {
		targets = append(targets, target{c.id, rtv.HostLegEndpoint(protoView(c.endpoints))})
	}
	m.mu.Unlock()
	for _, t := range targets {
		if t.addr == "" {
			continue
		}
		ok := rtv.ProbeHostLeg(t.addr, probeTimeout)
		m.mu.Lock()
		if c := m.conns[t.id]; c != nil {
			c.sendMu.Lock()
			if ok {
				if c.probeFails >= 2 {
					m.log.Info("rtvpool: relay host leg recovered", "id", t.id)
				}
				c.probeFails = 0
			} else {
				c.probeFails++
				if c.probeFails >= 2 {
					m.log.Warn("rtvpool: relay host leg unreachable, skipping assignment",
						"id", t.id, "addr", t.addr)
				}
			}
			c.sendMu.Unlock()
		}
		m.mu.Unlock()
	}
}

func (m *Manager) touchDB(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = m.st.Q().TouchRelay(ctx, id)
}

// Snapshot 观测面（/api/rtv/stats 的 relays 段）。
func (m *Manager) Snapshot() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []map[string]any{}
	for _, c := range m.conns {
		c.sendMu.Lock()
		out = append(out, map[string]any{
			"id": c.id, "region": c.region, "status": c.status, "online": true,
			"probeFails":   c.probeFails,
			"clockDriftMs": c.drift.Milliseconds(),
			"stats":        c.stats,
			"liveSessions": c.liveSess,
			"lastBeat":     c.lastBeat.Format(time.RFC3339),
		})
		c.sendMu.Unlock()
	}
	return out
}

// ---------------- 杂项 ----------------

func decodePubkey(hexStr string) (ed25519.PublicKey, error) {
	b, err := hex.DecodeString(normalizeHex(hexStr))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("bad ed25519 pubkey")
	}
	return ed25519.PublicKey(b), nil
}

func normalizeHex(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= '0' && ch <= '9', ch >= 'a' && ch <= 'f':
			out = append(out, ch)
		case ch >= 'A' && ch <= 'F':
			out = append(out, ch-'A'+'a')
		}
	}
	return string(out)
}

func newRelayID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("rl-%x", b[:]), nil
}

func protoView(eps []proto.EndpointDesc) []rtv.EndpointDescView {
	out := make([]rtv.EndpointDescView, 0, len(eps))
	for _, e := range eps {
		out = append(out, rtv.EndpointDescView{Transport: e.Transport, Host: e.Host, Port: e.Port})
	}
	return out
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
