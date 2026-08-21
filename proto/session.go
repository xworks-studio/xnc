package proto

import (
	"encoding/json"
	"time"
)

// 会话 kind。Phase 2 exec；shell Phase 3 注册；file/screen/tunnel 由后续 Phase 注册。
const KindExec = "exec"

// KindShell 交互式终端会话（ConPTY，Phase 3）。
const KindShell = "shell"

// KindFile 文件上传/下载会话（Phase 4）。
const KindFile = "file"

// KindTunnel 端口隧道会话（RDP 等，Phase 4）。
const KindTunnel = "tunnel"

// KindScreen 桌面流会话（DXGI 捕获 + H.264，Phase 6）。
const KindScreen = "screen"

// SessionOpen 经控制连接下发：agent 按 WsURL（含 token 的绝对 URL）拨号。
type SessionOpen struct {
	SessionID  string          `json:"sessionId"`
	Kind       string          `json:"kind"`
	Params     json.RawMessage `json:"params"`
	AgentToken string          `json:"agentToken"`
	WsURL      string          `json:"wsUrl"`
	ExpiresAt  time.Time       `json:"expiresAt"`
}

// SessionRefused agent 不支持 kind 等能力协商失败时经控制连接回送。
type SessionRefused struct {
	SessionID string `json:"sessionId"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// SessionClose server → agent 的会话清理通知（client 断开/超时/节点侧主动结束）。
type SessionClose struct {
	SessionID string `json:"sessionId"`
	Reason    string `json:"reason"`
}

// ExecParams 会话 Params 的 exec 形态；command 与 script 二选一。
type ExecParams struct {
	Command    string `json:"command,omitempty"`
	Script     string `json:"script,omitempty"`
	TimeoutSec int    `json:"timeoutSec,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
}

// ExecResult exec 会话 WS 的终态 text 帧；超时/被杀时 ExitCode 为 null。
type ExecResult struct {
	ExitCode   *int   `json:"exitCode"`
	TimedOut   bool   `json:"timedOut"`
	DurationMs int64  `json:"durationMs"`
}

// ShellParams 会话 Params 的 shell 形态；Cols/Rows 为 0 时用默认 120x30，
// Shell 为空时由 agent 按探测结果决定。
type ShellParams struct {
	Cols  int    `json:"cols,omitempty"`
	Rows  int    `json:"rows,omitempty"`
	Shell string `json:"shell,omitempty"`
}

// ShellBegin agent → client：实际使用的 shell（SHELL_BEGIN text 帧）。
type ShellBegin struct {
	Shell string `json:"shell"`
}

// ShellResize client → agent：终端尺寸变化（SHELL_RESIZE text 帧）。
type ShellResize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// FileParams SESSION_OPEN params：upload 必带 size+sha256，download 只带 path。
type FileParams struct {
	Direction string `json:"direction"` // "upload" | "download"
	Path      string `json:"path"`      // 绝对路径
	Size      int64  `json:"size,omitempty"`
	Sha256    string `json:"sha256,omitempty"`
}

// FileBegin agent → client 的数据流开始标记。
type FileBegin struct {
	Direction string `json:"direction"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Sha256    string `json:"sha256"`
}

// FileResult 终态：写入方计算实际 sha256 与声明比对。ok=false 表示校验失败。
type FileResult struct {
	Bytes  int64  `json:"bytes"`
	Sha256 string `json:"sha256"`
	Ok     bool   `json:"ok"`
}

// FileError 错误终态。
type FileError struct {
	Code string `json:"code"` // FILE_NOT_FOUND / FILE_TOO_LARGE / HASH_MISMATCH
}

// TunnelParams SESSION_OPEN params：target 枚举（server 白名单解析为 host/port）。
type TunnelParams struct {
	Target string `json:"target"` // "rdp"
}

// ScreenParams SESSION_OPEN params：fps 默认 15（上限 30），quality 默认 60，
// maxWidth 默认 1920；0 值由 server 端补默认后再下发。
type ScreenParams struct {
	Fps      int  `json:"fps,omitempty"`      // 默认 15，上限 30
	Quality  int  `json:"quality,omitempty"`  // JPEG/H.264 质量，默认 60
	MaxWidth int  `json:"maxWidth,omitempty"` // 默认 1920
	Snapshot bool `json:"snapshot,omitempty"` // 单帧 JPEG 模式（不走 H.264 流）
}

// ScreenBegin agent → client：流开始（SCREEN_BEGIN text 帧）。
type ScreenBegin struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	State  string `json:"state"` // capturing / locked / no_session
	Codec  string `json:"codec"` // "h264"
}

// ScreenState agent → client：捕获状态变化（SCREEN_STATE text 帧）。
type ScreenState struct {
	State string `json:"state"` // capturing / locked / no_session
}

// Screen 会话 WS 二进制帧子头：帧类型显式随帧走（第 1 字节），消费端免
// NALU 嗅探判定 key/delta——历史上三处独立嗅探实现各自漂移，是 WebCodecs
// 拒帧类问题的温床。
const (
	ScreenBinKey   byte = 0x01 // H.264 关键帧（Annex-B AU，首 NALU 必为 SPS）
	ScreenBinDelta byte = 0x02 // H.264 增量帧（Annex-B AU）
	ScreenBinJPEG  byte = 0x03 // JPEG 单帧（快照模式）
)

// MaxSessionFrameBytes 会话 WS 单帧上限（server/agent/CLI 三处共用同一
// 常量）。8MiB：H.264 大 I 帧实测 <1MB，JPEG 快照 <2MB，留足余量；此前
// server 1MiB / agent 32MiB 不一致会在高码率 I 帧上断流。
const MaxSessionFrameBytes = 8 << 20
