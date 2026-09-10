// handshake.go — XNIP 双向认证握手(spec §9.3)。
//
// 密钥为 pipe_secret:xnc-core 在 spawn/授权时生成的一次性短 TTL 随机密钥,
// 分别安全递交给 worker(继承句柄)与 agent(RPC 响应)。握手双方各自用
// HMAC-SHA256 证明持有同一 secret;任一侧失败即断连 + 审计 ipc_auth_failed。
//
// 流程(payload 均为固定二进制布局,小端):
//
//	A → B: HELLO      [pid u32][nonce 16B]
//	B → A: HELLO_PROOF [pid u32][nonce 16B][hmac32 = HMAC(secret, A.nonce)]
//	A → B: PROOF       [hmac32 = HMAC(secret, B.nonce)]
//
// PID/映像路径校验(OpenProcess + 签名)在连接层做(C++ 侧 M1 补全);
// 本包只负责证明计算与 payload 编解码,可跨平台单测。
package ipc

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	NonceSize  = 16
	ProofSize  = sha256.Size
	helloLen   = 4 + NonceSize
	helloPrfLn = 4 + NonceSize + ProofSize
	proofLen   = ProofSize
)

// NewNonce 生成 16 字节随机 nonce。
func NewNonce() []byte { n := make([]byte, NonceSize); rand.Read(n); return n }

// Proof 计算 HMAC-SHA256(secret, nonce)。
func Proof(secret, nonce []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write(nonce)
	return m.Sum(nil)
}

// VerifyProof 常量时间比较 HMAC(secret, nonce) 与 got。
func VerifyProof(secret, nonce, got []byte) bool {
	return hmac.Equal(Proof(secret, nonce), got)
}

// EncodeHello 编码 HELLO payload。
func EncodeHello(pid uint32, nonce []byte) []byte {
	if len(nonce) != NonceSize {
		panic("ipc: nonce size")
	}
	p := make([]byte, helloLen)
	binary.LittleEndian.PutUint32(p, pid)
	copy(p[4:], nonce)
	return p
}

// DecodeHello 解码 HELLO payload。
func DecodeHello(p []byte) (pid uint32, nonce []byte, err error) {
	if len(p) != helloLen {
		return 0, nil, fmt.Errorf("ipc: hello payload %d bytes, want %d", len(p), helloLen)
	}
	return binary.LittleEndian.Uint32(p), p[4:helloLen], nil
}

// EncodeHelloProof 编码 HELLO_PROOF payload(应答方回自己的 pid+nonce,
// 并附上对发起方 nonce 的证明)。
func EncodeHelloProof(pid uint32, nonce, proof []byte) []byte {
	if len(nonce) != NonceSize || len(proof) != ProofSize {
		panic("ipc: hello_proof size")
	}
	p := make([]byte, helloPrfLn)
	binary.LittleEndian.PutUint32(p, pid)
	copy(p[4:], nonce)
	copy(p[4+NonceSize:], proof)
	return p
}

// DecodeHelloProof 解码 HELLO_PROOF payload。
func DecodeHelloProof(p []byte) (pid uint32, nonce, proof []byte, err error) {
	if len(p) != helloPrfLn {
		return 0, nil, nil, fmt.Errorf("ipc: hello_proof payload %d bytes, want %d", len(p), helloPrfLn)
	}
	return binary.LittleEndian.Uint32(p), p[4 : 4+NonceSize], p[4+NonceSize:], nil
}

// EncodeProof / DecodeProof 编解码 PROOF payload。
func EncodeProof(proof []byte) []byte {
	if len(proof) != ProofSize {
		panic("ipc: proof size")
	}
	return append([]byte(nil), proof...)
}

func DecodeProof(p []byte) ([]byte, error) {
	if len(p) != proofLen {
		return nil, errors.New("ipc: proof payload size")
	}
	return p, nil
}
