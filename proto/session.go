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

// KindScreen 单帧快照会话（M2-Slice3 Task 3 换轨）：流式 H.264 管线已
// 退役（流式观看走 KindDesktop 实时桌面会话）；kind 名与 REST/CLI 语法
// 保留，snapshot=true 时经 xnc-core 0x0111 一次性 spawn
// xnc-desktop --jpeg-single 回传 JPEG（0x03 子帧），流式请求由 agent 以
// 稳定码 SCREEN_STREAM_RETIRED 拒绝。
const KindScreen = "screen"

// KindDesktop 实时桌面会话（M1-Slice2）：agent 侧 xnc-desktop rt pipe →
// Pion WebRTC publisher（relay-only）→ 会话 WS 只走 JSON 信令（词汇契约
// 见 agent/desktop/session.go 文件头，T5/T6 消费）。
const KindDesktop = "desktop"

// DesktopIceAll 是 iceTransportPolicy 的非 relay 覆盖值，仅回环单测使用
// （无 TURN 环境）；缺省（空或 "relay"）= 生产强约束 relay。
const DesktopIceAll = "all"

// DesktopTurnConfig 是 desktop 会话的 TURN 中继配置（server 经
// SESSION_OPEN params 下发；dev = 非 TLS turn:<host>:3478?transport=tcp，
// TLS/443 = M2）。credential 绝不入任何日志。
type DesktopTurnConfig struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

// Configured 报告 TURN 配置是否完整（URLs/username/credential 全非空）。
// desktop 会话 relay-only：任一缺失即视为未配置（server 侧 503
// TURN_UNCONFIGURED 的判定；nil 接收者安全）。
func (t *DesktopTurnConfig) Configured() bool {
	return t != nil && len(t.URLs) > 0 && t.Username != "" && t.Credential != ""
}

// DesktopParams 会话 Params 的 desktop 形态（M1-Slice2）。
type DesktopParams struct {
	// Signaling 目前仅 "webrtc"；空视同 "webrtc"（本片唯一形态）。
	Signaling string             `json:"signaling,omitempty"`
	Turn      *DesktopTurnConfig `json:"turn,omitempty"`
	// WTSSession 目标 WTS 会话 id；0 = 活动控制台会话（dev 默认）。
	WTSSession uint32 `json:"wtsSession,omitempty"`
	// IceTransportPolicy 缺省 "relay"；DesktopIceAll 仅测试。
	IceTransportPolicy string `json:"iceTransportPolicy,omitempty"`
	// LeaseID（M2-Slice3 Task 4）：server 侧 per-node 仲裁的唯一活约 id。
	// 仅授予会话的 params 携带（每节点同时至多一个）；未携带 = view-only。
	// agent 侧输入转发以本字段为凭（本地仲裁表已退役，spec §11.1）。
	LeaseID string `json:"leaseId,omitempty"`
	// Capabilities（M2-Slice3 Task 4）：server 按 RBAC 角色在会话创建时
	// 计算并下发（viewer: [screen.view]; operator: +[input.mouse,
	// input.keyboard]; owner: +[input.secure_attention, shell.system]），
	// agent 侧强制：input.* 缺失拒转发对应输入、input.secure_attention
	// 缺失拒 SAS（spec §14）。客户端提交值被白名单剥离，只来自 server。
	Capabilities []string `json:"capabilities,omitempty"`
}

// Desktop capability 词汇（server 下发 / agent 强制，spec §14）。
const (
	CapScreenView        = "screen.view"
	CapInputMouse        = "input.mouse"
	CapInputKeyboard     = "input.keyboard"
	CapInputSecureAttn   = "input.secure_attention"
	CapShellSystem       = "shell.system"
)

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
// System(M2-Slice2):true = SYSTEM 令牌显式请求(server 侧 owner-only
// RBAC + 审计 system=true;agent 经 xnc-core CreateShell token_kind 落
// 地);缺省 false = 用户令牌(spec §8.4,加字段向后兼容)。
type ExecParams struct {
	Command    string   `json:"command,omitempty"`
	Script     string   `json:"script,omitempty"`
	TimeoutSec int      `json:"timeoutSec,omitempty"`
	Cwd        string   `json:"cwd,omitempty"`
	Shell      string   `json:"shell,omitempty"` // auto|bash|pwsh|powershell|cmd
	Env        []string `json:"env,omitempty"`   // KEY=VAL 列表
	System     bool     `json:"system,omitempty"`
}

// ExecResult exec 会话 WS 的终态 text 帧；超时/被杀时 ExitCode 为 null。
// Code 非 "" = 稳定拒绝码(CORE_UNAVAILABLE / NO_ACTIVE_SESSION /
// SESSION_MISMATCH…,创建即失败,进程未启动)。
type ExecResult struct {
	ExitCode   *int   `json:"exitCode"`
	TimedOut   bool   `json:"timedOut"`
	DurationMs int64  `json:"durationMs"`
	Code       string `json:"code,omitempty"`
	// Truncated(M2-Slice2 T5):oneshot 输出超出背压预算发生丢弃时为
	// true,且输出流末尾带 stderr 标记行 "[xnc] output truncated: N
	// bytes dropped"(加字段,旧 client 忽略)。
	Truncated bool `json:"truncated,omitempty"`
}

// ShellParams 会话 Params 的 shell 形态；Cols/Rows 为 0 时用默认 120x30，
// Shell 为空时由 agent 按探测结果决定。System 语义同 ExecParams。
type ShellParams struct {
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	Shell  string `json:"shell,omitempty"`
	System bool   `json:"system,omitempty"`
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

// ScreenParams SESSION_OPEN params（M2-Slice3：快照专用；fps/quality 字段
// 仅为兼容保留，流式已退役）。maxWidth 默认 1920（核心侧 box-filter 降
// 采样上限）；0 值由 server 端补默认后再下发。
type ScreenParams struct {
	Fps      int  `json:"fps,omitempty"`      // 兼容保留（流式退役，忽略）
	Quality  int  `json:"quality,omitempty"`  // 兼容保留（JPEG 质量由桌面侧固定 0.85）
	MaxWidth int  `json:"maxWidth,omitempty"` // 默认 1920（降采样上限）
	Snapshot bool `json:"snapshot,omitempty"` // 单帧 JPEG 模式（唯一支持的模式）
}

// ScreenBegin agent → client：流开始（SCREEN_BEGIN text 帧）。
type ScreenBegin struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	State  string `json:"state"` // capturing / locked / no_session
	Codec  string `json:"codec"` // 快照 "jpeg"（"h264" 为退役流式词汇）
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
