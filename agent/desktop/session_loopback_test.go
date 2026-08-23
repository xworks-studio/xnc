// session_loopback_test.go — T4 回环门:同一进程内「真实 desktop Handler(会话
// WS 信令)+ 真实 Publisher(Pion 发送侧)+ 真实 viewer PeerConnection(Pion
// 接收侧)」三方经 httptest WS 与 ICE host 候选(非 relay——relay-only 需真
// TURN,属 T6 拓扑门)连成一条链,断言:
//
//  1. viewer 依信令词汇(ready/offer/answer/ice)完成协商并收到视频帧
//     (samplebuilder 组回 AU,帧数 > N);
//  2. viewer 发 RTCP PLI → Publisher RTCP 泵 → Handler 将其转成
//     fakeSource.RequestKeyframe("pli") → fakeSource 产出新 IDR → viewer
//     观测到新关键帧(PLI→IDR 全链闭环);
//  3. 会话关闭(ctx 取消)→ Source.Close + Starter.Stop 依序发生,
//     Handler 返回(无 goroutine 泄漏)。
//
// fakeSource 产合成 Annex-B(合法 NAL 头、字节内容不解码校验):IDR AU 足够
// 大(>MTU)以覆盖 FU-A 分片路径。
package desktop

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

// ---- fake Starter/Source ----

type fakeSource struct {
	mu        sync.Mutex
	keyReqs   []string
	nextKey   bool // 下帧强制 IDR(镜像 host 的 needsKeyframe)
	closed    bool
	closeOnce sync.Once

	frameCh   chan Frame
	stateCh   chan StateEvent
	displayCh chan DisplayChangedEvent
	done      chan struct{}
}

func newFakeSource() *fakeSource {
	return &fakeSource{
		frameCh:   make(chan Frame, 32),
		stateCh:   make(chan StateEvent, 4),
		displayCh: make(chan DisplayChangedEvent, 4),
		done:      make(chan struct{}),
	}
}

func (s *fakeSource) RecvFrame(ctx context.Context) (Frame, bool) {
	select {
	case f, ok := <-s.frameCh:
		return f, ok
	case <-ctx.Done():
		return Frame{}, false
	case <-s.done:
		return Frame{}, false
	}
}

func (s *fakeSource) RecvState(ctx context.Context) (StateEvent, bool) {
	select {
	case ev, ok := <-s.stateCh:
		return ev, ok
	case <-ctx.Done():
		return StateEvent{}, false
	case <-s.done:
		return StateEvent{}, false
	}
}

func (s *fakeSource) Hello() *HelloInfo {
	return &HelloInfo{Gen: 1, W: 1920, H: 1080, Fps: 30, MaxSubs: 4}
}

// KeyRequests 返回已收到的 keyframe 请求 reason 快照。
func (s *fakeSource) KeyRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.keyReqs...)
}

func (s *fakeSource) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *fakeSource) RequestKeyframe(reason string) error {
	s.mu.Lock()
	s.keyReqs = append(s.keyReqs, reason)
	s.nextKey = true
	s.mu.Unlock()
	return nil
}

// SubID/SendInput/RecvCursor:Slice3 Source 接口扩展的最小实现(视频回环
// 用例不驱动输入/光标;输入路径的真实验证在 input_loopback_test.go)。
func (s *fakeSource) SubID() uint32 { return 7 }

func (s *fakeSource) SendInput(_ []byte) error { return nil }

func (s *fakeSource) RecvCursor(ctx context.Context) (CursorEvent, bool) {
	select {
	case <-ctx.Done():
		return CursorEvent{}, false
	case <-s.done:
		return CursorEvent{}, false
	}
}

// RecvDisplay:0x010A 镜像(M2-Slice1 Task 2);pushDisplay 注入事件。
func (s *fakeSource) RecvDisplay(ctx context.Context) (DisplayChangedEvent, bool) {
	select {
	case ev, ok := <-s.displayCh:
		return ev, ok
	case <-ctx.Done():
		return DisplayChangedEvent{}, false
	case <-s.done:
		return DisplayChangedEvent{}, false
	}
}

// pushDisplay 注入一条显示变化事件(非阻塞;会话泵消费)。
func (s *fakeSource) pushDisplay(ev DisplayChangedEvent) {
	select {
	case s.displayCh <- ev:
	default:
	}
}

// pushState 注入一条 STATE 事件(非阻塞;M2-Slice1 Task 5 词汇覆盖用)。
func (s *fakeSource) pushState(ev StateEvent) {
	select {
	case s.stateCh <- ev:
	default:
	}
}

func (s *fakeSource) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.done)
	})
	return nil
}

// run 以 100fps(加速回环)合成帧直到 done;每 60 帧或 keyframe 请求后产出 IDR。
func (s *fakeSource) run(t *testing.T) {
	t.Helper()
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		mono := uint64(1_000_000)
		var i int
		for {
			select {
			case <-s.done:
				return
			case <-ticker.C:
				mono += 10_000 // 10ms 帧距
				i++
				s.mu.Lock()
				force := s.nextKey
				s.nextKey = false
				s.mu.Unlock()
				key := force || i%60 == 1
				f := Frame{Key: key, MonoUs: mono, AU: synthAU(key, mono)}
				select {
				case s.frameCh <- f:
				case <-s.done:
					return
				default: // 消费落后即丢(镜像真实客户端)
				}
			}
		}
	}()
}

// ---- 合成 Annex-B ----

var (
	synSPS = []byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x64, 0x00, 0x1f, 0xac, 0xd9, 0x40, 0x50, 0x05, 0xbb, 0x01, 0x6c, 0x80}
	synPPS = []byte{0x00, 0x00, 0x00, 0x01, 0x68, 0xeb, 0xec, 0xb2, 0x2c}
)

// synthAU 产出一帧 Annex-B AU:key = SPS+PPS+大 IDR slice(>MTU 触发 FU-A),
// delta = P slice。slice 载荷用 0xA5 填充(绝不含 start code 序列)。
func synthAU(key bool, mono uint64) []byte {
	var au []byte
	if key {
		au = append(au, synSPS...)
		au = append(au, synPPS...)
	}
	hdr := byte(0x41) // nal_ref_idc=2, type=1 (non-IDR slice)
	if key {
		hdr = 0x65 // type=5 (IDR slice)
	}
	au = append(au, 0x00, 0x00, 0x00, 0x01, hdr, 0x88, byte(mono), byte(mono>>8))
	n := 2000
	if !key {
		n = 600
	}
	for i := 0; i < n; i++ {
		au = append(au, 0xa5)
	}
	return au
}

// fakeStarter 记录 Start/Stop 生命周期;SendSAS(Task 5)记录 reason 并
// 返回预设结果(默认 ok/hr=0),驱动 secure_attention 回环两分支。
type fakeStarter struct {
	src    *fakeSource
	stops  atomic.Int32
	starts atomic.Int32

	sasMu      sync.Mutex
	sasReasons []string
	sasResult  SasResult
	// sasBlock 非 nil 时 SendSAS 阻塞至其被 close(M2-Slice2 Task 1:
	// blocking fake 钉 in-flight 去重与信令循环不阻塞)。
	sasBlock chan struct{}
}

func (st *fakeStarter) Start(_ context.Context, _ uint32) (Source, error) {
	st.starts.Add(1)
	return st.src, nil
}

func (st *fakeStarter) Stop() error {
	st.stops.Add(1)
	return nil
}

// SendSAS 实现 SasCaller(Task 5):记录 reason 快照,回 canned 结果。
// sasBlock 置位时阻塞(T1 去重测试的 blocking fake)。
func (st *fakeStarter) SendSAS(reason string) SasResult {
	st.sasMu.Lock()
	st.sasReasons = append(st.sasReasons, reason)
	res := st.sasResult
	block := st.sasBlock
	st.sasMu.Unlock()
	if block != nil {
		<-block
	}
	return res
}

// sasCalls 返回已收 reason 快照。
func (st *fakeStarter) sasCalls() []string {
	st.sasMu.Lock()
	defer st.sasMu.Unlock()
	return append([]string(nil), st.sasReasons...)
}

// ---- viewer 侧 ----

type viewerStats struct {
	rtpPkts      atomic.Uint64
	frames       atomic.Uint64
	keyframes    atomic.Uint64
	firstFrameAt atomic.Int64
	firstFrameCh chan struct{}
	trackCh      chan *webrtc.TrackRemote
}

func newViewerStats() *viewerStats {
	return &viewerStats{
		firstFrameCh: make(chan struct{}),
		trackCh:      make(chan *webrtc.TrackRemote, 1),
	}
}

// startViewerPC 建接收侧 PC(recvonly video,与 Publisher 同一编解码注册)。
func startViewerPC(t *testing.T) (*webrtc.PeerConnection, *viewerStats) {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := RegisterDesktopCodecs(m); err != nil {
		t.Fatalf("viewer codecs: %v", err)
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		t.Fatalf("viewer interceptors: %v", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("viewer pc: %v", err)
	}
	st := newViewerStats()
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatalf("viewer transceiver: %v", err)
	}
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		select { // 通知 PLI 发送方 track 就绪(容量 1,重复发安全)
		case st.trackCh <- tr:
		default:
		}
		sb := samplebuilder.New(64, &codecs.H264Packet{}, 90000)
		for {
			pkt, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			st.rtpPkts.Add(1)
			sb.Push(pkt)
			for {
				smp := sb.Pop()
				if smp == nil {
					break
				}
				st.frames.Add(1)
				if st.firstFrameAt.CompareAndSwap(0, time.Now().UnixNano()) {
					close(st.firstFrameCh)
				}
				if IsKeyframeAU(smp.Data) {
					st.keyframes.Add(1)
				}
			}
		}
	})
	return pc, st
}

// readSignal 从 viewer WS 读一条信令帧(map 形态便于按 type 分派)。
func readSignal(t *testing.T, ctx context.Context, ws *websocket.Conn, d time.Duration) map[string]any {
	t.Helper()
	type res struct {
		m   map[string]any
		err error
	}
	ch := make(chan res, 1)
	go func() {
		_, r, err := ws.Reader(ctx)
		if err != nil {
			ch <- res{err: err}
			return
		}
		b, err := io.ReadAll(r)
		if err != nil {
			ch <- res{err: err}
			return
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			ch <- res{err: err}
			return
		}
		ch <- res{m: m}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("read signal: %v", r.err)
		}
		return r.m
	case <-time.After(d):
		t.Fatalf("timeout reading signal frame after %v", d)
		return nil
	}
}

// drainUntil 持续读信令直到谓词命中(ICE 候选乱序到达)。
func drainUntil(t *testing.T, ctx context.Context, ws *websocket.Conn, d time.Duration,
	pred func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		m := readSignal(t, ctx, ws, time.Until(deadline))
		if pred(m) {
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout draining signals")
		}
	}
}

func sendJSON(t *testing.T, ctx context.Context, ws *websocket.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal signal: %v", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ws.Write(wctx, websocket.MessageText, b); err != nil {
		t.Fatalf("write signal: %v", err)
	}
}

func waitFrames(t *testing.T, v *viewerStats, n uint64, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for v.frames.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("frames=%d want>=%d (rtp=%d kf=%d)", v.frames.Load(), n, v.rtpPkts.Load(), v.keyframes.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitKeyframes(t *testing.T, v *viewerStats, n uint64, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for v.keyframes.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("keyframes=%d want>=%d", v.keyframes.Load(), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var errWaitTimeout = errors.New("wait timeout")

func waitStops(s *fakeStarter, n int32, d time.Duration) error {
	deadline := time.Now().Add(d)
	for s.stops.Load() < n {
		if time.Now().After(deadline) {
			return errWaitTimeout
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// TestSessionLoopbackVideoAndPLI 是 T4 的核心回环门(见文件头)。
func TestSessionLoopbackVideoAndPLI(t *testing.T) {
	src := newFakeSource()
	src.run(t)
	st := &fakeStarter{src: src}
	h := &Handler{Log: slog.Default(), Starter: st}

	// 会话 WS:httptest server 端跑 Handler;viewer 作 WS 客户端。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(1 << 20)
		h.Handle(ctx, c, "sess-1", json.RawMessage(`{"signaling":"webrtc","iceTransportPolicy":"all"}`))
		close(handlerDone)
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	vws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("viewer dial: %v", err)
	}
	vws.SetReadLimit(1 << 20)
	defer vws.CloseNow()

	// ① ready 先于 answer(词汇契约)。
	got := drainUntil(t, ctx, vws, 10*time.Second, func(m map[string]any) bool { return m["type"] == vocabReady })
	if got["width"].(float64) != 1920 || got["fps"].(float64) != 30 {
		t.Fatalf("ready dims = %v", got)
	}

	// ①b display_changed 信令帧(M2-Slice1 Task 2):源 0x010A →
	// {"type":"display_changed",generation,w,h,reason} 原样透传。
	src.pushDisplay(DisplayChangedEvent{Gen: 3, W: 1280, H: 720, Reason: "resolution"})
	dc := drainUntil(t, ctx, vws, 5*time.Second, func(m map[string]any) bool {
		return m["type"] == vocabDisplayChanged
	})
	if dc["generation"].(float64) != 3 || dc["w"].(float64) != 1280 ||
		dc["h"].(float64) != 720 || dc["reason"] != "resolution" {
		t.Fatalf("display_changed frame = %v", dc)
	}

	// ② viewer 发 offer → agent 回 answer(信令词汇闭环)。
	vpc, vstats := startViewerPC(t)
	defer func() { _ = vpc.Close() }()
	iceCh := make(chan webrtc.ICECandidateInit, 16)
	vpc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			iceCh <- c.ToJSON()
		}
	})
	offer, err := vpc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := vpc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	sendJSON(t, ctx, vws, map[string]any{"type": vocabOffer, "sdp": offer.SDP})

	ans := drainUntil(t, ctx, vws, 10*time.Second, func(m map[string]any) bool { return m["type"] == vocabAnswer })
	if err := vpc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: ans["sdp"].(string)}); err != nil {
		t.Fatalf("set remote answer: %v", err)
	}
	// trickle 双向:viewer 本地候选 → agent;agent 候选 → viewer(后台泵,
	// 读失败即退——主线程只轮询连接状态,杜绝 readSignal 阻塞漏检)。
	go func() {
		for c := range iceCh {
			sendJSON(t, ctx, vws, map[string]any{"type": vocabICE, "candidate": c})
		}
	}()
	go func() {
		for {
			mt, r, err := vws.Reader(ctx)
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
			if json.Unmarshal(b, &m) != nil || m["type"] != vocabICE || m["candidate"] == nil {
				continue
			}
			var ci webrtc.ICECandidateInit
			jb, _ := json.Marshal(m["candidate"])
			if json.Unmarshal(jb, &ci) == nil {
				_ = vpc.AddICECandidate(ci)
			}
		}
	}()
	connected := time.Now().Add(10 * time.Second)
	for vpc.ConnectionState() != webrtc.PeerConnectionStateConnected {
		if time.Now().After(connected) {
			t.Fatalf("viewer never connected, state=%v", vpc.ConnectionState())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// ③ 帧流:首帧 + 帧数断言。
	var track *webrtc.TrackRemote
	select {
	case track = <-vstats.trackCh: // OnTrack 一次性投递,留作 PLI 目标
	case <-time.After(10 * time.Second):
		t.Fatalf("viewer: no track")
	}
	select {
	case <-vstats.firstFrameCh:
	case <-time.After(10 * time.Second):
		t.Fatalf("no first frame; rtp=%d", vstats.rtpPkts.Load())
	}
	waitFrames(t, vstats, 20, 10*time.Second)
	kf0 := vstats.keyframes.Load()
	if kf0 == 0 {
		t.Fatalf("no keyframe before PLI (frames=%d)", vstats.frames.Load())
	}

	// ④ PLI → RequestKeyframe("pli") → 新 IDR:
	if err := vpc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}}); err != nil {
		t.Fatalf("write PLI: %v", err)
	}
	gotPLI := false
	deadline := time.Now().Add(5 * time.Second)
	for !gotPLI && time.Now().Before(deadline) {
		for _, r := range src.KeyRequests() {
			if r == "pli" {
				gotPLI = true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gotPLI {
		t.Fatalf("PLI never reached source as RequestKeyframe(\"pli\"); reqs=%v", src.KeyRequests())
	}
	waitKeyframes(t, vstats, kf0+1, 5*time.Second) // PLI → 新 IDR(松弛 5s 抗抖动)

	// ④b keyframe-req 信令帧(浏览器 PLI 按钮路径,T6)→ 同一
	// RequestKeyframe 路径(reason="viewer-pli")→ 新 IDR。
	kf1 := vstats.keyframes.Load()
	sendJSON(t, ctx, vws, map[string]any{"type": vocabKeyframeReq})
	gotViewerPLI := false
	deadline = time.Now().Add(5 * time.Second)
	for !gotViewerPLI && time.Now().Before(deadline) {
		for _, r := range src.KeyRequests() {
			if r == "viewer-pli" {
				gotViewerPLI = true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gotViewerPLI {
		t.Fatalf("keyframe-req never reached source as RequestKeyframe(\"viewer-pli\"); reqs=%v", src.KeyRequests())
	}
	waitKeyframes(t, vstats, kf1+1, 5*time.Second)

	// ⑤ 会话关闭:ctx 取消 → Source 关闭 + Stop 恰一次 + Handler 返回。
	cancel()
	select {
	case <-handlerDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("Handler did not return after ctx cancel")
	}
	if err := waitStops(st, 1, 5*time.Second); err != nil {
		t.Fatalf("Starter.Stop not called: %v", err)
	}
	if !src.isClosed() {
		t.Fatalf("source not closed after session end")
	}
}

// TestSessionSecureAttentionAndStateVocab — M2-Slice1 Task 5 回环门:
//
//  1. viewer → {"type":"secure_attention"} → Handler 经 Starter(SasCaller)
//     以 reason="viewer" 调 SendSAS → 回 {"type":"secure_attention_result",
//     ok,hr[,code]}(ok 与 denied 两分支,hr 原样透传);
//  2. Starter 无 SasCaller 能力 → 回 ok=false code="unsupported"(向后
//     兼容:旧拓扑 agent 不断连);
//  3. T2 状态词汇覆盖:STATE{recovering/capture_rebuilt/backend_changed}
//     → {"type":"state",code,recoverable} 原样透传(与 display_changed
//     同一条泵路径,词汇统一)。
func TestSessionSecureAttentionAndStateVocab(t *testing.T) {
	src := newFakeSource()
	st := &fakeStarter{src: src, sasResult: SasResult{OK: true, HR: 0x80070005}}
	h := &Handler{Log: slog.Default(), Starter: st}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(1 << 20)
		h.Handle(ctx, c, "sess-sas", json.RawMessage(`{"signaling":"webrtc","iceTransportPolicy":"all"}`))
		close(handlerDone)
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	vws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("viewer dial: %v", err)
	}
	vws.SetReadLimit(1 << 20)
	defer vws.CloseNow()

	drainUntil(t, ctx, vws, 10*time.Second, func(m map[string]any) bool { return m["type"] == vocabReady })

	// ① ok 分支:reason="viewer" 到达 Starter;hr 原样透传。
	sendJSON(t, ctx, vws, map[string]any{"type": vocabSecureAttention})
	res := drainUntil(t, ctx, vws, 5*time.Second, func(m map[string]any) bool {
		return m["type"] == vocabSecureAttentionResult
	})
	if res["ok"] != true || res["hr"].(float64) != 0x80070005 {
		t.Fatalf("secure_attention_result(ok) = %v", res)
	}
	if calls := st.sasCalls(); len(calls) != 1 || calls[0] != "viewer" {
		t.Fatalf("SendSAS calls = %v, want [viewer]", calls)
	}

	// ② denied 分支:ok=false + 稳定码(门控拒绝的透传面)。
	st.sasMu.Lock()
	st.sasResult = SasResult{OK: false, HR: 0, Code: "SAS_DENIED"}
	st.sasMu.Unlock()
	sendJSON(t, ctx, vws, map[string]any{"type": vocabSecureAttention})
	res = drainUntil(t, ctx, vws, 5*time.Second, func(m map[string]any) bool {
		return m["type"] == vocabSecureAttentionResult
	})
	if res["ok"] != false || res["code"] != "SAS_DENIED" {
		t.Fatalf("secure_attention_result(denied) = %v", res)
	}
	if calls := st.sasCalls(); len(calls) != 2 {
		t.Fatalf("SendSAS calls = %d after second request", len(calls))
	}

	// ③ 状态词汇:STATE 新稳定码经同一泵路径透传(字段逐一对账)。
	for _, code := range []string{"recovering", "capture_rebuilt", "backend_changed"} {
		src.pushState(StateEvent{Code: code, Recoverable: true})
		got := drainUntil(t, ctx, vws, 5*time.Second, func(m map[string]any) bool {
			return m["type"] == vocabState && m["code"] == code
		})
		if got["recoverable"] != true {
			t.Fatalf("state %s frame = %v (recoverable must ride along)", code, got)
		}
	}
	// 非可恢复事件同样透传(recoverable=false 不被吞)。
	src.pushState(StateEvent{Code: "backend_changed", Recoverable: false})
	got := drainUntil(t, ctx, vws, 5*time.Second, func(m map[string]any) bool {
		return m["type"] == vocabState && m["recoverable"] == false
	})
	if got["code"] != "backend_changed" {
		t.Fatalf("state(recoverable=false) frame = %v", got)
	}

	cancel()
	select {
	case <-handlerDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("Handler did not return after ctx cancel")
	}
}

// TestSessionSecureAttentionUnsupported — Starter 不实现 SasCaller(非
// Windows 桩 / 无 core 拓扑):回 ok=false code="unsupported",会话不断。
func TestSessionSecureAttentionUnsupported(t *testing.T) {
	src := newFakeSource()
	h := &Handler{Log: slog.Default(), Starter: &bareStarter{src: src}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(1 << 20)
		h.Handle(ctx, c, "sess-sas2", json.RawMessage(`{"signaling":"webrtc"}`))
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	vws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("viewer dial: %v", err)
	}
	vws.SetReadLimit(1 << 20)
	defer vws.CloseNow()

	drainUntil(t, ctx, vws, 10*time.Second, func(m map[string]any) bool { return m["type"] == vocabReady })
	sendJSON(t, ctx, vws, map[string]any{"type": vocabSecureAttention})
	res := drainUntil(t, ctx, vws, 5*time.Second, func(m map[string]any) bool {
		return m["type"] == vocabSecureAttentionResult
	})
	if res["ok"] != false || res["code"] != "unsupported" {
		t.Fatalf("secure_attention_result(unsupported) = %v", res)
	}
	cancel()
}

// TestSessionSecureAttentionDedupBusy — M2-Slice2 Task 1 回环门:
//
//  1. blocking fake(SendSAS 阻塞至放行)期间,第二条 secure_attention
//     立即回 {ok:false,code:"busy"},不触达 Starter;
//  2. SAS 在途不阻塞信令循环:keyframe-req 仍到达 RequestKeyframe;
//  3. 放行后第一条回 ok;in-flight 解除后新 SAS 可再发(去重不是熔断)。
func TestSessionSecureAttentionDedupBusy(t *testing.T) {
	src := newFakeSource()
	st := &fakeStarter{src: src, sasResult: SasResult{OK: true, HR: 0x1}}
	st.sasBlock = make(chan struct{})
	h := &Handler{Log: slog.Default(), Starter: st}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(1 << 20)
		h.Handle(ctx, c, "sess-sas3", json.RawMessage(`{"signaling":"webrtc"}`))
		close(handlerDone)
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	vws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("viewer dial: %v", err)
	}
	vws.SetReadLimit(1 << 20)
	defer vws.CloseNow()

	drainUntil(t, ctx, vws, 10*time.Second, func(m map[string]any) bool { return m["type"] == vocabReady })

	// ① 第一条 SAS 在途(blocking fake 摁住);第二条立即回 busy。
	sendJSON(t, ctx, vws, map[string]any{"type": vocabSecureAttention})
	sendJSON(t, ctx, vws, map[string]any{"type": vocabSecureAttention})
	res := drainUntil(t, ctx, vws, 5*time.Second, func(m map[string]any) bool {
		return m["type"] == vocabSecureAttentionResult
	})
	if res["ok"] != false || res["code"] != "busy" {
		t.Fatalf("secure_attention_result(busy) = %v", res)
	}
	if calls := st.sasCalls(); len(calls) != 1 {
		t.Fatalf("SendSAS calls = %v, busy must not reach Starter", calls)
	}

	// ② 信令循环未被 SAS 阻塞:keyframe-req 在阻塞期间仍到达 Source。
	sendJSON(t, ctx, vws, map[string]any{"type": vocabKeyframeReq})
	gotViewerPLI := false
	deadline := time.Now().Add(5 * time.Second)
	for !gotViewerPLI && time.Now().Before(deadline) {
		for _, r := range src.KeyRequests() {
			if r == "viewer-pli" {
				gotViewerPLI = true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gotViewerPLI {
		t.Fatalf("signaling loop stalled while SAS in flight; reqs=%v", src.KeyRequests())
	}

	// ③ 放行 → 第一条回 ok;之后新 SAS 立即通过(去重解除)。
	close(st.sasBlock)
	res = drainUntil(t, ctx, vws, 5*time.Second, func(m map[string]any) bool {
		return m["type"] == vocabSecureAttentionResult && m["code"] == nil
	})
	if res["ok"] != true || res["hr"].(float64) != 1 {
		t.Fatalf("secure_attention_result(ok after unblock) = %v", res)
	}
	sendJSON(t, ctx, vws, map[string]any{"type": vocabSecureAttention})
	res = drainUntil(t, ctx, vws, 5*time.Second, func(m map[string]any) bool {
		return m["type"] == vocabSecureAttentionResult
	})
	if res["ok"] != true {
		t.Fatalf("secure_attention_result(released) = %v", res)
	}
	if calls := st.sasCalls(); len(calls) != 2 {
		t.Fatalf("SendSAS calls = %v, want [viewer viewer]", calls)
	}

	cancel()
	select {
	case <-handlerDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("Handler did not return after ctx cancel")
	}
}

// bareStarter 只有 Start/Stop(无 SasCaller):unsupported 分支的注入面。
type bareStarter struct{ src *fakeSource }

func (st *bareStarter) Start(_ context.Context, _ uint32) (Source, error) { return st.src, nil }
func (st *bareStarter) Stop() error                                      { return nil }
