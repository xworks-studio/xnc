// input_loopback_test.go — Task 3 回环门(真实双 PeerConnection):
//
//  1. TestInputLoopbackSequence:viewer 经 input/mouse DataChannel 发
//     MOVE/BUTTON/KEY/TEXT/LOCK 序列 → agent 预校验 + 0x0108 编码 →
//     fake host(inputHost 独立解码,镜像 native DecodeInputMsg)断言序列
//     与 seq 强制;stale seq 在 agent 侧丢弃;cursor 通道反向透传 0x0109。
//  2. TestLeaseLoopbackTwoViewers:双 WS 会话共享 Handler lease 表 ——
//     首请求授予、并发请求 denied{held}、非持有者输入丢弃+计数不到 host、
//     30s idle(测试注入缩短)撤销并广播 lease_revoked、断连释放后可移交。
//
// viewer 会话封装 connectViewerInput:WS 信令(ready/offer/answer/ice)+
// 三条 agent 侧通道(input 可靠 / mouse、cursor 不可靠)的 OnDataChannel
// 收集与开关等待。host 侧 recorder 由 inputFakeStarter 每会话一个
// inputFakeSource(sub_id 递增),共享 inputHost 记账。
package desktop

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

// ---- host 侧 0x0108 记录器(独立手写解码,镜像 C++ DecodeInputMsg)----

type inputRec struct {
	SubID     uint32
	Seq       uint64
	Type      uint8
	X, Y      int32
	Buttons   uint16
	Btn, Down uint8
	Trackpad  uint8
	Scan      uint16
	Extended  uint8
	Text      []uint16
	Caps, Num uint8
}

// inputHost 镜像 InputManager 的 per-sub_id 严格递增 seq:回退 → 静默丢弃。
type inputHost struct {
	mu    sync.Mutex
	recs  []inputRec
	last  map[uint32]uint64
	seen  map[uint32]bool
	stale uint64
}

func newInputHost() *inputHost {
	return &inputHost{last: map[uint32]uint64{}, seen: map[uint32]bool{}}
}

func (h *inputHost) inject(payload []byte) {
	if len(payload) < 15 { // 13 头 + 最短 payload'(BUTTON/LOCK 2B)
		return
	}
	r := inputRec{SubID: binary.LittleEndian.Uint32(payload), Seq: binary.LittleEndian.Uint64(payload[4:]), Type: payload[12]}
	p := payload[13:]
	ok := true
	switch r.Type {
	case inputTypeMove:
		if len(p) != 10 {
			ok = false
			break
		}
		r.X, r.Y = int32(binary.LittleEndian.Uint32(p)), int32(binary.LittleEndian.Uint32(p[4:]))
		r.Buttons = binary.LittleEndian.Uint16(p[8:])
		ok = r.Buttons&^uint16(0x1F) == 0
	case inputTypeButton:
		if len(p) != 2 {
			ok = false
			break
		}
		r.Btn, r.Down = p[0], p[1]
		ok = (r.Btn == 1 || r.Btn == 2 || r.Btn == 4 || r.Btn == 8 || r.Btn == 16) && r.Down <= 1
	case inputTypeWheel:
		if len(p) != 9 {
			ok = false
			break
		}
		r.X, r.Y = int32(binary.LittleEndian.Uint32(p)), int32(binary.LittleEndian.Uint32(p[4:]))
		r.Trackpad = p[8]
	case inputTypeKey:
		if len(p) != 4 {
			ok = false
			break
		}
		r.Scan = binary.LittleEndian.Uint16(p)
		r.Down, r.Extended = p[2], p[3]
		ok = r.Scan != 0 && r.Down <= 1 && r.Extended <= 1
	case inputTypeText:
		if len(p) < 4 {
			ok = false
			break
		}
		n := int(binary.LittleEndian.Uint16(p))
		if n == 0 || n > 512 || len(p) != 2+2*n {
			ok = false
			break
		}
		r.Text = make([]uint16, n)
		for i := 0; i < n; i++ {
			r.Text[i] = binary.LittleEndian.Uint16(p[2+2*i:])
		}
	case inputTypeLock:
		if len(p) != 2 {
			ok = false
			break
		}
		r.Caps, r.Num = p[0], p[1]
		ok = r.Caps <= 1 && r.Num <= 1
	default:
		ok = false
	}
	if !ok || r.SubID == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seen[r.SubID] && r.Seq <= h.last[r.SubID] {
		h.stale++
		return // 镜像 InputManager:replay 丢弃(仍消费)
	}
	h.seen[r.SubID] = true
	h.last[r.SubID] = r.Seq
	h.recs = append(h.recs, r)
}

func (h *inputHost) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.recs)
}

func (h *inputHost) records() []inputRec {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]inputRec(nil), h.recs...)
}

// ---- fake Source/Starter(每会话一 source,共享 host 记账)----

type inputFakeSource struct {
	host     *inputHost
	subID    uint32
	frameCh  chan Frame
	stateCh  chan StateEvent
	cursorCh chan CursorEvent
	done     chan struct{}

	closeOnce sync.Once
	mu        sync.Mutex
	closed    bool
	keyReqs   []string
}

func (s *inputFakeSource) RecvFrame(ctx context.Context) (Frame, bool) {
	select {
	case f, ok := <-s.frameCh:
		return f, ok
	case <-ctx.Done():
		return Frame{}, false
	case <-s.done:
		return Frame{}, false
	}
}

func (s *inputFakeSource) RecvState(ctx context.Context) (StateEvent, bool) {
	select {
	case ev, ok := <-s.stateCh:
		return ev, ok
	case <-ctx.Done():
		return StateEvent{}, false
	case <-s.done:
		return StateEvent{}, false
	}
}

func (s *inputFakeSource) RecvCursor(ctx context.Context) (CursorEvent, bool) {
	select {
	case ev, ok := <-s.cursorCh:
		return ev, ok
	case <-ctx.Done():
		return CursorEvent{}, false
	case <-s.done:
		return CursorEvent{}, false
	}
}

// RecvDisplay:0x010A 镜像(M2-Slice1 Task 2);输入用例不驱动显示变化,
// 恒阻塞到源终结(接口完整性实现)。
func (s *inputFakeSource) RecvDisplay(ctx context.Context) (DisplayChangedEvent, bool) {
	select {
	case <-ctx.Done():
		return DisplayChangedEvent{}, false
	case <-s.done:
		return DisplayChangedEvent{}, false
	}
}

func (s *inputFakeSource) Hello() *HelloInfo {
	return &HelloInfo{Gen: 1, W: 1920, H: 1080, Fps: 30, MaxSubs: 4}
}

func (s *inputFakeSource) RequestKeyframe(reason string) error {
	s.mu.Lock()
	s.keyReqs = append(s.keyReqs, reason)
	s.mu.Unlock()
	return nil
}

func (s *inputFakeSource) SubID() uint32 { return s.subID }

func (s *inputFakeSource) SendInput(payload []byte) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return errSourceClosed
	}
	s.host.inject(payload)
	return nil
}

func (s *inputFakeSource) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.done)
	})
	return nil
}

var errSourceClosed = errClosedSource{}

type errClosedSource struct{}

func (errClosedSource) Error() string { return "input fake source closed" }

type inputFakeStarter struct {
	host *inputHost
	mu   sync.Mutex
	next uint32
	last *inputFakeSource
}

func (st *inputFakeStarter) Start(_ context.Context, _ uint32) (Source, error) {
	st.mu.Lock()
	st.next++
	src := &inputFakeSource{host: st.host, subID: 100 + st.next,
		frameCh: make(chan Frame, 8), stateCh: make(chan StateEvent, 4),
		cursorCh: make(chan CursorEvent, 4), done: make(chan struct{})}
	st.last = src
	st.mu.Unlock()
	return src, nil
}

func (st *inputFakeStarter) Last() *inputFakeSource {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.last
}

func (st *inputFakeStarter) Stop() error { return nil }

// ---- viewer 会话封装 ----

type viewerSession struct {
	t   *testing.T
	ctx context.Context
	ws  *websocket.Conn
	pc  *webrtc.PeerConnection

	mu   sync.Mutex
	dcs  map[string]*webrtc.DataChannel
	msgs map[string]chan []byte

	sig chan map[string]any // 非 ICE 的 agent 信令帧(lease 等)
}

// connectViewerInput 建一条完整 viewer 会话:WS + offer/answer + trickle +
// 等待三条 agent 通道 open。viewer 侧先建一条 DC 保证 offer 含
// m=application(浏览器默认如此;pion 需显式)。
func connectViewerInput(t *testing.T, ctx context.Context, wsURL string) *viewerSession {
	t.Helper()
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("viewer dial: %v", err)
	}
	ws.SetReadLimit(1 << 20)

	// ready 先行(词汇契约)。
	if got := drainUntil(t, ctx, ws, 10*time.Second, func(m map[string]any) bool { return m["type"] == vocabReady }); got == nil {
		t.Fatal("no ready frame")
	}

	pc, _ := startViewerPC(t)
	vs := &viewerSession{t: t, ctx: ctx, ws: ws, pc: pc,
		dcs: map[string]*webrtc.DataChannel{}, msgs: map[string]chan []byte{},
		sig: make(chan map[string]any, 64)}
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		ch := make(chan []byte, 64)
		vs.mu.Lock()
		vs.msgs[dc.Label()] = ch
		vs.dcs[dc.Label()] = dc
		vs.mu.Unlock()
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			select {
			case ch <- m.Data:
			default:
			}
		})
	})
	if _, err := pc.CreateDataChannel("neg", nil); err != nil { // m=application 触发器
		t.Fatalf("create neg dc: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	sendJSON(t, ctx, ws, map[string]any{"type": vocabOffer, "sdp": offer.SDP})
	ans := drainUntil(t, ctx, ws, 10*time.Second, func(m map[string]any) bool { return m["type"] == vocabAnswer })
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans["sdp"].(string)}); err != nil {
		t.Fatalf("set remote answer: %v", err)
	}

	// ICE 双向 + 其余信令入 sig。
	iceCh := make(chan webrtc.ICECandidateInit, 16)
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			iceCh <- c.ToJSON()
		}
	})
	go func() {
		for c := range iceCh {
			sendJSON(t, ctx, ws, map[string]any{"type": vocabICE, "candidate": c})
		}
	}()
	go func() {
		for {
			mt, r, err := ws.Reader(ctx)
			if err != nil {
				return
			}
			if mt != websocket.MessageText {
				continue
			}
			b, err := io.ReadAll(r)
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(b, &m) != nil {
				continue
			}
			if m["type"] == vocabICE && m["candidate"] != nil {
				var ci webrtc.ICECandidateInit
				jb, _ := json.Marshal(m["candidate"])
				if json.Unmarshal(jb, &ci) == nil {
					_ = pc.AddICECandidate(ci)
				}
				continue
			}
			select {
			case vs.sig <- m:
			default:
			}
		}
	}()

	deadline := time.Now().Add(10 * time.Second)
	for pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
		if time.Now().After(deadline) {
			t.Fatalf("viewer never connected, state=%v", pc.ConnectionState())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// 三条通道全部 open。
	for _, label := range []string{dcLabelInput, dcLabelMouse, dcLabelCursor} {
		ok := false
		for !ok {
			vs.mu.Lock()
			dc := vs.dcs[label]
			vs.mu.Unlock()
			if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
				ok = true
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("data channel %q never opened (got %v)", label, vs.dcLabels())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return vs
}

func (vs *viewerSession) dcLabels() []string {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	var out []string
	for k := range vs.dcs {
		out = append(out, k)
	}
	return out
}

func (vs *viewerSession) sendDC(label string, b []byte) {
	vs.t.Helper()
	vs.mu.Lock()
	dc := vs.dcs[label]
	vs.mu.Unlock()
	if dc == nil {
		vs.t.Fatalf("no data channel %q (have %v)", label, vs.dcLabels())
	}
	if err := dc.Send(b); err != nil {
		vs.t.Fatalf("send on %q: %v", label, err)
	}
}

// nextSignal 等待满足谓词的信令帧(跳过无关帧;ICE 已在泵内消化)。
func (vs *viewerSession) nextSignal(pred func(map[string]any) bool, d time.Duration) map[string]any {
	vs.t.Helper()
	deadline := time.Now().Add(d)
	for {
		var m map[string]any
		select {
		case m = <-vs.sig:
		case <-time.After(time.Until(deadline)):
			vs.t.Fatalf("timeout waiting for signal frame after %v", d)
		}
		if pred(m) {
			return m
		}
		if time.Now().After(deadline) {
			vs.t.Fatalf("signal predicate never matched, last=%v", m)
		}
	}
}

func (vs *viewerSession) recvDC(label string, d time.Duration) []byte {
	vs.t.Helper()
	vs.mu.Lock()
	ch := vs.msgs[label]
	vs.mu.Unlock()
	if ch == nil {
		vs.t.Fatalf("no messages channel for %q", label)
	}
	select {
	case b := <-ch:
		return b
	case <-time.After(d):
		vs.t.Fatalf("timeout reading %q data channel", label)
		return nil
	}
}

func (vs *viewerSession) close() {
	_ = vs.ws.CloseNow()
	_ = vs.pc.Close()
}

// startInputServer 起一个 httptest WS server,每连接一个 Handle 调用
// (多 viewer = 多会话,共享 Handler 与 lease 表)。
// startInputServer 起一个 httptest WS server,每连接一个 Handle 调用
// (多 viewer = 多会话,共享 Handler 与 server lease 登记表)。params 供
// 测试注入 server 侧签发形态(leaseId/capabilities;M2-Slice3 Task 4)。
func startInputServer(t *testing.T, ctx context.Context, h *Handler,
	params func(n int) json.RawMessage) string {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(1 << 20)
		h.Handle(ctx, c, fmt.Sprintf("sess-%d", n.Add(1)), params(int(n.Load())))
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// 固定 params(无 server leaseId = view-only;回环输入测试的缺省形态)。
func plainParams() json.RawMessage {
	return json.RawMessage(`{"signaling":"webrtc","iceTransportPolicy":"all"}`)
}

// TestInputLoopbackSequence:五类输入经真实 DataChannel → agent → 0x0108 →
// fake host;seq 回退在 agent 丢弃;cursor 反向到 viewer。
func TestInputLoopbackSequence(t *testing.T) {
	host := newInputHost()
	st := &inputFakeStarter{host: host}
	h := &Handler{Log: slog.Default(), Starter: st}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// server 授予形态:params 携带 leaseId + operator capability 集。
	const l1 = "1122334455667788"
	grantedParams := func(int) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(
			`{"signaling":"webrtc","iceTransportPolicy":"all","leaseId":%q,"capabilities":["screen.view","input.mouse","input.keyboard"]}`, l1))
	}
	wsURL := startInputServer(t, ctx, h, grantedParams)
	vs := connectViewerInput(t, ctx, wsURL)
	defer vs.close()

	// lease_request:持有 server 约 → granted 回显同一 leaseId(仲裁在
	// server;agent 只作答)。
	sendJSON(t, ctx, vs.ws, map[string]any{"type": vocabLeaseRequest})
	g := vs.nextSignal(func(m map[string]any) bool { return m["type"] == vocabLeaseGranted }, 5*time.Second)
	if got, _ := g["leaseId"].(string); got != l1 {
		t.Fatalf("leaseId = %q, want server-issued %q", got, l1)
	}

	// 输入序列:MOVE(mouse 通道)→ BUTTON/KEY/TEXT/LOCK(input 通道)。
	// mouse 通道不可靠无序,与 input 通道无跨通道保序——先等 MOVE 到达
	// 再发 input 突发,否则 BUTTON(seq2)先到会把 MOVE(seq1)判 staleSeq
	// 丢弃(seq 单调是跨通道一个计数器,by design;本测试钉顺序)。
	vs.sendDC(dcLabelMouse, encMouseMsg(1, 100, 200, 1))
	waitCount(t, host, 1)
	vs.sendDC(dcLabelInput, encInputMsg(2, inputTypeButton, []byte{1, 1}))
	vs.sendDC(dcLabelInput, encInputMsg(3, inputTypeKey, keyPayload(0x1E, 1, 0)))
	vs.sendDC(dcLabelInput, encInputMsg(4, inputTypeText, textPayload([]uint16{0x68, 0x4E2D})))
	vs.sendDC(dcLabelInput, encInputMsg(5, inputTypeLock, []byte{1, 0}))
	waitCount(t, host, 5)

	recs := host.records()
	src := st.Last()
	if src == nil {
		t.Fatal("no source created")
	}
	wantTypes := []uint8{inputTypeMove, inputTypeButton, inputTypeKey, inputTypeText, inputTypeLock}
	for i, r := range recs {
		if r.SubID != src.SubID() {
			t.Fatalf("rec %d subID=%d, want %d", i, r.SubID, src.SubID())
		}
		if r.Seq != uint64(i+1) {
			t.Fatalf("rec %d seq=%d, want %d", i, r.Seq, i+1)
		}
		if r.Type != wantTypes[i] {
			t.Fatalf("rec %d type=%d, want %d", i, r.Type, wantTypes[i])
		}
	}
	if recs[0].X != 100 || recs[0].Y != 200 || recs[0].Buttons != 1 {
		t.Fatalf("MOVE rec = %+v", recs[0])
	}
	if recs[1].Btn != 1 || recs[1].Down != 1 {
		t.Fatalf("BUTTON rec = %+v", recs[1])
	}
	if recs[2].Scan != 0x1E || recs[2].Down != 1 || recs[2].Extended != 0 {
		t.Fatalf("KEY rec = %+v", recs[2])
	}
	if len(recs[3].Text) != 2 || recs[3].Text[0] != 0x68 || recs[3].Text[1] != 0x4E2D {
		t.Fatalf("TEXT rec = %+v", recs[3])
	}
	if recs[4].Caps != 1 || recs[4].Num != 0 {
		t.Fatalf("LOCK rec = %+v", recs[4])
	}

	// seq 回退:重放 KEY(seq 3)→ agent 丢弃,host 不见;随后 WHEEL(seq 6)
	// 证明流未受损。
	vs.sendDC(dcLabelInput, encInputMsg(3, inputTypeKey, keyPayload(0x1E, 1, 0)))
	time.Sleep(200 * time.Millisecond)
	if host.count() != 5 {
		t.Fatalf("stale seq reached host: count=%d", host.count())
	}
	wheel := make([]byte, 9)
	gPut32(wheel, 0, s32u(-3))
	gPut32(wheel, 4, 120)
	wheel[8] = 1
	vs.sendDC(dcLabelInput, encInputMsg(6, inputTypeWheel, wheel))
	waitCount(t, host, 6)
	last := host.records()[5]
	if last.Type != inputTypeWheel || last.X != -3 || last.Y != 120 || last.Trackpad != 1 {
		t.Fatalf("WHEEL rec = %+v", last)
	}

	// cursor:fake host 发 0x0109 语义事件 → viewer cursor 通道收 [x][y][vis]。
	src.cursorCh <- CursorEvent{X: 100, Y: 50, Visible: true}
	cb := vs.recvDC(dcLabelCursor, 5*time.Second)
	if len(cb) != 9 {
		t.Fatalf("cursor payload %d bytes, want 9", len(cb))
	}
	if int32(binary.LittleEndian.Uint32(cb)) != 100 ||
		int32(binary.LittleEndian.Uint32(cb[4:])) != 50 || cb[8] != 1 {
		t.Fatalf("cursor payload = %x", cb)
	}
	src.cursorCh <- CursorEvent{X: -20, Y: 7, Visible: false}
	cb = vs.recvDC(dcLabelCursor, 5*time.Second)
	if int32(binary.LittleEndian.Uint32(cb)) != -20 || cb[8] != 0 {
		t.Fatalf("cursor payload 2 = %x", cb)
	}
}

// TestLeaseLoopbackTwoViewers(M2-Slice3 Task 4,split-arbitration 全链):
// server 授予会话(params 带 leaseId L1)输入到达 host;view-only 会话
// (无 leaseId)lease_request → denied{held}、输入丢弃不到 host;持有者
// 断连后,server 向新 viewer 签发 L2(新会话 params)→ 输入经新 sub 到
// 达 host(移交闭环;server = 谁 MAY hold,agent = 执行)。
func TestLeaseLoopbackTwoViewers(t *testing.T) {
	host := newInputHost()
	st := &inputFakeStarter{host: host}
	h := &Handler{Log: slog.Default(), Starter: st}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wsURL := startInputServer(t, ctx, h, func(n int) json.RawMessage {
		switch n {
		case 1: // v1:server 授予 L1
			return json.RawMessage(`{"signaling":"webrtc","iceTransportPolicy":"all","leaseId":"aaaaaaaaaaaaaaaa","capabilities":["screen.view","input.mouse","input.keyboard"]}`)
		default: // v2+:view-only(server 未授予)
			return plainParams()
		}
	})

	// v1(持有者):lease_request → granted;输入到达 host。
	v1 := connectViewerInput(t, ctx, wsURL)
	defer v1.close()
	sendJSON(t, ctx, v1.ws, map[string]any{"type": vocabLeaseRequest})
	if g := v1.nextSignal(func(m map[string]any) bool { return m["type"] == vocabLeaseGranted }, 5*time.Second); g == nil {
		t.Fatal("v1 grant missing")
	}
	v1.sendDC(dcLabelMouse, encMouseMsg(1, 7, 8, 0))
	waitCount(t, host, 1)

	// v2(view-only):denied{held} + 输入丢弃(不到 host,不断连)。
	v2 := connectViewerInput(t, ctx, wsURL)
	defer v2.close()
	sendJSON(t, ctx, v2.ws, map[string]any{"type": vocabLeaseRequest})
	d := v2.nextSignal(func(m map[string]any) bool { return m["type"] == vocabLeaseDenied }, 5*time.Second)
	if d["reason"] != "held" {
		t.Fatalf("denied reason = %v, want held", d["reason"])
	}
	v2.sendDC(dcLabelMouse, encMouseMsg(1, 5, 5, 0))
	time.Sleep(300 * time.Millisecond)
	if host.count() != 1 {
		t.Fatalf("view-only input reached host: %+v", host.records())
	}
	// v2 仍连接(丢弃不惩罚断连):会话信令仍应答。
	sendJSON(t, ctx, v2.ws, map[string]any{"type": vocabKeyframeReq})
	time.Sleep(100 * time.Millisecond)

	// v1 断连 → agent 释放本地登记;server 撤约并签发 L2 给新 viewer v3
	// (模拟:server 在旧持有者关闭后对新会话授予,新 params 带 L2)。
	v1.close()
	v3 := connectViewerInput(t, ctx, wsURL)
	defer v3.close()
	sendJSON(t, ctx, v3.ws, map[string]any{"type": vocabLeaseRequest})
	if g := v3.nextSignal(func(m map[string]any) bool { return m["type"] == vocabLeaseDenied }, 5*time.Second); g == nil {
		t.Fatal("v3 (view-only) must be denied before server grants it")
	}
	v3.close()

	// server 现在授予 v4:新会话 params 带 L2 → 输入经新 sub 到达 host。
	wsURL2 := startInputServer(t, ctx, h, func(int) json.RawMessage {
		return json.RawMessage(`{"signaling":"webrtc","iceTransportPolicy":"all","leaseId":"bbbbbbbbbbbbbbbb","capabilities":["screen.view","input.mouse","input.keyboard"]}`)
	})
	v4 := connectViewerInput(t, ctx, wsURL2)
	defer v4.close()
	sendJSON(t, ctx, v4.ws, map[string]any{"type": vocabLeaseRequest})
	if g := v4.nextSignal(func(m map[string]any) bool { return m["type"] == vocabLeaseGranted }, 5*time.Second); g == nil {
		t.Fatal("v4 grant missing after server re-grant")
	}
	v4.sendDC(dcLabelMouse, encMouseMsg(1, 10, 20, 0))
	waitCount(t, host, 2)
	recs := host.records()
	if recs[0].X != 7 || recs[0].Y != 8 || recs[0].SubID == recs[1].SubID {
		t.Fatalf("v1 rec = %+v, v4 rec = %+v (sub must differ)", recs[0], recs[1])
	}
	if recs[1].X != 10 || recs[1].Y != 20 {
		t.Fatalf("v4 MOVE rec = %+v", recs[1])
	}
}

// TestCapabilityLoopbackEnforced(M2-Slice3 Task 4):会话持 server lease
// 但 capability 集只读(viewer 形态)→ 输入丢弃不到 host;secure_attention
// → {ok:false,code:"capability_denied"} 不触达 Starter。
func TestCapabilityLoopbackEnforced(t *testing.T) {
	host := newInputHost()
	src := newFakeSource()
	st := &fakeStarter{src: src, sasResult: SasResult{OK: true}}
	h := &Handler{Log: slog.Default(), Starter: st}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wsURL := startInputServer(t, ctx, h, func(int) json.RawMessage {
		return json.RawMessage(`{"signaling":"webrtc","iceTransportPolicy":"all","leaseId":"cccccccccccccccc","capabilities":["screen.view"]}`)
	})
	vs := connectViewerInput(t, ctx, wsURL)
	defer vs.close()

	// 持有 lease(viewer 形态被授予输入约):lease_request granted,
	// 但 input.* 缺失 → 输入丢弃。
	sendJSON(t, ctx, vs.ws, map[string]any{"type": vocabLeaseRequest})
	if g := vs.nextSignal(func(m map[string]any) bool { return m["type"] == vocabLeaseGranted }, 5*time.Second); g == nil {
		t.Fatal("grant missing")
	}
	vs.sendDC(dcLabelMouse, encMouseMsg(1, 5, 5, 0))
	time.Sleep(300 * time.Millisecond)
	if host.count() != 0 {
		t.Fatalf("capability-less input reached host: %+v", host.records())
	}

	// SAS:未下发 input.secure_attention → capability_denied,不触达 Starter。
	sendJSON(t, ctx, vs.ws, map[string]any{"type": vocabSecureAttention})
	res := vs.nextSignal(func(m map[string]any) bool { return m["type"] == vocabSecureAttentionResult }, 5*time.Second)
	if res["ok"] != false || res["code"] != "capability_denied" {
		t.Fatalf("secure_attention_result = %v, want capability_denied", res)
	}
	if calls := st.sasCalls(); len(calls) != 0 {
		t.Fatalf("SendSAS calls = %v, must not reach Starter without capability", calls)
	}
}
