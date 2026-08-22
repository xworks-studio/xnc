package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	"xnc/proto"
)

// fileMaxBytes mirrors the server's upload cap (fileMaxBytes in the api
// package): rejected locally before any traffic, like exec's timeout check.
const fileMaxBytes = 256 * 1024 * 1024

func newUploadCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "upload <node> <local> <remote>",
		Aliases: []string{"put"},
		Short:   "Upload a file to a node (alias: put)",
		Args:    cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFileTransfer(cmd, args[0], "upload", args[1], args[2])
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func newDownloadCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "download <node> <remote> <local>",
		Aliases: []string{"get"},
		Short:   "Download a file from a node (alias: get)",
		Args:    cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFileTransfer(cmd, args[0], "download", args[2], args[1])
		},
	}
	addJSONFlag(cmd)
	return cmd
}

// runFileTransfer drives `xnc upload` / `xnc download`: resolve node → POST
// the session request (upload pre-reads the local file for size+sha256 and
// caps at 256MB) → dial the session WS → upload streams the local file as
// 64KB binary chunks / download accumulates binary frames → the terminal
// FILE_RESULT or FILE_ERROR frame ends the loop → download writes the file
// and re-verifies the sha256 locally (the agent's hash is never trusted
// blindly: ok=false or a hash mismatch exits 246, FILE_NOT_FOUND 244).
func runFileTransfer(cmd *cobra.Command, node, direction, local, remote string) error {
	cl, usage := dial(cmd, true)
	if usage != "" {
		return failUsage(cmd, usage)
	}
	ref, e := resolveNode(cl, node)
	if e != nil {
		return failAPI(cmd, e)
	}
	label := ref.Name
	if label == "" {
		label = ref.ID // UUID arg: no name resolution round trip happened
	}

	body := map[string]any{"direction": direction, "path": remote}
	if direction == "upload" {
		data, err := os.ReadFile(local)
		if err != nil {
			return failAPI(cmd, proto.Err(2, "USAGE", err.Error()))
		}
		if len(data) > fileMaxBytes {
			return failAPI(cmd, proto.Err(2, "USAGE", "file exceeds 256MB"))
		}
		h := sha256.Sum256(data)
		body["size"] = len(data)
		body["sha256"] = hex.EncodeToString(h[:])
	}

	endpoint := "files/upload"
	if direction == "download" {
		endpoint = "files/download"
	}
	var created struct {
		SessionID    string `json:"sessionId"`
		Token        string `json:"token"`
		WebsocketURL string `json:"websocketUrl"`
	}
	if e := cl.Do("POST", "/api/nodes/"+url.PathEscape(ref.ID)+"/"+endpoint, body, &created); e != nil {
		return failAPI(cmd, e)
	}

	ws, err := dialSession(cl.Base, created.WebsocketURL)
	if err != nil {
		return failAPI(cmd, proto.Err(0, "NETWORK", err.Error()))
	}
	defer ws.CloseNow()
	ctx := cmd.Context()
	start := time.Now()

	if direction == "upload" {
		go func() { // send the local file as 64KB binary chunks with progress
			data, _ := os.ReadFile(local)
			total := len(data)
			for off := 0; off < total; off += 64 * 1024 {
				end := off + 64*1024
				if end > total {
					end = total
				}
				wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				if err := ws.Write(wctx, websocket.MessageBinary, data[off:end]); err != nil {
					cancel()
					return
				}
				cancel()
				if !jsonOut(cmd) { // progress indicator (non-JSON mode only)
					fmt.Fprintf(os.Stderr, "\ruploading: %d/%d KB (%d%%)",
						end/1024, total/1024, end*100/total)
				}
			}
			if !jsonOut(cmd) {
				fmt.Fprintf(os.Stderr, "\r%42s\r", "") // clear progress line
			}
		}()
	}

	// Main loop: accumulate binary frames (download) until a terminal
	// FILE_RESULT / FILE_ERROR text frame (labeled break, no goto).
	var downloaded []byte
	var result *proto.FileResult
	var fileErr *proto.FileError
loop:
	for {
		kind, data, err := readWS(ctx, ws)
		if err != nil {
			break loop
		}
		switch kind {
		case "binary":
			if direction == "download" {
				downloaded = append(downloaded, data...)
				if !jsonOut(cmd) {
					fmt.Fprintf(os.Stderr, "\rdownloading: %d KB", len(downloaded)/1024)
				}
			}
		case "text":
			var m proto.Message
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			switch m.Type {
			case "FILE_BEGIN":
				// data stream start marker (no action)
			case "FILE_RESULT":
				var fr proto.FileResult
				if m.Decode(&fr) == nil {
					result = &fr
					break loop
				}
			case "FILE_ERROR":
				var fe proto.FileError
				if m.Decode(&fe) == nil {
					fileErr = &fe
					break loop
				}
			}
		}
	}
	if fileErr != nil {
		return fileErrExit(cmd, fileErr)
	}
	if result == nil {
		return failAPI(cmd, proto.Err(0, "NETWORK", "session ended without result"))
	}
	if !jsonOut(cmd) && direction == "download" {
		fmt.Fprintf(os.Stderr, "\r%42s\r", "") // clear progress line
	}
	if !result.Ok { // the writing side already failed its own verify
		return failAPI(cmd, proto.Err(246, proto.CodeHashMismatch, "sha256 mismatch"))
	}
	if direction == "download" {
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return failAPI(cmd, proto.Err(250, proto.CodeInternal, err.Error()))
		}
		if err := os.WriteFile(local, downloaded, 0o600); err != nil {
			return failAPI(cmd, proto.Err(250, proto.CodeInternal, err.Error()))
		}
		// client-side verify: recompute the hash of what actually arrived
		chk := sha256.Sum256(downloaded)
		if hex.EncodeToString(chk[:]) != result.Sha256 {
			_ = os.Remove(local)
			return failAPI(cmd, proto.Err(246, proto.CodeHashMismatch, "sha256 mismatch"))
		}
	}
	if jsonOut(cmd) {
		PrintJSON(true, map[string]any{
			"node": label, "bytes": result.Bytes, "sha256": result.Sha256,
			"ok": result.Ok, "durationMs": time.Since(start).Milliseconds(),
		}, nil)
		return nil
	}
	fmt.Printf("%s %s: %d bytes, sha256 %s (ok=%v)\n", direction, label, result.Bytes, result.Sha256, result.Ok)
	return nil
}

// fileErrExit maps an agent FILE_ERROR frame to the stable exit codes:
// FILE_NOT_FOUND→244, FILE_TOO_LARGE/HASH_MISMATCH→246, else 250.
func fileErrExit(cmd *cobra.Command, fe *proto.FileError) error {
	switch fe.Code {
	case proto.CodeFileNotFound:
		return failAPI(cmd, proto.Err(244, fe.Code, "file not found"))
	case proto.CodeFileTooLarge, proto.CodeHashMismatch:
		return failAPI(cmd, proto.Err(246, fe.Code, fe.Code))
	default:
		return failAPI(cmd, proto.Err(250, fe.Code, fe.Code))
	}
}
