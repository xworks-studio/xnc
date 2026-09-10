//go:build windows

// Package desktop — RTV 形态的 desktop 会话 thin handler（2026-09-08 重构）。
//
// 职责收缩为纯生命周期编排：
//
//	SESSION_OPEN  → coreclient.StartCapture(wts, cfg)（cfg = endpoint/
//	                nodeId/token 的 xnc-host stdin JSON；经 xnc-core spawn，
//	                host 自行 QUIC 直连 relay 注册）
//	SESSION_CLOSE → 会话引用计数归零 → coreclient.StopCapture
//
// 媒体/输入/QoS 均不经 agent（host ↔ relay ↔ 浏览器直达）；崩溃监督由
// xnc-core 的 DesktopSupervisor 承担（退避重启 + 崩溃循环降级锁定，重启
// 原样重放首次的 cfg blob——server 侧 HostToken 按节点绑定，重放即合法
// 再注册）。Pion/WebRTC/rt-pipe 旧栈已随重构删除。
package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"golang.org/x/sys/windows"

	"xnc/agent/coreclient"
	"xnc/proto"
)

// hostCfg 是 xnc-host --stdin-config 消费的配置 JSON（host/src/main.rs
// StdinConfig 的生产者侧镜像；token 绝不入日志）。
type hostCfg struct {
	Endpoint string `json:"endpoint"`
	NodeID   string `json:"nodeId"`
	Token    string `json:"token"`
	// TLSInsecure 透传 proto.DesktopParams 同名 dev 开关（自签 dev 栈）。
	TLSInsecure bool `json:"tlsInsecure,omitempty"`
	// CertSHA256 透传 proto.DesktopParams 同名 relay 自签证书钉扎指纹
	// （纯 IP relay 形态；空 = 标准 Web PKI，host 侧回退系统根校验）。
	CertSHA256 string `json:"certSha256,omitempty"`
}

// Handler 实现 session.WslessHandler：一个 desktop 会话 = 一次引用计数
// StartCapture + 阻塞至 ctx 取消 + 引用计数 StopCapture。
type Handler struct {
	Log *slog.Logger

	pipe   string
	secret []byte
	nodeID string

	mu       sync.Mutex
	client   *coreclient.Client
	sessions map[string]bool // sessionID → 活跃（引用计数）
}

// NewHandler 生产入口：凭据 = env（优先，dev-console 保持既有语义）→
// 生产缺省（XNCCore 服务约定，core-secret.hex）。拿不到凭据返回 nil
// （desktop kind 不注册，零行为变化）。
func NewHandler(stateDir, nodeID string, log *slog.Logger) *Handler {
	pipe, secret, err := coreclient.ResolveCoreEndpoint(stateDir)
	if err != nil {
		if log == nil {
			log = slog.Default()
		}
		log.Info("desktop: core credentials unavailable, kind not registered", "err", err)
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &Handler{Log: log, pipe: pipe, secret: secret, nodeID: nodeID,
		sessions: map[string]bool{}}
}

// ensureClientLocked 返回活的 core 连接（探活失败即重拨）。调用方持 mu。
func (h *Handler) ensureClientLocked() (*coreclient.Client, error) {
	if h.client != nil {
		if _, err := h.client.Ping(); err == nil {
			return h.client, nil
		}
		_ = h.client.Close()
		h.client = nil
	}
	c, err := coreclient.Dial(h.pipe, h.secret)
	if err != nil {
		return nil, fmt.Errorf("desktop: core dial: %w", err)
	}
	h.client = c
	return c, nil
}

// SessionStart：解析 DesktopParams → StartCapture（幂等，多会话共享同一
// host）→ 记账 → 阻塞至 ctx 取消（SESSION_CLOSE 由引擎取消 ctx 触发）。
// 启动失败仅记日志并返回——server 侧 Opening TTL 兜底回收会话。
func (h *Handler) SessionStart(ctx context.Context, sessionID string, params json.RawMessage) {
	var p proto.DesktopParams
	if err := json.Unmarshal(params, &p); err != nil || p.StreamEndpoint == "" || p.HostToken == "" {
		h.log().Warn("desktop: bad session params, ignoring session", "session", sessionID, "err", err)
		return
	}
	cfg, err := json.Marshal(hostCfg{Endpoint: p.StreamEndpoint, NodeID: h.nodeID, Token: p.HostToken, TLSInsecure: p.TLSInsecure, CertSHA256: p.CertSHA256})
	if err != nil {
		h.log().Warn("desktop: cfg marshal", "session", sessionID, "err", err)
		return
	}

	wts := p.WTSSession
	if wts == 0 {
		wts = windows.WTSGetActiveConsoleSessionId()
	}

	h.mu.Lock()
	if h.sessions[sessionID] {
		h.mu.Unlock()
		return // 引擎重复分发（不应发生），幂等忽略
	}
	c, err := h.ensureClientLocked()
	h.mu.Unlock()
	if err != nil {
		h.log().Warn("desktop: core unavailable", "session", sessionID, "err", err)
		return
	}
	pid, gen, err := c.StartCapture(wts, cfg)
	if err != nil {
		h.log().Warn("desktop: start_capture failed", "session", sessionID, "err", err)
		return
	}
	h.mu.Lock()
	h.sessions[sessionID] = true
	h.mu.Unlock()
	h.log().Info("desktop host started",
		"session", sessionID, "host_pid", pid, "gen", gen, "wts", wts) // 无 token/凭据

	<-ctx.Done()

	h.release(sessionID)
}

// release 会话终结：引用计数减一，最后一个会话 best-effort StopCapture
// （幂等；失败仅记日志——孤儿 child 由 core 侧 watcher 收）。
func (h *Handler) release(sessionID string) {
	h.mu.Lock()
	if !h.sessions[sessionID] {
		h.mu.Unlock()
		return
	}
	delete(h.sessions, sessionID)
	last := len(h.sessions) == 0
	c := h.client
	h.mu.Unlock()
	if !last || c == nil {
		return
	}
	if err := c.StopCapture(); err != nil {
		h.log().Warn("desktop: stop_capture failed", "err", err)
	}
	h.log().Info("desktop host stopped (last session closed)")
}

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}
