// xnc-shell-probe — M2-Slice2 Task 3 live 检查客户端(dev-only):
// 拨号 core console pipe → M0 握手 → 0x0120 MSG_CREATE_SHELL(user/system
// 令牌)→ 拨号返回的 xnc-shell pipe → 握手 → oneshot 输出/退出码打印 →
// 0x0121 MSG_KILL_SHELL 清场。所有值仅用于实测记录,命令行零敏感字段
// (secret 经参数注入的仅是 dev core 的 --smoke-secret,dev 拓扑先例)。
//
// 用法:
//
//	xnc-shell-probe --core-pipe \\.\pipe\xnc-core --secret <64hex>
//	                [--token user|system] [--profile CMD]
//	                [--command "whoami"] [--timeout 30]
package main

import (
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"xnc/proto/ipc"
)

// canonicalPipe 裸名补全为完整管道路径(远端 shell 的引号/转义层会吃
// 反斜杠,裸名形式最稳)。
func canonicalPipe(p string) string {
	if !strings.Contains(p, `\`) {
		return `\\.\pipe\` + p
	}
	return p
}

const (
	msgCreateShell = 0x0120
	msgKillShell   = 0x0121
	msgShellData   = 0x0123
	msgShellExit   = 0x0125
	streamStdout   = 1 // [u8 stream 0=stdin,1=stdout,2=stderr]
)

// handshake 客户端半边(spec 9.3,镜像 shellhost 集成测试)。
func handshake(conn net.Conn, secret []byte) error {
	my := ipc.NewNonce()
	if err := ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: ipc.MsgHello, Payload: ipc.EncodeHello(uint32(os.Getpid()), my),
	}); err != nil {
		return err
	}
	f, err := ipc.ReadFrame(conn)
	if err != nil || f.MessageType != ipc.MsgHelloProof {
		return fmt.Errorf("expected HELLO_PROOF: %v %v", f, err)
	}
	_, peer, proof, err := ipc.DecodeHelloProof(f.Payload)
	if err != nil || !ipc.VerifyProof(secret, my, proof) {
		return fmt.Errorf("server proof rejected")
	}
	return ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: ipc.MsgProof, Payload: ipc.EncodeProof(ipc.Proof(secret, peer)),
	})
}

func buildCreatePayload(wts uint32, kind, profile, mode uint8, cols, rows uint16, command string, timeout uint32) []byte {
	p := make([]byte, 0, 32+len(command))
	be := binary.LittleEndian
	p = be.AppendUint32(p, wts)
	p = append(p, kind, profile, mode)
	p = be.AppendUint16(p, cols)
	p = be.AppendUint16(p, rows)
	p = be.AppendUint16(p, 0) // cwdLen = 0
	p = be.AppendUint16(p, 0) // envLen = 0
	p = be.AppendUint16(p, uint16(len(command)))
	p = append(p, command...)
	p = be.AppendUint32(p, timeout)
	return p
}

func main() {
	corePipe := flag.String("core-pipe", `\\.\pipe\xnc-core`, "core console pipe name")
	secretHex := flag.String("secret", "", "core pipe secret (64 hex)")
	token := flag.String("token", "user", "user | system")
	profile := flag.String("profile", "CMD", "POWERSHELL|PWSH|CMD|BASH")
	command := flag.String("command", "whoami", "oneshot command line")
	timeout := flag.Uint("timeout", 30, "oneshot timeout seconds")
	flag.Parse()

	sec, err := hex.DecodeString(*secretHex)
	if len(*secretHex) == 0 || err != nil {
		fmt.Fprintln(os.Stderr, "xnc-shell-probe: --secret <hex> required")
		os.Exit(2)
	}
	var kind uint8 = 0
	if *token == "system" {
		kind = 1
	} else if *token != "user" {
		fmt.Fprintln(os.Stderr, "xnc-shell-probe: --token must be user|system")
		os.Exit(2)
	}
	var profileEnum uint8
	var okp bool
	if profileEnum, okp = map[string]uint8{"POWERSHELL": 0, "PWSH": 1, "CMD": 2, "BASH": 3}[*profile]; !okp {
		fmt.Fprintln(os.Stderr, "xnc-shell-probe: bad --profile")
		os.Exit(2)
	}

	core, err := winio.DialPipe(canonicalPipe(*corePipe), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "xnc-shell-probe: dial core: %v\n", err)
		os.Exit(1)
	}
	// long-lived core connection: covers multi-minute oneshots (the deferred
	// KillShell cleanup must still be writable when the command finishes)
	_ = core.SetDeadline(time.Now().Add(10 * time.Minute))
	if err := handshake(core, sec); err != nil {
		fmt.Fprintf(os.Stderr, "xnc-shell-probe: core handshake: %v\n", err)
		os.Exit(1)
	}

	// wts=1:dev 拓扑的 LIVE 活动 console 会话(dev 实测门;system/user
	// 都按会话 1 spawn)。
	req := buildCreatePayload(1, kind, profileEnum, 1 /*oneshot*/, 80, 25, *command, uint32(*timeout))
	if err := ipc.WriteFrame(core, &ipc.Frame{MessageType: msgCreateShell, RequestID: 7, Payload: req}); err != nil {
		fmt.Fprintf(os.Stderr, "xnc-shell-probe: send CREATE_SHELL: %v\n", err)
		os.Exit(1)
	}
	resp, err := ipc.ReadFrame(core)
	if err != nil {
		fmt.Fprintf(os.Stderr, "xnc-shell-probe: read response: %v\n", err)
		os.Exit(1)
	}
	if resp.Flags&ipc.FlagError != 0 {
		fmt.Printf("create_shell: ERROR code=%s token=%s\n", string(resp.Payload), *token)
		os.Exit(2)
	}
	if len(resp.Payload) < 6+32 {
		fmt.Fprintf(os.Stderr, "xnc-shell-probe: short ok payload (%d)\n", len(resp.Payload))
		os.Exit(1)
	}
	pid := binary.LittleEndian.Uint32(resp.Payload[0:4])
	nlen := binary.LittleEndian.Uint16(resp.Payload[4:6])
	if int(6+nlen+32) > len(resp.Payload) {
		fmt.Fprintln(os.Stderr, "xnc-shell-probe: bad name length")
		os.Exit(1)
	}
	shellPipe := string(resp.Payload[6 : 6+nlen])
	shellSecret := append([]byte(nil), resp.Payload[6+nlen:6+nlen+32]...)
	fmt.Printf("create_shell: OK pid=%d pipe=%s token=%s profile=%s\n", pid, shellPipe, *token, *profile)
	defer func() { // 清场:kill 该 shell(core 侧按句柄限定)
		_ = ipc.WriteFrame(core, &ipc.Frame{
			MessageType: msgKillShell, RequestID: 8,
			Payload: binary.LittleEndian.AppendUint32(nil, pid),
		})
		_, _ = ipc.ReadFrame(core)
	}()

	shell, err := winio.DialPipe(shellPipe, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "xnc-shell-probe: dial shell pipe: %v\n", err)
		os.Exit(1)
	}
	_ = shell.SetDeadline(time.Now().Add(60 * time.Second))
	if err := handshake(shell, shellSecret); err != nil {
		fmt.Fprintf(os.Stderr, "xnc-shell-probe: shell handshake: %v\n", err)
		os.Exit(1)
	}

	var stdout []byte
	for {
		f, err := ipc.ReadFrame(shell)
		if err != nil {
			fmt.Fprintf(os.Stderr, "xnc-shell-probe: shell read: %v\n", err)
			os.Exit(1)
		}
		switch f.MessageType {
		case msgShellData:
			if len(f.Payload) >= 5 && f.Payload[0] == streamStdout {
				stdout = append(stdout, f.Payload[5:]...)
			}
		case msgShellExit:
			code := binary.LittleEndian.Uint32(f.Payload)
			fmt.Printf("shell_exit: code=%d\nstdout:\n%s", code, string(stdout))
			return
		}
	}
}
