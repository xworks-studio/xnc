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
	cfgs      []VideoConfig // QoS 决策记录(M3 Task 3)
	nextKey   bool          // 下帧强制 IDR(镜像 host 的 needsKeyframe)
	closed    bool
	closeOnce sync.Once
	// gapAfterConnect 仅供主 RTP 回环门使用：等待 connect IDR 请求后
	// 输出 1s/31s 两帧。其他共用 fake 保持立即产帧语义。
	gapAfterConnect bool
	// keyEveryFrame 仅供双 viewer 回环用例(终审 lean#4):每帧都产 IDR。
	// fake 的 frameCh 是单通道,两条会话泵分流帧(生产是 host 对每订阅
	// 广播、每会话各自见全帧)—— 按 60 帧节奏出关键帧时,某一侧泵可能
	// 系统性地拿不到关键帧而永久 waitIDR(preKeyDropped 500+ 的实测形
	// 态);每帧皆关键让任一侧都能即刻重建参考链,断言不受分流时序影响。
	keyEveryFrame bool
	monoBase      uint64

	frameCh   chan Frame
	stateCh   chan StateEvent
	displayCh chan DisplayChangedEvent
	done      chan struct{}
}

var fakeSourceMonoBase atomic.Uint64

func newFakeSource() *fakeSource {
	return &fakeSource{
		frameCh:   make(chan Frame, 32),
		stateCh:   make(chan StateEvent, 4),
		displayCh: make(chan DisplayChangedEvent, 4),
		done:      make(chan struct{}),
		// Replacement capture sources share the host's process-wide monotonic
		// clock; distinct bases keep reattach fixtures faithful to that contract.
		monoBase: fakeSourceMonoBase.Add(1_000_000_000),
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

// SetVideoConfig 记录 QoS 决策(M3 Task 3;fake 恒受理)。
func (s *fakeSource) SetVideoConfig(cfg VideoConfig) error {
	s.mu.Lock()
	s.cfgs = append(s.cfgs, cfg)
	s.mu.Unlock()
	return nil
}

// VideoConfigs 返回已收 VideoConfig 快照。
func (s *fakeSource) VideoConfigs() []VideoConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]VideoConfig(nil), s.cfgs...)
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

// run 在连接请求首张 IDR 后以 100fps(加速回环)合成帧直到
// done。首两帧的演示时间相差 30s，用于钉住 RTP 时钟必须把长静止
// 间隔放在当前帧上；之后每 60 帧或 keyframe 请求后产出 IDR。
func (s *fakeSource) run(t *testing.T) {
	t.Helper()
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		mono := s.monoBase
		var i int
		started := !s.gapAfterConnect
		for {
			select {
			case <-s.done:
				return
			case <-ticker.C:
				s.mu.Lock()
				force := s.nextKey
				if force {
					s.nextKey = false
				}
				s.mu.Unlock()
				if !started && !force {
					continue
				}
				if !started {
					mono = 1_000_000
					started = true
				} else if s.gapAfterConnect && i == 1 {
					mono = 31_000_000
				} else {
					mono += 10_000 // 10ms 帧距
				}
				i++
				key := force || s.keyEveryFrame || i%60 == 1
				// v2 身份透传(M3 Task 1):epoch 恒定(重挂 fake 不镜像
				// capture 重建——epoch 恢复路径由 viewer_sender_test 确定性
				// 覆盖),EncodeSeq 逐帧递增。
				f := Frame{
					Key: key, PresentMonoUs: mono, AU: synthAU(key, mono),
					CaptureEpoch: 1, CodecEpoch: 1, ContentID: 1,
					EncodeSeq: uint64(i), SourceMonoUs: mono - 1_000,
				}
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
	rtpPkts        atomic.Uint64
	frames         atomic.Uint64
	keyframes      atomic.Uint64
	firstFrameAt   atomic.Int64
	firstFrameCh   chan struct{}
	trackCh        chan *webrtc.TrackRemote
	rtpTimestampCh chan uint32
}

func newViewerStats() *viewerStats {
	return &viewerStats{
		firstFrameCh:   make(chan struct{}),
		trackCh:        make(chan *webrtc.TrackRemote, 1),
		rtpTimestampCh: make(chan uint32, 128),
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
			if pkt.Marker {
				select {
				case st.rtpTimestampCh <- pkt.Timestamp:
				default:
				}
			}
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
	src.gapAfterConnect = true
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
		h.Handle(ctx, c, "sess-1", json.RawMessage(`{"signaling":"webrtc","iceTransportPolicy":"all","capabilities":["screen.view","input.mouse","input.keyboard","input.secure_attention"]}`))
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
	var firstRTP, secondRTP uint32
	select {
	case firstRTP = <-vstats.rtpTimestampCh:
	case <-time.After(10 * time.Second):
		t.Fatal("viewer: no first frame RTP timestamp")
	}
	select {
	case secondRTP = <-vstats.rtpTimestampCh:
	case <-time.After(10 * time.Second):
		t.Fatal("viewer: no second frame RTP timestamp")
	}
	if delta := secondRTP - firstRTP; delta != 2_700_000 {
		t.Fatalf("30s presentation gap RTP delta=%d, want 2700000", delta)
	}
	waitFrames(t, vstats, 20, 10*time.Second)
	kf0 := vstats.keyframes.Load()
	if kf0 == 0 {
		t.Fatalf("no keyframe before PLI (frames=%d)", vstats.frames.Load())
	}

	// ④ PLI → KeyframeCoordinator(M3 Task 2:connect 请求后 250ms 冷却内的
	// PLI 会被合并——真实 viewer 在未恢复时会按节奏重发,此处同样重发)
	// → RequestKeyframe("pli") → 新 IDR:
	gotPLI := false
	deadline := time.Now().Add(5 * time.Second)
	for !gotPLI && time.Now().Before(deadline) {
		if err := vpc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(track.SSRC())}}); err != nil {
			t.Fatalf("write PLI: %v", err)
		}
		for _, r := range src.KeyRequests() {
			if r == "pli" {
				gotPLI = true
			}
		}
		time.Sleep(300 * time.Millisecond)
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
		h.Handle(ctx, c, "sess-sas", json.RawMessage(`{"signaling":"webrtc","iceTransportPolicy":"all","capabilities":["screen.view","input.mouse","input.keyboard","input.secure_attention"]}`))
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
		h.Handle(ctx, c, "sess-sas2", json.RawMessage(`{"signaling":"webrtc","capabilities":["screen.view","input.secure_attention"]}`))
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
		h.Handle(ctx, c, "sess-sas3", json.RawMessage(`{"signaling":"webrtc","capabilities":["screen.view","input.secure_attention"]}`))
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
func (st *bareStarter) Stop() error                                       { return nil }

// ---- 终审 lean#4:双 viewer 的旁观者暂停/恢复分派回环门 ----

// startViewerPCNoTWCC 建不协商 transport-cc 的接收侧 PC(本用例的确定性
// 形态):H264 反馈声明去掉 transport-cc 且不装 RegisterDefaultInterceptors
//(其末尾的 ConfigureTWCCSender 会按引擎级注册重新带回 transport-cc)→
// SDP 不协商 TWCC、viewer 无反馈发送件 → agent 侧 GCC 收不到任何 TWCC →
// OnTargetBitrateChange 永不触发、agent est 恒为 0 —— 旁观者暂停/恢复
// 完全由测试驱动的浏览器 est(viewer_feedback)裁决,不依赖环回 GCC 的
// 收敛时序(生产里浏览器才有的 availableIncomingBitrate 缺席形态)。
func startViewerPCNoTWCC(t *testing.T) (*webrtc.PeerConnection, *viewerStats) {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: h264FmtpLine,
			RTCPFeedback: []webrtc.RTCPFeedback{
				{Type: webrtc.TypeRTCPFBCCM, Parameter: "fir"},
				{Type: webrtc.TypeRTCPFBNACK},
				{Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"},
			},
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatalf("viewer codecs: %v", err)
	}
	// 空拦截器注册表:接收侧无需任何主动件(RTCP RR/NACK 响应在环回上
	// 既不产生也不需要)。
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(&interceptor.Registry{}))
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
		select { // 与 startViewerPC 同款(容量 1,重复发安全)
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
			if pkt.Marker {
				select {
				case st.rtpTimestampCh <- pkt.Timestamp:
				default:
				}
			}
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

// loopbackViewer 是双 viewer 用例的一侧:WS + PC + 已收 state 帧 code 序列
//(暂停/恢复断言面;后台读泵收集,互斥快照读取)。
type loopbackViewer struct {
	ws    *websocket.Conn
	pc    *webrtc.PeerConnection
	stats *viewerStats

	mu     sync.Mutex
	states []string
}

// hasState 报告是否已收到指定 code 的 state 帧(非阻塞快照)。
func (v *loopbackViewer) hasState(code string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, c := range v.states {
		if c == code {
			return true
		}
	}
	return false
}

// negotiateLoopbackViewer 建一条完整协商的 viewer 会话:dial(路径携带
// sessionID)→ ready → offer/answer → 双向 trickle(后台读泵转发候选并
// 收集 state 帧;与 TestSessionLoopbackVideoAndPLI 同款流程,抽成一侧一
// 个 helper 以驱动两条并行会话)。PC 走 startViewerPCNoTWCC(见其注释:
// 环回无 TWCC → 无 agent est,浏览器 est 全权)。
func negotiateLoopbackViewer(t *testing.T, ctx context.Context, srvURL, sessionID string) *loopbackViewer {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srvURL, "http") + "/" + sessionID
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("viewer %s dial: %v", sessionID, err)
	}
	ws.SetReadLimit(1 << 20)
	drainUntil(t, ctx, ws, 10*time.Second, func(m map[string]any) bool { return m["type"] == vocabReady })

	pc, stats := startViewerPCNoTWCC(t)
	iceCh := make(chan webrtc.ICECandidateInit, 16)
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			iceCh <- c.ToJSON()
		}
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("viewer %s offer: %v", sessionID, err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("viewer %s set local: %v", sessionID, err)
	}
	sendJSON(t, ctx, ws, map[string]any{"type": vocabOffer, "sdp": offer.SDP})
	ans := drainUntil(t, ctx, ws, 10*time.Second, func(m map[string]any) bool { return m["type"] == vocabAnswer })
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: ans["sdp"].(string)}); err != nil {
		t.Fatalf("viewer %s set remote answer: %v", sessionID, err)
	}
	go func() { // viewer → agent 候选
		for c := range iceCh {
			sendJSON(t, ctx, ws, map[string]any{"type": vocabICE, "candidate": c})
		}
	}()
	v := &loopbackViewer{ws: ws, pc: pc, stats: stats}
	go func() { // agent → viewer 候选 + state 帧收集
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
			if m["type"] == vocabState {
				if code, ok := m["code"].(string); ok {
					v.mu.Lock()
					v.states = append(v.states, code)
					v.mu.Unlock()
				}
			}
		}
	}()
	connected := time.Now().Add(10 * time.Second)
	for pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
		if time.Now().After(connected) {
			t.Fatalf("viewer %s never connected, state=%v", sessionID, pc.ConnectionState())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return v
}

// TestSessionLoopbackSpectatorPauseResume — 终审 lean#4:controller + spectator
// 两条会话共享一条流(同一 Handler/Starter → 同一 streamQoS)的
// PauseSpectator/ResumeSpectator 分派回环:
//
//  1. 双方完成协商、帧流在跑;持续 viewer_feedback:ctrl est 8M、spec est
//     100k(< 35%×8M)→ 35% 规则 → spec WS 收 state
//     spectator_network_paused,其 ViewerSender 进入 paused;
//  2. est 恢复(spec 上报 16M > 50%×8M 持续 ≥5s)→ spec WS 收
//     spectator_network_resumed,发送器离开 paused(Resume = waitIDR)。
//
// 环回里走浏览器 est 路径(startViewerPCNoTWCC:viewer 不协商 transport-cc
// → agent 侧 GCC 无 TWCC 输入、agent est 恒 0 —— 环回没有真实 GCC,与
// 生产浏览器 availableIncomingBitrate 缺席的形态一致),暂停/恢复判据完
// 全由测试注入的 estimatedBps 驱动,确定性成立。
func TestSessionLoopbackSpectatorPauseResume(t *testing.T) {
	src := newFakeSource()
	src.keyEveryFrame = true // 双泵分流下任一侧都能即刻过 waitIDR(见字段注释)
	src.run(t)
	st := &fakeStarter{src: src}
	h := &Handler{Log: slog.Default(), Starter: st}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handlersDone := make(chan struct{}, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(1 << 20)
		h.Handle(ctx, c, strings.TrimPrefix(r.URL.Path, "/"),
			json.RawMessage(`{"signaling":"webrtc","iceTransportPolicy":"all","capabilities":["screen.view"]}`))
		handlersDone <- struct{}{}
	}))
	defer srv.Close()

	ctrl := negotiateLoopbackViewer(t, ctx, srv.URL, "ctrl")
	defer func() { _ = ctrl.pc.Close(); ctrl.ws.CloseNow() }()
	spec := negotiateLoopbackViewer(t, ctx, srv.URL, "spec")
	defer func() { _ = spec.pc.Close(); spec.ws.CloseNow() }()

	// 媒体在跑(共享 fakeSource 的帧在两条会话间分流,任意一侧见帧即可)。
	waitFrames(t, spec.stats, 1, 10*time.Second)

	// specSenderState:分派臂的直接观测(spec 会话发送器状态快照)。
	specSenderState := func() string {
		h.qos.mu.Lock()
		defer h.qos.mu.Unlock()
		if s := h.qos.sessions["spec"]; s != nil && s.pub != nil {
			return s.pub.vs.Stats().State
		}
		return ""
	}

	fbFrame := func(est uint64) map[string]any {
		return map[string]any{"type": vocabViewerFeedback, "visible": true,
			"estimatedBps": est, "queueMs": 5}
	}

	// dumpQoS:失败现场诊断(控制器内部态一眼可见)。
	dumpQoS := func(t *testing.T) {
		t.Helper()
		h.qos.mu.Lock()
		defer h.qos.mu.Unlock()
		c := h.qos.ctrl
		t.Logf("qos dump: controller=%q controllerBps=%d estSource=%q emitted=%v",
			c.ControllerID(), c.ControllerBps(), c.EstSource(), c.emitted)
		now := time.Now()
		for id, v := range c.viewers {
			t.Logf("qos dump: viewer=%s visible=%v bps=%d agentEst=%d agentEstAge=%s paused=%v",
				id, v.visible, v.bps, v.agentEst, now.Sub(v.agentEstAt), v.paused)
		}
	}

	// ① 先就位 controller:双 WS 是两条独立连接,反馈的跨连接到达顺序
	// 不受控 —— 先到先得的席位必须显式钉住(偶发 spec 先到即当选,暂停
	// 判据永不作用于 controller,用例将饿死在暂停等待上)。只发 ctrl 反馈
	// 直至选举落定,再引入 spec。
	barrier := time.Now().Add(10 * time.Second)
	for {
		sendJSON(t, ctx, ctrl.ws, fbFrame(8_000_000))
		time.Sleep(100 * time.Millisecond)
		h.qos.mu.Lock()
		elected := h.qos.ctrl.ControllerID()
		h.qos.mu.Unlock()
		if elected == "ctrl" {
			break
		}
		if time.Now().After(barrier) {
			dumpQoS(t)
			t.Fatalf("ctrl never elected controller, controller=%q", elected)
		}
	}

	// ② 暂停分派:35% 规则(ctrl est 8M、spec est 100k < 2.8M)。环回无
	// agent est(见用例头注释),第一对反馈即应裁决;循环驱动直至 state
	// 帧到达(抗 WS/信令时序抖动)。
	deadline := time.Now().Add(45 * time.Second)
	for !spec.hasState(vocabStateSpectatorPaused) && time.Now().Before(deadline) {
		sendJSON(t, ctx, ctrl.ws, fbFrame(8_000_000))
		sendJSON(t, ctx, spec.ws, fbFrame(100_000))
		time.Sleep(300 * time.Millisecond)
	}
	if !spec.hasState(vocabStateSpectatorPaused) {
		dumpQoS(t)
		t.Fatal("spectator never received spectator_network_paused (35% rule dispatch)")
	}
	if ctrl.hasState(vocabStateSpectatorPaused) {
		t.Fatal("controller session must not receive spectator_network_paused")
	}
	dl := time.Now().Add(5 * time.Second)
	for specSenderState() != "paused" {
		if time.Now().After(dl) {
			t.Fatalf("spec sender never paused, state=%q", specSenderState())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// ③ 恢复分派:spec 上报 16M(> 50%×controllerBps=8M → 线 4M)持续
	// ≥5s → spectator_network_resumed + 发送器离开 paused(waitIDR)。
	deadline = time.Now().Add(25 * time.Second)
	for !spec.hasState(vocabStateSpectatorResumed) && time.Now().Before(deadline) {
		sendJSON(t, ctx, ctrl.ws, fbFrame(8_000_000))
		sendJSON(t, ctx, spec.ws, fbFrame(16_000_000))
		time.Sleep(300 * time.Millisecond)
	}
	if !spec.hasState(vocabStateSpectatorResumed) {
		t.Fatal("spectator never received spectator_network_resumed (est recovery dispatch)")
	}
	dl = time.Now().Add(10 * time.Second)
	for state := specSenderState(); state == "paused"; state = specSenderState() {
		if time.Now().After(dl) {
			t.Fatalf("spec sender never resumed, state=%q", state)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// ④ 收线:ctx 取消 → 两条 Handle 都返回。
	cancel()
	for i := 0; i < 2; i++ {
		select {
		case <-handlersDone:
		case <-time.After(10 * time.Second):
			t.Fatalf("Handle %d did not return after ctx cancel", i)
		}
	}
}
