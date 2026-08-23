// shellhost.go — exec/shell 会话的进程创建抽象(M2-Slice2 Task 4):
// 旧直连 spawn(os/exec + ConPTY)已按 ledger 裁决移除,一律经
// xnc-core CreateShell(0x0120,令牌语义 spec §8.4)→ xnc-shell pipe
// (shellpipe 客户端)。本文件定义跨平台接口与错误形态;Windows 实现
// 在 shellhost_windows.go,非 Windows 桩在 shellhost_other.go(生产
// agent 仅部署 Windows,桩保证包可移植构建)。
package session

import "fmt"

// Shell 稳定码(core RejectedError 码 + agent 侧传输层码)。
const (
	// CodeCoreUnavailable:core 不可达(未配置/未运行/拨号失败)——dev
	// 拓扑下 exec/shell 依赖运行中的 xnc-core,缺失即此码(文档化行为)。
	CodeCoreUnavailable = "CORE_UNAVAILABLE"
	// CodeNoActiveSession:用户令牌请求但无活动用户会话(spec §8.4,
	// 不隐式提权)。
	CodeNoActiveSession = "NO_ACTIVE_SESSION"
)

// ShellHostError 是 CreateShell 的拒绝形态:Code 为稳定 ASCII 码
// (CORE_UNAVAILABLE / NO_ACTIVE_SESSION / SESSION_MISMATCH /
// BAD_PAYLOAD / SPAWN_FAILED / PIPE_TIMEOUT / TOKEN_FAILED)。
type ShellHostError struct{ Code string }

func (e *ShellHostError) Error() string {
	return fmt.Sprintf("shellhost: create shell rejected: %s", e.Code)
}

// ShellSpec 是一次 shell 创建请求的会话侧形态。
type ShellSpec struct {
	System      bool   // true = SYSTEM 令牌(默认用户令牌)
	Profile     string // POWERSHELL / PWSH / CMD / BASH(白名单名)
	Interactive bool   // false = oneshot(Command 必填)
	Cols, Rows  int    // interactive 几何
	Cwd         string
	Env         []string // "K=V"(值不得含 '\n')
	Command     string   // oneshot 内联命令 / 脚本调用串
	TimeoutSec  int      // oneshot 超时
}

// ShellStream 是一条 shell 输出数据(stderr 标记;interactive 的 pty
// 输出恒为 stdout 流)。
type ShellStream struct {
	Stderr bool
	Bytes  []byte
}

// ShellProc 是一个经 core 创建的 shell 进程(oneshot 或 interactive)。
// Stream/Exit 通道在连接终结后关闭;Kill 为杀树语义(0x0126 + 核心
// KillShell 兜底)。
type ShellProc interface {
	// Profile 返回 xnc-shell 实际生效的 profile 名(SHELL_BEGIN 内容)。
	Profile() string
	WriteStdin(p []byte) error
	Resize(cols, rows int) error
	Kill() error
	Stream() <-chan ShellStream
	Exit() <-chan uint32
	Close() error
}

// ShellHost 创建 shell(经 xnc-core);拒绝以 *ShellHostError 返回。
type ShellHost interface {
	CreateShell(spec ShellSpec) (ShellProc, error)
}
