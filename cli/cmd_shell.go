package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"xnc/proto"
)

// Fallback terminal size when the real one cannot be queried (must match the
// server's shellDefaultCols/Rows).
const (
	shellFallbackCols = 120
	shellFallbackRows = 30
	// shellResizePoll: ConPTY has no resize event on the client side, so the
	// terminal size is polled and SHELL_RESIZE is sent on change.
	shellResizePoll = 500 * time.Millisecond
)

func newShellCmd() *cobra.Command {
	var cols, rows int
	cmd := &cobra.Command{
		Use:   "shell <node> [--cols N] [--rows N]",
		Short: "Open an interactive shell on a node",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShell(cmd, args, cols, rows)
		},
	}
	cmd.Flags().IntVar(&cols, "cols", 0,
		"initial terminal width (default: current terminal, else 120)")
	cmd.Flags().IntVar(&rows, "rows", 0,
		"initial terminal height (default: current terminal, else 30)")
	return cmd
}

// runShell drives `xnc shell`: TTY check (raw mode is meaningless without
// one, so non-TTY is a usage error before any traffic) → resolve node →
// POST /shell with the initial size → raw mode → dial the session WS →
// pump stdin as binary frames, poll for resizes, and stream binary output
// to stdout until the peer closes (exit 0) or sends ERROR (exit 250).
func runShell(cmd *cobra.Command, args []string, flagCols, flagRows int) error {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return failUsage(cmd, "xnc shell requires an interactive terminal; use xnc exec for scripting")
	}

	cl, usage := dial(cmd, true)
	if usage != "" {
		return failUsage(cmd, usage)
	}
	ref, e := resolveNode(cl, args[0])
	if e != nil {
		return failAPI(cmd, e)
	}

	cols, rows, err := term.GetSize(fd)
	if err != nil || cols < 1 || rows < 1 {
		cols, rows = shellFallbackCols, shellFallbackRows
	}
	if flagCols > 0 {
		cols = flagCols
	}
	if flagRows > 0 {
		rows = flagRows
	}

	var created struct {
		SessionID    string `json:"sessionId"`
		Token        string `json:"token"`
		WebsocketURL string `json:"websocketUrl"`
	}
	if e := cl.Do("POST", "/api/nodes/"+url.PathEscape(ref.ID)+"/shell",
		map[string]any{"cols": cols, "rows": rows}, &created); e != nil {
		return failAPI(cmd, e)
	}

	oldState, err := term.MakeRaw(fd)
	if err == nil {
		defer func() { _ = term.Restore(fd, oldState) }()
	}

	ws, err := dialSession(cl.Base, created.WebsocketURL)
	if err != nil {
		return failAPI(cmd, proto.Err(0, "NETWORK", err.Error()))
	}
	defer ws.CloseNow()

	ctx := cmd.Context()
	stdinDone := make(chan struct{})
	go func() { // stdin → ws binary
		defer close(stdinDone)
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				we := ws.Write(wctx, websocket.MessageBinary, buf[:n])
				cancel()
				if we != nil {
					return
				}
			}
			if err != nil {
				return // EOF (Ctrl+D/Z) → input side ends
			}
		}
	}()

	resizeDone := make(chan struct{})
	go func() { // size polling → SHELL_RESIZE text frames
		defer close(resizeDone)
		lastCols, lastRows := cols, rows
		tick := time.NewTicker(shellResizePoll)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				c, r, err := term.GetSize(fd)
				if err != nil || (c == lastCols && r == lastRows) {
					continue
				}
				lastCols, lastRows = c, r
				m, _ := proto.NewMsg("SHELL_RESIZE", proto.ShellResize{Cols: c, Rows: r})
				b, _ := json.Marshal(m)
				wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_ = ws.Write(wctx, websocket.MessageText, b)
				cancel()
			}
		}
	}()

	// Main loop: ws → stdout.
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return nil // peer closed (exit command / session end) → clean exit 0
		}
		switch typ {
		case websocket.MessageBinary:
			_, _ = os.Stdout.Write(data)
		case websocket.MessageText:
			var m proto.Message
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			switch m.Type {
			case "SHELL_BEGIN":
				var sb proto.ShellBegin
				if m.Decode(&sb) == nil {
					fmt.Fprintf(os.Stderr, "\r\n[xnc] connected: %s\r\n", sb.Shell)
				}
			case proto.TypeError:
				var ep proto.ErrorPayload
				if m.Decode(&ep) == nil {
					fmt.Fprintf(os.Stderr, "\r\n[xnc] error: %s: %s\r\n", ep.Code, ep.Message)
					return &exitError{code: exitInternal} // 250
				}
			}
		}
	}
}
