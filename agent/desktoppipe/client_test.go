//go:build windows

// client_test.go — desktoppipe 用例:进程内假 desktop host(测试专用,
// 仅说 0x0102-0x0107 固定二进制协议;握手半边复用 coreclient.
// ServerHandshake),覆盖 Dial(握手+ATTACH+HOST_HELLO)、FRAME/STATE
// 泵、RequestKeyframe 的 reason 透传、Close 的 DETACH、错误路径
// (secret 不一致 / too_many_subs / sub_id=0)。payload 布局在测试侧
// 独立手写,镜像 native/desktop/rt_pipe_server.h(双实现交叉)。
package desktoppipe

import (
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"xnc/agent/coreclient"
	"xnc/proto/ipc"
)

// ---- 测试侧编码器(布局 = C++ rt_pipe_server.h)----

func tPut32(p []byte, off int, v uint32) { binary.LittleEndian.PutUint32(p[off:], v) }

// s32u:负常量直接转 uint32 是编译错(常量溢出),经变量转即可。
func s32u(v int32) uint32                { return uint32(v) }
func tPut64(p []byte, off int, v uint64) { binary.LittleEndian.PutUint64(p[off:], v) }

func tEncHostHello(gen, w, h, fps, maxSubs uint32) []byte {
	p := make([]byte, 20)
	tPut32(p, 0, gen)
	tPut32(p, 4, w)
	tPut32(p, 8, h)
	tPut32(p, 12, fps)
	tPut32(p, 16, maxSubs)
	return p
}

func tEncFrame(monoUs uint64, key bool, au []byte) []byte {
	p := make([]byte, 17+len(au))
	tPut32(p, 0, 0) // sub_id_target = 0(广播)
	tPut64(p, 4, monoUs)
	if key {
		p[12] = 1
	}
	tPut32(p, 13, uint32(len(au)))
	copy(p[17:], au)
	return p
}

func tEncState(code string, recoverable bool) []byte {
	p := make([]byte, 33)
	copy(p, code)
	if recoverable {
		p[32] = 1
	}
	return p
}

// ---- Pipe v2(0x0205)测试侧编码器(布局 = native EncodeFrameEventV2;
// 双实现交叉;黄金向量单独由 native 逐字节输出固定,见 v2GoldenHex)----

// tCRC 是测试侧 CRC32C 表(Castagnoli;与 native Crc32c 同语义)。
var tCRC = crc32.MakeTable(crc32.Castagnoli)

// tEncFrameV2 编码 0x0205 载荷:72 字节 LE 固定头
// (header_bytes + 六个身份 u64 + w/h + flags + payload_len + crc32c)
// + Annex-B AU。
func tEncFrameV2(ce, ke, content, seq, src, pres uint64, w, h, flags uint32, au []byte) []byte {
	p := make([]byte, 72+len(au))
	tPut32(p, 0, 72)
	tPut64(p, 4, ce)
	tPut64(p, 12, ke)
	tPut64(p, 20, content)
	tPut64(p, 28, seq)
	tPut64(p, 36, src)
	tPut64(p, 44, pres)
	tPut32(p, 52, w)
	tPut32(p, 56, h)
	tPut32(p, 60, flags)
	tPut32(p, 64, uint32(len(au)))
	copy(p[72:], au)
	c := crc32.Update(0, tCRC, p[:68])
	c = crc32.Update(c, tCRC, make([]byte, 4)) // crc 字段原位为零的 4 字节
	c = crc32.Update(c, tCRC, p[72:])
	tPut32(p, 68, c)
	return p
}

// tEncHostHelloV2 编码 v2 HOST_HELLO:legacy 载荷 + 尾随 u32 media_protocol=2
// (native EncodeHostHelloV2;base 恒含 displays count 字段)。
func tEncHostHelloV2(gen, w, h, fps, maxSubs uint32) []byte {
	p := make([]byte, 20)
	tPut32(p, 0, gen)
	tPut32(p, 4, w)
	tPut32(p, 8, h)
	tPut32(p, 12, fps)
	tPut32(p, 16, maxSubs)
	p = append(p, 0, 0, 0, 0) // displays count = 0
	p = append(p, 0, 0, 0, 0)
	tPut32(p, len(p)-4, 2) // media_protocol = 2
	return p
}

// ---- v2 假 host:握手 → ATTACH → HOST_HELLO(v1 或 v2 载荷)→ 按序下发消息 ----

type v2Msg struct {
	msgType uint16
	payload []byte
}

// v2Host 支持任意 hello 载荷 + 任意后续消息序列(0x0205/0x0105 混排),
// 供版本失配 / 身份回归 / 正常 v2 泵测试复用。
type v2Host struct {
	ln     net.Listener
	secret string
	hello  []byte
	msgs   []v2Msg
}

func startV2Host(t *testing.T, secret string, hello []byte, msgs []v2Msg) *v2Host {
	return startV2HostNamed(t, t.Name(), secret, hello, msgs)
}

// startV2HostNamed 允许同一测试内起多个 host(pipe 名加后缀去重)。
func startV2HostNamed(t *testing.T, name, secret string, hello []byte, msgs []v2Msg) *v2Host {
	t.Helper()
	ln, err := winio.ListenPipe(`\\.\pipe\xnc-desktoppipe-test-`+name, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	h := &v2Host{ln: ln, secret: secret, hello: hello, msgs: msgs}
	go h.serve()
	return h
}

func (h *v2Host) name() string { return h.ln.Addr().String() }

func (h *v2Host) serve() {
	conn, err := h.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	if err := coreclient.ServerHandshake(conn, []byte(h.secret)); err != nil {
		return
	}
	f, err := ipc.ReadFrame(conn)
	if err != nil || f.MessageType != msgAttach {
		return
	}
	if err := ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgHostHello,
		Payload: h.hello}); err != nil {
		return
	}
	for _, m := range h.msgs {
		if err := ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: m.msgType,
			Payload: m.payload}); err != nil {
			return
		}
	}
	for { // 读循环:KEYFRAME_REQ 等忽略;断连即退出
		if _, err := ipc.ReadFrame(conn); err != nil {
			return
		}
	}
}

// fakeHost 观察:ATTACH payload / KEYFRAME_REQ reason / DETACH sub_id /
// INPUT 0x0108 原始 payload;pushCursor 可主动下发 0x0109。
type fakeHost struct {
	ln        net.Listener
	secret    []byte
	attachCh  chan []byte // 原始 ATTACH payload
	kfCh      chan string // KEYFRAME_REQ reason
	detachCh  chan uint32 // DETACH sub_id
	inputCh   chan []byte // MSG_INPUT 原始 payload(Slice3)
	rejectSub bool        // true: ATTACH 后回 STATE{too_many_subs} 并断连

	wmu  sync.Mutex // 串行化事件写(burst 与 cursor)
	conn net.Conn
}

func startFakeHost(t *testing.T, secret string, rejectSub bool) *fakeHost {
	t.Helper()
	ln, err := winio.ListenPipe(`\\.\pipe\xnc-desktoppipe-test-`+t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	h := &fakeHost{
		ln: ln, secret: []byte(secret), rejectSub: rejectSub,
		attachCh: make(chan []byte, 1),
		kfCh:     make(chan string, 1),
		detachCh: make(chan uint32, 1),
		inputCh:  make(chan []byte, 8),
	}
	go h.serve()
	return h
}

func (h *fakeHost) name() string { return h.ln.Addr().String() }

// pushCursor 下发一条 0x0109 事件(HOST_HELLO 后任意时刻)。
func (h *fakeHost) pushCursor(x, y int32, visible bool) {
	p := make([]byte, 9)
	tPut32(p, 0, uint32(x))
	tPut32(p, 4, uint32(y))
	if visible {
		p[8] = 1
	}
	h.wmu.Lock()
	defer h.wmu.Unlock()
	if h.conn == nil {
		return
	}
	_ = ipc.WriteFrame(h.conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgCursor, Payload: p})
}

// tEncDisplayChanged 编码 0x010A [u32 gen][u32 w][u32 h][reason 24B NUL 填充]。
func tEncDisplayChanged(gen, w, h uint32, reason string) []byte {
	p := make([]byte, 12+24)
	tPut32(p, 0, gen)
	tPut32(p, 4, w)
	tPut32(p, 8, h)
	copy(p[12:], reason)
	return p
}

// pushDisplay 下发一条 0x010A 事件(M2-Slice1 Task 2)。
func (h *fakeHost) pushDisplay(gen, w, ht uint32, reason string) {
	h.wmu.Lock()
	defer h.wmu.Unlock()
	if h.conn == nil {
		return
	}
	_ = ipc.WriteFrame(h.conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgDisplayChg,
		Payload: tEncDisplayChanged(gen, w, ht, reason)})
}

// serve 走生产时序:握手 → ATTACH → HOST_HELLO → 两帧(key+delta)+ STATE
// → 请求循环(KEYFRAME_REQ 回一个新 key 帧;DETACH/BYE 结束)。
func (h *fakeHost) serve() {
	conn, err := h.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	h.wmu.Lock()
	h.conn = conn
	h.wmu.Unlock()
	if err := coreclient.ServerHandshake(conn, h.secret); err != nil {
		return // 客户端证明失败即断(secret 不一致用例)
	}
	f, err := ipc.ReadFrame(conn)
	if err != nil || f.MessageType != msgAttach {
		return
	}
	h.attachCh <- append([]byte(nil), f.Payload...)
	if h.rejectSub {
		_ = ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgState,
			Payload: tEncState("too_many_subs", true)})
		return // 断连
	}
	_ = ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgHostHello,
		Payload: tEncHostHello(3, 1920, 1080, 30, 4)})
	_ = ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgFrame,
		Payload: tEncFrame(111, true, []byte{0, 0, 0, 1, 0x65})})
	_ = ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgFrame,
		Payload: tEncFrame(222, false, []byte{0, 0, 0, 1, 0x41})})
	_ = ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgState,
		Payload: tEncState("capture_rebuilt", true)})
	for {
		f, err := ipc.ReadFrame(conn)
		if err != nil {
			return
		}
		switch f.MessageType {
		case msgInput:
			if len(f.Payload) >= 13 {
				h.inputCh <- append([]byte(nil), f.Payload...)
			}
		case msgKeyframeReq:
			if len(f.Payload) != 36 {
				return
			}
			reason := string(f.Payload[4:36])
			for i := 0; i < len(reason); i++ { // NUL 截断
				if reason[i] == 0 {
					reason = reason[:i]
					break
				}
			}
			h.kfCh <- reason
			_ = ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgFrame,
				Payload: tEncFrame(333, true, []byte{0, 0, 0, 1, 0x65, 0x88})})
		case msgDetach:
			if len(f.Payload) == 4 {
				h.detachCh <- binary.LittleEndian.Uint32(f.Payload)
			}
			return
		case ipc.MsgBye:
			return
		case ipc.MsgPing:
			_ = ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagResponse, MessageType: ipc.MsgPong,
				RequestID: f.RequestID})
		}
	}
}

func recvOrFatal[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
		return *new(T)
	}
}

// TestDialAndPump:全时序 —— ATTACH payload 精确校验、Hello 解码、
// key/delta FRAME、STATE、RequestKeyframe reason 透传 + 新 key 帧、
// Close 发 DETACH 且泵退出后 FrameCh 关闭。
func TestDialAndPump(t *testing.T) {
	h := startFakeHost(t, "desktop-pipe-secret", false)
	sub, err := Dial(h.name(), "desktop-pipe-secret", 7, SubOpts{MaxFPS: 30, MaxW: 1920, Bitrate: 2300000})
	if err != nil {
		t.Fatal(err)
	}

	// 服务端看到的 ATTACH payload = [u32 7][u32 30][u32 1920][u32 2300000]。
	att := recvOrFatal[[]byte](t, h.attachCh, "ATTACH payload")
	want := make([]byte, 16)
	tPut32(want, 0, 7)
	tPut32(want, 4, 30)
	tPut32(want, 8, 1920)
	tPut32(want, 12, 2300000)
	if string(att) != string(want) {
		t.Fatalf("ATTACH payload = %x, want %x", att, want)
	}

	hello := sub.Hello()
	// M2-S3:HelloInfo 含 slice 字段,不可整体比较,逐字段断言。
	if hello == nil || hello.Gen != 3 || hello.W != 1920 || hello.H != 1080 ||
		hello.Fps != 30 || hello.MaxSubs != 4 || len(hello.Displays) != 0 {
		t.Fatalf("Hello() = %+v", hello)
	}

	f1 := recvOrFatal[Frame](t, sub.FrameCh(), "key frame")
	if !f1.Key || f1.PresentMonoUs != 111 || string(f1.AU) != string([]byte{0, 0, 0, 1, 0x65}) {
		t.Fatalf("key frame = %+v", f1)
	}
	f2 := recvOrFatal[Frame](t, sub.FrameCh(), "delta frame")
	if f2.Key || f2.PresentMonoUs != 222 || string(f2.AU) != string([]byte{0, 0, 0, 1, 0x41}) {
		t.Fatalf("delta frame = %+v", f2)
	}
	st := recvOrFatal[StateEvent](t, sub.StateCh(), "state event")
	if st.Code != "capture_rebuilt" || !st.Recoverable {
		t.Fatalf("state = %+v", st)
	}

	// PLI → RequestKeyframe:reason 必须原样到达,随后收到新 key 帧。
	if err := sub.RequestKeyframe("pli"); err != nil {
		t.Fatal(err)
	}
	if got := recvOrFatal[string](t, h.kfCh, "KEYFRAME_REQ reason"); got != "pli" {
		t.Fatalf("reason = %q, want pli", got)
	}
	f3 := recvOrFatal[Frame](t, sub.FrameCh(), "post-PLI key frame")
	if !f3.Key || f3.PresentMonoUs != 333 || len(f3.AU) != 6 {
		t.Fatalf("post-PLI key frame = %+v", f3)
	}

	if err := sub.Close(); err != nil {
		t.Fatal(err)
	}
	if got := recvOrFatal[uint32](t, h.detachCh, "DETACH sub_id"); got != 7 {
		t.Fatalf("DETACH sub_id = %d, want 7", got)
	}
	// 泵退出后通道关闭:消费者 range 自然结束。
	select {
	case _, ok := <-sub.FrameCh():
		if ok {
			t.Fatal("FrameCh must be closed after Close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("FrameCh not closed after Close")
	}
	// 主动 Close 的终结语义:Done 关闭、Err 为 nil(非故障下线)。
	select {
	case <-sub.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done() not closed after Close")
	}
	if err := sub.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil after Close", err)
	}
}

// TestDisplayChangedPump:0x010A → DisplayCh 交付结构化事件 + Hello() 更新
// (gen/w/h;fps/max_subs 继承旧值)+ HelloCh 推送(M2-Slice1 Task 2)。
func TestDisplayChangedPump(t *testing.T) {
	h := startFakeHost(t, "desktop-pipe-secret", false)
	sub, err := Dial(h.name(), "desktop-pipe-secret", 8, SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close() //nolint:errcheck // teardown best-effort

	h.pushDisplay(4, 2560, 1440, "resolution")

	ev := recvOrFatal[DisplayChanged](t, sub.DisplayCh(), "display_changed event")
	if ev.Gen != 4 || ev.W != 2560 || ev.H != 1440 || ev.Reason != "resolution" {
		t.Fatalf("display event = %+v", ev)
	}
	hello := sub.Hello()
	if hello == nil || hello.Gen != 4 || hello.W != 2560 || hello.H != 1440 {
		t.Fatalf("Hello() after 0x010A = %+v", hello)
	}
	if hello.Fps != 30 || hello.MaxSubs != 4 {
		t.Fatalf("Hello() must inherit fps/max_subs, got %+v", hello)
	}
	hh := recvOrFatal[HelloInfo](t, sub.HelloCh(), "hello update push")
	if hh.Gen != 4 || hh.W != 2560 || hh.H != 1440 {
		t.Fatalf("HelloCh push = %+v", hh)
	}
}

// TestDisplayChangedBadPayload:畸形 0x010A(35 字节)→ 泵按协议错误下线。
func TestDisplayChangedBadPayload(t *testing.T) {
	h := startFakeHost(t, "desktop-pipe-secret", false)
	sub, err := Dial(h.name(), "desktop-pipe-secret", 9, SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	h.wmu.Lock()
	_ = ipc.WriteFrame(h.conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgDisplayChg,
		Payload: make([]byte, 35)})
	h.wmu.Unlock()
	select {
	case <-sub.Done():
		if sub.Err() == nil {
			t.Fatal("Err() must carry the decode failure")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pump must tear down on malformed 0x010A")
	}
}

// TestDialRejectsWrongSecret:握手证明失败必须断连报错。
func TestDialRejectsWrongSecret(t *testing.T) {
	h := startFakeHost(t, "server-secret", false)
	_, err := Dial(h.name(), "client-secret", 1, SubOpts{MaxFPS: 30, MaxW: 1920, Bitrate: 2000000})
	if err == nil {
		t.Fatal("Dial must fail when secrets differ")
	}
}

// TestDialTooManySubs:STATE{too_many_subs} → Dial 错误(带 code)。
func TestDialTooManySubs(t *testing.T) {
	h := startFakeHost(t, "desktop-pipe-secret", true)
	_, err := Dial(h.name(), "desktop-pipe-secret", 2, SubOpts{MaxFPS: 30, MaxW: 1920, Bitrate: 2000000})
	if err == nil {
		t.Fatal("Dial must fail on too_many_subs")
	}
}

// TestDialRejectsZeroSubID:sub_id=0 是服务端非法值,客户端前置拒绝。
func TestDialRejectsZeroSubID(t *testing.T) {
	if _, err := Dial(`\\.\pipe\xnc-desktoppipe-none`, "s", 0, SubOpts{}); err == nil {
		t.Fatal("Dial must reject subID 0")
	}
}

// TestInputEncodeAndCursor(M1-Slice3 Task 3):0x0108 出站 —— SendInput 自动
// 填 sub_id,host 侧逐字节黄金比对(布局 = C++ EncodeInputMsg 的第二实
// 现);EncodeInputMsg 黄金字节 + 非法 type 拒绝;0x0109 入站 → CursorCh。
func TestInputEncodeAndCursor(t *testing.T) {
	h := startFakeHost(t, "desktop-pipe-secret", false)
	sub, err := Dial(h.name(), "desktop-pipe-secret", 7, SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	// MOVE:host 收到的 payload 与手写镜像逐字节一致。
	if err := sub.SendInput(&InputMsg{Seq: 1, Type: InputMove, X: -10, Y: 20, Buttons: 3}); err != nil {
		t.Fatal(err)
	}
	got := recvOrFatal[[]byte](t, h.inputCh, "MOVE payload")
	want := make([]byte, 23)
	tPut32(want, 0, 7) // sub_id 由 Sub.SendInput 填充
	tPut64(want, 4, 1)
	want[12] = InputMove
	tPut32(want, 13, s32u(-10))
	tPut32(want, 17, 20)
	want[21], want[22] = 3, 0
	if string(got) != string(want) {
		t.Fatalf("MOVE payload = %x, want %x", got, want)
	}

	// TEXT(代理对按 unit)与 KEY:EncodeInputMsg 黄金字节。
	enc := EncodeInputMsg(&InputMsg{SubID: 7, Seq: 2, Type: InputText, Text: []uint16{0x68, 0x4E2D}})
	wantText := make([]byte, 19)
	tPut32(wantText, 0, 7)
	tPut64(wantText, 4, 2)
	wantText[12] = InputText
	binary.LittleEndian.PutUint16(wantText[13:], 2)
	binary.LittleEndian.PutUint16(wantText[15:], 0x68)
	binary.LittleEndian.PutUint16(wantText[17:], 0x4E2D)
	if string(enc) != string(wantText) {
		t.Fatalf("TEXT encode = %x, want %x", enc, wantText)
	}
	if err := sub.SendInputPayload(enc); err != nil {
		t.Fatal(err)
	}
	if got := recvOrFatal[[]byte](t, h.inputCh, "TEXT payload"); string(got) != string(wantText) {
		t.Fatalf("TEXT payload = %x", got)
	}

	// 非法 type:Encode 拒绝 + SendInput 报错。
	if EncodeInputMsg(&InputMsg{SubID: 7, Type: 99}) != nil {
		t.Fatal("EncodeInputMsg must reject unknown type")
	}
	if err := sub.SendInput(&InputMsg{Type: 99}); err == nil {
		t.Fatal("SendInput must fail on unknown type")
	}
	if err := sub.SendInput(&InputMsg{Type: InputText, Text: make([]uint16, maxInputTextUnits+1)}); err == nil {
		t.Fatal("SendInput must fail on oversized TEXT")
	}

	// 0x0109 入站 → CursorCh。
	h.pushCursor(100, -50, true)
	ev := recvOrFatal[CursorEvent](t, sub.CursorCh(), "cursor event 1")
	if ev.X != 100 || ev.Y != -50 || !ev.Visible {
		t.Fatalf("cursor = %+v", ev)
	}
	h.pushCursor(-1, 2, false)
	ev = recvOrFatal[CursorEvent](t, sub.CursorCh(), "cursor event 2")
	if ev.X != -1 || ev.Y != 2 || ev.Visible {
		t.Fatalf("cursor 2 = %+v", ev)
	}
}

// ---- FrameCh 溢出 / 断连路径(fix wave 补测)----

// overflowHost:握手 → ATTACH → HOST_HELLO 后按 burstCh 指令突发 delta;
// 每收到一条 KEYFRAME_REQ 记录 reason 并回一个 key 帧(解除客户端的
// 合并位,使下一轮溢出可再次触发)。v2 形态(v2=true)说 0x0205 帧 + v2
// hello,seq/content_id 经 wmu 串行单调递增(镜像生产编码器)。
type overflowHost struct {
	ln      net.Listener
	secret  string
	burstCh chan int
	kfCh    chan string // 每条收到的 KEYFRAME_REQ reason
	v2      bool        // v2 媒体协议(0x0205 + v2 hello)
	seq     uint64      // v2:连续 encode_seq/content_id(wmu 串行)
}

// v2 以参数注入而非事后改字段:h.v2 由 serve 读取,go h.serve() 之后写
// 即数据竞态形状(M1-deferred nit,Task 6 fold)。
func startOverflowHost(t *testing.T, secret string, v2 bool) *overflowHost {
	t.Helper()
	ln, err := winio.ListenPipe(`\\.\pipe\xnc-desktoppipe-test-`+t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	h := &overflowHost{
		ln: ln, secret: secret, v2: v2,
		burstCh: make(chan int, 4),
		kfCh:    make(chan string, 16),
	}
	// close(burstCh) 让 serve 退出并关闭已接受的连接:同进程重复运行
	//(-count>1)时旧 pipe 实例不残留,重名 ListenPipe 不再 Access denied。
	t.Cleanup(func() { close(h.burstCh) })
	go h.serve()
	return h
}

// startOverflowHostV2:v2 媒体协议形态(M1 Task 4 溢出/WAIT_IDR 测试)。
func startOverflowHostV2(t *testing.T, secret string) *overflowHost {
	return startOverflowHost(t, secret, true)
}

func (h *overflowHost) name() string { return h.ln.Addr().String() }

// nextV2Seq 取下一个 v2 seq(调用方须持 wmu)。
func (h *overflowHost) nextV2Seq() uint64 {
	h.seq++
	return h.seq
}

func (h *overflowHost) serve() {
	conn, err := h.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	if err := coreclient.ServerHandshake(conn, []byte(h.secret)); err != nil {
		return
	}
	f, err := ipc.ReadFrame(conn)
	if err != nil || f.MessageType != msgAttach {
		return
	}
	hello := tEncHostHello(1, 64, 48, 15, 4)
	if h.v2 {
		hello = tEncHostHelloV2(1, 64, 48, 15, 4)
	}
	if err := ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgHostHello,
		Payload: hello}); err != nil {
		return
	}
	var wmu sync.Mutex // 串行化 burst 写与 key 帧应答写(以及 v2 seq 分配)
	go func() {        // 控制帧读取:KEYFRAME_REQ → 记 reason → 回 key
		for {
			f, err := ipc.ReadFrame(conn)
			if err != nil {
				return
			}
			if f.MessageType != msgKeyframeReq || len(f.Payload) != 36 {
				continue
			}
			reason := string(f.Payload[4:36])
			for i := 0; i < len(reason); i++ {
				if reason[i] == 0 {
					reason = reason[:i]
					break
				}
			}
			h.kfCh <- reason
			wmu.Lock()
			if h.v2 {
				seq := h.nextV2Seq()
				_ = ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgFrameV2,
					Payload: tEncFrameV2(1, 1, seq, seq, seq, seq, 64, 48, 1, []byte{0, 0, 0, 1, 0x65})})
			} else {
				_ = ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgFrame,
					Payload: tEncFrame(999, true, []byte{0, 0, 0, 1, 0x65})})
			}
			wmu.Unlock()
		}
	}()
	for n := range h.burstCh {
		for i := 0; i < n; i++ {
			wmu.Lock()
			var err error
			if h.v2 {
				seq := h.nextV2Seq()
				err = ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgFrameV2,
					Payload: tEncFrameV2(1, 1, seq, seq, seq, seq, 64, 48, 0, []byte{0, 0, 0, 1, 0x41})})
			} else {
				err = ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgFrame,
					Payload: tEncFrame(uint64(i+1), false, []byte{0, 0, 0, 1, 0x41})})
			}
			wmu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// drainUntilKeyFrame 消费 FrameCh 直到出现 key 帧,返回沿途 delta 数。
func drainUntilKeyFrame(t *testing.T, s *Sub) int {
	t.Helper()
	deltas := 0
	for {
		select {
		case f, ok := <-s.FrameCh():
			if !ok {
				t.Fatal("FrameCh closed before a key frame arrived")
			}
			if f.Key {
				return deltas
			}
			deltas++
		case <-time.After(3 * time.Second):
			t.Fatal("timeout waiting for key frame")
		}
	}
}

// TestFrameChOverflowMergesKeyframeRequest:消费侧不读 → FrameCh(16)满 →
// 丢 delta 并合并恰一次 reason=client_overflow 的 KEYFRAME_REQ(§7.9 客户端
// 镜像);key 帧送达清位后,第二轮溢出再次恰触发一次。
func TestFrameChOverflowMergesKeyframeRequest(t *testing.T) {
	h := startOverflowHost(t, "desktop-pipe-secret", false)
	sub, err := Dial(h.name(), "desktop-pipe-secret", 7, SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	// 第一轮:20 delta(> 缓冲 16)不消费 → 溢出丢帧 + 恰一次合并请求。
	// (等待 kfCh 期间测试不读 FrameCh,首个溢出 delta 必然已丢弃。)
	h.burstCh <- 20
	if got := recvOrFatal[string](t, h.kfCh, "first client_overflow request"); got != "client_overflow" {
		t.Fatalf("first keyframe reason = %q, want client_overflow", got)
	}
	// host 收到请求即回 key;key 非阻塞送达(满时先清空缓冲帧)。
	d1 := drainUntilKeyFrame(t, sub)

	// 第二轮:仍不消费 → 再次溢出 → 恰第二次合并请求。
	h.burstCh <- 20
	if got := recvOrFatal[string](t, h.kfCh, "second client_overflow request"); got != "client_overflow" {
		t.Fatalf("second keyframe reason = %q, want client_overflow", got)
	}
	// 合并语义:两轮突发共 40 delta,只允许 2 条请求 —— 稳定窗口内无第三条。
	select {
	case extra := <-h.kfCh:
		t.Fatalf("unexpected extra KEYFRAME_REQ %q (merge broken)", extra)
	case <-time.After(300 * time.Millisecond):
	}
	// 断言确实发生了丢弃:两轮共 40 delta,送达的远少于 40。
	d2 := 0
drain:
	for {
		select {
		case f, ok := <-sub.FrameCh():
			if !ok {
				t.Fatal("FrameCh closed while draining round 2")
			}
			if !f.Key {
				d2++
			}
		default:
			break drain
		}
	}
	// 断言确实发生了丢弃(M1-deferred nit,Task 6 fold:旧 `d1+d2 >= 40`
	// 恒假——key 送达前 drainFrameCh 清空缓冲,两轮存活 delta ≤ 通道容量,
	// 永远到不了 40)。真实界:d1(清空后被 key 替代)+ d2(第二轮缓冲内
	// delta)之和不得超过 FrameCh 容量 frameChDepth;40 - 存活数 ≥ 24 即
	// 溢出丢弃的直接证据。
	if d1+d2 > frameChDepth {
		t.Fatalf("delivered %d+%d deltas exceeds FrameCh capacity %d (overflow drain semantics broken)", d1, d2, frameChDepth)
	}
}

// TestFrameChOverflowWaitsIdr(M1 Task 4):v2 连接 FrameCh 溢出 → 清空缓冲帧
// + 置 needKey + 恰一次合并 keyframe 请求 + 抑制后续 delta(恢复 IDR 前
// 不送达任何 delta);仅 IDR 到达后恢复投递。溢出清空语义:恢复 IDR 前
// 送达的 delta 数必须为 0。
func TestFrameChOverflowWaitsIdr(t *testing.T) {
	h := startOverflowHostV2(t, "desktop-pipe-secret")
	sub, err := Dial(h.name(), "desktop-pipe-secret", 7, SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	// 20 v2 delta(> 缓冲 16)不消费 → 溢出:清空缓冲帧 + 恰一次合并请求。
	h.burstCh <- 20
	if got := recvOrFatal[string](t, h.kfCh, "overflow keyframe request"); got != "client_overflow" {
		t.Fatalf("keyframe reason = %q, want client_overflow", got)
	}
	// 缓冲帧已清空、后续 delta 被抑制:恢复 IDR 之前不得出现任何 delta。
	if d := drainUntilKeyFrame(t, sub); d != 0 {
		t.Fatalf("%d deltas delivered before the recovery IDR, want 0 (buffered frames must be cleared)", d)
	}
	// 恢复:IDR 之后 delta 恢复投递(带完整 v2 身份)。
	h.burstCh <- 1
	f := recvOrFatal[Frame](t, sub.FrameCh(), "post-recovery delta")
	if f.Key || f.CaptureEpoch != 1 || f.CodecEpoch != 1 || f.ContentID == 0 || f.EncodeSeq == 0 {
		t.Fatalf("post-recovery frame = %+v", f)
	}
	// 合并语义:两段突发只允许恰一条请求。
	select {
	case extra := <-h.kfCh:
		t.Fatalf("unexpected extra KEYFRAME_REQ %q (merge broken)", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestDoneErrOnHostDisconnect:HOST_HELLO 后 host 不辞而别(进程退出形态)
// → 泵以读错误下线:Done() 关闭、Err() 非 nil、FrameCh/StateCh 关闭,
// 其后 Close() 仍安全幂等。
func TestDoneErrOnHostDisconnect(t *testing.T) {
	secret := "desktop-pipe-secret"
	ln, err := winio.ListenPipe(`\\.\pipe\xnc-desktoppipe-test-`+t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if err := coreclient.ServerHandshake(conn, []byte(secret)); err != nil {
			return
		}
		f, err := ipc.ReadFrame(conn)
		if err != nil || f.MessageType != msgAttach {
			return
		}
		_ = ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgHostHello,
			Payload: tEncHostHello(1, 64, 48, 15, 4)})
		// 不发 DETACH、不发 BYE:直接断连。
	}()

	sub, err := Dial(ln.Addr().String(), secret, 7, SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done() not closed after host disconnect")
	}
	if err := sub.Err(); err == nil {
		t.Fatal("Err() must be non-nil after host disconnect")
	}
	select {
	case _, ok := <-sub.FrameCh():
		if ok {
			t.Fatal("FrameCh must be closed after host disconnect")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("FrameCh not closed after host disconnect")
	}
	select {
	case _, ok := <-sub.StateCh():
		if ok {
			t.Fatal("StateCh must be closed after host disconnect")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("StateCh not closed after host disconnect")
	}
	_ = sub.Close() // 幂等安全;Err 保持非 nil(下线原因不被抹除)
	if err := sub.Err(); err == nil {
		t.Fatal("Err() must stay non-nil after post-disconnect Close")
	}
}

// ---- M2-Slice3 Task 5:HOST_HELLO displays[] 解析 + 0x0128 切换 ----

// 黄金载荷与 native/desktop/desktop_selftest.cpp 的 hh-head-golden 同源
// (gen=7 1920x1080@30 + 3 项 displays;双实现交叉)。
func TestDecodeHostHelloDisplaysGolden(t *testing.T) {
	p := make([]byte, 24+3*21)
	tPut32(p, 0, 7)
	tPut32(p, 4, 1920)
	tPut32(p, 8, 1080)
	tPut32(p, 12, 30)
	tPut32(p, 16, 4)
	tPut32(p, 20, 3)
	// entry0: idx=0 origin(-2560,0) 2560x1440 非 primary
	tPut32(p, 24, 0)
	tPut32(p, 28, s32u(-2560))
	tPut32(p, 32, 0)
	tPut32(p, 36, 2560)
	tPut32(p, 40, 1440)
	// entry1: idx=1 origin(0,0) 1920x1080 primary
	tPut32(p, 45, 1)
	tPut32(p, 65, 1)
	// entry2: idx=2 origin(1920,0) 1280x1024 非 primary
	tPut32(p, 66, 2)
	tPut32(p, 70, 1920)
	tPut32(p, 78, 1280)
	tPut32(p, 82, 1024)

	h, err := decodeHostHello(p)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h.Gen != 7 || h.W != 1920 || h.H != 1080 || h.Fps != 30 || h.MaxSubs != 4 {
		t.Fatalf("legacy fields: %+v", h)
	}
	if len(h.Displays) != 3 {
		t.Fatalf("displays len = %d, want 3", len(h.Displays))
	}
	d0, d1, d2 := h.Displays[0], h.Displays[1], h.Displays[2]
	if d0.Index != 0 || d0.OriginX != -2560 || d0.W != 2560 || d0.H != 1440 || d0.Primary {
		t.Fatalf("d0: %+v", d0)
	}
	if d1.Index != 1 || d1.Primary != true {
		t.Fatalf("d1: %+v", d1)
	}
	if d2.Index != 2 || d2.OriginX != 1920 || d2.W != 1280 || d2.H != 1024 || d2.Primary {
		t.Fatalf("d2: %+v", d2)
	}
}

// 旧 server 的 20B 载荷仍可解(Displays nil)。
func TestDecodeHostHelloLegacy20(t *testing.T) {
	h, err := decodeHostHello(tEncHostHello(3, 1920, 1080, 30, 4))
	if err != nil || h.Gen != 3 || h.MaxSubs != 4 {
		t.Fatalf("legacy decode: %+v err=%v", h, err)
	}
	if h.Displays != nil {
		t.Fatalf("legacy displays = %+v, want nil", h.Displays)
	}
}

// 截断/count 不一致拒绝。
func TestDecodeHostHelloDisplaysMalformed(t *testing.T) {
	if _, err := decodeHostHello(make([]byte, 26)); err == nil {
		t.Fatal("26-byte payload accepted")
	}
	if _, err := decodeHostHello(make([]byte, 21)); err == nil {
		t.Fatal("21-byte payload accepted")
	}
	p := make([]byte, 24+21)
	tPut32(p, 20, 2) // count=2 但仅 1 项
	if _, err := decodeHostHello(p); err == nil {
		t.Fatal("count mismatch accepted")
	}
}

// SendSwitchDisplay 线上形态:0x0128 + [u32 idx](4 字节小端)。
func TestSendSwitchDisplayWire(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	sub := &Sub{conn: c, subID: 5}
	errCh := make(chan error, 1)
	go func() { errCh <- sub.SendSwitchDisplay(2) }()
	f, err := ipc.ReadFrame(s)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if f.MessageType != msgSwitchDisp {
		t.Fatalf("message type = %#04x, want 0x0128", f.MessageType)
	}
	if len(f.Payload) != 4 || binary.LittleEndian.Uint32(f.Payload) != 2 {
		t.Fatalf("payload = %v, want [2 0 0 0]", f.Payload)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("send: %v", err)
	}
}

// ---- M1 Task 3:Pipe v2(0x0205)解码与校验 ----

// v2GoldenHex 是 native Task 2 EncodeFrameEventV2 的逐字节输出
// (header_bytes=72;身份 1,2,3,4,5,6;1920x1080;flags=1 key;payload_len=5;
// crc;载荷 00 00 00 01 65)。原样引用,禁止手工拼装。
const v2GoldenHex = "4800000001000000000000000200000000000000030000000000000004000000000000000500000000000000060000000000000080070000380400000100000005000000ab36b1e70000000165"

func v2Golden(t *testing.T) []byte {
	t.Helper()
	p, err := hex.DecodeString(v2GoldenHex)
	if err != nil {
		t.Fatalf("golden hex: %v", err)
	}
	return p
}

// TestFrameV2GoldenVector:decodeFrameV2 解 native 黄金向量,逐字段断言;
// 测试侧编码器须重现同一批字节(双实现交叉)。
func TestFrameV2GoldenVector(t *testing.T) {
	f, err := decodeFrameV2(v2Golden(t))
	if err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	if !f.Key || f.CaptureEpoch != 1 || f.CodecEpoch != 2 || f.ContentID != 3 ||
		f.EncodeSeq != 4 || f.SourceMonoUs != 5 || f.PresentMonoUs != 6 ||
		f.W != 1920 || f.H != 1080 || string(f.AU) != string([]byte{0, 0, 0, 1, 0x65}) {
		t.Fatalf("golden frame = %+v", f)
	}
	// 测试侧第二实现逐字节重现 native 黄金向量。
	enc := tEncFrameV2(1, 2, 3, 4, 5, 6, 1920, 1080, 1, []byte{0, 0, 0, 1, 0x65})
	if hex.EncodeToString(enc) != v2GoldenHex {
		t.Fatalf("test encoder diverges from native golden:\n got %s\nwant %s",
			hex.EncodeToString(enc), v2GoldenHex)
	}
}

// TestFrameV2CRCReject:载荷/头各翻转一位 → CRC32C 失配拒绝。
func TestFrameV2CRCReject(t *testing.T) {
	// 载荷位翻转。
	flip := v2Golden(t)
	flip[76] ^= 1
	if _, err := decodeFrameV2(flip); err == nil {
		t.Fatal("payload bit flip accepted")
	}
	// 头字段位翻转(capture_epoch 低字节)。
	flip = v2Golden(t)
	flip[4] ^= 0xFF
	if _, err := decodeFrameV2(flip); err == nil {
		t.Fatal("header bit flip accepted")
	}
}

// TestFrameV2LengthReject:截断头 / header_bytes 错 / 尾随字节 /
// payload_len 溢出全部拒绝。
func TestFrameV2LengthReject(t *testing.T) {
	// 头不足 72 字节。
	if _, err := decodeFrameV2(v2Golden(t)[:71]); err == nil {
		t.Fatal("71-byte payload accepted")
	}
	// 截断载荷:header 声明 payload_len=5,仅 2 字节在场(72+2 = 74)。
	if _, err := decodeFrameV2(v2Golden(t)[:74]); err == nil {
		t.Fatal("truncated payload accepted (payload_len=5, 2 bytes present)")
	}
	// header_bytes != 72(未知布局)。
	bad := v2Golden(t)
	tPut32(bad, 0, 73)
	if _, err := decodeFrameV2(bad); err == nil {
		t.Fatal("header_bytes=73 accepted")
	}
	// 尾随字节:72 + payload_len != len(p)。
	if _, err := decodeFrameV2(append(append([]byte(nil), v2Golden(t)...), 0)); err == nil {
		t.Fatal("trailing byte accepted")
	}
	// payload_len 溢出:8 MiB + 1(无载荷,校验须在分配前拒绝)。
	cap := make([]byte, 72)
	tPut32(cap, 0, 72)
	tPut32(cap, 64, 8<<20+1)
	if _, err := decodeFrameV2(cap); err == nil {
		t.Fatal("payload_len = 8 MiB + 1 accepted")
	}
	// 恰 8 MiB 边界可解(载荷完整)。
	big := tEncFrameV2(1, 2, 3, 4, 5, 6, 1920, 1080, 0, make([]byte, 8<<20))
	f, err := decodeFrameV2(big)
	if err != nil || len(f.AU) != 8<<20 {
		t.Fatalf("exact 8 MiB payload rejected: %v", err)
	}
}

// TestFrameV2UnknownVersion:hello media_protocol 未知 / v2 帧落入 v1 连接
// / v1 帧落入 v2 连接 → 拒绝。
func TestFrameV2UnknownVersion(t *testing.T) {
	// hello 尾随 u32 = 3(未知媒体协议)→ 拒绝。
	badHello := tEncHostHelloV2(1, 64, 48, 15, 4)
	tPut32(badHello, len(badHello)-4, 3)
	if _, err := decodeHostHello(badHello); err == nil {
		t.Fatal("hello with media_protocol=3 accepted")
	}
	// v1 hello(20B 无尾随)+ 0x0205 帧 → 泵以协议错误下线。
	h := startV2Host(t, "desktop-pipe-secret", tEncHostHello(1, 64, 48, 15, 4),
		[]v2Msg{{msgFrameV2, tEncFrameV2(1, 2, 3, 4, 5, 6, 1920, 1080, 1, []byte{0, 0, 0, 1, 0x65})}})
	sub, err := Dial(h.name(), "desktop-pipe-secret", 7, SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub.Done():
		if sub.Err() == nil {
			t.Fatal("Err() must carry the version mismatch")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pump must tear down on v2 frame over v1 connection")
	}
	// 失配帧不得送达 FrameCh。
	select {
	case f, ok := <-sub.FrameCh():
		if ok {
			t.Fatalf("v2 frame over v1 connection delivered: %+v", f)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("FrameCh not closed after teardown")
	}

	// v2 hello + 0x0105 帧 → 同样拒绝(版本方向相反)。
	h2 := startV2HostNamed(t, t.Name()+"-rev", "desktop-pipe-secret", tEncHostHelloV2(1, 64, 48, 15, 4),
		[]v2Msg{{msgFrame, tEncFrame(111, true, []byte{0, 0, 0, 1, 0x65})}})
	sub2, err := Dial(h2.name(), "desktop-pipe-secret", 8, SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub2.Done():
		if sub2.Err() == nil {
			t.Fatal("Err() must carry the version mismatch (v1 frame over v2)")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pump must tear down on v1 frame over v2 connection")
	}
}

// TestFrameV2EpochRegression:v2 泵 —— 合法帧送达;身份回退(seq 重复)
// 在进 FrameCh 前拒绝,泵下线,回归帧永不送达。
func TestFrameV2EpochRegression(t *testing.T) {
	hello := tEncHostHelloV2(1, 64, 48, 15, 4)
	// 帧 1:epoch(1,2) content 3 seq 4;帧 2:seq 重复 → 回归。
	msgs := []v2Msg{
		{msgFrameV2, tEncFrameV2(1, 2, 3, 4, 5, 6, 1920, 1080, 1, []byte{0, 0, 0, 1, 0x65})},
		{msgFrameV2, tEncFrameV2(1, 2, 3, 4, 5, 9, 1920, 1080, 0, []byte{0, 0, 0, 1, 0x41})},
	}
	h := startV2Host(t, "desktop-pipe-secret", hello, msgs)
	sub, err := Dial(h.name(), "desktop-pipe-secret", 7, SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	// 合法帧送达且身份完整。
	f1 := recvOrFatal[Frame](t, sub.FrameCh(), "v2 first frame")
	if !f1.Key || f1.CaptureEpoch != 1 || f1.CodecEpoch != 2 || f1.ContentID != 3 ||
		f1.EncodeSeq != 4 || f1.PresentMonoUs != 6 || f1.W != 1920 || f1.H != 1080 {
		t.Fatalf("v2 first frame = %+v", f1)
	}
	// 回归帧:泵下线,Err 非 nil。
	select {
	case <-sub.Done():
		if sub.Err() == nil {
			t.Fatal("Err() must carry the identity regression")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pump must tear down on identity regression")
	}
	// 回归帧绝不得出现在 FrameCh(通道随下线关闭)。
	select {
	case f, ok := <-sub.FrameCh():
		if ok {
			t.Fatalf("regressed frame delivered: %+v", f)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("FrameCh not closed after teardown")
	}
}

// TestFrameV2EpochRebaseline:epoch 前进重基线 —— 新 epoch 内 seq 回小仍接受;
// epoch 回退拒绝。
func TestFrameV2EpochRebaseline(t *testing.T) {
	led := &frameLedger{}
	id := func(ce, ke, content, seq uint64) frameIdentity {
		return frameIdentity{captureEpoch: ce, codecEpoch: ke, contentID: content, encodeSeq: seq}
	}
	if !led.accept(id(1, 1, 1, 1)) {
		t.Fatal("first identity rejected")
	}
	if led.accept(id(1, 1, 1, 1)) {
		t.Fatal("exact repeat accepted")
	}
	// 同 epoch 内容推进 + seq 递增 → 接受。
	if !led.accept(id(1, 1, 2, 2)) {
		t.Fatal("content advance rejected")
	}
	// 同内容重编码带新 seq → 接受。
	if !led.accept(id(1, 1, 2, 3)) {
		t.Fatal("re-encode with new seq rejected")
	}
	// content 回退 → 拒绝。
	if led.accept(id(1, 1, 1, 4)) {
		t.Fatal("content regress accepted")
	}
	// capture_epoch 前进 → 重基线(seq 回小仍接受)。
	if !led.accept(id(2, 1, 1, 1)) {
		t.Fatal("capture epoch advance rejected")
	}
	// capture_epoch 回退 → 拒绝。
	if led.accept(id(1, 2, 5, 5)) {
		t.Fatal("capture epoch regress accepted")
	}
	// codec_epoch 前进 → 重基线;回退 → 拒绝。
	if !led.accept(id(2, 2, 1, 1)) {
		t.Fatal("codec epoch advance rejected")
	}
	if led.accept(id(2, 1, 9, 9)) {
		t.Fatal("codec epoch regress accepted")
	}
}

// TestFrameV2Pump:v2 hello + 两条合法 v2 帧 → Hello() 带 MediaProtocol、
// 帧经 FrameCh 全字段送达;无回退时泵保持健康。
func TestFrameV2Pump(t *testing.T) {
	hello := tEncHostHelloV2(3, 1920, 1080, 30, 4)
	msgs := []v2Msg{
		{msgFrameV2, tEncFrameV2(1, 2, 3, 4, 5, 6, 1920, 1080, 1, []byte{0, 0, 0, 1, 0x65})},
		{msgFrameV2, tEncFrameV2(1, 2, 4, 5, 7, 8, 1920, 1080, 0, []byte{0, 0, 0, 1, 0x41})},
	}
	h := startV2Host(t, "desktop-pipe-secret", hello, msgs)
	sub, err := Dial(h.name(), "desktop-pipe-secret", 7, SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	hi := sub.Hello()
	if hi == nil || hi.MediaProtocol != 2 || hi.Gen != 3 || hi.W != 1920 || hi.H != 1080 {
		t.Fatalf("v2 Hello() = %+v", hi)
	}
	f1 := recvOrFatal[Frame](t, sub.FrameCh(), "v2 key frame")
	if !f1.Key || f1.CaptureEpoch != 1 || f1.CodecEpoch != 2 || f1.ContentID != 3 ||
		f1.EncodeSeq != 4 || f1.SourceMonoUs != 5 || f1.PresentMonoUs != 6 ||
		string(f1.AU) != string([]byte{0, 0, 0, 1, 0x65}) {
		t.Fatalf("v2 key frame = %+v", f1)
	}
	f2 := recvOrFatal[Frame](t, sub.FrameCh(), "v2 delta frame")
	if f2.Key || f2.ContentID != 4 || f2.EncodeSeq != 5 || f2.PresentMonoUs != 8 {
		t.Fatalf("v2 delta frame = %+v", f2)
	}
}

// ---- M1 Task 4:0x020B STREAM_DISCONTINUITY ----

// tEncStreamDiscontinuity 编码 0x020B [u64 capture_epoch][u64 codec_epoch]
// [char reason[32]] NUL 填充(layout = native EncodeStreamDiscontinuity,
// 48 字节精确长度)。
func tEncStreamDiscontinuity(ce, ke uint64, reason string) []byte {
	p := make([]byte, 48)
	tPut64(p, 0, ce)
	tPut64(p, 8, ke)
	copy(p[16:], reason)
	return p
}

// TestDecodeStreamDiscontinuity:0x020B 载荷黄金字节 + 畸形拒绝。
func TestDecodeStreamDiscontinuity(t *testing.T) {
	p := tEncStreamDiscontinuity(2, 3, "stream_discontinuity")
	want := make([]byte, 48)
	tPut64(want, 0, 2)
	tPut64(want, 8, 3)
	copy(want[16:], "stream_discontinuity")
	if string(p) != string(want) {
		t.Fatalf("encode = %x, want %x", p, want)
	}
	ev, err := decodeStreamDiscontinuity(p)
	if err != nil || ev.CaptureEpoch != 2 || ev.CodecEpoch != 3 ||
		ev.Reason != "stream_discontinuity" {
		t.Fatalf("decode = %+v err=%v", ev, err)
	}
	if _, err := decodeStreamDiscontinuity(p[:47]); err == nil {
		t.Fatal("47-byte payload accepted")
	}
	if _, err := decodeStreamDiscontinuity(append(append([]byte(nil), p...), 0)); err == nil {
		t.Fatal("49-byte payload accepted")
	}
}

// TestStreamDiscontinuityV2:v2 泵 —— 0x020B 清空已缓冲的旧 epoch 帧 +
// needKey,随后该 epoch 对的 IDR 恢复投递;断流本身不致命。
func TestStreamDiscontinuityV2(t *testing.T) {
	hello := tEncHostHelloV2(1, 64, 48, 15, 4)
	msgs := []v2Msg{
		{msgFrameV2, tEncFrameV2(1, 1, 3, 4, 5, 6, 64, 48, 1, []byte{0, 0, 0, 1, 0x65})},
		{msgFrameV2, tEncFrameV2(1, 1, 4, 5, 7, 8, 64, 48, 0, []byte{0, 0, 0, 1, 0x41})},
		{msgStreamDisc, tEncStreamDiscontinuity(2, 1, "stream_discontinuity")},
		{msgFrameV2, tEncFrameV2(2, 1, 5, 6, 9, 10, 64, 48, 1, []byte{0, 0, 0, 1, 0x65})},
		{msgFrameV2, tEncFrameV2(2, 1, 6, 7, 11, 12, 64, 48, 0, []byte{0, 0, 0, 1, 0x41})},
	}
	h := startV2Host(t, "desktop-pipe-secret", hello, msgs)
	sub, err := Dial(h.name(), "desktop-pipe-secret", 7, SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	// host 一次性下发全部 5 帧;等泵处理完(断流已清空缓冲帧)后再读:
	// 缓冲中只应剩断流之后的两帧,首帧必须是新 epoch 的 IDR。
	time.Sleep(300 * time.Millisecond)
	f1 := recvOrFatal[Frame](t, sub.FrameCh(), "post-discontinuity IDR")
	if !f1.Key || f1.CaptureEpoch != 2 || f1.CodecEpoch != 1 || f1.ContentID != 5 {
		t.Fatalf("first frame after discontinuity = %+v", f1)
	}
	// 恢复:IDR 之后 delta 正常投递。
	f2 := recvOrFatal[Frame](t, sub.FrameCh(), "post-discontinuity delta")
	if f2.Key || f2.CaptureEpoch != 2 || f2.ContentID != 6 {
		t.Fatalf("recovery delta = %+v", f2)
	}
	// 断流前的缓冲帧(epoch 1 的 IDR + delta)必须已被清空,不残留。
	select {
	case extra, ok := <-sub.FrameCh():
		if ok {
			t.Fatalf("stale pre-discontinuity frame survived the drain: %+v", extra)
		}
	case <-time.After(100 * time.Millisecond):
	}
	// 泵保持健康(断流不致命)。
	select {
	case <-sub.Done():
		t.Fatalf("pump torn down on discontinuity: %v", sub.Err())
	default:
	}
}

// TestStreamDiscontinuityEpochRegression:断流携带的 epoch 对相对已收身份
// 回退 → 按身份回归致命下线(WAIT_IDR 不覆盖说谎的 host)。
func TestStreamDiscontinuityEpochRegression(t *testing.T) {
	hello := tEncHostHelloV2(1, 64, 48, 15, 4)
	msgs := []v2Msg{
		{msgFrameV2, tEncFrameV2(2, 1, 3, 4, 5, 6, 64, 48, 1, []byte{0, 0, 0, 1, 0x65})},
		{msgStreamDisc, tEncStreamDiscontinuity(1, 1, "stream_discontinuity")},
	}
	h := startV2Host(t, "desktop-pipe-secret", hello, msgs)
	sub, err := Dial(h.name(), "desktop-pipe-secret", 7, SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub.Done():
		if sub.Err() == nil {
			t.Fatal("Err() must carry the discontinuity regression")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pump must tear down on discontinuity epoch regression")
	}
}

// TestStreamDiscontinuityV1Reject:v1 连接收到 0x020B → 协议错误下线
// (0x020B 是 v2 模式消息,镜像 0x0205 的版本拒绝)。
func TestStreamDiscontinuityV1Reject(t *testing.T) {
	h := startV2Host(t, "desktop-pipe-secret", tEncHostHello(1, 64, 48, 15, 4),
		[]v2Msg{{msgStreamDisc, tEncStreamDiscontinuity(2, 1, "stream_discontinuity")}})
	sub, err := Dial(h.name(), "desktop-pipe-secret", 7, SubOpts{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-sub.Done():
		if sub.Err() == nil {
			t.Fatal("Err() must carry the version mismatch")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pump must tear down on v1 discontinuity")
	}
}

// ---- M3 Task 3:HOST_HELLO capabilities 扩展 + SET_VIDEO_CONFIG ----

// tEncHostHelloCaps:v2 扩展 hello(displays 块 + media_protocol=2 + 尾随
// capabilities;镜像 native EncodeHostHelloV2Caps)。
func tEncHostHelloCaps(gen, w, h, fps, maxSubs uint32, caps uint32) []byte {
	p := tEncHostHello(gen, w, h, fps, maxSubs)
	p = append(p, 0, 0, 0, 0) // [u32 displays count = 0]
	p = append(p, 2, 0, 0, 0) // [u32 media_protocol = 2]
	p = append(p, byte(caps), byte(caps>>8), byte(caps>>16), byte(caps>>24))
	return p
}

// TestDecodeHostHelloCapabilities:三种形状各自成立——legacy(无扩展)、
// v2(尾随 media_protocol)、v2+caps(再尾随 capabilities;M3 Task 3);
// capabilities 值透传,缺省 0。
func TestDecodeHostHelloCapabilities(t *testing.T) {
	h, err := decodeHostHello(tEncHostHello(1, 64, 48, 15, 4))
	if err != nil || h.MediaProtocol != 0 || h.Capabilities != 0 {
		t.Fatalf("legacy hello: h=%+v err=%v", h, err)
	}
	h, err = decodeHostHello(tEncHostHelloCaps(2, 64, 48, 15, 4, 0))
	if err != nil || h.MediaProtocol != 2 || h.Capabilities != 0 {
		t.Fatalf("v2 hello (no caps): h=%+v err=%v", h, err)
	}
	h, err = decodeHostHello(tEncHostHelloCaps(3, 1920, 1080, 30, 4, CapSetVideoConfig))
	if err != nil || h.Gen != 3 || h.W != 1920 || h.Fps != 30 ||
		h.MediaProtocol != 2 || h.Capabilities != CapSetVideoConfig {
		t.Fatalf("v2+caps hello: h=%+v err=%v", h, err)
	}
	// 截断/displays 巧合字节仍拒绝。
	if _, err := decodeHostHello(tEncHostHelloCaps(1, 64, 48, 15, 4, CapSetVideoConfig)[:30]); err == nil {
		t.Fatal("truncated v2+caps hello must be rejected")
	}
}

// TestSendVideoConfigWire:能力广告在场 → 0x0129 [u32 kbps][u32 fps]
// [u32 max_w];未广告 → ErrVideoConfigUnsupported 且不写线。
func TestSendVideoConfigWire(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	sub := &Sub{conn: c, subID: 5, hello: &HelloInfo{MediaProtocol: 2, Capabilities: CapSetVideoConfig}}
	errCh := make(chan error, 1)
	go func() { errCh <- sub.SendVideoConfig(2300, 30, 1920) }()
	f, err := ipc.ReadFrame(s)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if f.MessageType != msgSetVideoCfg {
		t.Fatalf("message type = %#04x, want 0x0129", f.MessageType)
	}
	if len(f.Payload) != 12 ||
		binary.LittleEndian.Uint32(f.Payload) != 2300 ||
		binary.LittleEndian.Uint32(f.Payload[4:]) != 30 ||
		binary.LittleEndian.Uint32(f.Payload[8:]) != 1920 {
		t.Fatalf("payload = %v, want [2300 30 1920] LE", f.Payload)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("send: %v", err)
	}

	// 未广告能力(v1 host / 旧 v2 host):拒绝,不触达 pipe。
	noCap := &Sub{conn: c, subID: 6, hello: &HelloInfo{}}
	if err := noCap.SendVideoConfig(1, 2, 3); err == nil ||
		err.Error() != ErrVideoConfigUnsupported.Error() {
		t.Fatalf("want ErrVideoConfigUnsupported, got %v", err)
	}
	s.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := ipc.ReadFrame(s); err == nil {
		t.Fatal("unsupported host must not see a 0x0129 on the wire")
	}
}

// TestSendViewerMetricsWire(M4 交付指标):能力广告在场 → 0x012A
// [u32 fps_x10][u32 e2e_p95_ms];未广告 → 无操作(不写线、不报错 ——
// 指标转发是 best-effort)。
func TestSendViewerMetricsWire(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	sub := &Sub{conn: c, subID: 5,
		hello: &HelloInfo{MediaProtocol: 2, Capabilities: CapSetVideoConfig | CapViewerMetrics}}
	errCh := make(chan error, 1)
	go func() { errCh <- sub.SendViewerMetrics(24.7, 83) }()
	f, err := ipc.ReadFrame(s)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if f.MessageType != msgViewerMtrc {
		t.Fatalf("message type = %#04x, want 0x012A", f.MessageType)
	}
	if len(f.Payload) != 8 ||
		binary.LittleEndian.Uint32(f.Payload) != 247 ||
		binary.LittleEndian.Uint32(f.Payload[4:]) != 83 {
		t.Fatalf("payload = %v, want [247 83] LE", f.Payload)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("send: %v", err)
	}

	// 未广告能力:静默无操作(绝不为指标打扰 host)。
	noCap := &Sub{conn: c, subID: 6,
		hello: &HelloInfo{MediaProtocol: 2, Capabilities: CapSetVideoConfig}}
	if err := noCap.SendViewerMetrics(30, 50); err != nil {
		t.Fatalf("unadvertised host must be a silent no-op, got %v", err)
	}
	s.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := ipc.ReadFrame(s); err == nil {
		t.Fatal("unadvertised host must not see a 0x012A on the wire")
	}
}
