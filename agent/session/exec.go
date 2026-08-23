// exec.go — 会话 kind=exec 的命令模式(M2-Slice2 Task 4 起经 xnc-core
// 创建:xnc-shell oneshot 模式,令牌语义 spec §8.4——默认用户令牌,
// System=true 显式 SYSTEM):stdout/stderr 以 1 字节前缀(0x01/0x02)的
// binary 帧流式转发到会话 WS,退出后发终态 EXEC_RESULT;超时与
// SESSION_CLOSE(ctx 取消)都杀整个进程树(0x0126 + 核心 KillShell)。
// 旧直连 spawn 路径已移除(ledger 裁决:无双路径);core 不可达 →
// EXEC_RESULT.Code=CORE_UNAVAILABLE(dev 拓扑文档化行为)。
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/coder/websocket"

	"xnc/agent/machineinfo"
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

// Exec 命令模式处理器。Host 为 nil 时用 DefaultShellHost(dev 环境变量);
// 测试注入 fake。TmpDir 为空 = os.TempDir()（脚本模式临时文件目录，
// 测试注入点）。
type Exec struct {
	Log    *slog.Logger
	Host   ShellHost
	TmpDir string
}

func NewExec(log *slog.Logger) *Exec { return &Exec{Log: log, Host: DefaultShellHost(log)} }

func (ex *Exec) logger() *slog.Logger {
	if ex.Log != nil {
		return ex.Log
	}
	return slog.Default()
}

func (ex *Exec) host() ShellHost {
	if ex.Host != nil {
		return ex.Host
	}
	return nil // 无凭据:创建即 CORE_UNAVAILABLE
}

// Handle 运行一条命令直至终态：pump 输出 → EXEC_RESULT → 正常关闭 WS。
// 返回即会话结束（引擎随后 CloseNow）。
func (ex *Exec) Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage) {
	var p proto.ExecParams
	if err := json.Unmarshal(params, &p); err != nil {
		ex.result(ctx, ws, nil, false, 0, "")
		return
	}
	// sessionID 会进入临时脚本路径：非白名单形态（如 "../../evil"）即拒绝
	// 整会话，路径穿越在落盘前终结。
	if !sessionIDRe.MatchString(sessionID) {
		ex.logger().Warn("exec refused malformed session id", "session", sessionID)
		ex.result(ctx, ws, nil, false, 0, "")
		return
	}
	if len(p.Script) > execMaxScriptBytes {
		// server 已拦 256KB；agent 兜底，防篡改路径直接落盘超大文件
		ex.result(ctx, ws, nil, false, 0, "")
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
			ex.result(ctx, ws, nil, false, time.Since(start).Milliseconds(), "")
			return
		}
		defer func() { _ = os.Remove(ex.scriptPath(sessionID)) }()
	}
	command, profile, err := buildExecCommand(p, ex.scriptPath(sessionID))
	if err != nil {
		ex.logger().Warn("exec build command failed", "session", sessionID, "err", err)
		ex.result(ctx, ws, nil, false, time.Since(start).Milliseconds(), "BAD_PAYLOAD")
		return
	}

	host := ex.host()
	if host == nil {
		ex.result(ctx, ws, nil, false, time.Since(start).Milliseconds(), CodeCoreUnavailable)
		return
	}
	proc, err := host.CreateShell(ShellSpec{
		System:     p.System,
		Profile:    profile,
		Cwd:        p.Cwd,
		Env:        p.Env,
		Command:    command,
		TimeoutSec: int(deadline / time.Second),
	})
	if err != nil {
		code := CodeCoreUnavailable
		var she *ShellHostError
		if errors.As(err, &she) {
			code = she.Code
		}
		ex.logger().Warn("exec create shell rejected", "session", sessionID, "code", code)
		ex.result(ctx, ws, nil, false, time.Since(start).Milliseconds(), code)
		return
	}

	// 输出泵:shellpipe stream → 会话 WS binary 帧。
	wrote := make(chan struct{})
	go func() {
		defer close(wrote)
		for d := range proc.Stream() {
			prefix := stdoutPrefix
			if d.Stderr {
				prefix = stderrPrefix
			}
			frame := append([]byte{prefix}, d.Bytes...)
			wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
			werr := ws.Write(wctx, websocket.MessageBinary, frame)
			cancel()
			if werr != nil {
				return
			}
		}
	}()

	timedOut := make(chan struct{}, 1)
	timer := time.AfterFunc(deadline, func() {
		// 非阻塞投递:xnc-shell 自带同参超时(shellhost 杀树),本定时
		// 器只驱动 TimedOut 上报;杀树统一走下方 select 的 Kill 路径,
		// 保证单次 Kill(重复 Kill 在对端已终结时会阻塞写)。
		select {
		case timedOut <- struct{}{}:
		default:
		}
	})
	defer timer.Stop()

	var exitCode *int
	timed := false
	exit := proc.Exit()
	select {
	case code := <-exit:
		// shellhost 侧超时先杀时,EXIT 与本地定时器竞争:以 timed 位
		// 为准(超时终态 ExitCode=null,镜像旧语义)。
		select {
		case <-timedOut:
			timed = true
		default:
		}
		if !timed && code < 0x80000000 {
			c := int(code)
			exitCode = &c
		}
	case <-ctx.Done():
		_ = proc.Kill()
		<-exit
	case <-timedOut:
		timed = true
		_ = proc.Kill() // 0x0126 杀树 + 核心 KillShell 兜底
		<-exit
	}
	// 排空输出泵再发终态(镜像旧 pumps.Wait 语义:末段输出不因终态帧
	// 提前而截断;连接已死时由写时限兜底)。
	select {
	case <-wrote:
	case <-time.After(execWriteTimeout):
	}
	_ = proc.Close()
	ex.logger().Debug("exec finished", "session", sessionID, "timedOut", timed, "durationMs", time.Since(start).Milliseconds())
	ex.result(ctx, ws, exitCode, timed, time.Since(start).Milliseconds(), "")
}

// result 发送终态 EXEC_RESULT 并以正常关闭码收线；写失败静默（会话已死）。
// code 非 "" = 稳定拒绝码(CORE_UNAVAILABLE / NO_ACTIVE_SESSION…)透传。
func (ex *Exec) result(ctx context.Context, ws *websocket.Conn, code *int, timed bool, ms int64, stable string) {
	msg, _ := proto.NewMsg(typeExecResult, proto.ExecResult{ExitCode: code, TimedOut: timed, DurationMs: ms, Code: stable})
	b, _ := json.Marshal(msg)
	wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
	defer cancel()
	_ = ws.Write(wctx, websocket.MessageText, b)
	_ = ws.Close(websocket.StatusNormalClosure, "")
}

// buildExecCommand 把 ExecParams 解析为 (oneshot command, profile 白名
// 名)。auto 由 agent 侧按 machineinfo 探测序解析为具体 profile(core 只
// 认枚举,不接收路径,spec §8.2)。脚本模式命令 = 临时文件调用串
// (pwsh/powershell: `& '<path>'`;bash: 前斜杠路径;cmd: 路径原样——
// 与旧直连行为一致)。
func buildExecCommand(p proto.ExecParams, scriptPath string) (command, profile string, err error) {
	shell := p.Shell
	if shell == "" || shell == "auto" {
		shells := machineinfo.DetectShells()
		shell = shells[0]
	}
	switch shell {
	case "bash":
		profile = "BASH"
		if p.Script != "" {
			command = "'" + filepath.ToSlash(scriptPath) + "'"
		} else {
			command = p.Command
		}
	case "cmd":
		profile = "CMD"
		if p.Script != "" {
			command = scriptPath
		} else {
			command = p.Command
		}
	case "pwsh", "powershell":
		profile = "POWERSHELL"
		if shell == "pwsh" {
			profile = "PWSH"
		}
		if p.Script != "" {
			command = "& '" + scriptPath + "'"
		} else {
			command = p.Command
		}
	default:
		return "", "", fmt.Errorf("unsupported shell %q (available: bash, pwsh, powershell, cmd)", shell)
	}
	if p.Script == "" && p.Command == "" {
		return "", "", fmt.Errorf("command or script required")
	}
	return command, profile, nil
}

func (ex *Exec) writeScript(sessionID, script string) (string, error) {
	path := ex.scriptPath(sessionID)
	// 0o644:shell 经用户令牌运行,须可读(agent SYSTEM 落盘,0644 让
	// 用户 token 侧的 xnc-shell 能读取)。
	return path, os.WriteFile(path, []byte(script), 0o644)
}

func (ex *Exec) scriptPath(sessionID string) string {
	dir := ex.TmpDir
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "xnc-"+sessionID+".ps1")
}
