//go:build windows

// Package desktoppipe 实现 agent 侧 xnc-desktop 实时 pipe 订阅者客户端
// (M1-Slice2 Task 3):winio 拨号 + M0 双向 HMAC 握手(proto/ipc,
// spec §9.3)+ ATTACH(0x0102)+ FRAME(0x0105)泵。消息集为固定二进制
// 子集(native/desktop/rt_pipe_server.h 的 Go 镜像;protobuf 迁移
// Slice3),payload 全部小端:
//
//	MSG_ATTACH       0x0102 req   [u32 sub_id][u32 max_fps][u32 max_w][u32 bitrate]
//	MSG_DETACH       0x0103 req   [u32 sub_id]
//	MSG_KEYFRAME_REQ 0x0104 req   [u32 sub_id][char reason[32]](NUL 填充)
//	MSG_FRAME        0x0105 event [u32 sub_id_target=0 广播][u64 mono_us]
//	                            [u8 key][u32 len][au bytes]
//	MSG_HOST_HELLO   0x0106 event [u32 gen][u32 w][u32 h][u32 fps][u32 max_subs]
//	MSG_STATE        0x0107 event [char code[32]][u8 recoverable]
//	MSG_INPUT        0x0108 req   [u32 sub_id][u64 seq][u8 type][payload']
//	                            (M1-Slice3 出站;payload' 布局见 InputMsg)
//	MSG_CURSOR       0x0109 event [s32 x][s32 y][u8 visible]
//	                            (M1-Slice3 入站;HOST_HELLO 流空间逻辑 px)
//
// sub_id 由调用方选定(非 0、每 host 唯一——core 侧 StartCapture 每次会话
// 一连接,随机 u32 即可)。ATTACH 成功无显式响应:HOST_HELLO 即确认
// (T2 服务端契约);失败形态 = ATTACH FlagError 帧或 STATE{too_many_subs}
// 后断连。消费模型:FrameCh/StateCh 缓冲通道,FrameCh 满时丢 delta 并
// 合并请求一次 keyframe(镜像服务端 §7.9 语义),key 帧阻塞送达(可被
// Close 解除)。
package desktoppipe

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"xnc/proto/ipc"
)

// 消息类型(0x0102-0x0109;native/desktop/rt_pipe_server.h 镜像)。
const (
	msgAttach      uint16 = 0x0102
	msgDetach      uint16 = 0x0103
	msgKeyframeReq uint16 = 0x0104
	msgFrame       uint16 = 0x0105
	msgHostHello   uint16 = 0x0106
	msgState       uint16 = 0x0107
	msgInput       uint16 = 0x0108
	msgCursor      uint16 = 0x0109
)

// 0x0108 type 值(C++ kInput* 镜像)。
const (
	InputMove   uint8 = 1
	InputButton uint8 = 2
	InputWheel  uint8 = 3
	InputKey    uint8 = 4
	InputText   uint8 = 5
	InputLock   uint8 = 6
)

// kMaxTextUnits 镜像(desktop 侧防御上限;agent 侧另有 ≤2KiB 预算)。
const maxInputTextUnits = 512

const (
	// handshakeTimeout 覆盖拨号与三步握手。
	handshakeTimeout = 5 * time.Second
	// attachTimeout 是 ATTACH 后等待 HOST_HELLO(或失败形态)的时限。
	attachTimeout = 5 * time.Second
	// ctrlWriteTimeout 是控制帧(DETACH/KEYFRAME_REQ)写 deadline。
	ctrlWriteTimeout = 2 * time.Second
	// frameChDepth 吸收编码突发;服务端每订阅者队列深度 3,客户端略宽。
	frameChDepth = 16
	// stateChDepth 覆盖 capture_rebuilt 等稀疏事件。
	stateChDepth = 8
	// cursorChDepth 覆盖 8ms 轮询突发;满时丢弃(不可靠通道语义,
	// 下一事件最多 8ms 后到)。
	cursorChDepth = 8
	// reasonLen 是 KEYFRAME_REQ reason 字段宽度(NUL 填充,有效 31)。
	reasonLen = 32
)

// SubOpts 是 ATTACH 携带的整形参数(0 = 未指定,由 host 用默认)。
type SubOpts struct {
	MaxFPS  uint32
	MaxW    uint32
	Bitrate uint32
}

// Frame 是一条解码后的视频 AU(Annex-B,统一 4 字节起始码)。
type Frame struct {
	Key    bool
	MonoUs uint64
	AU     []byte
}

// HelloInfo 是 HOST_HELLO 内容;gen 递增代表 capture 重建(T2 语义)。
type HelloInfo struct {
	Gen     uint32
	W, H    uint32
	Fps     uint32
	MaxSubs uint32
}

// StateEvent 是 STATE 事件(stable code + 可恢复性)。
type StateEvent struct {
	Code        string
	Recoverable bool
}

// InputMsg 是 0x0108 消息的解码形态(payload' 字段逐 type 复用,镜像
// native InputMsg)。EncodeInputMsg 用 SubID/Seq/Type + 载荷字段产线。
type InputMsg struct {
	SubID    uint32
	Seq      uint64
	Type     uint8
	X, Y     int32 // MOVE 坐标 / WHEEL dx,dy
	Buttons  uint16
	Btn      uint8
	Down     uint8 // BUTTON/KEY 共用
	Trackpad uint8
	Scan     uint16
	Extended uint8
	Text     []uint16
	Caps     uint8
	Num      uint8
}

// CursorEvent 是 0x0109 事件(HOST_HELLO 流空间逻辑 px)。
type CursorEvent struct {
	X, Y    int32
	Visible bool
}

// Sub 是一条已 ATTACH 的订阅连接。读侧由泵 goroutine 独占,控制帧写
// 由 writeMu 串行;Close 后各通道随泵退出而关闭。
type Sub struct {
	conn  net.Conn
	subID uint32

	writeMu sync.Mutex // 串行化 DETACH/KEYFRAME_REQ 写

	mu       sync.Mutex // 守护 hello/needKey/closed/pumpErr
	hello    *HelloInfo
	needKey  bool // 消费侧丢 delta 后合并的 keyframe 请求标志
	closed   bool
	pumpErr  error
	closeOne sync.Once
	closeErr error

	frameCh  chan Frame
	stateCh  chan StateEvent
	cursorCh chan CursorEvent
	done     chan struct{}
	doneOnce sync.Once
}

// Dial 连接 xnc-desktop 实时 pipe,完成握手与 ATTACH,等待 HOST_HELLO
// (即 ATTACH 确认)后返回。失败形态:握手证明失败、ATTACH FlagError
// (如 DUP)、STATE{too_many_subs}、超时。成功后泵 goroutine 开始向
// FrameCh/StateCh 交付;HOST_HELLO 更新 Hello()(首帧已同步就绪)。
func Dial(pipe, secret string, subID uint32, opts SubOpts) (*Sub, error) {
	if subID == 0 {
		return nil, errors.New("desktoppipe: subID must be non-zero (server rejects 0)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()
	conn, err := winio.DialPipeContext(ctx, pipe)
	if err != nil {
		return nil, fmt.Errorf("desktoppipe: dial %s: %w", pipe, err)
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := clientHandshake(conn, []byte(secret)); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(attachTimeout))
	if err := ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: msgAttach, RequestID: 1, Payload: encodeAttach(subID, opts),
	}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("desktoppipe: send attach: %w", err)
	}

	s := &Sub{
		conn: conn, subID: subID,
		frameCh:  make(chan Frame, frameChDepth),
		stateCh:  make(chan StateEvent, stateChDepth),
		cursorCh: make(chan CursorEvent, cursorChDepth),
		done:     make(chan struct{}),
	}
	// ATTACH 窗口内同步等待 HOST_HELLO;期间到达的 STATE 先入通道
	// (too_many_subs 升格为 Dial 错误)。
	for {
		f, err := ipc.ReadFrame(conn)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("desktoppipe: attach: no host_hello: %w", err)
		}
		switch f.MessageType {
		case msgHostHello:
			h, err := decodeHostHello(f.Payload)
			if err != nil {
				conn.Close()
				return nil, err
			}
			s.hello = h
			_ = conn.SetDeadline(time.Time{})
			go s.pump()
			return s, nil
		case msgState:
			ev, err := decodeState(f.Payload)
			if err != nil {
				conn.Close()
				return nil, err
			}
			if ev.Code == "too_many_subs" {
				conn.Close()
				return nil, fmt.Errorf("desktoppipe: attach rejected: %s", ev.Code)
			}
			// 与 pump 相同的非阻塞语义:pre-hello STATE 帧多于缓冲时丢弃,
			// 绝不阻塞 Dial(事件稀疏且多为瞬时提示)。
			select {
			case s.stateCh <- ev:
			default:
			}
		case msgAttach:
			if f.Flags&ipc.FlagError != 0 {
				conn.Close()
				return nil, fmt.Errorf("desktoppipe: attach rejected: %s", respText(f))
			}
			// ATTACH 无成功响应帧;FlagResponse 且无错误 = 协议噪声,忽略。
		default:
			// FRAME 先于 HOST_HELLO 属非常序;丢弃等待 hello。
		}
	}
}

// FrameCh 交付 FRAME 事件;连接终结(含 Close)后关闭。
func (s *Sub) FrameCh() <-chan Frame { return s.frameCh }

// StateCh 交付 STATE 事件;连接终结后关闭。通道满时丢弃(事件稀疏且
// 多为瞬时提示;capture_rebuilt 语义由 Hello().Gen 承载)。
func (s *Sub) StateCh() <-chan StateEvent { return s.stateCh }

// CursorCh 交付 0x0109 光标事件;连接终结后关闭。满时丢弃(位置语义
// 最新即准;host 仅在变化时发送)。
func (s *Sub) CursorCh() <-chan CursorEvent { return s.cursorCh }

// SendInput 发送一条 0x0108 输入消息(SubID 自动填充本订阅 id;预校验
// 由调用方——agent/desktop——负责,本层只编码)。写失败/已关返回错误。
func (s *Sub) SendInput(m *InputMsg) error {
	m.SubID = s.subID
	p := EncodeInputMsg(m)
	if p == nil {
		return fmt.Errorf("desktoppipe: encode input type %d", m.Type)
	}
	return s.SendInputPayload(p)
}

// SendInputPayload 发送已编码的 0x0108 payload(调用方保证布局与
// sub_id;EncodeInputMsg 的产物或 agent 侧等价编码)。
func (s *Sub) SendInputPayload(p []byte) error {
	return s.writeCtrl(&ipc.Frame{MessageType: msgInput, Payload: p})
}

// Hello 返回最近一次 HOST_HELLO(副本);Dial 成功后恒非 nil。
func (s *Sub) Hello() *HelloInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hello == nil {
		return nil
	}
	h := *s.hello
	return &h
}

// SubID 返回本订阅的 sub_id(0x0108 输入消息必须携带本值)。
func (s *Sub) SubID() uint32 { return s.subID }

// Done 在泵退出(连接终结)时关闭;Err 返回终结原因(Close 主动关闭
// 时为 nil)。
func (s *Sub) Done() <-chan struct{} { return s.done }

func (s *Sub) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pumpErr
}

// RequestKeyframe 发送 KEYFRAME_REQ(reason 进入服务端记账;截断到
// 31 字节)。写失败返回错误;连接已关返回 errClosed。
func (s *Sub) RequestKeyframe(reason string) error {
	return s.writeCtrl(&ipc.Frame{MessageType: msgKeyframeReq, Payload: encodeKeyframeReq(s.subID, reason)})
}

// Close 发送 DETACH(尽力而为)、关闭连接并置 done(解除泵的可能
// 阻塞);数据通道由泵退出时统一关闭(泵是唯一发送方,杜绝
// send-on-closed 竞态)。幂等;返回底层连接关闭错误。
func (s *Sub) Close() error {
	s.closeOne.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		// DETACH 绕过 closed 检查直写(本方法即关闭方);与
		// RequestKeyframe 的写经 writeMu 互斥。
		s.writeMu.Lock()
		_ = s.conn.SetWriteDeadline(time.Now().Add(ctrlWriteTimeout))
		_ = ipc.WriteFrame(s.conn, &ipc.Frame{MessageType: msgDetach, Payload: encodeDetach(s.subID)})
		_ = s.conn.SetWriteDeadline(time.Time{})
		s.writeMu.Unlock()
		s.closeErr = s.conn.Close()
		s.closeDone() // 泵随即退出并关闭 frameCh/stateCh
	})
	return s.closeErr
}

// closeDone 幂等关闭 done(Close 与泵的 teardown 都可能触发)。
func (s *Sub) closeDone() { s.doneOnce.Do(func() { close(s.done) }) }

var errClosed = errors.New("desktoppipe: subscription closed")

// writeCtrl 带时限串行写一条控制帧。
func (s *Sub) writeCtrl(f *ipc.Frame) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return errClosed
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(ctrlWriteTimeout))
	defer func() { _ = s.conn.SetWriteDeadline(time.Time{}) }()
	if err := ipc.WriteFrame(s.conn, f); err != nil {
		return fmt.Errorf("desktoppipe: write %#04x: %w", f.MessageType, err)
	}
	return nil
}

// pump 是唯一读方:FRAME → FrameCh(满则丢 delta + 合并 keyframe 请求,
// key 帧阻塞但可被 Done 解除)、HOST_HELLO → Hello()、STATE → StateCh;
// 读终结时关闭全部通道。
func (s *Sub) pump() {
	for {
		f, err := ipc.ReadFrame(s.conn)
		if err != nil {
			// 主动 Close 后的读错误不是故障;否则记为泵错误。
			s.teardown(err)
			return
		}
		switch f.MessageType {
		case msgFrame:
			frm, err := decodeFrame(f.Payload)
			if err != nil {
				s.teardown(err)
				return
			}
			if frm.Key {
				select {
				case s.frameCh <- frm:
				case <-s.done:
					s.teardown(nil)
					return
				}
				s.mu.Lock()
				s.needKey = false // key 已送达,合并请求清位
				s.mu.Unlock()
				continue
			}
			select {
			case s.frameCh <- frm:
			case <-s.done:
				s.teardown(nil)
				return
			default:
				// 消费侧落后:丢 delta,合并请求一次 keyframe(§7.9 客户端镜像)。
				s.mu.Lock()
				need := s.needKey
				s.needKey = true
				s.mu.Unlock()
				if !need {
					go func() { _ = s.RequestKeyframe("client_overflow") }() //nolint:errcheck // 记账请求,失败随连接终结
				}
			}
		case msgHostHello:
			h, err := decodeHostHello(f.Payload)
			if err != nil {
				s.teardown(err)
				return
			}
			s.mu.Lock()
			s.hello = h
			s.mu.Unlock()
		case msgState:
			ev, err := decodeState(f.Payload)
			if err != nil {
				s.teardown(err)
				return
			}
		select {
		case s.stateCh <- ev:
		case <-s.done:
			s.teardown(nil)
			return
		default: // 满则丢弃(见 StateCh 注释)
		}
	case msgCursor:
		ev, err := decodeCursor(f.Payload)
		if err != nil {
			s.teardown(err)
			return
		}
		select {
		case s.cursorCh <- ev:
		case <-s.done:
			s.teardown(nil)
			return
		default: // 满则丢弃(见 CursorCh 注释)
		}
		default:
			// PONG / 迟到的 ATTACH 应答等:忽略。
		}
	}
}

// teardown 是泵的唯一下线路径:记录终结原因(主动 Close 后为 nil)、
// 关闭 done(幂等,Close 侧可能已关)并关闭数据通道。泵是数据通道的
// 唯一发送方,进入 teardown 前必然已退出所有发送分支,故不存在
// send-on-closed 竞态;Close 只关闭 done 解除泵阻塞,绝不关数据通道。
func (s *Sub) teardown(err error) {
	s.mu.Lock()
	if !s.closed && s.pumpErr == nil {
		s.pumpErr = err
	}
	s.mu.Unlock()
	s.closeDone()
	close(s.frameCh)
	close(s.stateCh)
	close(s.cursorCh)
}

// ---- 握手与 payload 编解码 ----

// clientHandshake 是 M0 三步握手的发起方半边(镜像 coreclient.Dial 内
// 联时序):HELLO(pid+nonce) → 校验 HELLO_PROOF → PROOF。
func clientHandshake(conn net.Conn, secret []byte) error {
	myNonce := ipc.NewNonce()
	if err := ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: ipc.MsgHello,
		Payload:     ipc.EncodeHello(uint32(os.Getpid()), myNonce),
	}); err != nil {
		return fmt.Errorf("desktoppipe: send hello: %w", err)
	}
	f, err := ipc.ReadFrame(conn)
	if err != nil {
		return fmt.Errorf("desktoppipe: read hello_proof: %w", err)
	}
	if f.MessageType != ipc.MsgHelloProof {
		return fmt.Errorf("desktoppipe: handshake: got message type %#x, want HELLO_PROOF", f.MessageType)
	}
	_, peerNonce, proofC, err := ipc.DecodeHelloProof(f.Payload)
	if err != nil {
		return fmt.Errorf("desktoppipe: decode hello_proof: %w", err)
	}
	if !ipc.VerifyProof(secret, myNonce, proofC) {
		return errors.New("desktoppipe: server proof rejected")
	}
	if err := ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: ipc.MsgProof,
		Payload:     ipc.EncodeProof(ipc.Proof(secret, peerNonce)),
	}); err != nil {
		return fmt.Errorf("desktoppipe: send proof: %w", err)
	}
	return nil
}

func encodeAttach(subID uint32, o SubOpts) []byte {
	p := make([]byte, 16)
	binary.LittleEndian.PutUint32(p, subID)
	binary.LittleEndian.PutUint32(p[4:], o.MaxFPS)
	binary.LittleEndian.PutUint32(p[8:], o.MaxW)
	binary.LittleEndian.PutUint32(p[12:], o.Bitrate)
	return p
}

func encodeDetach(subID uint32) []byte {
	p := make([]byte, 4)
	binary.LittleEndian.PutUint32(p, subID)
	return p
}

// encodeKeyframeReq 编码 [u32 sub_id][reason 32B NUL 填充](截断 31)。
func encodeKeyframeReq(subID uint32, reason string) []byte {
	p := make([]byte, 4+reasonLen)
	binary.LittleEndian.PutUint32(p, subID)
	copy(p[4:], reason)
	return p
}

// decodeFrame 解码 FRAME 事件 [u32 target][u64 mono][u8 key][u32 len][au]。
func decodeFrame(p []byte) (Frame, error) {
	if len(p) < 17 {
		return Frame{}, fmt.Errorf("desktoppipe: frame payload %d bytes, want >= 17", len(p))
	}
	ln := binary.LittleEndian.Uint32(p[13:])
	if uint64(len(p)) != 17+uint64(ln) {
		return Frame{}, fmt.Errorf("desktoppipe: frame length mismatch: len field %d, payload %d", ln, len(p))
	}
	return Frame{
		Key:    p[12] != 0,
		MonoUs: binary.LittleEndian.Uint64(p[4:12]),
		AU:     append([]byte(nil), p[17:]...),
	}, nil
}

func decodeHostHello(p []byte) (*HelloInfo, error) {
	if len(p) != 20 {
		return nil, fmt.Errorf("desktoppipe: host_hello payload %d bytes, want 20", len(p))
	}
	return &HelloInfo{
		Gen:     binary.LittleEndian.Uint32(p),
		W:       binary.LittleEndian.Uint32(p[4:]),
		H:       binary.LittleEndian.Uint32(p[8:]),
		Fps:     binary.LittleEndian.Uint32(p[12:]),
		MaxSubs: binary.LittleEndian.Uint32(p[16:]),
	}, nil
}

// decodeState 解码 STATE 事件 [char code[32]][u8 recoverable]。
func decodeState(p []byte) (StateEvent, error) {
	if len(p) != 33 {
		return StateEvent{}, fmt.Errorf("desktoppipe: state payload %d bytes, want 33", len(p))
	}
	code := p[:32]
	if i := bytes.IndexByte(code, 0); i >= 0 {
		code = code[:i]
	}
	return StateEvent{Code: string(code), Recoverable: p[32] != 0}, nil
}

// decodeCursor 解码 0x0109 光标事件 [s32 x][s32 y][u8 visible]。
func decodeCursor(p []byte) (CursorEvent, error) {
	if len(p) != 9 {
		return CursorEvent{}, fmt.Errorf("desktoppipe: cursor payload %d bytes, want 9", len(p))
	}
	return CursorEvent{
		X:       int32(binary.LittleEndian.Uint32(p)),
		Y:       int32(binary.LittleEndian.Uint32(p[4:])),
		Visible: p[8] != 0,
	}, nil
}

// EncodeInputMsg 编码 0x0108 payload `[u32 sub_id][u64 seq][u8 type]
// [payload']`(与 native rt_pipe_server.h EncodeInputMsg 逐字节一致);
// 非法 type 返回 nil。不做语义校验(域校验在 agent 预校验层)。
func EncodeInputMsg(m *InputMsg) []byte {
	var p []byte
	switch m.Type {
	case InputMove:
		p = make([]byte, 23)
		binary.LittleEndian.PutUint32(p[13:], uint32(m.X))
		binary.LittleEndian.PutUint32(p[17:], uint32(m.Y))
		binary.LittleEndian.PutUint16(p[21:], m.Buttons)
	case InputButton:
		p = make([]byte, 15)
		p[13], p[14] = m.Btn, m.Down
	case InputWheel:
		p = make([]byte, 22)
		binary.LittleEndian.PutUint32(p[13:], uint32(m.X))
		binary.LittleEndian.PutUint32(p[17:], uint32(m.Y))
		p[21] = m.Trackpad
	case InputKey:
		p = make([]byte, 17)
		binary.LittleEndian.PutUint16(p[13:], m.Scan)
		p[15], p[16] = m.Down, m.Extended
	case InputText:
		if len(m.Text) > maxInputTextUnits {
			return nil
		}
		p = make([]byte, 15+2*len(m.Text))
		binary.LittleEndian.PutUint16(p[13:], uint16(len(m.Text)))
		for i, u := range m.Text {
			binary.LittleEndian.PutUint16(p[15+2*i:], u)
		}
	case InputLock:
		p = make([]byte, 15)
		p[13], p[14] = m.Caps, m.Num
	default:
		return nil
	}
	binary.LittleEndian.PutUint32(p, m.SubID)
	binary.LittleEndian.PutUint64(p[4:], m.Seq)
	p[12] = m.Type
	return p
}

// respText 提取 FlagError 帧的 ASCII 错误码(不可打印则给字节数)。
func respText(f *ipc.Frame) string {
	if len(f.Payload) == 0 {
		return "<empty>"
	}
	for _, b := range f.Payload {
		if b < 0x20 || b > 0x7E {
			return fmt.Sprintf("<%d binary bytes>", len(f.Payload))
		}
	}
	return string(f.Payload)
}
