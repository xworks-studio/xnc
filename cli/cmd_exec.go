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

// execTimeoutMax 与服务端 [1, 86400] 上限一致；客户端先于任何网络请求本地校验。
const execTimeoutMax = 86400

// checkExecTimeout: --timeout 越界（<1 或 >86400）→ 用法错误（exit 2）。
// 在 dial 之前调用：坏值绝不产生网络流量，也不进入服务端 400 路径。
func checkExecTimeout(cmd *cobra.Command, timeout int) error {
	if timeout < 1 || timeout > execTimeoutMax {
		return failUsage(cmd, "timeout must be 1-86400 seconds")
	}
	return nil
}

// execScriptMax caps the inline script payload for `xnc run`: bigger scripts
// wait for the upload+exec flow (Phase 4).
const execScriptMax = 256 * 1024

// execOutcome accumulates a finished exec run for the --json envelope.
type execOutcome struct {
	node      string
	exitCode  *int
	timedOut  bool
	duration  int64
	gotResult bool // terminal EXEC_RESULT frame was received
	stdout    strings.Builder
	stderr    strings.Builder
}

func newExecCmd() *cobra.Command {
	var timeout int
	var cwd string
	cmd := &cobra.Command{
		Use:   "exec <node> [--timeout N] [--cwd PATH] [--] <command...>",
		Short: "Run a command on a node and stream its output",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExec(cmd, args, timeout, cwd)
		},
	}
	cmd.Flags().IntVar(&timeout, "timeout", execTimeoutDefault,
		"command timeout in seconds (1-86400)")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory on the node")
	addJSONFlag(cmd)
	return cmd
}

func newRunCmd() *cobra.Command {
	var timeout int
	var file string
	cmd := &cobra.Command{
		Use:   "run <node> (--file x.ps1 | -) [--timeout N]",
		Short: "Run a PowerShell script from a file or stdin on a node",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRun(cmd, args, timeout, file)
		},
	}
	cmd.Flags().IntVar(&timeout, "timeout", execTimeoutDefault,
		"script timeout in seconds (1-86400)")
	cmd.Flags().StringVar(&file, "file", "", "script file path, or - to read stdin")
	addJSONFlag(cmd)
	return cmd
}

// runExec: validate --timeout → resolve node → hand the command body to the
// shared session loop.
func runExec(cmd *cobra.Command, args []string, timeout int, cwd string) error {
	if e := checkExecTimeout(cmd, timeout); e != nil {
		return e
	}
	cl, usage := dial(cmd, true)
	if usage != "" {
		return failUsage(cmd, usage)
	}
	ref, e := resolveNode(cl, args[0])
	if e != nil {
		return failAPI(cmd, e)
	}
	command := strings.Join(args[1:], " ")

	body := map[string]any{"command": command, "timeoutSec": timeout}
	if cwd != "" {
		body["cwd"] = cwd
	}
	return runSession(cl, ref, body, cmd)
}

// runRun: `xnc run <node> (--file <path> | -) [--timeout N]`. The script
// payload is read locally (--file PATH, or "-" for stdin; the positional form
// `run n1 -` is accepted too), prechecked against the 256KB inline cap, and
// then rides the same exec session loop with body {script, timeoutSec}. Every
// local failure is a usage error (exit 2) checked before any request is sent.
func runRun(cmd *cobra.Command, args []string, timeout int, file string) error {
	if e := checkExecTimeout(cmd, timeout); e != nil {
		return e
	}
	src := file
	if src == "" && len(args) > 1 {
		src = args[1] // positional form: xnc run n1 -
	}
	var script []byte
	var err error
	switch {
	case src == "-":
		script, err = io.ReadAll(os.Stdin)
	case src != "":
		script, err = os.ReadFile(src)
	default:
		err = fmt.Errorf("--file or - required")
	}
	if err != nil {
		return failUsage(cmd, err.Error())
	}
	if len(script) > execScriptMax {
		return failUsage(cmd, "script exceeds 256KB; upload+exec arrives in Phase 4")
	}

	cl, usage := dial(cmd, true)
	if usage != "" {
		return failUsage(cmd, usage)
	}
	ref, e := resolveNode(cl, args[0])
	if e != nil {
		return failAPI(cmd, e)
	}
	return runSession(cl, ref, map[string]any{
		"script": string(script), "timeoutSec": timeout}, cmd)
}

// runSession drives the shared kind=exec flow behind `xnc exec` and `xnc run`:
// POST body to /api/nodes/{id}/exec → dial the session WS → stream binary
// frames live while accumulating them → capture the EXEC_RESULT terminal
// frame → print the envelope → map to the process exit code (passthrough; 243
// only when timed out per EXEC_RESULT; 245 NETWORK when the connection ended
// without a result frame).
//
// With --json the live stream and the final envelope interleave on stdout by
// design: the envelope is emitted after the stream, so line-based (jsonl)
// consumers are unaffected.
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
		label = ref.ID // UUID arg: no name resolution round trip happened
	}
	out := execOutcome{node: label}
	ctx := cmd.Context()
	for {
		kind, data, err := readWS(ctx, ws)
		if err != nil {
			break // connection closed (normal path: EXEC_RESULT then agent close)
		}
		switch kind {
		case "binary":
			if len(data) == 0 {
				continue
			}
			if data[0] == frameStdout {
				if !jsonOut(cmd) { // --json：静默积累，不实时打印（管道消费友好）
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

	// --json mode is now silent (no streaming output on stdout), so the
	// envelope separator is no longer needed — stdout contains only the envelope.

	// Disconnect before the terminal frame is a NETWORK failure (245), not a
	// timeout: 243 is reserved for EXEC_RESULT.TimedOut (cli.md contract).
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
		// timed out (or result with no exit code: killed / refused start)
		return &exitError{code: exitTimeout}
	case *out.exitCode == 0:
		return nil
	default:
		return &exitError{code: *out.exitCode} // passthrough
	}
}
