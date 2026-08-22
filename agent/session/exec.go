// exec.go — 会话 kind=exec 的命令模式：子进程 stdout/stderr 以 1 字节前缀
// （0x01/0x02）的 binary 帧流式转发到会话 WS，退出后发终态 EXEC_RESULT；
// 超时与 SESSION_CLOSE（ctx 取消）都杀整个进程树。
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"xnc/agent/machineinfo"

	"github.com/coder/websocket"

	"xnc/proto"
)

const (
	stdoutPrefix = byte(0x01)
	stderrPrefix = byte(0x02)

	execWriteTimeout = 10 * time.Second

	// typeExecResult exec 会话 WS 的终态 text 帧类型。kind 私有词汇，不入
	// proto（design §3.2：会话面 text 类型按 kind 归属）。
	typeExecResult = "EXEC_RESULT"

	execDefaultTimeout = 300 * time.Second

	// execMaxScriptBytes agent 侧脚本兜底上限。server 已拦 256KB；此值
	// 只防绕过服务端校验的篡改路径直接在 agent 落盘超大文件（T7）。
	execMaxScriptBytes = 1024 * 1024
)

// sessionIDRe 临时脚本文件名（xnc-<sessionId>.ps1）中 sessionID 的白名单：
// UUID 形态（字母数字与连字符）。sessionID 来自服务端 SESSION_OPEN，凡带
// 路径分隔符、点等其它字符一律拒绝整会话，杜绝临时文件路径穿越（T6 评审
// 加固，T7 收口）。
var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// Exec 命令模式处理器。TmpDir 为空 = os.TempDir()（脚本模式临时文件目录，
// 测试注入点）。
type Exec struct {
	Log    *slog.Logger
	TmpDir string
}

func NewExec(log *slog.Logger) *Exec { return &Exec{Log: log} }

func (ex *Exec) logger() *slog.Logger {
	if ex.Log != nil {
		return ex.Log
	}
	return slog.Default()
}

// Handle 运行一条命令直至终态：pump 输出 → EXEC_RESULT → 正常关闭 WS。
// 返回即会话结束（引擎随后 CloseNow）。
func (ex *Exec) Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage) {
	var p proto.ExecParams
	if err := json.Unmarshal(params, &p); err != nil {
		ex.result(ctx, ws, nil, false, 0)
		return
	}
	// sessionID 会进入临时脚本路径：非白名单形态（如 "../../evil"）即拒绝
	// 整会话，路径穿越在落盘前终结。
	if !sessionIDRe.MatchString(sessionID) {
		ex.logger().Warn("exec refused malformed session id", "session", sessionID)
		ex.result(ctx, ws, nil, false, 0)
		return
	}
	if len(p.Script) > execMaxScriptBytes {
		// server 已拦 256KB；agent 兜底，防篡改路径直接落盘超大文件
		ex.result(ctx, ws, nil, false, 0)
		return
	}
	deadline := time.Duration(p.TimeoutSec) * time.Second
	if deadline <= 0 {
		deadline = execDefaultTimeout
	}
	start := time.Now()

	if p.Script != "" {
		if _, err := ex.writeScript(sessionID, p.Script); err != nil {
			ex.logger().Warn("exec write script failed", "session", sessionID, "err", err)
			ex.result(ctx, ws, nil, false, time.Since(start).Milliseconds())
			return
		}
		defer func() { _ = os.Remove(ex.scriptPath(sessionID)) }()
	}
	cmd, err := ex.buildCommand(p, sessionID)
	if err != nil {
		ex.result(ctx, ws, nil, false, time.Since(start).Milliseconds())
		return
	}
	if p.Cwd != "" {
		cmd.Dir = p.Cwd
	}
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		ex.logger().Warn("exec start failed", "session", sessionID, "err", err)
		ex.result(ctx, ws, nil, false, time.Since(start).Milliseconds())
		return
	}

	// StdoutPipe 语义：Wait 会关闭管道——必须先排空 pump 再 Wait，否则
	// 进程退出前的末段输出会被截断。
	var pumps sync.WaitGroup
	ex.pump(&pumps, ctx, ws, stdout, stdoutPrefix)
	ex.pump(&pumps, ctx, ws, stderr, stderrPrefix)
	waitCh := make(chan error, 1)
	go func() {
		pumps.Wait()
		waitCh <- cmd.Wait()
	}()

	timedOut := make(chan struct{}, 1)
	timer := time.AfterFunc(deadline, func() {
		timedOut <- struct{}{}
		killTree(cmd.Process.Pid)
	})
	defer timer.Stop()

	var exitCode *int
	timed := false
	select {
	case <-waitCh:
		if code := cmd.ProcessState.ExitCode(); code >= 0 {
			exitCode = &code
		}
	case <-ctx.Done():
		killTree(cmd.Process.Pid)
		<-waitCh
	case <-timedOut:
		timed = true
		killTree(cmd.Process.Pid)
		<-waitCh
	}
	ex.logger().Debug("exec finished", "session", sessionID, "timedOut", timed, "durationMs", time.Since(start).Milliseconds())
	ex.result(ctx, ws, exitCode, timed, time.Since(start).Milliseconds())
}

// pump 为一条输出流起一个转发 goroutine（stdout 与 stderr 各调一次）：
// 读到数据即包装 [prefix]+payload 写为 binary 帧；写失败（含 ctx 取消）或
// 流结束（EOF/管道随进程退出关闭）即退出，经 wg 汇报排空。
func (ex *Exec) pump(wg *sync.WaitGroup, ctx context.Context, ws *websocket.Conn, r io.Reader, prefix byte) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				frame := append([]byte{prefix}, buf[:n]...)
				wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
				werr := ws.Write(wctx, websocket.MessageBinary, frame)
				cancel()
				if werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
}

// result 发送终态 EXEC_RESULT 并以正常关闭码收线；写失败静默（会话已死）。
func (ex *Exec) result(ctx context.Context, ws *websocket.Conn, code *int, timed bool, ms int64) {
	msg, _ := proto.NewMsg(typeExecResult, proto.ExecResult{ExitCode: code, TimedOut: timed, DurationMs: ms})
	b, _ := json.Marshal(msg)
	wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
	defer cancel()
	_ = ws.Write(wctx, websocket.MessageText, b)
	_ = ws.Close(websocket.StatusNormalClosure, "")
}

// buildCommand 按请求的 shell 类型构建执行命令。
//
// shell 值：auto（探测最佳）、bash、pwsh、powershell、cmd。
// 命令模式内联执行（-c / -Command / cmd /c）；脚本模式执行已落盘文件。
// 环境变量经前缀注入（按 shell 语法）。
func (ex *Exec) buildCommand(p proto.ExecParams, sessionID string) (*exec.Cmd, error) {
	shell := p.Shell
	if shell == "" || shell == "auto" {
		shells := machineinfo.DetectShells()
		shell = shells[0]
	}

	// 环境变量前缀（命令模式时拼在命令前；脚本模式设到子进程 env）。
	prefix := envPrefix(shell, p.Env)

	var cmd *exec.Cmd
	scriptPath := ex.scriptPath(sessionID)

	switch shell {
	case "bash":
		exe := machineinfo.FindBash()
		if exe == "" {
			return nil, fmt.Errorf("bash not available on this node")
		}
		if p.Script != "" {
			cmd = exec.Command(exe, scriptPath)
		} else {
			cmd = exec.Command(exe, "-c", prefix+p.Command)
		}

	case "cmd":
		if p.Script != "" {
			cmd = exec.Command("cmd", "/c", prefix+scriptPath)
		} else {
			cmd = exec.Command("cmd", "/c", prefix+p.Command)
		}

	case "pwsh", "powershell":
		if p.Script != "" {
			// -ExecutionPolicy Bypass：默认 Restricted 策略会拒绝加载 .ps1 文件
			// （agent 以 LocalSystem 运行，本就是管理通道，策略在此非安全边界）。
			cmd = exec.Command(shell, "-NoLogo", "-NonInteractive", "-ExecutionPolicy", "Bypass",
				"-File", scriptPath)
		} else {
			cmd = exec.Command(shell, "-NoLogo", "-NonInteractive", "-Command", prefix+p.Command)
		}

	default:
		return nil, fmt.Errorf("unsupported shell %q (available: bash, pwsh, powershell, cmd)", shell)
	}

	// 脚本模式的环境变量直接设到子进程 env（比前缀注入更可靠）。
	if p.Script != "" && len(p.Env) > 0 {
		for _, kv := range p.Env {
			if k, v, ok := strings.Cut(kv, "="); ok {
				cmd.Env = append(cmd.Environ(), k+"="+v)
			}
		}
	}

	return cmd, nil
}

// envPrefix 按 shell 语法构建环境变量注入前缀。
func envPrefix(shell string, env []string) string {
	if len(env) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch shell {
		case "bash":
			sb.WriteString(fmt.Sprintf("export %s='%s'; ", k, v))
		case "cmd":
			sb.WriteString(fmt.Sprintf("set %s=%s&& ", k, v))
		default: // pwsh / powershell
			sb.WriteString(fmt.Sprintf("$env:%s='%s'; ", k, v))
		}
	}
	return sb.String()
}

func (ex *Exec) writeScript(sessionID, script string) (string, error) {
	path := ex.scriptPath(sessionID)
	return path, os.WriteFile(path, []byte(script), 0o600)
}

func (ex *Exec) scriptPath(sessionID string) string {
	dir := ex.TmpDir
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "xnc-"+sessionID+".ps1")
}
