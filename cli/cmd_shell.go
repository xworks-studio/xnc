package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"strconv"
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
		Short: "Open an interactive shell on a node (disconnect with ~. on an empty line)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShell(cmd, args, cols, rows)
		},
	}
	cmd.Flags().IntVar(&cols, "cols", 0,
		"initial terminal width (default: current terminal, else 120; disables auto-resize)")
	cmd.Flags().IntVar(&rows, "rows", 0,
		"initial terminal height (default: current terminal, else 30; disables auto-resize)")
	return cmd
}

// runShell drives `xnc shell`: TTY check → resolve node → POST /shell →
// dial the session WS → raw mode（拨号成功后才进 raw，错误信息在 cooked
// 模式干净打印）→ pump stdin（含 `~.` 本地断开转义）as binary frames,
// poll for resizes（显式 --cols/--rows 时禁用，避免尺寸回弹）, stream
// binary output to stdout, set the terminal title to the node name, and
// exit 0 when the peer closes（打印 disconnect 提示）or ERROR (exit 250).
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
	manualSize := false
	if flagCols > 0 {
		cols = flagCols
		manualSize = true
	}
	if flagRows > 0 {
		rows = flagRows
		manualSize = true
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

	ws, err := dialSession(cl.Base, created.WebsocketURL)
	if err != nil {
		return failAPI(cmd, proto.Err(0, "NETWORK", err.Error()))
	}
	defer ws.CloseNow()

	oldState, err := term.MakeRaw(fd)
	if err == nil {
		defer func() { _ = term.Restore(fd, oldState) }()
	}

	// 窗口标题带上机器名：滚屏后仍可见（OSC 0）。
	fmt.Printf("\x1b]0;xnc — %s\x07", ref.Name)
	defer fmt.Printf("\x1b]0;xnc\x07")

	ctx := cmd.Context()
	localClose := make(chan struct{}) // `~.` 或 stdin EOF 触发本地断开
	stdinDone := make(chan struct{})
	var debugDump *os.File // XNC_SHELL_DEBUG=<path> 时转储原始 stdin 字节（十六进制）
	if p := os.Getenv("XNC_SHELL_DEBUG"); p != "" {
		debugDump, _ = os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	}
	go func() { // stdin → ws binary，行首 `~.` 本地断开
		defer close(stdinDone)
		if debugDump != nil {
			defer debugDump.Close()
		}
		esc := newEscapeDetector()
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				if debugDump != nil {
					_, _ = debugDump.WriteString(fmt.Sprintf("% x\n", buf[:n]))
				}
				out, disconnect := esc.feed(buf[:n])
				if len(out) > 0 {
					wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
					we := ws.Write(wctx, websocket.MessageBinary, out)
					cancel()
					if we != nil {
						return
					}
				}
				if disconnect {
					close(localClose)
					return
				}
			}
			if err != nil {
				// stdin EOF：主动优雅关闭，让对端结束会话。
				_ = ws.Close(websocket.StatusNormalClosure, "stdin eof")
				return
			}
		}
	}()

	resizeDone := make(chan struct{})
	if !manualSize {
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
	} else {
		close(resizeDone)
	}

	// Main loop: ws → stdout.
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			if st := websocket.CloseStatus(err); st != -1 {
				fmt.Fprintf(os.Stderr, "\r\n[xnc] disconnected from %s (close %d)\r\n", ref.Name, st)
			} else {
				fmt.Fprintf(os.Stderr, "\r\n[xnc] disconnected from %s\r\n", ref.Name)
			}
			return nil // exit 命令 / 会话结束 / 本地断开 → 干净退出 0
		}
		select {
		case <-localClose:
			fmt.Fprintf(os.Stderr, "\r\n[xnc] disconnected from %s (~.)\r\n", ref.Name)
			return nil
		default:
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
					fmt.Fprintf(os.Stderr, "\r\n[xnc] connected to %s (%s) — disconnect with ~.\r\n", ref.Name, sb.Shell)
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

// escapeDetector 识别行首的断开序列（ssh 风格）：`~.` 或裸 `~`+回车。
// 输入有两层形态，都要处理：
//  1. 纯字节（终端未启用键盘增强协议）：`~` = 0x7E，全角变体 ～ ． 。；
//  2. win32 键盘增强协议（Windows Terminal 应远端 PSReadLine 请求启用）：
//     每个键是 `ESC [ vk;scan;char;down;... _` 序列——断开判定须在按键
//     事件层做（char=126/46/13），序列本身原样转发保真。
// 其余字节原样转发；状态跨 chunk 保持。
type escapeDetector struct {
	lineStart bool
	held      []byte // 行首悬置的 tilde（原始字节或整个按键序列），待裁决
	deferred  []byte // 悬置期间按序积压的事件（up/修饰键/其它序列），裁决时统一放行
	inCSI     bool
	csi       []byte // 累积中的 CSI 序列（含 ESC [）
}

func newEscapeDetector() *escapeDetector { return &escapeDetector{lineStart: true} }

var (
	tildeForms = [][]byte{
		{'~'},              // ASCII
		{0xEF, 0xBD, 0x9E}, // ～ U+FF5E（全角波浪）
	}
	dotForms = [][]byte{
		{0x2E},             // ASCII .
		{0xEF, 0xBC, 0x8E}, // ． U+FF0E（全角句点）
		{0xE3, 0x80, 0x82}, // 。 U+3002（CJK 句号）
	}
)

func bytesHasPrefix(b, p []byte) bool {
	if len(b) < len(p) {
		return false
	}
	for i := range p {
		if b[i] != p[i] {
			return false
		}
	}
	return true
}

func matchForm(b []byte, forms [][]byte) int { // 返回匹配长度，0 = 无
	for _, f := range forms {
		if bytesHasPrefix(b, f) {
			return len(f)
		}
	}
	return 0
}

// feed 返回（应转发的字节, 是否触发本地断开）。
func (d *escapeDetector) feed(in []byte) ([]byte, bool) {
	var out []byte
	i := 0
	for i < len(in) {
		if d.inCSI {
			b := in[i]
			i++
			d.csi = append(d.csi, b)
			if b >= 0x40 && b <= 0x7E { // 终止字节，序列完成
				seq := d.csi
				d.inCSI = false
				d.csi = nil
				fwd, esc := d.handleSeq(seq)
				out = append(out, fwd...)
				if esc {
					return out, true
				}
			}
			continue
		}
		if in[i] == 0x1b && i+1 < len(in) && in[i+1] == '[' {
			d.inCSI = true
			d.csi = []byte{0x1b, '['}
			i += 2
			continue
		}
		// 纯字节路径。
		b := in[i]
		if d.held != nil {
			held := d.held
			d.held = nil
			switch {
			case matchForm(in[i:], dotForms) > 0:
				return out, true
			case b == '\r' || b == '\n':
				return out, true // 裸 ~ + 回车
			case matchForm(in[i:], tildeForms) > 0:
				out = append(out, '~') // `~~` 输出单个字面 ~
				i += matchForm(in[i:], tildeForms)
				d.lineStart = false
				continue
			default:
				out = append(out, held...)
				out = append(out, b)
				d.lineStart = b == '\r' || b == '\n'
				i++
				continue
			}
		}
		if d.lineStart {
			if n := matchForm(in[i:], tildeForms); n > 0 {
				d.held = append([]byte(nil), in[i:i+n]...)
				i += n
				continue
			}
		}
		out = append(out, b)
		d.lineStart = b == '\r' || b == '\n'
		i++
	}
	return out, false
}

// handleSeq 裁决一个完整 CSI 序列。win32 键盘序列（`ESC [ vk;scan;char;down;.. _`）
// 按按键事件处理；其它 CSI（方向键等）原样转发。
// 悬置（held）期间到达的 up 事件/修饰键/其它序列按序积压（deferred），
// 由下一个 key-DOWN 统一裁决：断开则积压物放行、tilde 与触发键吞掉；
// 非触发键则 held+积压+当前按序全部放行——保证转发流不乱序。
func (d *escapeDetector) handleSeq(seq []byte) ([]byte, bool) {
	if len(seq) < 4 || seq[len(seq)-1] != '_' {
		return d.deferOrForward(seq), false // 非键盘序列
	}
	body := string(seq[2 : len(seq)-1]) // 去掉 ESC [ 与 _
	parts := strings.Split(body, ";")
	if len(parts) < 4 {
		return d.deferOrForward(seq), false
	}
	n := make([]int, len(parts))
	for j, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil {
			return d.deferOrForward(seq), false
		}
		n[j] = v
	}
	charCode, isDown := n[2], n[3] != 0

	if !isDown || charCode == 0 {
		// 抬起事件与纯修饰键（shift/ctrl/alt，char=0）：不改行状态、不裁决。
		return d.deferOrForward(seq), false
	}
	switch {
	case d.held != nil && (charCode == '.' || charCode == 0x0D):
		out := d.deferred // 断开：积压物放行，悬置 tilde 与本触发键吞掉
		d.held, d.deferred = nil, nil
		return out, true
	case d.held != nil: // 其它键：悬置+积压+当前按序放行（含 ~~ 语义：两 tilde 都进流）
		out := append(d.held, d.deferred...)
		out = append(out, seq...)
		d.held, d.deferred = nil, nil
		d.lineStart = false
		return out, false
	case d.lineStart && charCode == '~':
		d.held = append([]byte(nil), seq...)
		return nil, false
	case charCode == 0x0D:
		d.lineStart = true
		return seq, false
	default:
		d.lineStart = false
		return seq, false
	}
}

// deferOrForward：悬置期间积压，否则直接放行。
func (d *escapeDetector) deferOrForward(seq []byte) []byte {
	if d.held == nil {
		return seq
	}
	d.deferred = append(d.deferred, seq...)
	return nil
}
