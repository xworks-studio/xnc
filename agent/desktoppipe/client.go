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
//	MSG_FRAME_V2     0x0205 event (M1 Task 2/3;host 以 XNC_DESKTOP_PIPELINE_V2
//	                            门控):72 字节 LE 固定头(header_bytes + 六个
//	                            身份 u64 + w/h + flags + payload_len + crc32c)
//	                            + Annex-B 载荷;仅 hello media_protocol=2 时
//	                            使用,v1 连接收到即拒(反之亦然)
//	MSG_HOST_HELLO   0x0106 event [u32 gen][u32 w][u32 h][u32 fps][u32 max_subs]
//	                            (M1 Task 2:管道 v2 时尾随 [u32 media_protocol=2])
//	MSG_STATE        0x0107 event [char code[32]][u8 recoverable]
//	MSG_INPUT        0x0108 req   [u32 sub_id][u64 seq][u8 type][payload']
//	                            (M1-Slice3 出站;payload' 布局见 InputMsg)
//	MSG_CURSOR       0x0109 event [s32 x][s32 y][u8 visible]
//	                            (M1-Slice3 入站;HOST_HELLO 流空间逻辑 px)
//	MSG_DISPLAY_CHANGED 0x010A event [u32 gen][u32 w][u32 h][char reason[24]]
//	                            (M2-Slice1 Task 2 入站;统一 CaptureReset 改变
//	                            流几何时广播;消费侧视作 HOST_HELLO 更新)
//	MSG_STREAM_DISCONTINUITY 0x020B event (M1 Task 4;v2 模式):[u64 capture_epoch]
//	                            [u64 codec_epoch][char reason[32]](固定 reason)
//	                            host 在 epoch 前进(capture 重建)时广播;消费侧
//	                            清空缓冲帧并 WAIT_IDR,直到该 epoch 对的 IDR。
//	                            v1 连接收到即拒(反之亦然,镜像 0x0205)。
//
// sub_id 由调用方选定(非 0、每 host 唯一——core 侧 StartCapture 每次会话
// 一连接,随机 u32 即可)。ATTACH 成功无显式响应:HOST_HELLO 即确认
// (T2 服务端契约);失败形态 = ATTACH FlagError 帧或 STATE{too_many_subs}
// 后断连。消费模型:FrameCh/StateCh 缓冲通道;FrameCh 满时清空缓冲帧 +
// 合并请求一次 keyframe 并抑制 delta 直到 IDR(host 侧 WAIT_IDR 状态机的
// 镜像;M1 Task 4),key 帧非阻塞送达(满时先排空缓冲帧)。
package desktoppipe

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"net"
	"os"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"xnc/proto/ipc"
)

// 消息类型(0x0102-0x010A, 0x0205;native/desktop/rt_pipe_server.h 镜像)。
const (
	msgAttach      uint16 = 0x0102
	msgDetach      uint16 = 0x0103
	msgKeyframeReq uint16 = 0x0104
	msgFrame       uint16 = 0x0105
	msgHostHello   uint16 = 0x0106
	msgState       uint16 = 0x0107
	msgInput       uint16 = 0x0108
	msgCursor      uint16 = 0x0109
	msgDisplayChg  uint16 = 0x010A
	msgSwitchDisp  uint16 = 0x0128
	msgFrameV2     uint16 = 0x0205 // M1 Task 2/3:v2 已验证媒体帧
	msgStreamDisc  uint16 = 0x020B // M1 Task 4:断流(v2 模式;v1 收到即拒)
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
	// helloChDepth 覆盖 HOST_HELLO / 0x010A 更新(稀疏;满丢)。
	helloChDepth = 4
	// displayChDepth 覆盖分辨率/拓扑变化事件(极稀疏)。
	displayChDepth = 4
	// reasonLen 是 KEYFRAME_REQ reason 字段宽度(NUL 填充,有效 31)。
	reasonLen = 32
	// displayReasonLen 是 0x010A reason 字段宽度(NUL 填充,有效 23)。
	displayReasonLen = 24
	// displayEntryLen 是 HOST_HELLO displays[] 每项字节数(M2-S3 Task 5):
	// [u32 idx][s32 ox][s32 oy][u32 w][u32 h][u8 primary]。
	displayEntryLen = 21

	// v2HeaderBytes 是 0x0205 载荷的固定头长度(M1 Task 2;镜像 native
	// kV2HeaderBytes):[u32 header_bytes][u64 capture_epoch][u64 codec_epoch]
	// [u64 content_id][u64 encode_seq][u64 source_mono_us]
	// [u64 present_mono_us][u32 width][u32 height][u32 flags]
	// [u32 payload_len][u32 crc32c]。
	v2HeaderBytes = 72
	// maxAuBytes 是 v2 载荷上限(镜像 native kMaxAuBytes =
	// proto.MaxSessionFrameBytes)。
	maxAuBytes = 8 << 20
	// mediaProtocolV2 是扩展 HOST_HELLO 尾随的媒体协议版本号。
	mediaProtocolV2 uint32 = 2
	// flagKey 是 v2 flags 的 key 位(镜像 native AuFlags::kAuFlagKey)。
	flagKey uint32 = 1 << 0
)

// crc32cTable 是 v2 帧 CRC32C(Castagnoli,反射多项式 0x82F63B78,
// init/xorout 0xFFFFFFFF)查找表(镜像 native crc32c.h)。
var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// zero4 是 v2 CRC 覆盖中 crc 字段原位为零的 4 字节段。
var zero4 = [4]byte{}

// SubOpts 是 ATTACH 携带的整形参数(0 = 未指定,由 host 用默认)。
type SubOpts struct {
	MaxFPS  uint32
	MaxW    uint32
	Bitrate uint32
}

// Frame 是一条解码后的视频 AU(Annex-B,统一 4 字节起始码)。v2(0x0205)
// 帧携带完整媒体身份(镜像 native FrameIdentity);v1(0x0105)帧仅
// Key/PresentMonoUs/AU 有效,身份字段为 0。
type Frame struct {
	Key                         bool
	CaptureEpoch, CodecEpoch    uint64
	ContentID, EncodeSeq        uint64
	SourceMonoUs, PresentMonoUs uint64
	W, H                        uint32
	AU                          []byte
}

// HelloInfo 是 HOST_HELLO 内容;gen 递增代表 capture 重建(T2 语义)。
// Displays 是 M2-S3 Task 5 的 displays[] 块(旧 server 不携带 = nil);
// W/H 恒为「当前活动显示器」几何(与 displays[] 中某一项一致)。
// MediaProtocol 是 v2 扩展的尾随 u32(0 = 旧 host / 无字段,M1 Task 2)。
type HelloInfo struct {
	Gen           uint32
	W, H          uint32
	Fps           uint32
	MaxSubs       uint32
	Displays      []Display
	MediaProtocol uint32
}

// Display 是 HOST_HELLO displays[] 的一项(M2-S3 Task 5;与 native
// DisplayInfo 逐字段镜像)。Index 为稳定表索引(0x0128 携带同一值)。
type Display struct {
	Index   uint32
	OriginX int32
	OriginY int32
	W, H    uint32
	Primary bool
}

// StateEvent 是 STATE 事件(stable code + 可恢复性)。
type StateEvent struct {
	Code        string
	Recoverable bool
}

// DisplayChanged 是 0x010A 事件:统一 CaptureReset 改变了流几何
// (M2-Slice1 Task 2)。Gen 与随后 HOST_HELLO 的 gen 一致;Reason 为
// reset reason 稳定串(resolution / desktop_switch / change_backend…)。
type DisplayChanged struct {
	Gen    uint32
	W, H   uint32
	Reason string
}

// StreamDiscontinuity 是 0x020B 事件(M1 Task 4;仅 v2 连接):epoch 前进
// (capture 重建)时 host 广播 —— 消费者须丢弃缓冲帧并等待所携 epoch 对
// 的 IDR。Reason 为固定串(kStreamDiscontinuityReason 镜像)。
type StreamDiscontinuity struct {
	CaptureEpoch, CodecEpoch uint64
	Reason                   string
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

	mu         sync.Mutex // 守护 hello/mediaProto/needKey/closed/pumpErr
	hello      *HelloInfo
	mediaProto uint32 // 协商媒体协议(hello 尾随字段;0 = v1)
	needKey    bool   // WAIT_IDR 镜像:抑制 delta 直到下一个 IDR(泵独占读)
	closed     bool
	pumpErr    error
	closeOne   sync.Once
	closeErr   error
	ledger     frameLedger // v2 帧身份单调性(pump 独占)

	frameCh   chan Frame
	stateCh   chan StateEvent
	cursorCh  chan CursorEvent
	helloCh   chan HelloInfo // HOST_HELLO + 0x010A 更新流
	displayCh chan DisplayChanged
	done      chan struct{}
	doneOnce  sync.Once
}

// frameIdentity 是 v2 帧身份中参与单调性校验的字段(镜像 native
// FrameIdentity 的子集)。
type frameIdentity struct {
	captureEpoch, codecEpoch uint64
	contentID, encodeSeq     uint64
}

// frameLedger 镜像 native FrameIdentityLedger.Accept 的单调性规则:
// epoch 前进重基线;epoch/content 回退、seq 重复(<=)拒绝。
type frameLedger struct {
	hasLast bool
	last    frameIdentity
}

func (l *frameLedger) accept(id frameIdentity) bool {
	if !l.hasLast {
		l.hasLast = true
		l.last = id
		return true
	}
	if id.captureEpoch < l.last.captureEpoch {
		return false
	}
	if id.captureEpoch > l.last.captureEpoch {
		l.last = id
		return true
	}
	if id.codecEpoch < l.last.codecEpoch {
		return false
	}
	if id.codecEpoch > l.last.codecEpoch {
		l.last = id
		return true
	}
	if id.contentID < l.last.contentID {
		return false
	}
	if id.encodeSeq <= l.last.encodeSeq {
		return false
	}
	l.last = id
	return true
}

// acceptDiscontinuity 校验 0x020B 所携 epoch 对相对已收身份不回退
// (M1 Task 4;镜像 accept 的 epoch 序,不改动基线——基线仍由后续帧的
// accept 推进)。说 epoch 谎的 host 按身份回归致命下线,不走 WAIT_IDR。
func (l *frameLedger) acceptDiscontinuity(ce, ke uint64) bool {
	if !l.hasLast {
		return true
	}
	if ce < l.last.captureEpoch {
		return false
	}
	if ce > l.last.captureEpoch {
		return true
	}
	return ke >= l.last.codecEpoch
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
		frameCh:   make(chan Frame, frameChDepth),
		stateCh:   make(chan StateEvent, stateChDepth),
		cursorCh:  make(chan CursorEvent, cursorChDepth),
		helloCh:   make(chan HelloInfo, helloChDepth),
		displayCh: make(chan DisplayChanged, displayChDepth),
		done:      make(chan struct{}),
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
			s.mediaProto = h.MediaProtocol
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
		case msgDisplayChg:
			// ATTACH 窗口内到达的 0x010A(挂起期 attach 的恢复竞态):按
			// hello 更新处理,事件也走非阻塞投递。
			if ev, err := decodeDisplayChanged(f.Payload); err == nil {
				s.mu.Lock()
				s.hello = displayAsHello(ev, s.hello)
				h := *s.hello
				s.mu.Unlock()
				select {
				case s.helloCh <- h:
				default:
				}
				select {
				case s.displayCh <- ev:
				default:
				}
			}
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

// HelloCh 交付 HOST_HELLO / 0x010A 合成的 hello 更新(副本);连接终结后
// 关闭。满时丢弃(Hello() 恒有最新值;本通道是"想要推送"的消费侧便利)。
func (s *Sub) HelloCh() <-chan HelloInfo { return s.helloCh }

// DisplayCh 交付 0x010A DISPLAY_CHANGED 事件;连接终结后关闭。满时丢弃
// (事件稀疏;Hello()/HelloCh 承载最新几何)。
func (s *Sub) DisplayCh() <-chan DisplayChanged { return s.displayCh }

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

// SendSwitchDisplay 发送 0x0128 [u32 idx](M2-Slice3 Task 5)。host 校验
// idx < displays 数量:合法 → 统一 reset(reason=switch)重建绑定新输出
// (随后 HOST_HELLO 重发 + DISPLAY_CHANGED reason=switch);非法 → STATE
// {invalid_display}(可恢复)且不 reset。host 端能力/仲裁由调用方负责。
func (s *Sub) SendSwitchDisplay(idx uint32) error {
	p := make([]byte, 4)
	binary.LittleEndian.PutUint32(p, idx)
	return s.writeCtrl(&ipc.Frame{MessageType: msgSwitchDisp, RequestID: 1, Payload: p})
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

// pump 是唯一读方:FRAME → FrameCh(WAIT_IDR 镜像:满则清空 + 合并
// keyframe 请求,needKey 抑制 delta,key 非阻塞送达)、0x020B → 清空 +
// needKey、HOST_HELLO → Hello()、STATE → StateCh;读终结时关闭全部通道。
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
			if s.mediaProto == mediaProtocolV2 {
				s.teardown(fmt.Errorf("desktoppipe: v1 frame on v2 media connection"))
				return
			}
			frm, err := decodeFrame(f.Payload)
			if err != nil {
				s.teardown(err)
				return
			}
			if !s.deliverFrame(frm) {
				return
			}
		case msgFrameV2:
			if s.mediaProto != mediaProtocolV2 {
				s.teardown(fmt.Errorf("desktoppipe: v2 frame on v1 media connection"))
				return
			}
			frm, err := decodeFrameV2(f.Payload)
			if err != nil {
				s.teardown(err)
				return
			}
			// 身份单调性在进 FrameCh 前校验:回归帧直接拒收并下线。
			if !s.ledger.accept(frameIdentity{
				captureEpoch: frm.CaptureEpoch,
				codecEpoch:   frm.CodecEpoch,
				contentID:    frm.ContentID,
				encodeSeq:    frm.EncodeSeq,
			}) {
				s.teardown(fmt.Errorf("desktoppipe: v2 frame identity regression: capture_epoch=%d codec_epoch=%d content_id=%d encode_seq=%d",
					frm.CaptureEpoch, frm.CodecEpoch, frm.ContentID, frm.EncodeSeq))
				return
			}
			if !s.deliverFrame(frm) {
				return
			}
		case msgStreamDisc:
			// M1 Task 4:0x020B 是 v2 模式消息(v1 收到即拒,镜像 0x0205)。
			if s.mediaProto != mediaProtocolV2 {
				s.teardown(fmt.Errorf("desktoppipe: v2 stream discontinuity on v1 media connection"))
				return
			}
			ev, err := decodeStreamDiscontinuity(f.Payload)
			if err != nil {
				s.teardown(err)
				return
			}
			// 断流携带的 epoch 对必须不回退:说谎的 host 按身份回归下线
			// (WAIT_IDR 只恢复 overflow/discontinuity/epoch change)。
			if !s.ledger.acceptDiscontinuity(ev.CaptureEpoch, ev.CodecEpoch) {
				s.teardown(fmt.Errorf("desktoppipe: stream discontinuity epoch regression: capture_epoch=%d codec_epoch=%d",
					ev.CaptureEpoch, ev.CodecEpoch))
				return
			}
			// 清空缓冲帧(旧 epoch 已失效)→ WAIT_IDR(needKey):host 侧
			// reset 已强制该 epoch 对的 IDR,无需再发合并请求。
			s.drainFrameCh()
			s.mu.Lock()
			s.needKey = true
			s.mu.Unlock()
		case msgHostHello:
			h, err := decodeHostHello(f.Payload)
			if err != nil {
				s.teardown(err)
				return
			}
			s.mu.Lock()
			s.hello = h
			s.mediaProto = h.MediaProtocol
			s.mu.Unlock()
			select {
			case s.helloCh <- *h:
			case <-s.done:
				s.teardown(nil)
				return
			default: // 满则丢弃(Hello() 恒有最新值)
			}
		case msgDisplayChg:
			ev, err := decodeDisplayChanged(f.Payload)
			if err != nil {
				s.teardown(err)
				return
			}
			s.mu.Lock()
			h := displayAsHello(ev, s.hello) // fps/max_subs 继承旧值
			s.hello = h
			s.mu.Unlock()
			select {
			case s.displayCh <- ev:
			case <-s.done:
				s.teardown(nil)
				return
			default: // 满则丢弃(极稀疏;hello 已更新)
			}
			select {
			case s.helloCh <- *h:
			default:
			}
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

// deliverFrame 交付一帧到 FrameCh(M1 Task 4:host 侧 WAIT_IDR 状态机的
// 消费侧镜像)。needKey(WAIT_IDR)期间抑制一切 delta;delta 满时溢出:
// 清空缓冲帧(已缺解码参考,不可补回)、置 needKey、合并请求恰一次
// keyframe;key 帧非阻塞送达(送达前先排空缓冲帧,缓冲帧相对新 key 已
// 过期且清空后必有容量)。返回 false 表示泵已 teardown(调用方须退出)。
func (s *Sub) deliverFrame(frm Frame) bool {
	s.mu.Lock()
	needKey := s.needKey
	s.mu.Unlock()
	if frm.Key {
		// 排空再送达:key 永不阻塞泵(慢消费侧由溢出路径合并请求 IDR)。
		s.drainFrameCh()
		select {
		case s.frameCh <- frm:
		case <-s.done:
			s.teardown(nil)
			return false
		}
		s.mu.Lock()
		s.needKey = false // IDR 已送达,合并请求清位
		s.mu.Unlock()
		return true
	}
	if needKey {
		return true // WAIT_IDR:抑制 delta,直到 IDR
	}
	select {
	case s.frameCh <- frm:
		return true
	case <-s.done:
		s.teardown(nil)
		return false
	default:
		// 消费侧落后:清空缓冲帧(§9 客户端镜像),置 needKey,合并请求
		// 恰一次 keyframe。
		s.drainFrameCh()
		s.mu.Lock()
		first := !s.needKey
		s.needKey = true
		s.mu.Unlock()
		if first {
			go func() { _ = s.RequestKeyframe("client_overflow") }() //nolint:errcheck // 记账请求,失败随连接终结
		}
	}
	return true
}

// drainFrameCh 非阻塞排空 FrameCh(泵是唯一发送方,排空后再投递必有余量;
// 溢出/断流时被丢的缓冲帧是不可恢复的历史,丢弃正是恢复语义)。
func (s *Sub) drainFrameCh() {
	for {
		select {
		case <-s.frameCh:
		default:
			return
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
	close(s.helloCh)
	close(s.displayCh)
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
		Key:           p[12] != 0,
		PresentMonoUs: binary.LittleEndian.Uint64(p[4:12]),
		AU:            append([]byte(nil), p[17:]...),
	}, nil
}

// decodeFrameV2 解码并校验 0x0205 v2 媒体帧。校验顺序镜像 native
// DecodeFrameEventV2:完整头 → header_bytes == 72 → payload_len <= 8 MiB
// → 精确总长(无尾随字节)→ CRC32C——全部通过才分配载荷。
func decodeFrameV2(p []byte) (Frame, error) {
	if len(p) < v2HeaderBytes {
		return Frame{}, fmt.Errorf("desktoppipe: v2 frame payload %d bytes, want >= %d", len(p), v2HeaderBytes)
	}
	if hb := binary.LittleEndian.Uint32(p); hb != v2HeaderBytes {
		return Frame{}, fmt.Errorf("desktoppipe: v2 frame header_bytes %d, want %d", hb, v2HeaderBytes)
	}
	payloadLen := binary.LittleEndian.Uint32(p[64:])
	if payloadLen > maxAuBytes {
		return Frame{}, fmt.Errorf("desktoppipe: v2 frame payload_len %d exceeds 8 MiB", payloadLen)
	}
	if uint64(len(p)) != uint64(v2HeaderBytes)+uint64(payloadLen) {
		return Frame{}, fmt.Errorf("desktoppipe: v2 frame length mismatch: header %d + payload_len %d, got %d",
			v2HeaderBytes, payloadLen, len(p))
	}
	// CRC32C 覆盖 zero-crc 头 + 载荷:hash [0,68) + 4 个零字节 + 载荷
	// (镜像 native 的链式 Crc32c)。
	crc := crc32.Update(0, crc32cTable, p[:68])
	crc = crc32.Update(crc, crc32cTable, zero4[:])
	crc = crc32.Update(crc, crc32cTable, p[72:])
	if stored := binary.LittleEndian.Uint32(p[68:]); crc != stored {
		return Frame{}, fmt.Errorf("desktoppipe: v2 frame crc32c mismatch: got %#08x, want %#08x", crc, stored)
	}
	flags := binary.LittleEndian.Uint32(p[60:])
	return Frame{
		Key:           flags&flagKey != 0,
		CaptureEpoch:  binary.LittleEndian.Uint64(p[4:]),
		CodecEpoch:    binary.LittleEndian.Uint64(p[12:]),
		ContentID:     binary.LittleEndian.Uint64(p[20:]),
		EncodeSeq:     binary.LittleEndian.Uint64(p[28:]),
		SourceMonoUs:  binary.LittleEndian.Uint64(p[36:]),
		PresentMonoUs: binary.LittleEndian.Uint64(p[44:]),
		W:             binary.LittleEndian.Uint32(p[52:]),
		H:             binary.LittleEndian.Uint32(p[56:]),
		AU:            append([]byte(nil), p[72:]...),
	}, nil
}

// decodeHostHello 解码 HOST_HELLO:先按 legacy 形态(20B 或 24+21n)解;
// 失败再尝试 v2 扩展(尾随 u32 media_protocol=2;镜像 native 的先
// DecodeHostHello 后 DecodeHostHelloV2 顺序,避免 displays 尾 4 字节
// 巧合 == 2 的歧义)。两者皆失败 = 未知版本,拒绝。
func decodeHostHello(p []byte) (*HelloInfo, error) {
	if h, err := decodeHostHelloLegacy(p); err == nil {
		return h, nil
	}
	if len(p) >= 28 {
		mp := binary.LittleEndian.Uint32(p[len(p)-4:])
		if mp == mediaProtocolV2 {
			if h, err := decodeHostHelloLegacy(p[:len(p)-4]); err == nil {
				h.MediaProtocol = mp
				return h, nil
			}
		}
	}
	return nil, fmt.Errorf("desktoppipe: host_hello payload %d bytes, want 20, 24+21n or v2-extended", len(p))
}

// decodeHostHelloLegacy 解码 legacy HOST_HELLO(M2-S3 Task 5:20B 无
// displays,或 24B + 21B*n 扩展载荷)。
func decodeHostHelloLegacy(p []byte) (*HelloInfo, error) {
	if len(p) != 20 && (len(p) < 24 || (len(p)-24)%displayEntryLen != 0) {
		return nil, fmt.Errorf("desktoppipe: host_hello payload %d bytes, want 20 or 24+21n", len(p))
	}
	h := &HelloInfo{
		Gen:     binary.LittleEndian.Uint32(p),
		W:       binary.LittleEndian.Uint32(p[4:]),
		H:       binary.LittleEndian.Uint32(p[8:]),
		Fps:     binary.LittleEndian.Uint32(p[12:]),
		MaxSubs: binary.LittleEndian.Uint32(p[16:]),
	}
	if len(p) >= 24 {
		n := binary.LittleEndian.Uint32(p[20:])
		if uint64(len(p)-24) != uint64(n)*displayEntryLen {
			return nil, fmt.Errorf("desktoppipe: host_hello displays count %d vs payload %d", n, len(p))
		}
		h.Displays = make([]Display, 0, n)
		for i := uint32(0); i < n; i++ {
			e := p[24+i*displayEntryLen:]
			h.Displays = append(h.Displays, Display{
				Index:   binary.LittleEndian.Uint32(e),
				OriginX: int32(binary.LittleEndian.Uint32(e[4:])),
				OriginY: int32(binary.LittleEndian.Uint32(e[8:])),
				W:       binary.LittleEndian.Uint32(e[12:]),
				H:       binary.LittleEndian.Uint32(e[16:]),
				Primary: e[20] != 0,
			})
		}
	}
	return h, nil
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

// decodeStreamDiscontinuity 解码 0x020B 事件
// [u64 capture_epoch][u64 codec_epoch][char reason[32]](M1 Task 4;镜像
// native DecodeStreamDiscontinuity:48 字节精确长度)。
func decodeStreamDiscontinuity(p []byte) (StreamDiscontinuity, error) {
	if len(p) != 48 {
		return StreamDiscontinuity{}, fmt.Errorf("desktoppipe: stream discontinuity payload %d bytes, want 48", len(p))
	}
	reason := p[16:48]
	if i := bytes.IndexByte(reason, 0); i >= 0 {
		reason = reason[:i]
	}
	return StreamDiscontinuity{
		CaptureEpoch: binary.LittleEndian.Uint64(p),
		CodecEpoch:   binary.LittleEndian.Uint64(p[8:]),
		Reason:       string(reason),
	}, nil
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

// decodeDisplayChanged 解码 0x010A 事件
// [u32 gen][u32 w][u32 h][char reason[24]](M2-Slice1 Task 2)。
func decodeDisplayChanged(p []byte) (DisplayChanged, error) {
	if len(p) != 12+displayReasonLen {
		return DisplayChanged{}, fmt.Errorf("desktoppipe: display_changed payload %d bytes, want %d",
			len(p), 12+displayReasonLen)
	}
	reason := p[12 : 12+displayReasonLen]
	if i := bytes.IndexByte(reason, 0); i >= 0 {
		reason = reason[:i]
	}
	return DisplayChanged{
		Gen:    binary.LittleEndian.Uint32(p),
		W:      binary.LittleEndian.Uint32(p[4:]),
		H:      binary.LittleEndian.Uint32(p[8:]),
		Reason: string(reason),
	}, nil
}

// displayAsHello 把一条 0x010A 合成 hello 视图(gen/w/h;事件不携带
// fps/max_subs/displays/media_protocol,从 prev 继承,prev 为 nil 时留 0)。
func displayAsHello(ev DisplayChanged, prev *HelloInfo) *HelloInfo {
	h := &HelloInfo{Gen: ev.Gen, W: ev.W, H: ev.H}
	if prev != nil {
		h.Fps = prev.Fps
		h.MaxSubs = prev.MaxSubs
		h.Displays = prev.Displays // 0x010A 不携带 displays;沿用旧表
		h.MediaProtocol = prev.MediaProtocol
	}
	return h
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
