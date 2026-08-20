package proto

import (
	"encoding/json"
	"time"
)

// 会话 kind。Phase 2 exec；shell Phase 3 注册；file/screen/tunnel 由后续 Phase 注册。
const KindExec = "exec"

// KindShell 交互式终端会话（ConPTY，Phase 3）。
const KindShell = "shell"

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
