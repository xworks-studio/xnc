//go:build windows

// Package coreclient 实现 agent 侧 XNIP named pipe 客户端(spec §9):
// winio 拨号 + 双向认证握手(§9.3)+ 保活 Ping。帧编解码与证明计算
// 复用 xnc/proto/ipc;PID/映像路径校验(OpenProcess + Authenticode)
// 属连接层,由 xnc-core 侧(M1)补全,本包不感知。
//
// 仅 Windows 构建(named pipe);与 agent/session 的 windows-only 测试
// 同风格。Client M0 阶段非并发安全:Ping 期间独占连接。
package coreclient

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/Microsoft/go-winio"
	"xnc/proto/ipc"
)

const (
	// handshakeTimeout 覆盖拨号等待与整个三步握手,防对端僵死挂起。
	handshakeTimeout = 5 * time.Second
	// pingTimeout 是 Ping 等待 Pong 的读 deadline。
	pingTimeout = 2 * time.Second
)

// Client 是一条已完成握手的 pipe 连接。conn 仅经方法访问。
type Client struct {
	conn  net.Conn
	reqID uint32
}

// Dial 连接 named pipe 并完成三步握手:
//
//	写 HELLO(pid, nonce) → 读 HELLO_PROOF 并 VerifyProof(secret, 我方 nonce)
//	→ 写 PROOF(HMAC(secret, 对端 nonce))
//
// 任一步失败即关连接返回 err(spec §9.3:任一侧证明失败即断连)。
func Dial(pipeName string, secret []byte) (*Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()
	conn, err := winio.DialPipeContext(ctx, pipeName)
	if err != nil {
		return nil, fmt.Errorf("coreclient: dial %s: %w", pipeName, err)
	}
	// 握手全程受 deadline 约束;成功后清除,后续读写由各方法自理。
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))

	myNonce := ipc.NewNonce()
	if err := ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: ipc.MsgHello,
		Payload:     ipc.EncodeHello(uint32(os.Getpid()), myNonce),
	}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("coreclient: send hello: %w", err)
	}
	f, err := ipc.ReadFrame(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("coreclient: read hello_proof: %w", err)
	}
	if f.MessageType != ipc.MsgHelloProof {
		conn.Close()
		return nil, fmt.Errorf("coreclient: handshake: got message type %#x, want HELLO_PROOF", f.MessageType)
	}
	_, peerNonce, proofC, err := ipc.DecodeHelloProof(f.Payload)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("coreclient: decode hello_proof: %w", err)
	}
	if !ipc.VerifyProof(secret, myNonce, proofC) {
		conn.Close()
		return nil, fmt.Errorf("coreclient: server proof rejected")
	}
	if err := ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: ipc.MsgProof,
		Payload:     ipc.EncodeProof(ipc.Proof(secret, peerNonce)),
	}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("coreclient: send proof: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	return &Client{conn: conn}, nil
}

// Ping 发送 MsgPing 并等待同 RequestID 的 MsgPong 响应,返回 RTT。
// 读设 2s deadline;期间到达的不相干帧(事件流等)被丢弃继续等待。
func (c *Client) Ping() (time.Duration, error) {
	c.reqID++
	id := c.reqID
	_ = c.conn.SetReadDeadline(time.Now().Add(pingTimeout))
	defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()

	start := time.Now()
	if err := ipc.WriteFrame(c.conn, &ipc.Frame{
		MessageType: ipc.MsgPing,
		RequestID:   id,
	}); err != nil {
		return 0, fmt.Errorf("coreclient: send ping: %w", err)
	}
	for {
		f, err := ipc.ReadFrame(c.conn)
		if err != nil {
			return 0, fmt.Errorf("coreclient: read pong: %w", err)
		}
		if f.MessageType == ipc.MsgPong && f.Flags&ipc.FlagResponse == ipc.FlagResponse && f.RequestID == id {
			return time.Since(start), nil
		}
	}
}

// Close 关闭底层连接。
func (c *Client) Close() error { return c.conn.Close() }

// ServerHandshake 在已接受的连接上执行服务端握手半边:
//
//	读 HELLO → 写 HELLO_PROOF(自 pid + 自 nonce + Proof(secret, 发起方 nonce))
//	→ 读 PROOF 并 VerifyProof
//
// 任一步失败即断连返回 err;成功后清除 deadline,连接交还调用方。
// 测试用;xnc-core(C++ 侧 M1)与 xnc-shell 复用同一时序。
func ServerHandshake(conn net.Conn, secret []byte) error {
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	fail := func(err error) error {
		conn.Close()
		return err
	}

	f, err := ipc.ReadFrame(conn)
	if err != nil {
		return fail(fmt.Errorf("coreclient: server handshake: read hello: %w", err))
	}
	if f.MessageType != ipc.MsgHello {
		return fail(fmt.Errorf("coreclient: server handshake: got message type %#x, want HELLO", f.MessageType))
	}
	_, clientNonce, err := ipc.DecodeHello(f.Payload)
	if err != nil {
		return fail(fmt.Errorf("coreclient: server handshake: decode hello: %w", err))
	}
	myNonce := ipc.NewNonce()
	if err := ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: ipc.MsgHelloProof,
		Payload:     ipc.EncodeHelloProof(uint32(os.Getpid()), myNonce, ipc.Proof(secret, clientNonce)),
	}); err != nil {
		return fail(fmt.Errorf("coreclient: server handshake: send hello_proof: %w", err))
	}
	f, err = ipc.ReadFrame(conn)
	if err != nil {
		return fail(fmt.Errorf("coreclient: server handshake: read proof: %w", err))
	}
	if f.MessageType != ipc.MsgProof {
		return fail(fmt.Errorf("coreclient: server handshake: got message type %#x, want PROOF", f.MessageType))
	}
	proof, err := ipc.DecodeProof(f.Payload)
	if err != nil {
		return fail(fmt.Errorf("coreclient: server handshake: decode proof: %w", err))
	}
	if !ipc.VerifyProof(secret, myNonce, proof) {
		return fail(fmt.Errorf("coreclient: server handshake: client proof rejected"))
	}
	_ = conn.SetDeadline(time.Time{})
	return nil
}
