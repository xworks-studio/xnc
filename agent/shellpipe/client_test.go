//go:build windows

// client_test.go — shellpipe 客户端用例:net.Pipe 假服务端(注入
// dialPipe)走完整握手 + SHELL_BEGIN,验证 DATA 双向、RESIZE/KILL 控制帧
// 到达服务端、EXIT 退出码投递与连接终结行为。
package shellpipe

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto/ipc"
)

// fakeShell 起 net.Pipe 假 xnc-shell:握手 → BEGIN → 可选 canned DATA →
// 收集客户端控制帧(stdin/RESIZE/KILL)→ 需要时发 EXIT 并关连接。
type fakeShell struct {
	clientConn net.Conn
	secret     []byte

	mu       sync.Mutex
	stdin    []byte
	resizes  [][2]uint16
	killed   bool
	ctrlSeen chan struct{}
}

func withFakeShellPipe(t *testing.T, canned map[uint8][]byte, exitCode *uint32) *fakeShell {
	t.Helper()
	const secret = "shell-pipe-secret"
	f := &fakeShell{secret: []byte(secret), ctrlSeen: make(chan struct{}, 64)}

	oldDial := dialPipe
	serverConn, clientConn := net.Pipe()
	dialPipe = func(_ context.Context, _ string) (net.Conn, error) { return clientConn, nil }
	t.Cleanup(func() { dialPipe = oldDial })

	go func() {
		defer serverConn.Close()
		if err := ServerHandshake(serverConn, f.secret); err != nil {
			return
		}
		_ = ipc.WriteFrame(serverConn, &ipc.Frame{MessageType: msgShellBegin, Payload: EncodeBegin(80, 25, "PWSH")})
		for stream, data := range canned {
			_ = ipc.WriteFrame(serverConn, &ipc.Frame{MessageType: msgShellData, Payload: EncodeData(stream, data)})
		}
		if exitCode != nil {
			_ = ipc.WriteFrame(serverConn, &ipc.Frame{MessageType: msgShellExit, Payload: EncodeExit(*exitCode)})
			return // close -> client pump terminates
		}
		for {
			fr, err := ipc.ReadFrame(serverConn)
			if err != nil {
				return
			}
			switch fr.MessageType {
			case msgShellData:
				stream, data, err := decodeData(fr.Payload)
				if err != nil || stream != StreamStdin {
					continue
				}
				f.mu.Lock()
				f.stdin = append(f.stdin, data...)
				f.mu.Unlock()
				f.ctrlSeen <- struct{}{}
			case msgShellResize:
				c := binary.LittleEndian.Uint16(fr.Payload)
				r := binary.LittleEndian.Uint16(fr.Payload[2:])
				f.mu.Lock()
				f.resizes = append(f.resizes, [2]uint16{c, r})
				f.mu.Unlock()
				f.ctrlSeen <- struct{}{}
			case msgShellKill:
				f.mu.Lock()
				f.killed = true
				f.mu.Unlock()
				f.ctrlSeen <- struct{}{}
				_ = ipc.WriteFrame(serverConn, &ipc.Frame{MessageType: msgShellExit, Payload: EncodeExit(1)})
				return
			}
		}
	}()
	return f
}

type contextLike = interface {
	Deadline() (deadline time.Time, ok bool)
	Done() <-chan struct{}
	Err() error
	Value(key any) any
}

func TestDialBeginAndCannedOutput(t *testing.T) {
	exit := uint32(7)
	f := withFakeShellPipe(t, map[uint8][]byte{StreamStdout: []byte("hello"), StreamStderr: []byte("boom")}, &exit)
	c, err := Dial("fake", f.secret)
	require.NoError(t, err)
	defer c.Close()
	require.Equal(t, Begin{Cols: 80, Rows: 25, Profile: "PWSH"}, c.Begin)

	var out, errb []byte
	deadline := time.After(5 * time.Second)
	for out == nil || errb == nil {
		select {
		case <-deadline:
			t.Fatalf("canned output incomplete: stdout=%q stderr=%q", out, errb)
		case d, ok := <-c.DataCh():
			require.True(t, ok)
			if d.Stream == StreamStdout {
				out = d.Bytes
			} else {
				errb = d.Bytes
			}
		}
	}
	assert.Equal(t, "hello", string(out))
	assert.Equal(t, "boom", string(errb))

	select {
	case code := <-c.ExitCh():
		assert.EqualValues(t, 7, code)
	case <-time.After(5 * time.Second):
		t.Fatal("no exit code")
	}
	// EXIT 后连接终结:DataCh 关闭。
	_, ok := <-c.DataCh()
	assert.False(t, ok)
}

func TestStdinResizeKill(t *testing.T) {
	f := withFakeShellPipe(t, nil, nil)
	c, err := Dial("fake", f.secret)
	require.NoError(t, err)

	require.NoError(t, c.WriteStdin([]byte("dir\r")))
	require.NoError(t, c.Resize(100, 40))
	require.NoError(t, c.Kill())

	for i := 0; i < 3; i++ {
		select {
		case <-f.ctrlSeen:
		case <-time.After(5 * time.Second):
			t.Fatalf("control frame %d not seen", i)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, "dir\r", string(f.stdin))
	assert.Equal(t, [][2]uint16{{100, 40}}, f.resizes)
	assert.True(t, f.killed)
}
