// cmd_exec.go — xnc exec：统一远程执行入口（合并旧 exec + run）。
//
// 三种输入方式：
//
//	xnc exec <node> "command"                  ← 内联命令
//	xnc exec <node> --file <path>              ← 脚本文件
//	echo ... | xnc exec <node> -               ← stdin 管道
//
// Shell 选择（--shell auto|bash|pwsh|powershell|cmd）+ 环境变量（--env K=V）
// + 工作目录（--cwd）+ 超时（--timeout 秒）。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"xnc/proto"
)

// Session frame vocabulary for kind=exec (private to the kind; the server
// relays session frames opaque). Mirrors agent/session/exec.go.
const (
	typeExecResult = "EXEC_RESULT"
	frameStdout    = byte(0x01)
	frameStderr    = byte(0x02)
)

const execTimeoutDefault = 300
const execTimeoutMax = 86400
const execScriptMax = 256 * 1024

type execOutcome struct {
	node      string
	exitCode  *int
	timedOut  bool
	duration  int64
	gotResult bool
	stdout    strings.Builder
	stderr    strings.Builder
}

func newExecCmd() *cobra.Command {
	var (
		timeout int
		cwd     string
		shell   string
		env     []string
		file    string
	)
	cmd := &cobra.Command{
		Use:   "exec <node> [flags] <command...> | --file <path> | -",
		Short: "Run a command or script on a node",
		Long: `Run a command or script on a node.

Input modes (exactly one):
  positional args    inline command (joined with spaces)
  --file <path>      script file
  - (positional)     read script from stdin

Shells:
  --shell auto       detect best available (bash > pwsh > powershell)
  --shell bash       Git Bash / WSL (simplest quoting)
  --shell pwsh       PowerShell Core
  --shell powershell Windows PowerShell 5.1
  --shell cmd        cmd.exe

Examples:
  xnc exec node1 "hostname"
  xnc exec node1 --shell bash "grep error /var/log/syslog"
  xnc exec node1 --shell cmd "dir C:\\xnc"
  xnc exec node1 --file deploy.ps1
  cat script.sh | xnc exec node1 -
  xnc exec node1 --cwd C:\\xnc --env DEBUG=1 "tool.exe"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runExecUnified(c, args, timeout, cwd, shell, env, file)
		},
	}
	cmd.Flags().IntVar(&timeout, "timeout", execTimeoutDefault,
		"timeout in seconds (1-86400)")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory on the node")
	cmd.Flags().StringVar(&shell, "shell", "auto",
		"shell type: auto, bash, pwsh, powershell, cmd")
	cmd.Flags().StringArrayVar(&env, "env", nil,
		"environment variable KEY=VAL (repeatable)")
	cmd.Flags().StringVar(&file, "file", "",
		"script file path (alternative to positional command)")
	addJSONFlag(cmd)
	return cmd
}

// newRunCmd 保留 run 作为 exec --stdin 的别名（脚本兼容）。
func newRunCmd() *cobra.Command {
	var timeout int
	cmd := &cobra.Command{
		Use:   "run <node> (- | --file <path>) [--timeout N]",
		Short: "Run a script on a node (alias for: exec --stdin)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			file, _ := c.Flags().GetString("file")
			return runExecUnified(c, args, timeout, "", "auto", nil, file)
		},
	}
	cmd.Flags().IntVar(&timeout, "timeout", execTimeoutDefault,
		"script timeout in seconds (1-86400)")
	var runFile string
	cmd.Flags().StringVar(&runFile, "file", "", "script file path (or use - for stdin)")
	addJSONFlag(cmd)
	return cmd
}

// runExecUnified 统一执行入口。
func runExecUnified(cmd *cobra.Command, args []string, timeout int, cwd, shell string, env []string, file string) error {
	if e := checkExecTimeout(cmd, timeout); e != nil {
		return e
	}
	if shell != "auto" && shell != "bash" && shell != "pwsh" && shell != "powershell" && shell != "cmd" {
		return failUsage(cmd, "shell must be auto, bash, pwsh, powershell, or cmd")
	}

	// 解析输入源：--file > "-" stdin > 位置参数（dial 之前——本地校验
	// 优先，坏输入不产生网络请求）。
	var script, command string
	switch {
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return failUsage(cmd, "read "+file+": "+err.Error())
		}
		script = string(b)
	case len(args) > 1 && args[1] == "-":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return failUsage(cmd, "read stdin: "+err.Error())
		}
		script = string(b)
	default:
		command = strings.Join(args[1:], " ")
	}

	// 无输入源（既无命令也无脚本）→ 用法错误。
	if script == "" && command == "" {
		return failUsage(cmd, "provide a command, --file <path>, or - (stdin)")
	}

	// 脚本大小校验（同样在 dial 之前）。
	if len(script) > execScriptMax {
		return failUsage(cmd, "script exceeds 256KB; use upload + exec --file")
	}

	cl, usage := dial(cmd, true)
	if usage != "" {
		return failUsage(cmd, usage)
	}
	ref, e := resolveNode(cl, args[0])
	if e != nil {
		return failAPI(cmd, e)
	}

	body := map[string]any{"timeoutSec": timeout, "shell": shell}
	if command != "" {
		body["command"] = command
	}
	if script != "" {
		body["script"] = script
	}
	if cwd != "" {
		body["cwd"] = cwd
	}
	if len(env) > 0 {
		body["env"] = env
	}
	return runSession(cl, ref, body, cmd)
}

// checkExecTimeout: --timeout 越界（<1 或 >86400）→ 用法错误（exit 2）。
func checkExecTimeout(cmd *cobra.Command, timeout int) error {
	if timeout < 1 || timeout > execTimeoutMax {
		return failUsage(cmd, "timeout must be 1-86400 seconds")
	}
	return nil
}

// runSession drives the shared kind=exec flow:
// POST body → dial the session WS → stream binary frames live while
// accumulating them → capture the EXEC_RESULT terminal frame → print the
// envelope → map to the process exit code (passthrough).
func runSession(cl *Client, ref nodeRef, body map[string]any, cmd *cobra.Command) error {
	var created struct {
		SessionID    string `json:"sessionId"`
		Token        string `json:"token"`
		WebsocketURL string `json:"websocketUrl"`
	}
	if e := cl.Do("POST", "/api/nodes/"+url.PathEscape(ref.ID)+"/exec", body, &created); e != nil {
		return failAPI(cmd, e)
	}

	ws, err := dialSession(cl.Base, created.WebsocketURL)
	if err != nil {
		return failAPI(cmd, proto.Err(0, "NETWORK", err.Error()))
	}
	defer ws.CloseNow()

	label := ref.Name
	if label == "" {
		label = ref.ID
	}
	out := execOutcome{node: label}
	ctx := cmd.Context()
	for {
		kind, data, err := readWS(ctx, ws)
		if err != nil {
			break
		}
		switch kind {
		case "binary":
			if len(data) == 0 {
				continue
			}
			if data[0] == frameStdout {
				if !jsonOut(cmd) {
					os.Stdout.Write(data[1:])
				}
				out.stdout.Write(data[1:])
			} else {
				if !jsonOut(cmd) {
					os.Stderr.Write(data[1:])
				}
				out.stderr.Write(data[1:])
			}
		case "text":
			var m proto.Message
			if json.Unmarshal(data, &m) != nil || m.Type != typeExecResult {
				continue
			}
			var r proto.ExecResult
			if m.Decode(&r) == nil {
				out.exitCode, out.timedOut, out.duration = r.ExitCode, r.TimedOut, r.DurationMs
				out.gotResult = true
			}
		}
	}

	if !out.gotResult {
		e := proto.Err(0, "NETWORK", "session ended without result")
		if jsonOut(cmd) {
			PrintJSON(false, nil, e)
		} else {
			fmt.Fprintln(os.Stderr, "xnc: session ended without result")
		}
		return &exitError{code: exitNet}
	}

	if jsonOut(cmd) {
		ec := any(nil)
		if out.exitCode != nil {
			ec = *out.exitCode
		}
		PrintJSON(true, map[string]any{
			"node": out.node, "exitCode": ec, "stdout": out.stdout.String(),
			"stderr": out.stderr.String(), "durationMs": out.duration, "timedOut": out.timedOut,
		}, nil)
	}
	switch {
	case out.exitCode == nil:
		return &exitError{code: exitTimeout}
	case *out.exitCode == 0:
		return nil
	default:
		return &exitError{code: *out.exitCode}
	}
}
