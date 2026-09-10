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

// KindScreen screen 会话（LEGACY：2026-09-08 RTV 重构后整体退役）。
// 流式与快照通道（xnc-core 0x0111 --jpeg-single）均已删除；server 端点
// 已移除，CLI 报退役提示，agent 侧 handler 保留为防御面（旧客户端仍会
// 发该 kind），恒回 SCREEN_STREAM_RETIRED / SCREEN_SNAPSHOT_UNSUPPORTED。
// 常量与下方 Screen* 类型保留作文档锚点与旧二进制兼容面；快照恢复列
// RTV 设计文档后续 PATCH 清单。
const KindScreen = "screen"

// KindDesktop 实时桌面会话（RTV 重构，2026-09-08）：agent 收到 SESSION_OPEN
// 后经 xnc-core 拉起 xnc-host（Rust），host 持本参数的 StreamEndpoint+
// HostToken 直连 server QUIC 腿注册；媒体不经 agent，浏览器经 server 的
// WT/WS 腿观看（中继协议见 server/internal/rtv 与重构 spec）。
const KindDesktop = "desktop"

// DesktopParams 会话 Params 的 desktop 形态（RTV）。
type DesktopParams struct {
	// StreamEndpoint host 腿 QUIC 地址（host:port，UDP 4433 形态）。
	StreamEndpoint string `json:"streamEndpoint,omitempty"`
	// HostToken host 注册令牌：server 会话创建时签发、绑定节点，经
	// SESSION_OPEN → agent → xnc-core → stdin 交给 xnc-host；host 凭它
	// 向 relay 注册（重连/崩溃重启重放同一 token = 合法再注册）。
	// 绝不入日志/argv。
	HostToken string `json:"hostToken,omitempty"`
	// WTSSession 目标 WTS 会话 id；0 = 活动控制台会话（dev 默认）。
	WTSSession uint32 `json:"wtsSession,omitempty"`
	// TLSInsecure（dev-only）：dev 栈自签证书时 host 跳过服务端证书校验。
	// 只来自 server config XNC_RTV_INSECURE_TLS；客户端提交值被白名单
	// 剥离；生产绝不开（server 侧高声告警）。
	TLSInsecure bool `json:"tlsInsecure,omitempty"`
	// CertSHA256 纯 IP relay 自签证书钉扎：host 腿 rustls 按此指纹
	// （relay 自签证书 DER 的 SHA-256，64 位 hex）校验服务器证书，跳过
	// 系统 Web PKI；空 = 标准 Web PKI（域名 relay，系统根校验）。
	CertSHA256 string `json:"certSha256,omitempty"`
}

// Desktop capability 词汇（RTV 后由 server 中继侧强制：input 门控按
// 会话 lease + capability；保留词汇供 RBAC 计算与 relay 门控共用）。
const (
	CapScreenView      = "screen.view"
	CapInputMouse      = "input.mouse"
	CapInputKeyboard   = "input.keyboard"
	CapInputSecureAttn = "input.secure_attention"
	CapShellSystem     = "shell.system"
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
	Code    string `json:"code"`              // FILE_NOT_FOUND / FILE_TOO_LARGE / HASH_MISMATCH / ACCESS_DENIED / INTERNAL
	Message string `json:"message,omitempty"` // 可读的底层错误文本（如 rename 失败原因）；旧 agent 不发此字段，CLI 端容缺省
}

// TunnelParams SESSION_OPEN params：target 枚举（server 白名单解析为 host/port）。
type TunnelParams struct {
	Target string `json:"target"` // "rdp"
}

// ScreenParams SESSION_OPEN params —— LEGACY：screen 会话整体退役（见
// KindScreen 注释），本组类型仅为旧二进制兼容面保留，生产链路不再使用。
type ScreenParams struct {
	Fps      int  `json:"fps,omitempty"`      // legacy：流式退役，忽略
	Quality  int  `json:"quality,omitempty"`  // legacy：快照通道已删，忽略
	MaxWidth int  `json:"maxWidth,omitempty"` // legacy
	Snapshot bool `json:"snapshot,omitempty"` // legacy：唯一曾支持的模式，现已拒绝
}

// ScreenBegin agent → client：流开始（SCREEN_BEGIN text 帧）—— legacy 词表。
type ScreenBegin struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	State  string `json:"state"` // capturing / locked / no_session
	Codec  string `json:"codec"` // legacy："jpeg"/"h264" 词汇均已退役
}

// ScreenState agent → client：捕获状态变化（SCREEN_STATE text 帧）—— legacy。
type ScreenState struct {
	State string `json:"state"` // capturing / locked / no_session
}

// Screen 会话 WS 二进制帧子头（legacy 词表，流式与快照通道均已退役）。
const (
	ScreenBinKey   byte = 0x01 // legacy：H.264 关键帧
	ScreenBinDelta byte = 0x02 // legacy：H.264 增量帧
	ScreenBinJPEG  byte = 0x03 // legacy：JPEG 单帧（快照模式）
)

// MaxSessionFrameBytes 会话 WS 单帧上限（server/agent/CLI 三处共用同一
// 常量）。8MiB：H.264 大 I 帧实测 <1MB，JPEG 快照 <2MB，留足余量；此前
// server 1MiB / agent 32MiB 不一致会在高码率 I 帧上断流。
const MaxSessionFrameBytes = 8 << 20
