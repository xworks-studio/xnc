// Package session 实现 agent 侧会话引擎：SESSION_OPEN → 按 WsURL 拨号 →
// kind 分发给注册的 Handler；SESSION_CLOSE → 取消会话 ctx（触发处理器
// 内部的清理路径，如 exec 杀进程树）。
package session

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/coder/websocket"

	"xnc/proto"
)

// wsReadLimit 会话 WS 单帧读上限：统一取 proto.MaxSessionFrameBytes
// （file 64KiB chunk 与 screen 大 I 帧共用此限；详见 HandleSessionOpen 内注释）。
const wsReadLimit = proto.MaxSessionFrameBytes

// Handler 会话 kind 处理器（与会话 WS 一一对应）。ctx 由 Engine 持有：
// SESSION_CLOSE 到来即取消；ws 归 Engine 关闭（处理器返回后 CloseNow）。
type Handler interface {
	Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage)
}

// WslessHandler 无会话 WS 的处理器（RTV desktop 形态：SESSION_OPEN 只
// 触发本地编排——拉起 xnc-host；媒体经 host QUIC 直连 relay，不经 agent，
// 因此没有会话 WS 可拨）。SessionStart 应阻塞至 ctx 取消（引擎在
// SESSION_CLOSE 时取消 ctx），并在退出前完成自有清理。
type WslessHandler interface {
	SessionStart(ctx context.Context, sessionID string, params json.RawMessage)
}

// Engine 实现 connect.Handler：消费控制连接下发的 SESSION_OPEN/SESSION_CLOSE。
// 注意：分发器的 ctx 随控制连接死亡而取消，因此 HandleSessionOpen/Close 一律
// 忽略入参 ctx——每个会话自持 context.WithCancel(context.Background())。
type Engine struct {
	log         *slog.Logger
	sendControl func(m proto.Message) error
	handlers    map[string]any // Handler 或 WslessHandler（见 Register）

	mu     sync.Mutex
	active map[string]context.CancelFunc
}

func NewEngine(log *slog.Logger, sendControl func(m proto.Message) error) *Engine {
	return &Engine{log: log, sendControl: sendControl,
		handlers: map[string]any{}, active: map[string]context.CancelFunc{}}
}

// Register 注册 kind 处理器（Handler 或 WslessHandler 二选一；注册时
// 校验形态，Run 后调用仍属未定义行为——无并发写保护需求）。
func (e *Engine) Register(kind string, h any) {
	switch h.(type) {
	case Handler, WslessHandler:
	default:
		panic("session: handler must implement Handler or WslessHandler")
	}
	e.handlers[kind] = h
}

// HandleSessionOpen 拨号会话 WS 并分发；未知 kind 经控制连接回 SESSION_REFUSED。
func (e *Engine) HandleSessionOpen(_ context.Context, so proto.SessionOpen) {
	h, ok := e.handlers[so.Kind]
	if !ok {
		e.log.Warn("unsupported session kind", "kind", so.Kind, "session", so.SessionID)
		if e.sendControl != nil {
			msg, _ := proto.NewMsg(proto.TypeSessionRefused, proto.SessionRefused{
				SessionID: so.SessionID, Code: proto.CodeKindUnsupported,
				Message: "kind not supported by agent",
			})
			_ = e.sendControl(msg)
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.active[so.SessionID] = cancel
	e.mu.Unlock()

	go func() {
		defer func() {
			e.mu.Lock()
			delete(e.active, so.SessionID)
			e.mu.Unlock()
			cancel()
		}()
		// RTV desktop：无会话 WS（媒体不经 agent），直接进编排路径。
		if wl, ok := h.(WslessHandler); ok {
			wl.SessionStart(ctx, so.SessionID, so.Params)
			return
		}
		ws, _, err := websocket.Dial(ctx, so.WsURL, nil)
		if err != nil {
			e.log.Warn("session dial failed", "session", so.SessionID, "err", err)
			return // Opening TTL 兜底
		}
		// 读限在 handler 读帧前生效：file upload 的 64KiB binary chunk 超
		// coder/websocket 默认 32768，不抬会首帧即断（1MiB 留余量仍防滥用）。
		ws.SetReadLimit(wsReadLimit)
		defer ws.CloseNow()
		h.(Handler).Handle(ctx, ws, so.SessionID, so.Params)
	}()
}

// HandleSessionClose 取消会话 ctx：触发 ExecManager 的取消路径（杀进程树、
// 清理、退出）。未知会话（已结束/不存在）静默忽略。
func (e *Engine) HandleSessionClose(_ context.Context, sc proto.SessionClose) {
	e.mu.Lock()
	cancel, ok := e.active[sc.SessionID]
	e.mu.Unlock()
	if ok {
		cancel()
	}
}
