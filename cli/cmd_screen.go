// cmd_screen.go — `xnc screen`：
//
//	xnc screen <node> --snapshot out.jpg  单帧 JPEG 快照（agent 侧 helper
//	                                   --jpeg-single，经 WS 以单个 binary 帧回送）
//	xnc screen <node> --open           生成本地 HTML（canvas + WebCodecs H.264
//	                                   解码）并用默认浏览器打开实时预览
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	"xnc/proto"
)

// screenWSReadLimit：快照 JPEG 单帧可达数 MB，流式 I 帧亦超 1MiB——放大
// 会话读上限（dialSession 的默认 1MiB 不够）。
const screenWSReadLimit = 32 << 20

func newScreenCmd() *cobra.Command {
	var snapshotPath string
	var openBrowser bool
	cmd := &cobra.Command{
		Use:   "screen <node> [--snapshot <file.jpg>|--open]",
		Short: "Capture a node's screen: single JPEG snapshot or live browser preview",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if snapshotPath == "" && !openBrowser {
				return failUsage(cmd, "specify --snapshot <file.jpg> or --open")
			}
			if snapshotPath != "" && openBrowser {
				return failUsage(cmd, "--snapshot and --open are mutually exclusive")
			}
			if snapshotPath != "" {
				return runScreenSnapshot(cmd, args[0], snapshotPath)
			}
			return runScreenOpen(cmd, args[0])
		},
	}
	cmd.Flags().StringVar(&snapshotPath, "snapshot", "",
		"capture a single frame and write it to this JPEG file")
	cmd.Flags().BoolVar(&openBrowser, "open", false,
		"open a live screen preview in the default browser")
	return cmd
}

// screenSession 发起 screen 会话：POST /screen → 202 → 拨号会话 WS。
func screenSession(cl *Client, nodeID string, body map[string]any) (*websocket.Conn, string, error) {
	var created struct {
		SessionID    string `json:"sessionId"`
		WebsocketURL string `json:"websocketUrl"`
	}
	if e := cl.Do("POST", "/api/nodes/"+url.PathEscape(nodeID)+"/screen", body, &created); e != nil {
		return nil, "", e
	}
	ws, err := dialSession(cl.Base, created.WebsocketURL)
	if err != nil {
		return nil, "", err
	}
	ws.SetReadLimit(screenWSReadLimit)
	return ws, created.WebsocketURL, nil
}

// runScreenSnapshot 驱动 --snapshot：snapshot 会话 → 等首个 binary 帧（JPEG
// 字节）→ 写文件。ERROR text 帧映射为 API 错误。
func runScreenSnapshot(cmd *cobra.Command, nodeRef, outPath string) error {
	cl, usage := dial(cmd, true)
	if usage != "" {
		return failUsage(cmd, usage)
	}
	ref, e := resolveNode(cl, nodeRef)
	if e != nil {
		return failAPI(cmd, e)
	}

	ws, _, err := screenSession(cl, ref.ID, map[string]any{"snapshot": true})
	if err != nil {
		return failAPI(cmd, proto.Err(0, "NETWORK", err.Error()))
	}
	defer ws.CloseNow()

	ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
	defer cancel()
	for {
		typ, data, err := readWS(ctx, ws)
		if err != nil {
			return failAPI(cmd, proto.Err(0, "NETWORK", "snapshot session ended without a frame"))
		}
		switch typ {
		case "binary":
			if len(data) == 0 || data[0] != 0xFF || data[1] != 0xD8 {
				return failAPI(cmd, proto.Err(0, proto.CodeInternal, "received frame is not a JPEG"))
			}
			if err := os.WriteFile(outPath, data, 0o644); err != nil {
				return failUsage(cmd, "write "+outPath+": "+err.Error())
			}
			fmt.Fprintf(cmd.OutOrStderr(), "snapshot saved: %s (%d bytes)\n", outPath, len(data))
			return nil
		case "text":
			var m proto.Message
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			if m.Type == proto.TypeError {
				var ep proto.ErrorPayload
				if m.Decode(&ep) == nil {
					return failAPI(cmd, proto.Err(0, ep.Code, ep.Message))
				}
			}
		}
	}
}

// runScreenOpen 驱动 --open：普通流式会话 → 生成本地 HTML（内嵌绝对 WS URL）
// → 默认浏览器打开。
func runScreenOpen(cmd *cobra.Command, nodeRef string) error {
	cl, usage := dial(cmd, true)
	if usage != "" {
		return failUsage(cmd, usage)
	}
	ref, e := resolveNode(cl, nodeRef)
	if e != nil {
		return failAPI(cmd, e)
	}

	ws, wsPath, err := screenSession(cl, ref.ID, map[string]any{})
	if err != nil {
		return failAPI(cmd, proto.Err(0, "NETWORK", err.Error()))
	}
	defer ws.CloseNow() // HTML 页面自己拨号；本地这条仅探测连通性

	wsURL := absoluteWSURL(cl.Base, wsPath)
	html := screenPreviewHTML(ref.Name, wsURL)

	tmp, err := os.CreateTemp("", "xnc-screen-*.html")
	if err != nil {
		return failUsage(cmd, "create temp file: "+err.Error())
	}
	path := tmp.Name()
	if _, err := tmp.WriteString(html); err != nil {
		tmp.Close()
		os.Remove(path)
		return failUsage(cmd, "write temp file: "+err.Error())
	}
	tmp.Close()

	fmt.Fprintf(cmd.OutOrStderr(), "opening screen preview for %s (%s)\n", ref.Name, path)
	if err := openInBrowser(path); err != nil {
		fmt.Fprintf(cmd.OutOrStderr(), "open %s in a browser manually\n", path)
	}
	// HTML 独立拨号（token 已内嵌），CLI 随即退出。
	return nil
}

// absoluteWSURL 将 server base + 相对 websocketUrl 拼为绝对 ws/wss URL。
func absoluteWSURL(server, wsPath string) string {
	u := strings.Replace(strings.Replace(server, "https://", "wss://", 1),
		"http://", "ws://", 1)
	return strings.TrimRight(u, "/") + wsPath
}

// openInBrowser 用平台默认程序打开文件。
func openInBrowser(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	switch runtime.GOOS {
	case "windows":
		return exec.Command("cmd", "/c", "start", "", abs).Start()
	case "darwin":
		return exec.Command("open", abs).Start()
	default:
		return exec.Command("xdg-open", abs).Start()
	}
}

// screenPreviewHTML 生成自包含预览页：canvas + WebCodecs H.264 解码。
// wsURL 必须是绝对 ws/wss URL（含 token），经 %s 内嵌。
func screenPreviewHTML(nodeName, wsURL string) string {
	return `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>XNC Screen — ` + nodeName + `</title>
<style>
  body { margin: 0; background: #1a1a2e; overflow: hidden; }
  #bar { position: fixed; top: 0; left: 0; right: 0; padding: 6px 12px;
         color: #cbd5e1; font: 13px system-ui, sans-serif; background: rgba(0,0,0,.5);
         z-index: 1; }
  canvas { display: block; width: 100vw; height: 100vh; object-fit: contain; }
</style>
</head>
<body>
<div id="bar">XNC Screen — ` + nodeName + ` — <span id="st">connecting</span></div>
<canvas id="c"></canvas>
<script>
const ws = new WebSocket("` + wsURL + `");
ws.binaryType = "arraybuffer";
const canvas = document.getElementById("c");
const ctx = canvas.getContext("2d");
const st = document.getElementById("st");
let decoder = null;
ws.onmessage = (ev) => {
  if (typeof ev.data === "string") {
    const msg = JSON.parse(ev.data);
    if (msg.type === "SCREEN_BEGIN") {
      canvas.width = msg.payload.width;
      canvas.height = msg.payload.height;
      if (msg.payload.state) st.textContent = msg.payload.state;
    } else if (msg.type === "SCREEN_STATE") {
      st.textContent = msg.payload.state;
    } else if (msg.type === "ERROR") {
      st.textContent = "error: " + (msg.payload && msg.payload.message || "unknown");
    }
    return;
  }
  const data = new Uint8Array(ev.data);
  // start code (00 00 01) 后第一字节低 5 位 = NALU 类型。
  const nalType = data.length > 4 ? data[4] & 0x1F : 0;
  const isKey = nalType === 5 || nalType === 7 || nalType === 8;
  if (!decoder) {
    if (nalType !== 7) return; // 等 SPS 才能配置解码器
    decoder = new VideoDecoder({
      output: (frame) => {
        ctx.drawImage(frame, 0, 0, canvas.width || frame.displayWidth,
                      canvas.height || frame.displayHeight);
        frame.close();
      },
      error: (e) => { st.textContent = "decoder error: " + e.message; },
    });
    decoder.configure({ codec: "avc1.42E01E", optimizeForLatency: true });
  }
  decoder.decode(new EncodedVideoChunk({
    type: isKey ? "key" : "delta",
    timestamp: performance.now(),
    data: data,
  }));
};
ws.onclose = () => { st.textContent = "disconnected"; };
ws.onerror = () => { st.textContent = "connection error"; };
</script>
</body>
</html>
`
}
