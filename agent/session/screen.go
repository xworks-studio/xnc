// screen.go — 会话 kind=screen 的 agent 侧处理器(M2-Slice3 Task 3 换轨)。
//
// RTV 重构(2026-09-08):流式(screen 参数无 snapshot=true)与单帧快照
// 均退役——流式走 desktop 会话(RTV 链路),快照通道(xnc-core 0x0111 →
// xnc-desktop --jpeg-single)已随 C++ 栈删除,恢复为后续 PATCH(spec §8.4)。
// 两种请求分别以稳定码 SCREEN_STREAM_RETIRED / SCREEN_SNAPSHOT_
// UNSUPPORTED 拒绝。
package session

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/coder/websocket"

	"xnc/proto"
)

// 稳定码。
const (
	// CodeScreenStreamRetired:screen 流式模式已退役,使用 desktop 会话。
	CodeScreenStreamRetired = "SCREEN_STREAM_RETIRED"
	// CodeSnapshotUnsupported:快照通道已随 RTV 重构退役(后续 PATCH 恢复)。
	CodeSnapshotUnsupported = "SCREEN_SNAPSHOT_UNSUPPORTED"
)

const (
	// typeScreenBegin 快照回送前仍发一帧 SCREEN_BEGIN(codec=jpeg)。
	typeScreenBegin = "SCREEN_BEGIN"

	screenDefaultMaxWidth = 1920
)

// SnapshotProvider 是快照来源抽象(core 连接或测试 fake)。
type SnapshotProvider interface {
	// Snapshot 经核心取单帧 JPEG;maxWidth 0 = 由核心/桌面侧默认夹紧。
	Snapshot(maxWidth uint32) ([]byte, error)
}

// ScreenHandler 实现 session.Handler:一个 screen 会话 = 一次快照。
type ScreenHandler struct {
	Snap SnapshotProvider
	Log  *slog.Logger
}

// NewScreenHandler 构造使用默认 core 快照通路的处理器(Windows 经
// coreclient 0x0111;非 Windows / 凭据缺失 → CORE_UNAVAILABLE)。
// stateDir 传入以支持生产回落(XNCCore 服务约定;2026-08-24 修复:
// 此前走 env-only 的 DefaultShellHost,生产无 env 时快照恒 CORE_UNAVAILABLE,
// 而 exec/shell 已统一 ShellHostFromStateDir)。
func NewScreenHandler(stateDir string) *ScreenHandler {
	log := slog.Default()
	return &ScreenHandler{Snap: defaultSnapshotProvider(stateDir, log), Log: log}
}

// Handle:快照模式 → SCREEN_BEGIN + 0x03 JPEG binary + 收线;流式 →
// SCREEN_STREAM_RETIRED 稳定码错误。
func (h *ScreenHandler) Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage) {
	var p proto.ScreenParams
	_ = json.Unmarshal(params, &p)

	if !p.Snapshot {
		h.log().Warn("screen stream rejected (retired)", "session", sessionID)
		writeScreenText(ctx, ws, proto.TypeError, proto.ErrorPayload{
			Code:    CodeScreenStreamRetired,
			Message: "screen streaming is retired; use a desktop session for live view",
		})
		return
	}

	_ = h.Snap // 快照通路已退役(见文件头);保留字段供 PATCH 恢复
	h.log().Warn("screen snapshot rejected (retired)", "session", sessionID)
	writeScreenText(ctx, ws, proto.TypeError, proto.ErrorPayload{
		Code:    CodeSnapshotUnsupported,
		Message: "screen snapshot is retired with the RTV rewrite; live view via desktop sessions",
	})
}

func (h *ScreenHandler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func writeScreenText(ctx context.Context, ws *websocket.Conn, typ string, payload any) {
	msg, err := proto.NewMsg(typ, payload)
	if err != nil {
		return
	}
	jb, err := json.Marshal(msg)
	if err != nil {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
	defer cancel()
	_ = ws.Write(wctx, websocket.MessageText, jb)
}

func writeScreenBinary(ctx context.Context, ws *websocket.Conn, data []byte) {
	wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
	defer cancel()
	_ = ws.Write(wctx, websocket.MessageBinary, data)
}
