//go:build windows

// exec_loopback_windows_test.go — exec/shell 全链回环(M2-Slice2 Task 4):
// agent 会话引擎 → 真 coreShellHost(真 coreclient 协议栈,winio pipe)
// → fake core(0x0120/0x0121 应答)→ 真 shellpipe 客户端 → fake
// xnc-shell pipe(握手 + BEGIN + canned DATA/EXIT)。
package session

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/agent/coreclient"
	"xnc/agent/shellpipe"
	"xnc/proto"
	"xnc/proto/ipc"
)

// listenPipe 起一个 winio 命名 pipe listener。
func listenPipe(t *testing.T, name string) net.Listener {
	t.Helper()
	ln, err := winio.ListenPipe(`\\.\pipe\`+name, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// fakeCore 应答 0x0120/0x0121/PING;0x0120 可选拒绝码,成功则返回指向
// shellPipe 的描述符,并记录请求 payload 供断言。
type fakeCore struct {
	secret      []byte
	shellPipe   string
	shellSecret []byte
	reject      string

	pipeName string

	mu    sync.Mutex
	last  []byte // 最近 0x0120 payload
	kills []uint32
}

func (f *fakeCore) serve(t *testing.T, ln net.Listener) {
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.conn(t, conn)
		}
	}()
}

func (f *fakeCore) conn(t *testing.T, conn net.Conn) {
	defer conn.Close()
	if err := coreclient.ServerHandshake(conn, f.secret); err != nil {
		return
	}
	for {
		fr, err := ipc.ReadFrame(conn)
		if err != nil {
			return
		}
		switch fr.MessageType {
		case coreclient.MsgCreateShell:
			f.mu.Lock()
			f.last = append([]byte(nil), fr.Payload...)
			f.mu.Unlock()
			if f.reject != "" {
				_ = ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagResponse | ipc.FlagError,
					MessageType: coreclient.MsgCreateShell, RequestID: fr.RequestID,
					Payload: []byte(f.reject)})
				continue
			}
			p := make([]byte, 0, 6+len(f.shellPipe)+32)
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], 4242)
			p = append(p, b[:]...)
			binary.LittleEndian.PutUint16(b[:2], uint16(len(f.shellPipe)))
			p = append(p, b[:2]...)
			p = append(p, f.shellPipe...)
			p = append(p, f.shellSecret...)
			_ = ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagResponse,
				MessageType: coreclient.MsgCreateShell, RequestID: fr.RequestID, Payload: p})
		case coreclient.MsgKillShell:
			f.mu.Lock()
			f.kills = append(f.kills, binary.LittleEndian.Uint32(fr.Payload))
			f.mu.Unlock()
			_ = ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagResponse,
				MessageType: coreclient.MsgKillShell, RequestID: fr.RequestID})
		case ipc.MsgPing:
			_ = ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagResponse,
				MessageType: ipc.MsgPong, RequestID: fr.RequestID})
		}
	}
}

// fakeShellHostPipe 应答 xnc-shell pipe:握手 → BEGIN → scripted 阶段
// (canned 输出 + exit)→ 回声模式(stdin → stdout),记录 RESIZE/KILL。
type fakeShellHostPipe struct {
	secret  []byte
	profile string

	mu      sync.Mutex
	stdin   []byte
	resizes [][2]uint16
	killed  bool

	scripted func(write func(f *ipc.Frame) error)
}

func (f *fakeShellHostPipe) serve(t *testing.T, ln net.Listener) {
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if err := shellpipe.ServerHandshake(conn, f.secret); err != nil {
			t.Errorf("fake shell handshake: %v", err)
			return
		}
		w := func(fr *ipc.Frame) error { return ipc.WriteFrame(conn, fr) }
		_ = w(&ipc.Frame{MessageType: 0x0122, Payload: shellpipe.EncodeBegin(80, 25, f.profile)})
		if f.scripted != nil {
			f.scripted(w)
			return // scripted 终态后即关连接
		}
		for {
			fr, err := ipc.ReadFrame(conn)
			if err != nil {
				return
			}
			switch fr.MessageType {
			case 0x0123: // DATA
				stream, data, err := shellpipeDecodeDataForTest(fr.Payload)
				if err != nil || stream != 0 {
					continue
				}
				f.mu.Lock()
				f.stdin = append(f.stdin, data...)
				f.mu.Unlock()
				_ = w(&ipc.Frame{MessageType: 0x0123, Payload: shellpipe.EncodeData(1, append([]byte("echo:"), data...))})
			case 0x0124: // RESIZE
				f.mu.Lock()
				f.resizes = append(f.resizes, [2]uint16{
					binary.LittleEndian.Uint16(fr.Payload),
					binary.LittleEndian.Uint16(fr.Payload[2:])})
				f.mu.Unlock()
			case 0x0126: // KILL
				f.mu.Lock()
				f.killed = true
				f.mu.Unlock()
				_ = w(&ipc.Frame{MessageType: 0x0125, Payload: shellpipe.EncodeExit(1)})
				return
			}
		}
	}()
}

// shellpipeDecodeDataForTest 复制 0x0123 解码(布局断言)。
func shellpipeDecodeDataForTest(p []byte) (uint8, []byte, error) {
	if len(p) < 5 {
		return 0, nil, fmt.Errorf("short")
	}
	n := binary.LittleEndian.Uint32(p[1:])
	if uint64(len(p)) != 5+uint64(n) {
		return 0, nil, fmt.Errorf("mismatch")
	}
	return p[0], p[5:], nil
}

// startLoopback 装配 fake core + fake shell pipe,返回 fakeCore(其
// pipeName 字段供 hostFrom 构造 coreShellHost)。
func startLoopback(t *testing.T, shell *fakeShellHostPipe, coreReject string) *fakeCore {
	t.Helper()
	shellSecret := make([]byte, 32)
	for i := range shellSecret {
		shellSecret[i] = byte(i + 11)
	}
	if shell.secret == nil {
		shell.secret = shellSecret
	}
	shellPipeName := fmt.Sprintf("xnc-test-shell-%d", time.Now().UnixNano())
	shell.serve(t, listenPipe(t, shellPipeName))

	core := &fakeCore{
		secret:      []byte("loopback-core-secret"),
		shellPipe:   `\\.\pipe\` + shellPipeName,
		shellSecret: shellSecret,
		reject:      coreReject,
	}
	core.pipeName = `\\.\pipe\xnc-test-core-` + fmt.Sprintf("%d", time.Now().UnixNano())
	core.serve(t, listenPipe(t, core.pipeName[len(`\\.\pipe\`):]))
	return core
}

// hostFrom 用 fakeCore 的 pipe 名构造真 coreShellHost(真协议栈)。
func hostFrom(core *fakeCore) ShellHost {
	return &coreShellHost{pipe: core.pipeName, secret: core.secret, log: testLogger()}
}

// TestExecFullChainLoopback:oneshot 经全链(fake core + fake shell):
// canned stdout/stderr → EXEC_RESULT exit 7;请求 payload 的令牌/命令
// 字段在 fake core 侧可见。
func TestExecFullChainLoopback(t *testing.T) {
	shell := &fakeShellHostPipe{secret: nil, profile: "CMD", scripted: func(w func(f *ipc.Frame) error) {
		_ = w(&ipc.Frame{MessageType: 0x0123, Payload: shellpipe.EncodeData(1, []byte("hello-loopback"))})
		_ = w(&ipc.Frame{MessageType: 0x0123, Payload: shellpipe.EncodeData(2, []byte("boom"))})
		_ = w(&ipc.Frame{MessageType: 0x0125, Payload: shellpipe.EncodeExit(7)})
	}}
	shellSecret := make([]byte, 32)
	shellPipeName := fmt.Sprintf("xnc-t-shell-%d", time.Now().UnixNano())
	shell.secret = shellSecret
	shell.serve(t, listenPipe(t, shellPipeName))

	core := &fakeCore{
		secret:      []byte("lb-secret"),
		shellPipe:   `\\.\pipe\` + shellPipeName,
		shellSecret: shellSecret,
	}
	corePipe := fmt.Sprintf("xnc-t-core-%d", time.Now().UnixNano())
	core.serve(t, listenPipe(t, corePipe))

	host := &coreShellHost{pipe: `\\.\pipe\` + corePipe, secret: core.secret, log: testLogger()}
	ws := runExec(t, host, proto.ExecParams{Command: "whoami", TimeoutSec: 30, Shell: "cmd", System: true})
	stdout, stderr, res := collectExec(t, ws)
	assert.Contains(t, stdout, "hello-loopback")
	assert.Contains(t, stderr, "boom")
	require.NotNil(t, res.ExitCode)
	assert.Equal(t, 7, *res.ExitCode)
	assert.Empty(t, res.Code)

	// fake core 侧断言:token_kind=system, profile=CMD, mode=oneshot,
	// cmd 转发,wts=哨兵。
	core.mu.Lock()
	last := append([]byte(nil), core.last...)
	core.mu.Unlock()
	require.GreaterOrEqual(t, len(last), 20)
	assert.EqualValues(t, 0xFFFFFFFF, binary.LittleEndian.Uint32(last))
	assert.Equal(t, byte(1), last[4], "token kind system")
	assert.Equal(t, byte(2), last[5], "profile CMD")
	assert.Equal(t, byte(1), last[6], "mode oneshot")
	cmdLen := binary.LittleEndian.Uint16(last[22:]) // 4+3+2+2+2+2(cwd0/env0)=15... 见布局
	_ = cmdLen
	assert.Contains(t, string(last), "whoami")
}

// TestExecNoActiveSessionFullChain:fake core 拒 NO_ACTIVE_SESSION →
// EXEC_RESULT.Code 稳定码穿透。
func TestExecNoActiveSessionFullChain(t *testing.T) {
	shell := &fakeShellHostPipe{profile: "CMD"}
	core := startLoopback(t, shell, "NO_ACTIVE_SESSION")
	// 直接构造:用共享装配的 pipe 名(startLoopback 内部生成,这里用
	// 便捷包装)。
	host := hostFrom(core)
	ws := runExec(t, host, proto.ExecParams{Command: "whoami", Shell: "cmd"})
	_, _, res := collectExec(t, ws)
	assert.Nil(t, res.ExitCode)
	assert.Equal(t, "NO_ACTIVE_SESSION", res.Code)
}

// TestShellFullChainLoopback:交互 shell 经全链:BEGIN、stdin 回声、
// resize 字节抵达 xnc-shell 侧。
func TestShellFullChainLoopback(t *testing.T) {
	shell := &fakeShellHostPipe{profile: "PWSH"}
	core := startLoopback(t, shell, "")
	host := hostFrom(core)
	ws, done := runShell(t, host, proto.ShellParams{Cols: 80, Rows: 25, Shell: "pwsh"})
	defer func() { <-done }()

	// SHELL_BEGIN 报告实际 profile。
	begin := collectUntil(t, ws, func(k string, d []byte) bool {
		if k != "text" {
			return false
		}
		var m proto.Message
		return json.Unmarshal(d, &m) == nil && m.Type == "SHELL_BEGIN"
	}, 10*time.Second)
	var m proto.Message
	require.NoError(t, json.Unmarshal(begin, &m))
	var sb proto.ShellBegin
	require.NoError(t, m.Decode(&sb))
	assert.Equal(t, "PWSH", sb.Shell)

	// 键入 → 回声。
	writeBin(t, ws, []byte("ping\r"))
	collectUntil(t, ws, func(k string, d []byte) bool {
		return k == "binary" && string(d) == "echo:ping\r"
	}, 10*time.Second)

	// resize 字节抵达 fake xnc-shell。
	writeText(t, ws, mustShellMsg(t, "SHELL_RESIZE", proto.ShellResize{Cols: 100, Rows: 40}))
	require.Eventually(t, func() bool {
		shell.mu.Lock()
		defer shell.mu.Unlock()
		return len(shell.resizes) == 1 && shell.resizes[0] == [2]uint16{100, 40}
	}, 5*time.Second, 50*time.Millisecond)

	// 断开 → Kill(0x0126)抵达。
	_ = ws.CloseNow()
	require.Eventually(t, func() bool {
		shell.mu.Lock()
		defer shell.mu.Unlock()
		return shell.killed
	}, 10*time.Second, 100*time.Millisecond)
}
