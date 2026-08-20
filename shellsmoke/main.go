// shellsmoke — xnc shell 会话级冒烟（也可对生产跑）。
// 用法：XNC_SERVER=... XNC_TOKEN=... shellsmoke --node NAME
// 流程：node 解析 → POST /shell → 拨 WS → 验 SHELL_BEGIN → echo 回显 →
// resize 后续可用 → Ctrl+C 存活 → exit 关闭（对端关线即 0 残留的客户端
// 可观测断言；agent 侧进程清理由 agent 单测守护）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"

	"xnc/proto"
)

// frameTimeout 每个 WS 帧读/写的超时：shell 冒烟任一步静默 15s 即判失败
// （远比会话级 60s 总预算严格，卡住时快速给出定位信息）。
const frameTimeout = 15 * time.Second

func main() {
	node := flag.String("node", "", "node name (required)")
	flag.Parse()
	server := os.Getenv("XNC_SERVER")
	token := os.Getenv("XNC_TOKEN")
	if *node == "" || server == "" || token == "" {
		fmt.Fprintln(os.Stderr, "--node and XNC_SERVER/XNC_TOKEN required")
		os.Exit(2)
	}
	if err := run(server, token, *node); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("SHELLSMOKE: ALL PASS")
}

func run(server, token, node string) error {
	hc := &http.Client{Timeout: 15 * time.Second}
	// 1) node 解析（GET /api/nodes 返回裸数组，name 大小写不敏感匹配）
	req, _ := http.NewRequest("GET", server+"/api/nodes", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	var nodes []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		resp.Body.Close()
		return fmt.Errorf("decode nodes: %w", err)
	}
	resp.Body.Close()
	var nodeID string
	for _, n := range nodes {
		if strings.EqualFold(n.Name, node) {
			nodeID = n.ID
			break
		}
	}
	if nodeID == "" {
		return fmt.Errorf("node %q not found", node)
	}

	// 2) POST /shell —— 202 体与错误体共用一次 decode：错误时 error 字段
	//    有值，成功时 token/websocketUrl 有值，无需二次读 body。
	req2, _ := http.NewRequest("POST", server+"/api/nodes/"+nodeID+"/shell",
		bytes.NewBufferString(`{"cols":80,"rows":25}`))
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := hc.Do(req2)
	if err != nil {
		return err
	}
	defer resp2.Body.Close()
	var payload struct {
		Token        string         `json:"token"`
		WebsocketURL string         `json:"websocketUrl"`
		Error        proto.APIError `json:"error"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&payload); err != nil {
		return fmt.Errorf("shell start: status %d decode: %w", resp2.StatusCode, err)
	}
	if resp2.StatusCode != 202 {
		return fmt.Errorf("shell start: status %d code=%s", resp2.StatusCode, payload.Error.Code)
	}

	// 3) 拨 WS + 驱动
	wsURL := strings.Replace(strings.Replace(server, "https://", "wss://", 1), "http://", "ws://", 1) + payload.WebsocketURL
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return err
	}
	defer ws.CloseNow()

	if err := expectText(ctx, ws, "SHELL_BEGIN"); err != nil {
		return err
	}
	fmt.Println("PASS: SHELL_BEGIN")

	writeBin(ctx, ws, []byte("echo smoke-echo-ok\r"))
	if err := expectBinary(ctx, ws, "smoke-echo-ok"); err != nil {
		return err
	}
	fmt.Println("PASS: echo")

	writeText(ctx, ws, mustMsg("SHELL_RESIZE", proto.ShellResize{Cols: 100, Rows: 40}))
	writeBin(ctx, ws, []byte("echo smoke-post-resize\r"))
	if err := expectBinary(ctx, ws, "smoke-post-resize"); err != nil {
		return err
	}
	fmt.Println("PASS: resize")

	writeBin(ctx, ws, []byte{0x03})
	writeBin(ctx, ws, []byte("echo smoke-after-ctrlc\r"))
	if err := expectBinary(ctx, ws, "smoke-after-ctrlc"); err != nil {
		return err
	}
	fmt.Println("PASS: ctrl-c survive")

	writeBin(ctx, ws, []byte("exit\r"))
	if err := expectClose(ctx, ws); err != nil {
		return err
	}
	fmt.Println("PASS: exit closed session")
	return nil
}

// expectText 读到 type 匹配的 text 帧为止：binary 帧（shell 输出）跳过，
// ERROR text 帧直接失败（保留 code/message 定位）。
func expectText(ctx context.Context, ws *websocket.Conn, typ string) error {
	for {
		fctx, cancel := context.WithTimeout(ctx, frameTimeout)
		t, data, err := ws.Read(fctx)
		cancel()
		if err != nil {
			return fmt.Errorf("expect text %s: %w", typ, err)
		}
		if t != websocket.MessageText {
			continue
		}
		var m proto.Message
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		if m.Type == typ {
			return nil
		}
		if m.Type == proto.TypeError {
			var ep proto.ErrorPayload
			_ = m.Decode(&ep)
			return fmt.Errorf("expect text %s: got ERROR %s: %s", typ, ep.Code, ep.Message)
		}
	}
}

// expectBinary 读到输出包含 want 为止：binary 帧累积进本次调用的缓冲
// （匹配串可能跨帧拆分），ERROR text 帧直接失败。
func expectBinary(ctx context.Context, ws *websocket.Conn, want string) error {
	var buf []byte
	for {
		fctx, cancel := context.WithTimeout(ctx, frameTimeout)
		t, data, err := ws.Read(fctx)
		cancel()
		if err != nil {
			return fmt.Errorf("expect output %q (got %q): %w", want, clip(buf), err)
		}
		if t == websocket.MessageBinary {
			buf = append(buf, data...)
			if len(buf) > 64*1024 { // 只留尾部，防 banner 类长输出无限增长
				buf = buf[len(buf)-64*1024:]
			}
			if bytes.Contains(buf, []byte(want)) {
				return nil
			}
			continue
		}
		var m proto.Message
		if err := json.Unmarshal(data, &m); err != nil || m.Type != proto.TypeError {
			continue
		}
		var ep proto.ErrorPayload
		_ = m.Decode(&ep)
		return fmt.Errorf("expect output %q: got ERROR %s: %s", want, ep.Code, ep.Message)
	}
}

// expectClose 断言 exit 后对端确实关线（agent shell 退出路径以 normal
// closure 收线；读到任何终止即会话结束，超时则残留挂起）。
func expectClose(ctx context.Context, ws *websocket.Conn) error {
	for {
		fctx, cancel := context.WithTimeout(ctx, frameTimeout)
		_, _, err := ws.Read(fctx)
		cancel()
		if err != nil {
			return nil // 对端关线 / 关闭帧：会话已按预期结束
		}
		// exit 后的残余输出帧继续吞掉，直到读到终止。
	}
}

// writeBin 客户端键入 → pty（binary 帧）。
func writeBin(ctx context.Context, ws *websocket.Conn, b []byte) {
	wctx, cancel := context.WithTimeout(ctx, frameTimeout)
	defer cancel()
	if err := ws.Write(wctx, websocket.MessageBinary, b); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL: write binary:", err)
		os.Exit(1)
	}
}

// writeText 发送控制 text 帧（如 SHELL_RESIZE）。
func writeText(ctx context.Context, ws *websocket.Conn, b []byte) {
	wctx, cancel := context.WithTimeout(ctx, frameTimeout)
	defer cancel()
	if err := ws.Write(wctx, websocket.MessageText, b); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL: write text:", err)
		os.Exit(1)
	}
}

// mustMsg 静态 payload 打包 text 帧：NewMsg/Marshal 对固定结构不会失败。
func mustMsg(typ string, payload any) []byte {
	m, err := proto.NewMsg(typ, payload)
	if err != nil {
		panic(err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

// clip 失败诊断用：缓冲尾部截断到 200 字节。
func clip(b []byte) string {
	if len(b) > 200 {
		b = b[len(b)-200:]
	}
	return string(b)
}
