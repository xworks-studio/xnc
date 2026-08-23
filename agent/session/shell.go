// shell.go — 会话 kind=shell 的交互终端模式(M2-Slice2 Task 4 起经
// xnc-core 创建:xnc-shell interactive ConPTY,令牌语义 spec §8.4——
// 默认用户令牌,System=true 显式 SYSTEM)。pty 输出以 binary 帧裸流
// 转发,client 键入经 binary 帧写入;text 帧仅接受 SHELL_RESIZE。
// SHELL_BEGIN 先于任何输出帧;创建失败回 ERROR(SHELL_START_FAILED,
// message 携带稳定码)后关连接;shell 退出 / 对端断开 / ctx 取消都
// 触发 Kill(0x0126 杀树 + 核心 KillShell 兜底)。旧直连 ConPTY 路径
// 已移除(ledger 裁决:无双路径)。
package session

import (
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"runtime"

	"github.com/coder/websocket"

	"xnc/proto"
)

const (
	// typeShellBegin / typeShellResize shell 会话 WS 的 text 帧类型（kind 私有
	// 词汇，不入 proto——同 typeExecResult 的归属规则）。
	typeShellBegin  = "SHELL_BEGIN"
	typeShellResize = "SHELL_RESIZE"

	shellDefaultCols = 120
	shellDefaultRows = 30
	shellMaxDim      = 1000
)

// Shell 交互终端处理器。Log 为 nil 时用 slog.Default();Host 为 nil 时
// 用 DefaultShellHost(dev 环境变量),测试注入 fake。
type Shell struct {
	Log  *slog.Logger
	Host ShellHost
}

func NewShell(log *slog.Logger) *Shell { return &Shell{Log: log, Host: DefaultShellHost(log)} }

func (sh *Shell) logger() *slog.Logger {
	if sh.Log != nil {
		return sh.Log
	}
	return slog.Default()
}

// Handle 承载一个交互 shell 直至终态：shell 退出（exit）、对端断开或
// SESSION_CLOSE（ctx 取消）任一发生即收线。返回即会话结束（引擎随后
// CloseNow）。
func (sh *Shell) Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage) {
	var p proto.ShellParams
	_ = json.Unmarshal(params, &p)
	cols, rows := p.Cols, p.Rows
	if cols < 1 || cols > shellMaxDim {
		cols = shellDefaultCols
	}
	if rows < 1 || rows > shellMaxDim {
		rows = shellDefaultRows
	}

	profile := shellProfile(p.Shell)
	if runtime.GOOS != "windows" {
		sh.failStart(ctx, ws, "shell sessions require windows agent")
		return
	}
	host := sh.Host
	if sh.Host == nil {
		// 未注入(dev 无凭据):走默认,若无凭据即 CORE_UNAVAILABLE。
		if host = DefaultShellHost(sh.logger()); host == nil {
			sh.failStart(ctx, ws, CodeCoreUnavailable)
			return
		}
	}
	proc, err := host.CreateShell(ShellSpec{
		System:      p.System,
		Profile:     profile,
		Interactive: true,
		Cols:        cols,
		Rows:        rows,
	})
	if err != nil {
		code := CodeCoreUnavailable
		var she *ShellHostError
		if asShellHostError(err, &she) {
			code = she.Code
		}
		sh.logger().Warn("shell create rejected", "session", sessionID, "code", code)
		sh.failStart(ctx, ws, code)
		return
	}

	// SHELL_BEGIN 先于任何输出帧(实际生效 profile 来自 xnc-shell)。
	if b, err := proto.NewMsg(typeShellBegin, proto.ShellBegin{Shell: proc.Profile()}); err == nil {
		if jb, err2 := json.Marshal(b); err2 == nil {
			wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
			_ = ws.Write(wctx, websocket.MessageText, jb)
			cancel()
		}
	}

	done := make(chan struct{})
	go func() { // pty 输出 → ws binary
		defer close(done)
		for d := range proc.Stream() {
			wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
			if we := ws.Write(wctx, websocket.MessageBinary, d.Bytes); we != nil {
				cancel()
				return
			}
			cancel()
		}
	}()

	gone := make(chan struct{})
	go func() { // ws 输入 → pty；text 只认 SHELL_RESIZE；对端断开即结束会话
		defer close(gone)
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			switch typ {
			case websocket.MessageBinary:
				if err := proc.WriteStdin(data); err != nil {
					return
				}
			case websocket.MessageText:
				var m proto.Message
				if json.Unmarshal(data, &m) != nil || m.Type != typeShellResize {
					continue
				}
				var r proto.ShellResize
				if m.Decode(&r) == nil && r.Cols > 0 && r.Cols <= shellMaxDim && r.Rows > 0 && r.Rows <= shellMaxDim {
					_ = proc.Resize(r.Cols, r.Rows)
				}
			}
		}
	}()

	exit := proc.Exit()
	select {
	case <-exit: // shell 退出（exit 命令）
	case <-done: // 输出流结束（pipe 关闭）
	case <-gone: // 对端断开
	case <-ctx.Done(): // SESSION_CLOSE / 会话终止
	}
	_ = proc.Kill() // 杀树(自然退出路径为幂等 no-op 语义)
	_ = proc.Close()
	_ = ws.Close(websocket.StatusNormalClosure, "")
}

// failStart 创建失败路径：ERROR{SHELL_START_FAILED} text 帧后以错误码关线。
// msg 携带稳定拒绝码(CORE_UNAVAILABLE / NO_ACTIVE_SESSION…)。
func (sh *Shell) failStart(ctx context.Context, ws *websocket.Conn, code string) {
	m, _ := proto.NewMsg(proto.TypeError, proto.ErrorPayload{Code: proto.CodeShellStartFailed, Message: code})
	b, _ := json.Marshal(m)
	wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
	defer cancel()
	_ = ws.Write(wctx, websocket.MessageText, b)
	_ = ws.Close(websocket.StatusInternalError, "shell start failed")
}

// shellProfile 解析 ShellParams.Shell 为白名单 profile 名;空 = 探测
// (pwsh 优先,缺则 powershell)。
func shellProfile(sh string) string {
	switch sh {
	case "pwsh":
		return "PWSH"
	case "powershell":
		return "POWERSHELL"
	case "cmd":
		return "CMD"
	case "bash":
		return "BASH"
	}
	if _, err := exec.LookPath("pwsh"); err == nil {
		return "PWSH"
	}
	return "POWERSHELL"
}

// asShellHostError 提取 *ShellHostError(拒绝码)。
func asShellHostError(err error, target **ShellHostError) bool {
	for err != nil {
		if se, ok := err.(*ShellHostError); ok {
			*target = se
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
