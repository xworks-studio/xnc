//go:build windows

// shell_rpc_test.go — CreateShell/KillShell RPC(0x0120/0x0121)用例:
// 请求 payload 黄金字节(镜像 native/core 布局)、响应解码、FlagError
// 稳定码(NO_ACTIVE_SESSION)透传、Encode 阶段的前置域校验(env '\n')。
package coreclient

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto/ipc"
)

// encodeShellRespT 测试侧响应编码器(布局 = C++ core):
// [u32 pid][u16 nameLen][name utf8][32B secret]。
func encodeShellRespT(pid uint32, pipe string, secret []byte) []byte {
	p := make([]byte, 0, 6+len(pipe)+len(secret))
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], pid)
	p = append(p, b[:]...)
	binary.LittleEndian.PutUint16(b[:2], uint16(len(pipe)))
	p = append(p, b[:2]...)
	p = append(p, pipe...)
	p = append(p, secret...)
	return p
}

func TestCreateShellRoundTrip(t *testing.T) {
	name, ln := listen(t)
	const secret = "core-secret"
	shellSecret := make([]byte, 32)
	for i := range shellSecret {
		shellSecret[i] = byte(i + 5)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if err := ServerHandshake(conn, []byte(secret)); err != nil {
			t.Errorf("fake core handshake: %v", err)
			return
		}
		for {
			f, err := ipc.ReadFrame(conn)
			if err != nil {
				return
			}
			switch f.MessageType {
			case MsgCreateShell:
				// 黄金断言:[u32 wts=0xFFFFFFFF][u8 kind=1][u8 profile=2]
				// [u8 mode=1][u16 cols=80][u16 rows=25][u16 cwdLen=6]["C:\tmp"]
				// [u16 envLen=7]["A=1\nB=2"][u16 cmdLen=6]["whoami"]
				// [u32 timeout=60]
				want := []byte{
					0xFF, 0xFF, 0xFF, 0xFF, // wts sentinel
					0x01,       // token kind = system
					0x02,       // profile = CMD
					0x01,       // mode = oneshot
					0x50, 0x00, // cols 80
					0x19, 0x00, // rows 25
					0x06, 0x00, 'C', ':', '\\', 't', 'm', 'p',
					0x07, 0x00, 'A', '=', '1', '\n', 'B', '=', '2',
					0x06, 0x00, 'w', 'h', 'o', 'a', 'm', 'i',
					0x3C, 0x00, 0x00, 0x00, // timeout 60
				}
				if string(f.Payload) != string(want) {
					t.Errorf("create_shell payload = % x, want % x", f.Payload, want)
					return
				}
				_ = ipc.WriteFrame(conn, &ipc.Frame{
					Flags: ipc.FlagResponse, MessageType: MsgCreateShell,
					RequestID: f.RequestID,
					Payload:   encodeShellRespT(4321, `\\.\pipe\xnc-shell-1-1`, shellSecret),
				})
			case MsgKillShell:
				if len(f.Payload) != 4 || binary.LittleEndian.Uint32(f.Payload) != 4321 {
					t.Errorf("kill_shell payload = % x, want [u32 4321]", f.Payload)
				}
				_ = ipc.WriteFrame(conn, &ipc.Frame{
					Flags: ipc.FlagResponse, MessageType: MsgKillShell, RequestID: f.RequestID,
				})
			case ipc.MsgPing:
				_ = ipc.WriteFrame(conn, &ipc.Frame{
					Flags: ipc.FlagResponse, MessageType: ipc.MsgPong, RequestID: f.RequestID,
				})
			}
		}
	}()
	c, err := Dial(name, []byte(secret))
	require.NoError(t, err)
	defer c.Close()

	pid, pipe, sec, err := c.CreateShell(ShellCreateReq{
		WTS: WTSActiveConsole, TokenKind: TokenSystem, Profile: ProfileCmd,
		Mode: ModeOneshot, Cols: 80, Rows: 25, Cwd: `C:\tmp`,
		Env: []string{"A=1", "B=2"}, Cmd: "whoami", TimeoutSec: 60,
	})
	require.NoError(t, err)
	assert.EqualValues(t, 4321, pid)
	assert.Equal(t, `\\.\pipe\xnc-shell-1-1`, pipe)
	assert.Equal(t, shellSecret, sec)
	require.NoError(t, c.KillShell(pid))
}

func TestCreateShellRejected(t *testing.T) {
	name, ln := listen(t)
	const secret = "core-secret"
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if err := ServerHandshake(conn, []byte(secret)); err != nil {
			return
		}
		for {
			f, err := ipc.ReadFrame(conn)
			if err != nil {
				return
			}
			if f.MessageType == MsgCreateShell {
				_ = ipc.WriteFrame(conn, &ipc.Frame{
					Flags: ipc.FlagResponse | ipc.FlagError, MessageType: MsgCreateShell,
					RequestID: f.RequestID, Payload: []byte("NO_ACTIVE_SESSION"),
				})
			}
		}
	}()
	c, err := Dial(name, []byte(secret))
	require.NoError(t, err)
	defer c.Close()
	_, _, _, err = c.CreateShell(ShellCreateReq{
		TokenKind: TokenUser, Profile: ProfilePwsh, Mode: ModeOneshot, Cmd: "x",
	})
	var rej *RejectedError
	require.ErrorAs(t, err, &rej)
	assert.Equal(t, "NO_ACTIVE_SESSION", rej.Code)
}

func TestEncodeShellCreateReqValidation(t *testing.T) {
	base := ShellCreateReq{TokenKind: TokenUser, Profile: ProfileCmd, Mode: ModeOneshot, Cmd: "x"}
	_, err := EncodeShellCreateReq(ShellCreateReq{TokenKind: TokenUser, Profile: 9, Mode: ModeOneshot, Cmd: "x"})
	assert.Error(t, err)
	_, err = EncodeShellCreateReq(ShellCreateReq{TokenKind: TokenUser, Profile: ProfileCmd, Mode: 2, Cmd: "x"})
	assert.Error(t, err)
	_, err = EncodeShellCreateReq(ShellCreateReq{TokenKind: TokenUser, Profile: ProfileCmd, Mode: ModeOneshot})
	assert.Error(t, err, "oneshot without cmd")
	_, err = EncodeShellCreateReq(ShellCreateReq{TokenKind: TokenUser, Profile: ProfileCmd, Mode: ModeOneshot, Cmd: "x", Env: []string{"A=1\nsmuggled"}})
	assert.Error(t, err, "env value containing newline must be refused, never split")
	_, err = EncodeShellCreateReq(ShellCreateReq{TokenKind: TokenUser, Profile: ProfileCmd, Mode: ModeOneshot, Cmd: "x", Env: []string{"bare"}})
	assert.Error(t, err)
	// WTS=0 defaults to the live-active sentinel.
	p, err := EncodeShellCreateReq(base)
	require.NoError(t, err)
	assert.EqualValues(t, WTSActiveConsole, binary.LittleEndian.Uint32(p))
}
