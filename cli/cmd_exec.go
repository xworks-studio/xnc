package main

import (
	"encoding/json"
	"fmt"
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

// runExec: resolve node → POST exec → dial session WS → stream binary frames
// live while accumulating them → capture the EXEC_RESULT terminal frame →
// print the envelope → map to the process exit code (passthrough; 243 only
// when timed out per EXEC_RESULT; 245 NETWORK when the connection ended
// without a result frame).
//
// With --json the live stream and the final envelope interleave on stdout by
// design: the envelope is emitted after the stream, so line-based (jsonl)
// consumers are unaffected.
func runExec(cmd *cobra.Command, args []string, timeout int, cwd string) error {
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
				os.Stdout.Write(data[1:])
				out.stdout.Write(data[1:])
			} else {
				os.Stderr.Write(data[1:])
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

	// Envelope separation: if the live stream left stdout mid-line, start the
	// envelope on a fresh line so the trailing envelope line parses standalone.
	if jsonOut(cmd) && out.stdout.Len() > 0 && !strings.HasSuffix(out.stdout.String(), "\n") {
		os.Stdout.WriteString("\n")
	}

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
