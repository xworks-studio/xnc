// server.go — xnc-shell pipe 服务端骨架:单连接 accept → M0 双向 HMAC
// 握手(server 半边,镜像 core/desktoppipe 时序)→ 按 mode 分派。
// 握手失败即断连等下一个连接(secret 不外泄,无重试语义)。
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"

	"xnc/proto/ipc"
)

// serverOpts 汇总 main flag 解析结果(测试注入点)。
type serverOpts struct {
	pipe    string
	secret  []byte
	profile string // 已白名单化的 profile 名
	exe     string // 已解析的 exe 路径
	mode    string // "interactive" | "oneshot"
	cols    int
	rows    int
	cwd     string
	env     []string
	command string
	timeout int // 秒;<=0 = oneshot 默认 300,interactive 不适用
	log     *slog.Logger
}

func (o *serverOpts) logger() *slog.Logger {
	if o.log != nil {
		return o.log
	}
	return slog.Default()
}

// serveConn 对一条已 accept 的连接做握手 + 模式分派。
func serveConn(conn net.Conn, o *serverOpts) error {
	if err := serverHandshake(conn, o.secret); err != nil {
		_ = conn.Close()
		return fmt.Errorf("shellhost: handshake: %w", err)
	}
	switch o.mode {
	case "interactive":
		return runInteractive(conn, o)
	case "oneshot":
		return runOneshot(conn, o)
	}
	_ = conn.Close()
	return fmt.Errorf("shellhost: unknown mode %q", o.mode)
}

// serverHandshake 是 M0 三步握手的应答方半边:读 HELLO → 回
// HELLO_PROOF(pid+nonce+HMAC(secret, client nonce)) → 读 PROOF 校验。
func serverHandshake(conn net.Conn, secret []byte) error {
	f, err := ipc.ReadFrame(conn)
	if err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	if f.MessageType != ipc.MsgHello {
		return fmt.Errorf("got message type %#x, want HELLO", f.MessageType)
	}
	_, clientNonce, err := ipc.DecodeHello(f.Payload)
	if err != nil {
		return fmt.Errorf("decode hello: %w", err)
	}
	myNonce := ipc.NewNonce()
	if err := ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: ipc.MsgHelloProof,
		Payload: ipc.EncodeHelloProof(uint32(os.Getpid()), myNonce,
			ipc.Proof(secret, clientNonce)),
	}); err != nil {
		return fmt.Errorf("send hello_proof: %w", err)
	}
	f, err = ipc.ReadFrame(conn)
	if err != nil {
		return fmt.Errorf("read proof: %w", err)
	}
	if f.MessageType != ipc.MsgProof {
		return fmt.Errorf("got message type %#x, want PROOF", f.MessageType)
	}
	proof, err := ipc.DecodeProof(f.Payload)
	if err != nil {
		return fmt.Errorf("decode proof: %w", err)
	}
	if !ipc.VerifyProof(secret, myNonce, proof) {
		return errors.New("client proof rejected")
	}
	return nil
}

// writeFrame 发一帧(写时限由调用方经 SetWriteDeadline 控制)。
func writeFrame(conn net.Conn, f *ipc.Frame) error {
	_, err := conn.Write(f.Encode())
	return err
}
