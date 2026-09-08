// Package rtv — 桌面流中继（RTV，2026-09-08 重构）：host 注册、viewer 加入、
// 媒体字节扇出、控制最小路由。移植自 MVP-RTV server/relay.go（踩坑结晶的
// viewer 迁移 / hello 重试门闩 / hostOffline 语义原样保留），XNC 集成缝：
//
//   - Hub 键 = nodeId（host 按节点注册，多 XNC 会话共享同一 host 流）；
//   - host 注册必须持有效 HostToken（会话创建时经 SESSION_OPEN→agent→
//     core→stdin 下发；重连/崩溃重启重放同一 token = 合法再注册）；
//   - viewer 腿在连接期经回调鉴权（会话 token → node/session 绑定）；
//   - input 控制消息按 lease 门控（仅该节点当前活约持有会话放行）。
//
// 设计原则（协议规范 §0/§3）：服务器对媒体包按字节转发（不解析媒体语义，
// 仅读取 34B 头中的 captureUnixUs 用于 host→server 腿延迟统计），控制消息
// 做最小路由。多 viewer 扇出对应"多观察者"能力。
package rtv

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
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
	// Node/Session 是连接期鉴权得到的绑定（hub 操作与 input 门控用）。
	Node() string
	Session() string
	SendDatagram(b []byte) error
	SendControlJSON(v json.RawMessage) error
	Close()
}

// HostSession 一个已注册的采集端连接。
type HostSession struct {
	NodeID      string
	ConnectedAt time.Time
	hub         *Hub // RegisterHost 注入（input 门控回调用）

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
	latEMAUs          atomic.Int64 // host→server 腿延迟（含时钟偏差，观测用）
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

// forwardToHost viewer→host 控制消息转发（input 消息按门控放行——由
// Hub.gateInput 判定该 viewer 会话是否持节点活约）。
func (h *HostSession) forwardToHost(v json.RawMessage, from Viewer) {
	var probe struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(v, &probe)
	if probe.Type == "input" {
		if h.hub == nil || !h.hub.gateInput(from.Node(), from.Session()) {
			return // 无输入权：静默丢弃（不回错——旧栈同语义，view-only）
		}
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

// Hub 全局会话表（node 键）+ per-node HostToken 注册表。
type Hub struct {
	mu      sync.Mutex
	hosts   map[string]*HostSession
	tokens  map[string]string // nodeID → HostToken（会话创建时签发/复用）
	started time.Time
	nextID  atomic.Uint64

	// gateInput 由 api 层注入：该 (node, session) 是否持节点输入活约。
	gateInput func(node, session string) bool

	// hostTokenOf 由 api 层注入：节点当前活跃 desktop 会话 params 里的
	// HostToken（无活约/解析失败 = ""）。server 重启后 minted 表清空，而
	// 运行中的 host 重连时重放的是 spawn 时的 token——经活跃会话回查即可
	// 无持久化地恢复注册（会话仍在 = 凭据仍有效）。
	hostTokenOf func(node string) string
}

func NewHub() *Hub {
	return &Hub{hosts: map[string]*HostSession{}, tokens: map[string]string{},
		started: time.Now()}
}

// SetInputGate 注入 input 门控（须在 Start 前完成）。
func (g *Hub) SetInputGate(fn func(node, session string) bool) { g.gateInput = fn }

// SetHostTokenOf 注入活跃会话 token 回查（server 重启后的 host 重注册路径）。
func (g *Hub) SetHostTokenOf(fn func(node string) string) { g.hostTokenOf = fn }

func (g *Hub) NextViewerID() uint64 { return g.nextID.Add(1) }

// HostTokenFor 返回（必要时签发）节点的 HostToken。签发后跨会话稳定：
// core 侧 StartCapture 幂等复用运行中的 host，第二次会话的 cfg 不会送达
// host，故 token 必须可重放。32 字节随机（hex 传递）。
func (g *Hub) HostTokenFor(nodeID string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if t, ok := g.tokens[nodeID]; ok {
		return t
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand 失败不可恢复
	}
	t := hex.EncodeToString(b[:])
	g.tokens[nodeID] = t
	return t
}

// validateHost 校验 host 注册凭据（hello{nodeId, token}）：命中 minted 表，
// 或回查该节点任一活跃 desktop 会话的 params token（server 重启恢复路径）。
func (g *Hub) validateHost(nodeID, token string) bool {
	if nodeID == "" || token == "" {
		return false
	}
	g.mu.Lock()
	want := g.tokens[nodeID]
	g.mu.Unlock()
	if token == want && want != "" {
		return true
	}
	if g.hostTokenOf != nil {
		return token == g.hostTokenOf(nodeID) && token != ""
	}
	return false
}

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
	// 通知 host 新 viewer 数 + 合成一次 IDR 请求（rustdesk 新订阅者触发关键帧的对应物）
	h.notifyViewers()
	h.forwardToHostNoGate(mustJSON(map[string]any{"type": "frameLoss", "frameIndex": 0, "reason": "new-viewer"}))
	slog.Info("viewer joined", "node", nodeID, "viewer", w.ID(), "kind", w.Kind(), "total", h.viewerCount())
}

func (g *Hub) RemoveViewer(nodeID string, w Viewer) {
	h := g.Host(nodeID)
	if h == nil {
		return
	}
	h.mu.Lock()
	delete(h.viewers, w.ID())
	h.mu.Unlock()
	h.notifyViewers()
	slog.Info("viewer left", "node", nodeID, "viewer", w.ID(), "total", h.viewerCount())
}

// Snapshot statsz 输出（管理端挂载）。
func (g *Hub) Snapshot() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	hosts := []map[string]any{}
	for id, h := range g.hosts {
		hosts = append(hosts, map[string]any{
			"node":             id,
			"connectedSince":   h.ConnectedAt.Format(time.RFC3339),
			"viewers":          h.viewerCount(),
			"rxPkgs":           h.rxPkgs.Load(),
			"rxBytes":          h.rxBytes.Load(),
			"txPkgs":           h.txPkgs.Load(),
			"txBytes":          h.txBytes.Load(),
			"framesSeen":       h.framesSeen.Load(),
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
