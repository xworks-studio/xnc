//go:build windows

package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// runShell 起 httptest WS 让 Shell 作为客户端拨入；返回服务侧连接与 Handle
// 返回（清理已落定）时关闭的 done 信号。拨号错误经 errCh 回报（goroutine
// 内不 require）。
func runShell(t *testing.T, params proto.ShellParams) (*websocket.Conn, <-chan struct{}) {
	t.Helper()
	up := make(chan *websocket.Conn, 1)
	errCh := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		up <- c
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	raw, _ := json.Marshal(params)
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, _, err := websocket.Dial(context.Background(), "ws"+srv.URL[4:], nil)
		if err != nil {
			errCh <- err
			return
		}
		NewShell(testLogger()).Handle(context.Background(), c, "sess-shell", raw)
	}()
	select {
	case c := <-up:
		// LIFO：先于 srv.Close 关闭服务侧连接，解除 Shell 关闭握手与
		// srv.Close 等待 handler 退出之间的互等。
		t.Cleanup(func() { c.CloseNow() })
		return c, done
	case err := <-errCh:
		t.Fatalf("shell dial failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no ws")
	}
	return nil, done
}

// collectUntil 从 ws 读帧直到 match 返回 true（跳过非 binary 或不匹配的帧），
// 超时 fail。返回匹配帧原文。
func collectUntil(t *testing.T, ws *websocket.Conn, match func(kind string, data []byte) bool, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			t.Fatal("collectUntil: timeout waiting for match")
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		typ, data, err := ws.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		kind := "binary"
		if typ == websocket.MessageText {
			kind = "text"
		}
		if match(kind, data) {
			return data
		}
	}
}

// writeBin 发送 binary 帧（client 键入 / 原始 pty 输入字节）。
func writeBin(t *testing.T, ws *websocket.Conn, b []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, ws.Write(ctx, websocket.MessageBinary, b))
}

// writeText 发送 text 帧（控制消息，如 SHELL_RESIZE）。
func writeText(t *testing.T, ws *websocket.Conn, b []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, ws.Write(ctx, websocket.MessageText, b))
}

// mustShellMsg 构造 kind 私有 text 帧原文（proto.NewMsg → marshal）。
func mustShellMsg(t *testing.T, typ string, payload any) []byte {
	t.Helper()
	m, err := proto.NewMsg(typ, payload)
	require.NoError(t, err)
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

// processAlive 经 tasklist 检查 pid 是否仍在进程表。
func processAlive(t *testing.T, pid int64) bool {
	t.Helper()
	if pid == 0 {
		return false
	}
	pidStr := strconv.FormatInt(pid, 10)
	out, err := exec.Command("tasklist", "/FI", "PID eq "+pidStr).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), pidStr)
}

func TestShellEchoResizeCtrlC(t *testing.T) {
	ws, done := runShell(t, proto.ShellParams{Cols: 80, Rows: 25})

	// 1) SHELL_BEGIN 先行，报告实际 shell
	begin := collectUntil(t, ws, func(k string, d []byte) bool {
		if k != "text" {
			return false
		}
		var m proto.Message
		return json.Unmarshal(d, &m) == nil && m.Type == "SHELL_BEGIN"
	}, 10*time.Second)
	var sb proto.ShellBegin
	var m proto.Message
	require.NoError(t, json.Unmarshal(begin, &m))
	require.NoError(t, m.Decode(&sb))
	assert.NotEmpty(t, sb.Shell)

	// 2) echo 回显（VT 序列夹杂，bytes.Contains 判定）
	writeBin(t, ws, []byte("echo sh-echo-ok\r"))
	collectUntil(t, ws, func(k string, d []byte) bool {
		return k == "binary" && strings.Contains(string(d), "sh-echo-ok")
	}, 10*time.Second)

	// 3) resize 后继续工作
	writeText(t, ws, mustShellMsg(t, "SHELL_RESIZE", proto.ShellResize{Cols: 100, Rows: 40}))
	writeBin(t, ws, []byte("echo sh-post-resize\r"))
	collectUntil(t, ws, func(k string, d []byte) bool {
		return k == "binary" && strings.Contains(string(d), "sh-post-resize")
	}, 10*time.Second)

	// 4) Ctrl+C 会话存活
	writeBin(t, ws, []byte{0x03})
	writeBin(t, ws, []byte("echo sh-after-ctrlc\r"))
	collectUntil(t, ws, func(k string, d []byte) bool {
		return k == "binary" && strings.Contains(string(d), "sh-after-ctrlc")
	}, 10*time.Second)

	// 5) 退出 → Handle 返回（会话结束信号）
	writeBin(t, ws, []byte("exit\r"))
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Handle did not return after exit")
	}
}

func TestShellCloseKillsProcess(t *testing.T) {
	ws, _ := runShell(t, proto.ShellParams{Cols: 80, Rows: 25})
	collectUntil(t, ws, func(k string, d []byte) bool {
		return k == "text" && strings.Contains(string(d), "SHELL_BEGIN")
	}, 10*time.Second)

	// SHELL_BEGIN 已发出 → lastStartedPID 已写入 conPTY 记录的 shell PID。
	pid := lastStartedPID.Load()
	require.NotZero(t, pid, "shell PID must be recorded after start")

	_ = ws.CloseNow() // 模拟 client 断开

	require.Eventually(t, func() bool {
		return !processAlive(t, pid)
	}, 10*time.Second, 300*time.Millisecond, "shell process must die after disconnect")
}

// TestShellPromptPrefix：会话输出应出现 [HOSTNAME] PS 前缀提示符，
// 且启动命令注入无回显（-NoExit -Command 不经交互行编辑器）。
func TestShellPromptPrefix(t *testing.T) {
	host, _ := os.Hostname()
	want := "[" + host + "] PS "
	ws, done := runShell(t, proto.ShellParams{Cols: 80, Rows: 25})
	defer func() { _ = ws.CloseNow() }()
	_ = done

	// 单读循环：触发提示符渲染并累积全部输出（同时供回显断言）。
	writeBin(t, ws, []byte("\r"))
	var all []byte
	deadline := time.After(15 * time.Second)
	found := false
	for !found {
		select {
		case <-deadline:
			t.Fatalf("prompt prefix %q not seen; output so far: %q", want, all)
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		typ, data, err := ws.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read: %v (output: %q)", err, all)
		}
		if typ == websocket.MessageBinary {
			all = append(all, data...)
			if strings.Contains(string(all), want) {
				found = true
			}
		}
	}
	writeBin(t, ws, []byte("exit\r"))

	assert.NotContains(t, string(all), "function global:prompt",
		"startup command must not be echoed")
}
