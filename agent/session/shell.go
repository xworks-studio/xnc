// shell.go — 会话 kind=shell 的交互终端模式：Windows ConPTY 承载交互 shell，
// pty 输出以 binary 帧裸流转发，client 键入经 binary 帧写入 pty；text 帧仅
// 接受 SHELL_RESIZE。SHELL_BEGIN 先于任何输出帧；创建失败回 ERROR
// （SHELL_START_FAILED）后关连接；shell 退出 / 对端断开 / ctx 取消都触发
// KillAndClose（杀进程树 + 关 pty）。
package session

import (
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"runtime"
	"sync/atomic"

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

// lastStartedPID 仅供集成测试断言进程清理（生产无消费者）：startConPTY 成功
// 后立即写入最新 shell 子进程 PID。
var lastStartedPID atomic.Int64

// Shell 交互终端处理器。Log 为 nil 时用 slog.Default()。
type Shell struct{ Log *slog.Logger }

func NewShell(log *slog.Logger) *Shell { return &Shell{Log: log} }

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

	exe := p.Shell
	if exe == "" {
		exe = probeShell()
	}
	if runtime.GOOS != "windows" {
		sh.failStart(ctx, ws, "shell sessions require windows agent")
		return
	}
	if _, err := exec.LookPath(exe); err != nil {
		sh.failStart(ctx, ws, "shell not found: "+exe)
		return
	}

	// 提示符前缀：让每行提示符带上机器名（[HOSTNAME] PS C:\>）。
	// 经 -NoExit -Command 作为启动命令注入：不经过交互行编辑器，无回显
	// （比写入 pty 输入干净）；-Command 在 profile 之后执行，覆盖其 prompt。
	args := []string{"-NoLogo"}
	if exe == "pwsh" || exe == "powershell" {
		args = append(args, "-NoExit", "-Command",
			"function global:prompt { '[' + $env:COMPUTERNAME + '] PS ' + $executionContext.SessionState.Path.CurrentLocation + '> ' }")
	}
	pty, err := startConPTY(cols, rows, exe, args...)
	if err != nil {
		sh.logger().Warn("conpty start failed", "session", sessionID, "err", err)
		sh.failStart(ctx, ws, "conpty start failed")
		return
	}
	lastStartedPID.Store(int64(pty.pid))

	// SHELL_BEGIN 先于任何输出帧。
	if b, err := proto.NewMsg(typeShellBegin, proto.ShellBegin{Shell: exe}); err == nil {
		if jb, err2 := json.Marshal(b); err2 == nil {
			wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
			_ = ws.Write(wctx, websocket.MessageText, jb)
			cancel()
		}
	}

	done := make(chan struct{})
	go func() { // pty 输出 → ws binary
		defer close(done)
		buf := make([]byte, 32*1024)
		for {
			n, err := pty.outR.Read(buf)
			if n > 0 {
				wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
				if we := ws.Write(wctx, websocket.MessageBinary, buf[:n]); we != nil {
					cancel()
					return
				}
				cancel()
			}
			if err != nil {
				return
			}
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
				if _, err := pty.inW.Write(data); err != nil {
					return
				}
			case websocket.MessageText:
				var m proto.Message
				if json.Unmarshal(data, &m) != nil || m.Type != typeShellResize {
					continue
				}
				var r proto.ShellResize
				if m.Decode(&r) == nil && r.Cols > 0 && r.Cols <= shellMaxDim && r.Rows > 0 && r.Rows <= shellMaxDim {
					_ = pty.Resize(r.Cols, r.Rows)
				}
			}
		}
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- pty.Wait() }()
	select {
	case <-waitCh: // shell 退出（exit 命令）
	case <-done: // 输出流结束（pty 关闭）
	case <-gone: // 对端断开
	case <-ctx.Done(): // SESSION_CLOSE / 会话终止
	}
	pty.KillAndClose()
	_ = ws.Close(websocket.StatusNormalClosure, "")
}

// failStart 创建失败路径：ERROR{SHELL_START_FAILED} text 帧后以错误码关线。
func (sh *Shell) failStart(ctx context.Context, ws *websocket.Conn, msg string) {
	m, _ := proto.NewMsg(proto.TypeError, proto.ErrorPayload{Code: proto.CodeShellStartFailed, Message: msg})
	b, _ := json.Marshal(m)
	wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
	defer cancel()
	_ = ws.Write(wctx, websocket.MessageText, b)
	_ = ws.Close(websocket.StatusInternalError, "shell start failed")
}

// probeShell windows shell 探测：pwsh 优先，缺则 powershell。
func probeShell() string {
	if _, err := exec.LookPath("pwsh"); err == nil {
		return "pwsh"
	}
	return "powershell"
}
