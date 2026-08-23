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
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"os"
	"regexp"
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
	// interactive 冒烟模式(M2-Slice2 T5 门③):CREATE mode=interactive →
	// 等 SHELL_BEGIN → 发 stdin 行 → 读回显 → 发 RESIZE → 断言连接存活 +
	// 期望子串 → KILL。transcript 全量打 stdout(诚实入档)。
	interactive := flag.Bool("interactive", false, "interactive smoke: begin + echo roundtrip + resize")
	iCols := flag.Uint("cols", 100, "interactive initial cols")
	iRows := flag.Uint("rows", 30, "interactive initial rows")
	resizeTo := flag.String("resize", "132x43", "resize target WxH (resize smoke)")
	sendText := flag.String("send", "echo m2s2-interactive-ok", "text sent as one stdin line")
	expect := flag.String("expect", "m2s2-interactive-ok", "substring expected in pty output after send")
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
	mode, gcols, grows := uint8(1) /*oneshot*/, uint16(80), uint16(25)
	if *interactive {
		mode, gcols, grows = 0, uint16(*iCols), uint16(*iRows)
	}
	req := buildCreatePayload(1, kind, profileEnum, mode, gcols, grows, *command, uint32(*timeout))
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
	if *interactive {
		interactiveSmoke(shell, *sendText, *expect, *resizeTo, &stdout)
		return
	}
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

// interactiveSmoke 门③冒烟:BEGIN 等待(几何回读)→ stdin 行 → 回显
// 等待(期望子串,15s)→ RESIZE → resize 后输出继续抵达(连接存活)
// → 打 PASS/FAIL 行;stdout 累积为 transcript。
func interactiveSmoke(shell net.Conn, send, expect, resizeTo string, stdout *[]byte) {
	msgBegin := uint16(0x0122)
	msgResize := uint16(0x0124)
	msgState := uint16(0x0127)
	fail := func(format string, a ...any) {
		fmt.Printf("interactive: FAIL "+format+"\ntranscript:\n%s\n", append(a, string(*stdout))...)
		os.Exit(2)
	}
	// 1) BEGIN(超时 10s;STATE 先到 = spawn 失败)。
	_ = shell.SetDeadline(time.Now().Add(10 * time.Second))
	var f *ipc.Frame
	var err error
	for {
		f, err = ipc.ReadFrame(shell)
		if err != nil {
			fail("read before begin: %v", err)
		}
		if f.MessageType == msgState {
			fail("state before begin: %s", string(f.Payload))
		}
		if f.MessageType == msgBegin {
			break
		}
	}
	bcols := binary.LittleEndian.Uint16(f.Payload[0:2])
	brows := binary.LittleEndian.Uint16(f.Payload[2:4])
	fmt.Printf("interactive: BEGIN cols=%d rows=%d profile=%s\n", bcols, brows, nulString(f.Payload[4:20]))
	if bcols == 0 || brows == 0 {
		fail("begin geometry zero")
	}
	_ = shell.SetDeadline(time.Now().Add(15 * time.Second))
	// 2) stdin 行 + 回显等待。
	line := []byte(send + "\r")
	p := make([]byte, 5+len(line))
	p[0] = 0 // stdin
	binary.LittleEndian.PutUint32(p[1:], uint32(len(line)))
	copy(p[5:], line)
	if err := ipc.WriteFrame(shell, &ipc.Frame{MessageType: msgShellData, Payload: p}); err != nil {
		fail("send stdin: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(string(*stdout), expect) {
		if time.Now().After(deadline) {
			fail("echo roundtrip: %q not seen", expect)
		}
		f, err = ipc.ReadFrame(shell)
		if err != nil {
			fail("read during echo wait: %v", err)
		}
		if f.MessageType == msgShellData && len(f.Payload) >= 5 && f.Payload[0] == streamStdout {
			*stdout = append(*stdout, f.Payload[5:]...)
		}
	}
	fmt.Printf("interactive: echo-roundtrip OK (%q seen)\n", expect)
	// 3) RESIZE + 存活断言:resize 后再收 ≥1 帧输出或保持连接可读。
	var rw, rh uint
	if n, _ := fmt.Sscanf(resizeTo, "%dx%d", &rw, &rh); n != 2 || rw == 0 || rh == 0 {
		fail("bad --resize %q", resizeTo)
	}
	geoRe := regexp.MustCompile(fmt.Sprintf(`Columns:[ \t]+%d\b`, rw))
	rp := make([]byte, 4)
	binary.LittleEndian.PutUint16(rp, uint16(rw))
	binary.LittleEndian.PutUint16(rp[2:], uint16(rh))
	if err := ipc.WriteFrame(shell, &ipc.Frame{MessageType: msgResize, Payload: rp}); err != nil {
		fail("send resize: %v", err)
	}
	// resize 确认探针:让 cmd 打印新几何,回显必须仍能往返。
	gp := make([]byte, 5+len(gpCmd))
	gp[0] = 0
	binary.LittleEndian.PutUint32(gp[1:], uint32(len(gpCmd)))
	copy(gp[5:], gpCmd)
	_ = ipc.WriteFrame(shell, &ipc.Frame{MessageType: msgShellData, Payload: gp})
	_ = shell.SetDeadline(time.Now().Add(15 * time.Second))
	sawPost := false
	geoOK := false
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		f, err = ipc.ReadFrame(shell)
		if err != nil {
			// cmd `mode con` 输出完毕后 pty 沉默,read 到 deadline 即 i/o
			// timeout——已见 resize 后输出 + 几何确认则不算失败;真死连
			// (reset/EOF 类)在未见任何输出时才算。
			if sawPost {
				break
			}
			fail("connection died after resize: %v", err)
		}
		if f.MessageType == msgState {
			fail("state after resize: %s", string(f.Payload))
		}
		if f.MessageType == msgShellData && len(f.Payload) >= 5 && f.Payload[0] == streamStdout {
			*stdout = append(*stdout, f.Payload[5:]...)
			if !sawPost {
				sawPost = true
				fmt.Println("interactive: output continues after resize (conn alive)")
			}
			// cmd `mode con` 形态 "    Columns:          132";宽限期后即沉默。
			if geoRe.MatchString(string(*stdout)) {
				geoOK = true
				break
			}
		}
	}
	if !sawPost {
		fail("no output after resize")
	}
	if !geoOK {
		fail("resize geometry not confirmed (no 'Columns: <W>' in transcript)")
	}
	fmt.Printf("interactive: PASS (resize %s accepted, transcript follows)\n%s\n", resizeTo, string(*stdout))
}

// gpCmd 在 resize 后查询新几何(PowerShell $Host 用词 Cols=,cmd mode 用
// Columns =;两形态都探测)。
var gpCmd = []byte("\rmode con\r")

func nulString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}
