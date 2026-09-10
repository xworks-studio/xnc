package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	"xnc/proto"
)

// tunnelWsWriteTimeout caps each ws write from the local TCP side; RDP is
// chatty but never silent for a minute mid-exchange.
const tunnelWsWriteTimeout = 60 * time.Second

func newRdpCmd() *cobra.Command {
	var localPort int
	cmd := &cobra.Command{
		Use:   "rdp <node> [--local-port N]",
		Short: "Open Remote Desktop to a node via reverse tunnel (launches mstsc)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRdp(cmd, args[0], localPort)
		},
	}
	cmd.Flags().IntVar(&localPort, "local-port", 0, "local listen port (default: random)")
	return cmd
}

// runRdp drives `xnc rdp`: resolve node → POST /tunnel {target:"rdp"} → dial
// the session WS → listen on a local port (0 = random) → launch
// `mstsc /v:127.0.0.1:PORT` → pump the single connection bidirectionally
// (conn→ws binary, ws→conn binary). The only text frame on a tunnel is
// ERROR → exit 250. Ctrl+C or mstsc exit tears everything down and exits 0.
// mstsc reconnect attempts (multi-accept) are a Phase 5 refinement.
func runRdp(cmd *cobra.Command, node string, localPort int) error {
	cl, usage := dial(cmd, true)
	if usage != "" {
		return failUsage(cmd, usage)
	}
	ref, e := resolveNode(cl, node)
	if e != nil {
		return failAPI(cmd, e)
	}

	var created struct {
		SessionID    string `json:"sessionId"`
		Token        string `json:"token"`
		WebsocketURL string `json:"websocketUrl"`
	}
	if e := cl.Do("POST", "/api/nodes/"+url.PathEscape(ref.ID)+"/tunnel",
		map[string]any{"target": "rdp"}, &created); e != nil {
		return failAPI(cmd, e)
	}

	ws, err := dialSession(cl.Base, created.WebsocketURL)
	if err != nil {
		return failAPI(cmd, proto.Err(0, "NETWORK", err.Error()))
	}
	defer ws.CloseNow()
	ctx := cmd.Context()

	// local TCP listener (port 0 = random)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		return failAPI(cmd, proto.Err(0, "NETWORK", err.Error()))
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	fmt.Fprintf(os.Stderr, "[xnc] tunnel ready: 127.0.0.1:%d → %s (rdp) — Ctrl+C to close\n", port, ref.Name)

	// launch mstsc pointed at the local forward
	mstsc := exec.Command("mstsc", "/v:127.0.0.1:"+strconv.Itoa(port))
	if err := mstsc.Start(); err != nil {
		return failAPI(cmd, proto.Err(250, proto.CodeInternal, "mstsc launch: "+err.Error()))
	}
	defer mstsc.Process.Kill()

	// accept one connection (mstsc may reconnect; multi-accept is Phase 5)
	conn, err := ln.Accept()
	if err != nil {
		return nil
	}
	defer conn.Close()

	// bidirectional pump
	wsDone := make(chan struct{})
	go func() { // conn → ws
		defer close(wsDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				wctx, cancel := context.WithTimeout(ctx, tunnelWsWriteTimeout)
				we := ws.Write(wctx, websocket.MessageBinary, buf[:n])
				cancel()
				if we != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	// ws → conn
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			_ = conn.Close() // unblock the pump goroutine, then drain it
			<-wsDone
			return nil
		}
		if typ == websocket.MessageText { // the only tunnel text frame is ERROR
			var m proto.Message
			if json.Unmarshal(data, &m) == nil && m.Type == proto.TypeError {
				var ep proto.ErrorPayload
				_ = m.Decode(&ep)
				return failAPI(cmd, proto.Err(250, ep.Code, ep.Message))
			}
			continue
		}
		if _, err := conn.Write(data); err != nil {
			_ = conn.Close()
			<-wsDone
			return nil
		}
	}
}
