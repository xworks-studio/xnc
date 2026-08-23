//go:build windows

// integration_windows_test.go — 进程内 winio 客户端全链集成:握手 →
// oneshot(cmd /c echo hello → stdout 字节 + SHELL_EXIT 0)→ SHELL_KILL
// 杀树语义(长命命令 + kill → 非零退出,无孤儿)→ interactive 冒烟
// (SHELL_BEGIN + stdin 回显 + exit 终态)。
package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"xnc/proto/ipc"
)

// startShellServer 在独立 pipe 上起服务端 goroutine,返回 pipe 名与就绪信号。
func startShellServer(t *testing.T, o *serverOpts) string {
	t.Helper()
	pipe := `\\.\pipe\xnc-shell-test-` + fmt.Sprint(os.Getpid()) + "-" + t.Name()
	ln, err := listenNetPipe(pipe, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_ = serveConn(conn, o)
	}()
	return pipe
}

// dialShell 进程内客户端:拨号 + M0 握手(发起方半边,镜像 desktoppipe)。
func dialShell(t *testing.T, pipe string, secret []byte) net.Conn {
	t.Helper()
	conn, err := winio.DialPipe(pipe, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	myNonce := ipc.NewNonce()
	require.NoError(t, ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: ipc.MsgHello,
		Payload:     ipc.EncodeHello(uint32(os.Getpid()), myNonce),
	}))
	f, err := ipc.ReadFrame(conn)
	require.NoError(t, err)
	require.Equal(t, ipc.MsgHelloProof, f.MessageType)
	_, peerNonce, proof, err := ipc.DecodeHelloProof(f.Payload)
	require.NoError(t, err)
	require.True(t, ipc.VerifyProof(secret, myNonce, proof), "server proof rejected")
	require.NoError(t, ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: ipc.MsgProof,
		Payload:     ipc.EncodeProof(ipc.Proof(secret, peerNonce)),
	}))
	return conn
}

// readMsg 读一帧并断言类型。
func readMsg(t *testing.T, conn net.Conn, wantType uint16) []byte {
	t.Helper()
	f, err := ipc.ReadFrame(conn)
	require.NoError(t, err)
	require.Equal(t, wantType, f.MessageType, "got %#04x", f.MessageType)
	return f.Payload
}

func testSecret() []byte {
	return []byte("0123456789abcdef0123456789abcdef") // 32B;仅测试
}

func TestOneshotEchoHelloOverPipe(t *testing.T) {
	exe, err := exec.LookPath("cmd.exe")
	require.NoError(t, err)
	o := &serverOpts{
		secret: testSecret(), profile: "CMD", exe: exe, mode: "oneshot",
		command: "echo hello", timeout: 30, log: testLogger(),
	}
	conn := dialShell(t, startShellServer(t, o), o.secret)

	var stdout []byte
	for {
		f, err := ipc.ReadFrame(conn)
		require.NoError(t, err)
		switch f.MessageType {
		case msgShellData:
			stream, data, err := decodeData(f.Payload)
			require.NoError(t, err)
			require.Equal(t, streamStdout, stream)
			stdout = append(stdout, data...)
		case msgShellExit:
			code, err := decodeExit(f.Payload)
			require.NoError(t, err)
			assert.Equal(t, uint32(0), code)
			assert.Contains(t, string(stdout), "hello")
			return
		default:
			t.Fatalf("unexpected message %#04x", f.MessageType)
		}
	}
}

func TestOneshotKillKillsTree(t *testing.T) {
	exe, err := exec.LookPath("cmd.exe")
	require.NoError(t, err)
	o := &serverOpts{
		secret: testSecret(), profile: "CMD", exe: exe, mode: "oneshot",
		command: "ping -n 60 127.0.0.1", timeout: 300, log: testLogger(),
	}
	conn := dialShell(t, startShellServer(t, o), o.secret)

	// 服务端已起 ping(树:cmd → ping);发 KILL,期望非零退出且秒级落定。
	// (ping 会先吐 DATA 帧,消费直至 EXIT。)
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, ipc.WriteFrame(conn, &ipc.Frame{MessageType: msgShellKill}))

	start := time.Now()
	var payload []byte
	for {
		f, err := ipc.ReadFrame(conn)
		require.NoError(t, err)
		if f.MessageType == msgShellExit {
			payload = f.Payload
			break
		}
		require.Equal(t, msgShellData, f.MessageType, "unexpected message %#04x", f.MessageType)
	}
	code, err := decodeExit(payload)
	require.NoError(t, err)
	assert.NotEqual(t, uint32(0), code, "killed oneshot must not exit 0")
	assert.Less(t, time.Since(start), 15*time.Second, "kill must land in seconds, not wait out the timeout")

	// 无孤儿 ping:树杀(taskkill /T /F)后系统中不应再有 ping 实例
	//(测试机无并发 ping 的前提;查询失败不阻塞)。
	waitFor(t, 5*time.Second, "orphan ping.exe still running", func() bool {
		out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq ping.exe", "/FO", "CSV", "/NH").Output()
		if err != nil {
			return true
		}
		return !strings.Contains(strings.ToLower(string(out)), "ping.exe")
	})
}

func TestInteractiveSmokeOverPipe(t *testing.T) {
	exe, err := exec.LookPath("cmd.exe")
	require.NoError(t, err)
	o := &serverOpts{
		secret: testSecret(), profile: "CMD", exe: exe, mode: "interactive",
		cols: 80, rows: 25, log: testLogger(),
	}
	conn := dialShell(t, startShellServer(t, o), o.secret)

	// SHELL_BEGIN 先于任何输出。
	beginPayload := readMsg(t, conn, msgShellBegin)
	cols, rows, profile, err := decodeBegin(beginPayload)
	require.NoError(t, err)
	assert.Equal(t, uint16(80), cols)
	assert.Equal(t, uint16(25), rows)
	assert.Equal(t, "CMD", profile)

	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	// 回显一段命令再退出:cmd 交互下 "echo xnc-ok" + "exit"。
	require.NoError(t, ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: msgShellData, Payload: encodeData(streamStdin, []byte("echo xnc-ok\r")),
	}))

	var sawOutput bool
	for !sawOutput {
		f, err := ipc.ReadFrame(conn)
		require.NoError(t, err)
		require.Equal(t, msgShellData, f.MessageType)
		_, data, err := decodeData(f.Payload)
		require.NoError(t, err)
		if strings.Contains(string(data), "xnc-ok") {
			sawOutput = true
		}
	}

	// resize 冒烟(不期待响应帧,只验证不破坏连接)。
	require.NoError(t, ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: msgShellResize, Payload: encodeResize(100, 30),
	}))
	require.NoError(t, ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: msgShellData, Payload: encodeData(streamStdin, []byte("exit\r")),
	}))
	for { // exit 后仍有尾部输出(提示符回显):消费直至 EXIT
		f, err := ipc.ReadFrame(conn)
		require.NoError(t, err)
		if f.MessageType == msgShellExit {
			code, err := decodeExit(f.Payload)
			require.NoError(t, err)
			assert.Equal(t, uint32(0), code)
			return
		}
	}
}

// TestInteractiveKillReportsNonzeroExit T2 评审修复的钉子:交互 shell 被
// KILL 后 SHELL_EXIT 绝不上报 0(修复前 KILL 路径 exitCode nil → 0)。
func TestInteractiveKillReportsNonzeroExit(t *testing.T) {
	exe, err := exec.LookPath("cmd.exe")
	require.NoError(t, err)
	o := &serverOpts{
		secret: testSecret(), profile: "CMD", exe: exe, mode: "interactive",
		cols: 80, rows: 25, log: testLogger(),
	}
	conn := dialShell(t, startShellServer(t, o), o.secret)
	readMsg(t, conn, msgShellBegin) // BEGIN 先于任何输出

	time.Sleep(300 * time.Millisecond) // 让 cmd 进入就绪提示符
	require.NoError(t, ipc.WriteFrame(conn, &ipc.Frame{MessageType: msgShellKill}))

	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	for {
		f, err := ipc.ReadFrame(conn)
		require.NoError(t, err)
		if f.MessageType == msgShellExit {
			code, err := decodeExit(f.Payload)
			require.NoError(t, err)
			assert.NotEqual(t, uint32(0), code, "killed interactive shell must not report exit 0")
			return
		}
	}
}

// TestHandshakeRejectsWrongSecret 错误 secret 的客户端必须被拒。
func TestHandshakeRejectsWrongSecret(t *testing.T) {
	exe, err := exec.LookPath("cmd.exe")
	require.NoError(t, err)
	o := &serverOpts{
		secret: testSecret(), profile: "CMD", exe: exe, mode: "oneshot",
		command: "echo hi", timeout: 5, log: testLogger(),
	}
	pipe := startShellServer(t, o)

	conn, err := winio.DialPipe(pipe, nil)
	require.NoError(t, err)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	bad := []byte("ffffffffffffffffffffffffffffffff")
	myNonce := ipc.NewNonce()
	require.NoError(t, ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: ipc.MsgHello,
		Payload:     ipc.EncodeHello(uint32(os.Getpid()), myNonce),
	}))
	f, err := ipc.ReadFrame(conn)
	require.NoError(t, err)
	require.Equal(t, ipc.MsgHelloProof, f.MessageType)
	_, peerNonce, _, err := ipc.DecodeHelloProof(f.Payload)
	require.NoError(t, err)
	require.NoError(t, ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: ipc.MsgProof,
		Payload:     ipc.EncodeProof(ipc.Proof(bad, peerNonce)),
	}))
	// 服务器拒绝后关闭:下一帧读必须失败。
	_, err = ipc.ReadFrame(conn)
	require.Error(t, err, "server must disconnect on bad proof")
}
