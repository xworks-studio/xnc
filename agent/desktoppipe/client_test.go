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
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"xnc/agent/coreclient"
	"xnc/proto/ipc"
)

// ---- 测试侧编码器(布局 = C++ rt_pipe_server.h)----

func tPut32(p []byte, off int, v uint32) { binary.LittleEndian.PutUint32(p[off:], v) }
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

// fakeHost 观察:ATTACH payload / KEYFRAME_REQ reason / DETACH sub_id。
type fakeHost struct {
	ln        net.Listener
	secret    []byte
	attachCh  chan []byte // 原始 ATTACH payload
	kfCh      chan string // KEYFRAME_REQ reason
	detachCh  chan uint32 // DETACH sub_id
	rejectSub bool        // true: ATTACH 后回 STATE{too_many_subs} 并断连
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
	}
	go h.serve()
	return h
}

func (h *fakeHost) name() string { return h.ln.Addr().String() }

// serve 走生产时序:握手 → ATTACH → HOST_HELLO → 两帧(key+delta)+ STATE
// → 请求循环(KEYFRAME_REQ 回一个新 key 帧;DETACH/BYE 结束)。
func (h *fakeHost) serve() {
	conn, err := h.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
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
