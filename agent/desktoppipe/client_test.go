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
	if hello == nil || *hello != (HelloInfo{Gen: 3, W: 1920, H: 1080, Fps: 30, MaxSubs: 4}) {
		t.Fatalf("Hello() = %+v", hello)
	}

	f1 := recvOrFatal[Frame](t, sub.FrameCh(), "key frame")
	if !f1.Key || f1.MonoUs != 111 || string(f1.AU) != string([]byte{0, 0, 0, 1, 0x65}) {
		t.Fatalf("key frame = %+v", f1)
	}
	f2 := recvOrFatal[Frame](t, sub.FrameCh(), "delta frame")
	if f2.Key || f2.MonoUs != 222 || string(f2.AU) != string([]byte{0, 0, 0, 1, 0x41}) {
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
	if !f3.Key || f3.MonoUs != 333 || len(f3.AU) != 6 {
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
// 合并位,使下一轮溢出可再次触发)。
type overflowHost struct {
	ln      net.Listener
	secret  string
	burstCh chan int
	kfCh    chan string // 每条收到的 KEYFRAME_REQ reason
}

func startOverflowHost(t *testing.T, secret string) *overflowHost {
	t.Helper()
	ln, err := winio.ListenPipe(`\\.\pipe\xnc-desktoppipe-test-`+t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	h := &overflowHost{
		ln: ln, secret: secret,
		burstCh: make(chan int, 4),
		kfCh:    make(chan string, 16),
	}
	// close(burstCh) 让 serve 退出并关闭已接受的连接:同进程重复运行
	//(-count>1)时旧 pipe 实例不残留,重名 ListenPipe 不再 Access denied。
	t.Cleanup(func() { close(h.burstCh) })
	go h.serve()
	return h
}

func (h *overflowHost) name() string { return h.ln.Addr().String() }

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
	if err := ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgHostHello,
		Payload: tEncHostHello(1, 64, 48, 15, 4)}); err != nil {
		return
	}
	var wmu sync.Mutex // 串行化 burst 写与 key 帧应答写
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
			_ = ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgFrame,
				Payload: tEncFrame(999, true, []byte{0, 0, 0, 1, 0x65})})
			wmu.Unlock()
		}
	}()
	for n := range h.burstCh {
		for i := 0; i < n; i++ {
			wmu.Lock()
			err := ipc.WriteFrame(conn, &ipc.Frame{Flags: ipc.FlagEvent, MessageType: msgFrame,
				Payload: tEncFrame(uint64(i+1), false, []byte{0, 0, 0, 1, 0x41})})
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
	h := startOverflowHost(t, "desktop-pipe-secret")
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
	// host 收到请求即回 key;key 阻塞送达直到消费侧开始读。
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
	if d1+d2 >= 40 {
		t.Fatalf("no deltas dropped: delivered %d+%d of 40", d1, d2)
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
