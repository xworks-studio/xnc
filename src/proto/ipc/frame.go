// Package ipc 定义 XNC 节点内本地 IPC 协议(xnc-agent ↔ xnc-core ↔ workers,
// spec §9)。传输为 Windows named pipe;帧 = XNIP 二进制头(16B,小端)+
// 不透明 payload。应用层 RPC payload 为 protobuf(M1 接入 codegen);M0
// 握手消息为固定二进制布局(handshake.go),帧层不感知。
//
// 本包不依赖 Windows API,便于在任意平台单测;拨号/监听在调用方
// (agent/coreclient 用 go-winio,xnc-core 用 CreateNamedPipeW)。
package ipc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// 帧头布局(spec §9.2,全部小端):
//
//	偏移 长度 字段
//	0    4    magic "XNIP"
//	4    1    protocolVersion(=1)
//	5    1    flags
//	6    2    messageType
//	8    4    requestId
//	12   4    payloadLength
//	16   ..   payload
const (
	Magic           = "XNIP"
	ProtocolVersion = 1
	HeaderSize      = 16
	MaxFrameBytes   = 9 << 20 // 9 MiB:超过即协议错误断连(§9.2)
)

// Flags(§9.2:bit0=response,bit2=event,bit4=error)。
const (
	FlagResponse uint8 = 1 << 0
	FlagEvent    uint8 = 1 << 2
	FlagError    uint8 = 1 << 4
)

// MessageType 注册表。0x0001..0x000F 为帧级握手/保活(固定二进制布局);
// 0x0100 起为 protobuf 应用 RPC(M1 按 proto/ipc/*.proto codegen)。
const (
	MsgHello      uint16 = 0x0001 // 发起方 → 对端:pid + nonce(挑战)
	MsgHelloProof uint16 = 0x0002 // 对端 → 发起方:pid + nonce + 对发起方挑战的 HMAC 证明
	MsgProof      uint16 = 0x0003 // 发起方 → 对端:对对端挑战的 HMAC 证明(握手完成)
	MsgBye        uint16 = 0x0004 // 主动关闭前通知(尽力而为)
	MsgPing       uint16 = 0x0010
	MsgPong       uint16 = 0x0011
)

// Frame 是一条解码后的 XNIP 帧。Payload 与 wire 数据独立(副本)。
type Frame struct {
	Flags       uint8
	MessageType uint16
	RequestID   uint32
	Payload     []byte
}

// Encode 序列化为 wire 字节(头 + payload 副本)。
func (f *Frame) Encode() []byte {
	if len(f.Payload) > MaxFrameBytes {
		panic(fmt.Sprintf("ipc: payload %d exceeds MaxFrameBytes", len(f.Payload)))
	}
	out := make([]byte, HeaderSize+len(f.Payload))
	copy(out, Magic)
	out[4] = ProtocolVersion
	out[5] = f.Flags
	binary.LittleEndian.PutUint16(out[6:], f.MessageType)
	binary.LittleEndian.PutUint32(out[8:], f.RequestID)
	binary.LittleEndian.PutUint32(out[12:], uint32(len(f.Payload)))
	copy(out[HeaderSize:], f.Payload)
	return out
}

var (
	ErrBadMagic   = errors.New("ipc: bad magic")
	ErrBadVersion = errors.New("ipc: unsupported protocol version")
	ErrTooLarge   = errors.New("ipc: frame exceeds MaxFrameBytes")
	ErrTruncated  = errors.New("ipc: truncated frame")
)

// DecodeHeader 校验并解析 16 字节头,返回 payload 长度。
func DecodeHeader(h []byte) (messageType uint16, payloadLen int, err error) {
	if len(h) < HeaderSize {
		return 0, 0, ErrTruncated
	}
	if string(h[:4]) != Magic {
		return 0, 0, ErrBadMagic
	}
	if h[4] != ProtocolVersion {
		return 0, 0, fmt.Errorf("%w: got %d want %d", ErrBadVersion, h[4], ProtocolVersion)
	}
	n := binary.LittleEndian.Uint32(h[12:])
	if n > MaxFrameBytes {
		return 0, 0, fmt.Errorf("%w: %d bytes", ErrTooLarge, n)
	}
	return binary.LittleEndian.Uint16(h[6:]), int(n), nil
}

// Decode 解析完整 wire 帧(头 + payload)。
func Decode(data []byte) (*Frame, error) {
	mt, n, err := DecodeHeader(data)
	if err != nil {
		return nil, err
	}
	if len(data) < HeaderSize+n {
		return nil, ErrTruncated
	}
	return &Frame{
		Flags:       data[5],
		MessageType: mt,
		RequestID:   binary.LittleEndian.Uint32(data[8:]),
		Payload:     append([]byte(nil), data[HeaderSize:HeaderSize+n]...),
	}, nil
}

// ReadFrame 从流读一帧(io.ReadFull;流式 pipe/conn 用)。
func ReadFrame(r io.Reader) (*Frame, error) {
	var h [HeaderSize]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	mt, n, err := DecodeHeader(h[:])
	if err != nil {
		return nil, err
	}
	f := &Frame{
		Flags:       h[5],
		MessageType: mt,
		RequestID:   binary.LittleEndian.Uint32(h[8:]),
	}
	if n > 0 {
		f.Payload = make([]byte, n)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// WriteFrame 向流写一帧。
func WriteFrame(w io.Writer, f *Frame) error {
	_, err := w.Write(f.Encode())
	return err
}
