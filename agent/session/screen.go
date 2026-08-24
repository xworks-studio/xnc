// screen.go — 会话 kind=screen 的 agent 侧处理器(M2-Slice3 Task 3 换轨)。
//
// screen 流式管线(xnc-screen-helper + named pipe + FrameHub)已退役:
// 本文件只保留单帧快照——经 xnc-core MSG_SNAPSHOT(0x0111)一次性 spawn
// xnc-desktop --jpeg-single,取回 JPEG 字节,以既有 screen 会话 WS 二进制
// 子帧协议(proto.ScreenBinJPEG=0x03)回送,随即收线。快照默认 max_width
// = 1920(核心侧 box-filter 降采样)。
//
// 流式观看走 desktop 会话(StartCapture → xnc-desktop --console-rt);
// 对 stream 请求(screen 参数无 snapshot=true)以稳定码
// SCREEN_STREAM_RETIRED 拒绝。
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
	// CodeSnapshotFailed:快照失败(core 拒绝码或传输层错误并入消息)。
	CodeSnapshotFailed = "SNAPSHOT_FAILED"
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

	maxW := p.MaxWidth
	if maxW <= 0 {
		maxW = screenDefaultMaxWidth
	}
	jpeg, err := h.Snap.Snapshot(uint32(maxW))
	if err != nil {
		h.log().Warn("screen snapshot failed", "err", err)
		writeScreenText(ctx, ws, proto.TypeError, proto.ErrorPayload{
			Code:    CodeSnapshotFailed,
			Message: err.Error(),
		})
		return
	}
	writeScreenText(ctx, ws, typeScreenBegin, proto.ScreenBegin{
		State: "capturing", Codec: "jpeg",
	})
	writeScreenBinary(ctx, ws, append([]byte{proto.ScreenBinJPEG}, jpeg...))
	_ = ws.Close(websocket.StatusNormalClosure, "snapshot done")
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
