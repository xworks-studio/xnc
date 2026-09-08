// Package rtv — 桌面流中继（RTV，2026-09-08 重构；relay-plane 2026-09-08
// 抽包为共享模块）：host 注册、viewer 加入、媒体字节扇出、控制最小路由。
// 移植自 MVP-RTV server/relay.go（踩坑结晶的 viewer 迁移 / hello 重试门闩 /
// hostOffline 语义原样保留），relay-plane 集成缝：
//
//   - Hub 键 = nodeId（host 按节点注册，多 XNC 会话共享同一 host 流）；
//   - host/viewer 准入 = RelayTicket 离线验签（PlaneHost.VerifyToken，
//     spec §3.1：host 张按 (node,relay) 等值可重放，viewer 张带能力 claims）；
//   - 控制权仲裁 = 本地 Arbiter（spec §3.5：last-take-wins + 3s 冷却 +
//     60s 租约 TTL，准入判据 = ticket cap，策略留在签发侧）；
//   - 撤销 = 墓碑 + 断连（Server.KillSession，spec §3.4）。
//
// 设计原则（协议规范 §0/§3）：对媒体包按字节转发（不解析媒体语义，仅读
// 包头 captureUnixUs 用于延迟统计），控制消息做最小路由。
package rtv

import (
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Viewer 抽象 WebTransport 与 WebSocket 两条浏览器腿。
type Viewer interface {
	ID() uint64
	Kind() string // "wt" | "ws"
	// Node/Session 是连接期验签得到的绑定（hub 操作与 input 门控用）。
	Node() string
	Session() string
	// CanControl/CanInput/DisplayName 来自 viewer 票据 claims（cap/nm）——
	// 仲裁与门控的准入判据，relay 不做任何角色计算。
	CanControl() bool
	CanInput() bool
	DisplayName() string
	SendDatagram(b []byte) error
	SendControlJSON(v json.RawMessage) error
	Close()
}

// HostSession 一个已注册的采集端连接。
type HostSession struct {
	NodeID      string
	ConnectedAt time.Time
	hub         *Hub // RegisterHost 注入（input 门控/仲裁用）

	ctrlMu sync.Mutex
	// host 控制流（首个 bidi stream），由 legquic 持有注入
	sendControl func(json.RawMessage) error
	closeConn   func()

	mu        sync.Mutex
	viewers   map[uint64]Viewer
	lastConfg json.RawMessage // 最近一次 config（新 viewer 加入时补发）

	// 统计（原子）
	rxPkgs, rxBytes   atomic.Uint64
	txPkgs, txBytes   atomic.Uint64
	ctrlRx, ctrlTx    atomic.Uint64
	latEMAUs          atomic.Int64 // host→relay 腿延迟（含时钟偏差，观测用）
	framesSeen        atomic.Uint64
	lastCaptureUnixUs atomic.Int64
}

func (h *HostSession) viewerCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.viewers)
}

func (h *HostSession) broadcastControl(v json.RawMessage) {
	h.mu.Lock()
	viewers := make([]Viewer, 0, len(h.viewers))
	for _, w := range h.viewers {
		viewers = append(viewers, w)
	}
	h.mu.Unlock()
	for _, w := range viewers {
		if err := w.SendControlJSON(v); err != nil {
			slog.Warn("viewer control send failed", "viewer", w.ID(), "err", err)
		} else {
			h.ctrlTx.Add(1)
		}
	}
}

// fanoutMedia 媒体包扇出（不修改内容；解析头部仅为延迟统计）。
func (h *HostSession) fanoutMedia(data []byte) {
	h.rxPkgs.Add(1)
	h.rxBytes.Add(uint64(len(data)))
	if len(data) >= 32 {
		capUs := int64(binary.LittleEndian.Uint64(data[24:32]))
		if capUs > 0 {
			h.lastCaptureUnixUs.Store(capUs)
			nowUs := time.Now().UnixNano() / 1000
			d := nowUs - capUs
			if d > 0 && d < 60_000_000 { // >60ms 视为时钟偏差，丢弃样本
				old := h.latEMAUs.Load()
				if old == 0 {
					h.latEMAUs.Store(d)
				} else {
					h.latEMAUs.Store(old + (d-old)/8)
				}
			}
		}
	}
	h.mu.Lock()
	viewers := make([]Viewer, 0, len(h.viewers))
	for _, w := range h.viewers {
		viewers = append(viewers, w)
	}
	h.mu.Unlock()
	if len(viewers) == 0 {
		return
	}
	h.framesSeen.Add(1) // 至少有一个观察者时才计帧
	for _, w := range viewers {
		if err := w.SendDatagram(data); err == nil {
			h.txPkgs.Add(1)
			h.txBytes.Add(uint64(len(data)))
		} else {
			slog.Debug("viewer datagram send failed", "viewer", w.ID(), "err", err)
		}
	}
}

// forwardToHost viewer→host 控制消息转发（input 消息按仲裁机门控：持有
// 活约且 cap.input）。控制权流转消息（takeControl/releaseControl）在
// relay 终结（Arbiter 仲裁），不转发给 host。
func (h *HostSession) forwardToHost(v json.RawMessage, from Viewer) {
	var probe struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(v, &probe)
	switch probe.Type {
	case "input":
		if h.hub == nil || !h.hub.gateInputFor(from) {
			return // 无输入权：静默丢弃（不回错——旧栈同语义，view-only）
		}
	case "takeControl":
		if h.hub != nil {
			h.hub.handleTakeControl(from)
		}
		return
	case "releaseControl":
		if h.hub != nil {
			h.hub.handleReleaseControl(from.Node(), from)
		}
		return
	}
	h.ctrlMu.Lock()
	defer h.ctrlMu.Unlock()
	if h.sendControl == nil {
		return
	}
	if err := h.sendControl(v); err != nil {
		slog.Warn("host control forward failed", "err", err)
	}
}

func (h *HostSession) notifyViewers() {
	n := h.viewerCount()
	h.forwardToHostNoGate(mustJSON(map[string]any{"type": "viewers", "count": n}))
}

// forwardToHostNoGate 服务端自产消息（viewers/frameLoss 合成）直发。
func (h *HostSession) forwardToHostNoGate(v json.RawMessage) {
	h.ctrlMu.Lock()
	defer h.ctrlMu.Unlock()
	if h.sendControl == nil {
		return
	}
	if err := h.sendControl(v); err != nil {
		slog.Warn("host control forward failed", "err", err)
	}
}

func (h *HostSession) cacheConfig(v json.RawMessage) {
	h.mu.Lock()
	h.lastConfg = v
	h.mu.Unlock()
}

func (h *HostSession) cachedConfig() json.RawMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastConfg
}

// Hub 全局会话表（node 键）。控制权仲裁在本地 Arbiter（spec §3.5），
// 准入/活跃/事件上行经 events 回调（宿主注入：server = 日志/审计，
// relay 独立进程 = 控制连接转发）。
type Hub struct {
	mu      sync.Mutex
	hosts   map[string]*HostSession
	started time.Time
	nextID  atomic.Uint64

	// arbiter 传输级控制权仲裁机（票据 cap 为准入判据）。
	arbiter *Arbiter

	// events 生命周期事件上行（nil = 无观测）。
	events func(typ, node, session string)
}

func NewHub() *Hub {
	return &Hub{hosts: map[string]*HostSession{}, started: time.Now(), arbiter: NewArbiter()}
}

// Arbiter 暴露仲裁机（desktopStart 被动首约 / 测试直驱）。
func (g *Hub) Arbiter() *Arbiter { return g.arbiter }

func (g *Hub) emit(typ, node, session string) {
	if g.events != nil {
		g.events(typ, node, session)
	}
}

// gateInputFor input 门控：持有活约（TTL 内）且票据 cap.input。
func (g *Hub) gateInputFor(v Viewer) bool {
	holder, _ := g.arbiter.Holder(v.Node())
	return holder != "" && holder == v.Session() && v.CanInput()
}

// controlStateMsg 当前控制权归属消息（广播/单发共用）。
func (g *Hub) controlStateMsg(node string) json.RawMessage {
	holder, name := g.arbiter.Holder(node)
	return mustJSON(map[string]any{
		"type": "controlState", "holderSession": holder, "holderName": name,
	})
}

// broadcastControlState 向该节点当前全部 viewer 广播控制权归属。
func (g *Hub) broadcastControlState(node string) {
	msg := g.controlStateMsg(node)
	if h := g.Host(node); h != nil {
		h.broadcastControl(msg)
	}
}

// handleTakeControl viewer 显式接管：成功 → 广播 controlState（含接管者
// 自己，UI 据此切换）；失败（冷却/无能力）→ 仅回执发起者 + 同步一次状态。
func (g *Hub) handleTakeControl(from Viewer) {
	ok, reason := g.arbiter.Take(from.Node(), from.Session(), from.DisplayName(), from.CanControl())
	if !ok {
		slog.Info("control take denied", "node", from.Node(), "viewer", from.ID(), "reason", reason)
		_ = from.SendControlJSON(mustJSON(map[string]any{
			"type": "controlResult", "ok": false, "reason": reason,
		}))
		_ = from.SendControlJSON(g.controlStateMsg(from.Node()))
		return
	}
	slog.Info("control taken", "node", from.Node(), "viewer", from.ID(), "session", from.Session(), "reason", reason)
	g.broadcastControlState(from.Node())
}

// handleReleaseControl viewer 显式释放：广播（非持有者释放 = 无操作，
// 广播无害且让 UI 对齐）。
func (g *Hub) handleReleaseControl(node string, from Viewer) {
	g.arbiter.Release(node, from.Session())
	slog.Info("control released", "node", node, "viewer", from.ID(), "session", from.Session())
	g.broadcastControlState(node)
}

func (g *Hub) NextViewerID() uint64 { return g.nextID.Add(1) }

// RegisterHost 注册/替换 host 连接（同节点旧连接被关闭——重启场景）。
// 替换时把旧会话的 viewer 迁移到新会话：否则快速重启会静默丢弃所有
// viewer（页面收不到任何通知，config 仍在，画面永久卡死需手动刷新）。
func (g *Hub) RegisterHost(nodeID string, h *HostSession) (replaced bool) {
	h.hub = g
	g.mu.Lock()
	var migrated []Viewer
	if old, ok := g.hosts[nodeID]; ok {
		replaced = true
		old.mu.Lock()
		for _, w := range old.viewers {
			migrated = append(migrated, w)
		}
		old.viewers = map[uint64]Viewer{}
		old.mu.Unlock()
		old.closeConn()
	}
	g.hosts[nodeID] = h
	g.mu.Unlock()
	if len(migrated) > 0 {
		h.mu.Lock()
		for _, w := range migrated {
			h.viewers[w.ID()] = w
		}
		h.mu.Unlock()
		// 迁移 viewer 的解码状态对应旧流：合成 frameLoss 请求 IDR；
		// 同时让新 host 感知 viewer 数（host 侧 viewer 增加会强制产帧）
		h.notifyViewers()
		h.forwardToHostNoGate(mustJSON(map[string]any{"type": "frameLoss", "frameIndex": 0, "reason": "host-restart"}))
		slog.Info("viewers migrated to new host", "node", nodeID, "count", len(migrated))
	}
	slog.Info("host registered", "node", nodeID, "replaced", replaced)
	g.emit("host.register", nodeID, "")
	return replaced
}

func (g *Hub) UnregisterHost(nodeID string, h *HostSession) {
	g.mu.Lock()
	cur, ok := g.hosts[nodeID]
	if !ok || cur != h {
		// 已被新连接替换（RegisterHost 迁移过 viewer），无事可做
		g.mu.Unlock()
		return
	}
	delete(g.hosts, nodeID)
	h.mu.Lock()
	orphans := make([]Viewer, 0, len(h.viewers))
	for _, w := range h.viewers {
		orphans = append(orphans, w)
	}
	h.mu.Unlock()
	g.mu.Unlock()
	// host 真正消失：通知 viewer，页面据此重发 hello 等待 host 回来
	for _, w := range orphans {
		_ = w.SendControlJSON(mustJSON(map[string]any{"type": "hostOffline", "node": nodeID}))
	}
	slog.Info("host unregistered", "node", nodeID, "viewersNotified", len(orphans))
	g.emit("host.unregister", nodeID, "")
}

func (g *Hub) Host(nodeID string) *HostSession {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hosts[nodeID]
}

// IsViewerBound viewer 是否已挂在当前节点的 host 会话上（host 替换/
// 掉线后为 false，viewer 腿据此允许 hello 重试重新绑定）。
func (g *Hub) IsViewerBound(nodeID string, w Viewer) bool {
	h := g.Host(nodeID)
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.viewers[w.ID()]
	return ok
}

// AddViewer 注册 viewer；返回给 viewer 的首条提示消息。
func (g *Hub) AddViewer(nodeID string, w Viewer) {
	h := g.Host(nodeID)
	if h == nil {
		_ = w.SendControlJSON(mustJSON(map[string]any{"type": "hostOffline", "node": nodeID}))
		return
	}
	h.mu.Lock()
	h.viewers[w.ID()] = w
	h.mu.Unlock()
	// 补发缓存的 config（晚加入的 viewer 立即知道编码参数）
	if cfg := h.cachedConfig(); cfg != nil {
		_ = w.SendControlJSON(cfg)
	}
	// 单发当前控制权归属（晚加入者立即知道自己是否 view-only、谁在控）
	_ = w.SendControlJSON(g.controlStateMsg(nodeID))
	// 通知 host 新 viewer 数 + 合成一次 IDR 请求（rustdesk 新订阅者触发关键帧的对应物）
	h.notifyViewers()
	h.forwardToHostNoGate(mustJSON(map[string]any{"type": "frameLoss", "frameIndex": 0, "reason": "new-viewer"}))
	slog.Info("viewer joined", "node", nodeID, "viewer", w.ID(), "kind", w.Kind(), "total", h.viewerCount())
	g.emit("viewer.joined", nodeID, w.Session())
}

func (g *Hub) RemoveViewer(nodeID string, w Viewer) {
	h := g.Host(nodeID)
	if h == nil {
		return
	}
	h.mu.Lock()
	delete(h.viewers, w.ID())
	h.mu.Unlock()
	// 持有控制权的 viewer 离场即撤约（别让死会话占着等 TTL）
	g.arbiter.DropSession(nodeID, w.Session())
	h.notifyViewers()
	slog.Info("viewer left", "node", nodeID, "viewer", w.ID(), "total", h.viewerCount())
	g.emit("viewer.left", nodeID, w.Session())
}

// dropForKill 撤销断连：session 为空 = 节点级（host + 全部 viewer）；
// 否则仅踢该会话的 viewer（host 张按 node 铸造，会话级撤销不动 host）。
func (g *Hub) dropForKill(node, session string) {
	h := g.Host(node)
	if h == nil {
		return
	}
	if session == "" {
		g.UnregisterHost(node, h) // 向 viewer 广播 hostOffline
		h.closeConn()
		return
	}
	h.mu.Lock()
	var doomed []Viewer
	for id, w := range h.viewers {
		if w.Session() == session {
			doomed = append(doomed, w)
			delete(h.viewers, id)
		}
	}
	h.mu.Unlock()
	for _, w := range doomed {
		w.Close()
	}
	if len(doomed) > 0 {
		g.arbiter.DropSession(node, session)
		h.notifyViewers()
	}
}

// ActiveSessions 在服 viewer 的会话键 → viewer 数（控制连接 stats 上报
// 用：server 侧据此 Touch 活跃会话——外部 relay 的 viewer 触碰不出
// 进程，粘合与 idle 治理依赖此旁路）。
func (g *Hub) ActiveSessions() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]int{}
	for _, h := range g.hosts {
		h.mu.Lock()
		for _, w := range h.viewers {
			out[w.Session()]++
		}
		h.mu.Unlock()
	}
	return out
}

// Snapshot statsz 输出（管理端挂载）。
func (g *Hub) Snapshot() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	hosts := []map[string]any{}
	for id, h := range g.hosts {
		hosts = append(hosts, map[string]any{
			"node":           id,
			"connectedSince": h.ConnectedAt.Format(time.RFC3339),
			"viewers":        h.viewerCount(),
			"rxPkgs":         h.rxPkgs.Load(),
			"rxBytes":        h.rxBytes.Load(),
			"txPkgs":         h.txPkgs.Load(),
			"txBytes":        h.txBytes.Load(),
			"framesSeen":     h.framesSeen.Load(),
			"hostLegLatencyMs": func() float64 {
				v := h.latEMAUs.Load()
				if v == 0 {
					return -1
				}
				return float64(v) / 1000.0
			}(),
		})
	}
	return map[string]any{
		"uptimeSec": int(time.Since(g.started).Seconds()),
		"hosts":     hosts,
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
