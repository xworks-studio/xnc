// frame_meta_test.go — M3 Task 4:frame-meta 遥测通道的确定性单测。
//
// 覆盖三层:
//  1. 编解码:56 字节黄金向量(固定输入 + 固定 key;Task 5 TS 侧逐字节
//     平移同一向量)、小端字段落位、长度/版本拒绝、会话键控 hash 语义
//     (同 key 同输入确定;异 key 异 hash;绝无「裸内容指纹」)。
//  2. 发送路径接线(PC-free):tracker 只在本帧最后一包(Marker)写出后
//     恰一次汇出 meta(未被送出的帧——抑制/丢弃——绝不发);relay 的
//     非阻塞入队(满则丢 + 计数,绝不为遥测阻塞媒体)。
//  3. 回环门(真实双 PeerConnection):viewer 侧 "frame-meta" 通道按
//     unordered + 不重传协商;meta 的 rtpTimestamp 与该帧 RTP 最后一包
//     实际盖章的时戳逐帧相等(裁决 4:meta 是 per-viewer 的)。
package desktop

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/fnv"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// ---- 黄金向量(固定输入 + 固定 key;裁决 2:两侧共用,禁手拼)----

// frameMetaGoldenKey 是黄金向量固定的 64 位会话键(仅测试;生产键在
// Publisher 构造时随机生成,绝不落盘/入日志)。
const frameMetaGoldenKey uint64 = 0x0F1E2D3C4B5A6978

// frameMetaGolden 是黄金向量的输入字段。
var frameMetaGolden = FrameMetaV1{
	RTPTimestamp: 0x0024AC68,
	CodecEpoch:   0x0102030405060708,
	ContentID:    0x1122334455667788,
	EncodeSeq:    0xA5,
	SourceMonoUs: 0x0000C0DE00000042,
}

// frameMetaGoldenHex 是 frameMetaGolden + frameMetaGoldenKey 的精确 56 字节
// 编码(小端;offset 40..48 为键控 hash,其余与 key 无关)。
const frameMetaGoldenHex = "0100000068AC240008070605040302018877665544332211A5" +
	"0000000000000042000000DEC000008C2A99D0FD2F08BB0000000000000000"

// TestFrameMetaV1GoldenVector 钉死 56 字节逐字节编码(含键控 hash)与解码
// 往返。Task 5(TS)平移本向量;任何布局漂移在此即刻失败。
func TestFrameMetaV1GoldenVector(t *testing.T) {
	m := newFrameMetaV1(frameMetaGoldenKey, Frame{
		CaptureEpoch:  frameMetaGolden.CodecEpoch, // 注:capture epoch 不进记录
		CodecEpoch:    frameMetaGolden.CodecEpoch,
		ContentID:     frameMetaGolden.ContentID,
		EncodeSeq:     frameMetaGolden.EncodeSeq,
		SourceMonoUs:  frameMetaGolden.SourceMonoUs,
	}, frameMetaGolden.RTPTimestamp)
	b := encodeFrameMetaV1(m)
	if len(b) != frameMetaV1Size {
		t.Fatalf("encoded length = %d, want %d", len(b), frameMetaV1Size)
	}
	if got := hexOf(b); got != frameMetaGoldenHex {
		t.Fatalf("golden vector mismatch:\n got %s\nwant %s", got, frameMetaGoldenHex)
	}
	// 解码往返。
	d, err := decodeFrameMetaV1(b)
	if err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	if d != m {
		t.Fatalf("round-trip mismatch: got %+v want %+v", d, m)
	}
	// 同 key 同输入:逐字节确定。
	if b2 := encodeFrameMetaV1(newFrameMetaV1(frameMetaGoldenKey, Frame{
		CodecEpoch: frameMetaGolden.CodecEpoch, ContentID: frameMetaGolden.ContentID,
		EncodeSeq: frameMetaGolden.EncodeSeq, SourceMonoUs: frameMetaGolden.SourceMonoUs,
	}, frameMetaGolden.RTPTimestamp)); string(b2) != string(b) {
		t.Fatal("same key+input produced different bytes")
	}
}

// TestFrameMetaV1LayoutOffsets 逐字段验证 56 字节小端布局与保留区清零
//(布局表见 frame_meta.go)。
func TestFrameMetaV1LayoutOffsets(t *testing.T) {
	b := encodeFrameMetaV1(func() FrameMetaV1 {
		m := frameMetaGolden
		m.Hash64 = frameMetaHash64(frameMetaGoldenKey, frameMetaGolden)
		return m
	}())
	if len(b) != 56 {
		t.Fatalf("length = %d, want 56", len(b))
	}
	if b[0] != 1 {
		t.Fatalf("version = %d, want 1", b[0])
	}
	if b[1] != 0 {
		t.Fatalf("flags = %d, want 0 (reserved)", b[1])
	}
	checks := []struct {
		name string
		off  int
		want uint64
	}{
		{"rtpTimestamp", 4, uint64(frameMetaGolden.RTPTimestamp)},
		{"codecEpoch", 8, frameMetaGolden.CodecEpoch},
		{"contentId", 16, frameMetaGolden.ContentID},
		{"encodeSeq", 24, frameMetaGolden.EncodeSeq},
		{"sourceMonoUs", 32, frameMetaGolden.SourceMonoUs},
		{"hash64", 40, frameMetaHash64(frameMetaGoldenKey, frameMetaGolden)},
	}
	for _, c := range checks {
		var got uint64
		switch c.off {
		case 4:
			got = uint64(binary.LittleEndian.Uint32(b[4:]))
		default:
			got = binary.LittleEndian.Uint64(b[c.off:])
		}
		if got != c.want {
			t.Fatalf("%s@%d = %#x, want %#x (little-endian)", c.name, c.off, got, c.want)
		}
	}
	for i := 2; i < 4; i++ {
		if b[i] != 0 {
			t.Fatalf("reserved byte %d = %#x, want 0", i, b[i])
		}
	}
	for i := 48; i < 56; i++ {
		if b[i] != 0 {
			t.Fatalf("trailing reserved byte %d = %#x, want 0", i, b[i])
		}
	}
}

// TestFrameMetaV1DecodeRejectsVersionAndLength:非法长度/版本一律拒绝。
func TestFrameMetaV1DecodeRejectsVersionAndLength(t *testing.T) {
	good := encodeFrameMetaV1(newFrameMetaV1(frameMetaGoldenKey, Frame{
		CodecEpoch: 1, ContentID: 2, EncodeSeq: 3, SourceMonoUs: 4,
	}, 0x1234_5678))
	if _, err := decodeFrameMetaV1(good); err != nil {
		t.Fatalf("good record rejected: %v", err)
	}
	for _, n := range []int{0, 1, 55, 57, 64} {
		if _, err := decodeFrameMetaV1(make([]byte, n)); err == nil {
			t.Fatalf("length %d accepted, want rejection", n)
		}
	}
	for _, v := range []byte{0, 2, 0xFF} {
		bad := append([]byte(nil), good...)
		bad[0] = v
		if _, err := decodeFrameMetaV1(bad); err == nil {
			t.Fatalf("version %d accepted, want rejection", v)
		}
	}
}

// TestFrameMetaV1KeyedHashSemantics:裁决 1 的键控 hash——同 key 同输入
// 确定、异 key 异 hash(非可复用内容指纹)、与标准库 FNV-1a 交叉验证、
// 无 key 时(hash 覆盖字段裸 FNV)必不相等(键真实参与混合)。
func TestFrameMetaV1KeyedHashSemantics(t *testing.T) {
	base := frameMetaGolden
	h1 := frameMetaHash64(frameMetaGoldenKey, base)
	if h1 != frameMetaHash64(frameMetaGoldenKey, base) {
		t.Fatal("hash not deterministic for same key+input")
	}
	if h2 := frameMetaHash64(frameMetaGoldenKey^1, base); h2 == h1 {
		t.Fatal("different key produced identical hash (key not mixed in)")
	}
	// 同字段、两个 key:完整记录仅在 hash 字段(offset 40..48)不同。
	m1, m2 := base, base
	m1.Hash64, m2.Hash64 = h1, h1^0xFFFF
	b1, b2 := encodeFrameMetaV1(m1), encodeFrameMetaV1(m2)
	for i := 0; i < 56; i++ {
		inHash := i >= 40 && i < 48
		if (b1[i] != b2[i]) && !inHash {
			t.Fatalf("byte %d differs outside hash field", i)
		}
	}
	// 标准库 hash/fnv 交叉验证预映像算法(非自证)。
	pre := make([]byte, 0, 40)
	for _, u := range []uint64{base.CodecEpoch, base.ContentID, base.EncodeSeq, base.SourceMonoUs, frameMetaGoldenKey} {
		var w [8]byte
		binary.LittleEndian.PutUint64(w[:], u)
		pre = append(pre, w[:]...)
	}
	f := fnv.New64a()
	_, _ = f.Write(pre)
	if got := f.Sum64(); got != h1 {
		t.Fatalf("hash = %#x, want stdlib FNV-1a64 %#x over canonical preimage", got, h1)
	}
	// 无键裸字段 hash 必不同(键控语义成立)。
	nk := fnv.New64a()
	_, _ = nk.Write(pre[:32])
	if nk.Sum64() == h1 {
		t.Fatal("keyed hash equals unkeyed field hash")
	}
	// 异输入异 hash(逐字段扰动)。
	for i, mut := range []FrameMetaV1{
		{CodecEpoch: base.CodecEpoch + 1, ContentID: base.ContentID, EncodeSeq: base.EncodeSeq, SourceMonoUs: base.SourceMonoUs},
		{CodecEpoch: base.CodecEpoch, ContentID: base.ContentID + 1, EncodeSeq: base.EncodeSeq, SourceMonoUs: base.SourceMonoUs},
		{CodecEpoch: base.CodecEpoch, ContentID: base.ContentID, EncodeSeq: base.EncodeSeq + 1, SourceMonoUs: base.SourceMonoUs},
		{CodecEpoch: base.CodecEpoch, ContentID: base.ContentID, EncodeSeq: base.EncodeSeq, SourceMonoUs: base.SourceMonoUs + 1},
	} {
		if frameMetaHash64(frameMetaGoldenKey, mut) == h1 {
			t.Fatalf("field %d perturbation did not change hash", i)
		}
	}
}

// ---- 发送路径接线(PC-free)----

// metaRecorder 收集 tracker 汇出的 meta(线程安全)。
type metaRecorder struct {
	mu sync.Mutex
	ms []FrameMetaV1
}

func (r *metaRecorder) emit(m FrameMetaV1) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ms = append(r.ms, m)
}

func (r *metaRecorder) metas() []FrameMetaV1 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]FrameMetaV1(nil), r.ms...)
}

func metaPkt(ts uint32, marker bool) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Timestamp: ts, Marker: marker}}
}

// TestFrameMetaTrackerEmitsAfterMarkerOnly:meta 只在本帧最后一包
//(Marker)写出后汇出恰一次;身份/时戳取自该帧;未被送出的帧(抑制/
// 丢弃/冲刷前弃包)绝不发;合帧冲刷(旧帧余包在新帧 Enqueue 内写出)
// 的交错形态仍归属正确。
func TestFrameMetaTrackerEmitsAfterMarkerOnly(t *testing.T) {
	const key = 0xABCD_EF01_2345_6789
	rec := &metaRecorder{}
	tr := newFrameMetaTracker(key, rec.emit)

	fA := Frame{CodecEpoch: 1, ContentID: 10, EncodeSeq: 100, SourceMonoUs: 1000}
	fB := Frame{CodecEpoch: 1, ContentID: 11, EncodeSeq: 101, SourceMonoUs: 2000} // 从未写出(被抑制)
	fC := Frame{CodecEpoch: 2, ContentID: 12, EncodeSeq: 102, SourceMonoUs: 3000}
	fD := Frame{CodecEpoch: 2, ContentID: 12, EncodeSeq: 103, SourceMonoUs: 4000} // 与 fD' 同身份异帧

	const tsA, tsC, tsD, tsD2 uint32 = 1000, 2000, 3000, 4000

	// 帧 A:两包无 marker → 不发;stage B(将被丢弃,永不写出)→ stage C
	//(模拟合帧:在 C 的 Enqueue 内 A 的余包被冲刷写出)。
	tr.stage(fA)
	tr.onPacket(metaPkt(tsA, false))
	tr.onPacket(metaPkt(tsA, false))
	if got := rec.metas(); len(got) != 0 {
		t.Fatalf("meta before marker: %+v", got)
	}
	tr.stage(fB)
	tr.stage(fC)
	// A 的最后一包在 C 入队期间写出 → meta A(在最后一包之后)。
	tr.onPacket(metaPkt(tsA, true))
	// C 的包:首包换时戳 → 身份切到 C;最后一包 → meta C。
	tr.onPacket(metaPkt(tsC, false))
	tr.onPacket(metaPkt(tsC, true))

	// 同身份两帧(仅时戳前进):各发各的 meta。
	tr.stage(fD)
	tr.onPacket(metaPkt(tsD, true))
	tr.stage(fD)
	tr.onPacket(metaPkt(tsD2, true))

	got := rec.metas()
	if len(got) != 4 {
		t.Fatalf("emits = %d (%+v), want 4 (A,C,D,D)", len(got), got)
	}
	want := []struct {
		f  Frame
		ts uint32
	}{{fA, tsA}, {fC, tsC}, {fD, tsD}, {fD, tsD2}}
	for i, w := range want {
		g := got[i]
		if g.CodecEpoch != w.f.CodecEpoch || g.ContentID != w.f.ContentID ||
			g.EncodeSeq != w.f.EncodeSeq || g.SourceMonoUs != w.f.SourceMonoUs {
			t.Fatalf("emit %d identity = %+v, want frame %+v", i, g, w.f)
		}
		if g.RTPTimestamp != w.ts {
			t.Fatalf("emit %d rtpTimestamp = %d, want %d (per-viewer clock value)", i, g.RTPTimestamp, w.ts)
		}
		if g.Hash64 != frameMetaHash64(key, newFrameMetaV1(key, w.f, w.ts)) {
			t.Fatalf("emit %d hash = %#x, want keyed hash over identity", i, g.Hash64)
		}
	}
	// B 从未送出:不发 meta(上面 len==4 已含此断言;显式复核无 ContentID==11)。
	for i, g := range got {
		if g.ContentID == fB.ContentID {
			t.Fatalf("emit %d is never-sent frame B", i)
		}
	}
}

// TestFrameMetaTrackerNothingStagedNoEmit:未 stage 任何帧时包写出不产生
// meta(防御形态;生产上 WriteFrame 先于一切包写出)。
func TestFrameMetaTrackerNothingStagedNoEmit(t *testing.T) {
	rec := &metaRecorder{}
	tr := newFrameMetaTracker(1, rec.emit)
	tr.onPacket(metaPkt(7, true))
	if got := rec.metas(); len(got) != 0 {
		t.Fatalf("unstaged packet produced meta: %+v", got)
	}
}

// TestFrameMetaRelayNonBlockingAndDrops:入队永不阻塞(select/default),
// 缓冲满 → 丢弃 + telemetryDrops;发送失败 → 丢弃 + 计数;发送成功 →
// 字节到达出口;close 幂等收线。
func TestFrameMetaRelayNonBlockingAndDrops(t *testing.T) {
	// ① 出口永久阻塞:批量入队仍即刻返回(绝不阻塞媒体路径)。
	block := make(chan struct{})
	stuck := newFrameMetaRelay(func([]byte) error { <-block; return nil })
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 512; i++ {
			stuck.enqueue(frameMetaGolden)
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("enqueue blocked on a stuck DataChannel send")
	}
	if d := stuck.drops.Load(); d == 0 {
		t.Fatal("drops not counted on overflow of a stuck relay")
	}
	close(block)
	stuck.close()

	// ② 发送失败计数;成功送达。
	var mu sync.Mutex
	var sent [][]byte
	fail := true
	r := newFrameMetaRelay(func(b []byte) error {
		if fail {
			return errMetaTestSend
		}
		mu.Lock()
		cp := append([]byte(nil), b...)
		sent = append(sent, cp)
		mu.Unlock()
		return nil
	})
	r.enqueue(frameMetaGolden) // fail=true → drop 计数
	waitFor(t, time.Second, func() bool { return r.drops.Load() >= 1 })
	fail = false
	r.enqueue(frameMetaGolden)
	waitFor(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(sent) == 1
	})
	mu.Lock()
	b := sent[0]
	mu.Unlock()
	if len(b) != frameMetaV1Size {
		t.Fatalf("delivered %d bytes, want %d", len(b), frameMetaV1Size)
	}
	if d, err := decodeFrameMetaV1(b); err != nil || d != frameMetaGolden {
		t.Fatalf("delivered record = %+v err=%v, want golden", d, err)
	}
	r.close()
	r.close() // 幂等
}

var errMetaTestSend = &metaTestSendError{}

type metaTestSendError struct{}

func (*metaTestSendError) Error() string { return "meta test send failure" }

// ---- 回环门(真实双 PeerConnection;裁决 3/4)----

// connectViewerMeta 是 connectViewerInput 的 meta 观测变体:保留 viewerStats
//(marker 包的 RTP 时戳)以便逐帧比对 meta 时戳,并多等 frame-meta 通道
// open。其余形态(ready 先行 / offer-answer / trickle)与原封装一致。
func connectViewerMeta(t *testing.T, ctx context.Context, wsURL string) (*viewerSession, *viewerStats) {
	t.Helper()
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("viewer dial: %v", err)
	}
	ws.SetReadLimit(1 << 20)
	if got := drainUntil(t, ctx, ws, 10*time.Second, func(m map[string]any) bool { return m["type"] == vocabReady }); got == nil {
		t.Fatal("no ready frame")
	}
	pc, vstats := startViewerPC(t)
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
	if _, err := pc.CreateDataChannel("neg", nil); err != nil {
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
	for _, label := range []string{dcLabelInput, dcLabelMouse, dcLabelCursor, dcLabelFrameMeta} {
		for {
			vs.mu.Lock()
			dc := vs.dcs[label]
			vs.mu.Unlock()
			if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("data channel %q never opened (got %v)", label, vs.dcLabels())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return vs, vstats
}

// TestFrameMetaLoopbackRidesAfterLastRTP:真实双 PC——喂带 v2 身份的帧,
// viewer 侧 "frame-meta" 通道收到 56 字节记录;通道按 unordered+不重传
// 协商;meta 的 rtpTimestamp 与该帧 RTP 最后一包(marker)实际盖章的
// per-viewer 时戳逐帧相等(裁决 4);身份字段逐帧对应。
func TestFrameMetaLoopbackRidesAfterLastRTP(t *testing.T) {
	host := newInputHost()
	st := &inputFakeStarter{host: host}
	h := &Handler{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Starter: st}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wsURL := startInputServer(t, ctx, h, func(int) json.RawMessage { return plainParams() })
	vs, vstats := connectViewerMeta(t, ctx, wsURL)
	defer vs.close()
	src := st.Last()
	if src == nil {
		t.Fatal("no source created")
	}

	// 通道协商形态:unordered + 不重传(DNC 携带;viewer 侧解析)。
	vs.mu.Lock()
	fdc := vs.dcs[dcLabelFrameMeta]
	vs.mu.Unlock()
	if fdc == nil {
		t.Fatalf("no frame-meta channel (have %v)", vs.dcLabels())
	}
	if fdc.Ordered() {
		t.Fatal("frame-meta channel negotiated ordered, want unordered")
	}
	if mr := fdc.MaxRetransmits(); mr == nil || *mr != 0 {
		t.Fatalf("frame-meta MaxRetransmits = %v, want 0", mr)
	}

	frames := []Frame{
		{Key: true, PresentMonoUs: 1_000_000,
			CaptureEpoch: 3, CodecEpoch: 9, ContentID: 0x1122334455667788,
			EncodeSeq: 7, SourceMonoUs: 999_000, AU: vsNAL(true, 1, 40)},
		{PresentMonoUs: 1_033_000,
			CaptureEpoch: 3, CodecEpoch: 9, ContentID: 0x1122334455667789,
			EncodeSeq: 8, SourceMonoUs: 1_032_000, AU: vsNAL(false, 2, 40)},
	}
	hashes := make(map[uint64]bool)
	for i, f := range frames {
		src.frameCh <- f
		// 先收 RTP 最后一包(marker)的实际时戳,再收 meta:顺序喂帧,
		// 配对无歧义(SRTP/SCTP 之间无跨流保序)。
		var rtpTS uint32
		select {
		case rtpTS = <-vstats.rtpTimestampCh:
		case <-time.After(10 * time.Second):
			t.Fatalf("frame %d: no RTP packet observed", i)
		}
		b := vs.recvDC(dcLabelFrameMeta, 10*time.Second)
		if len(b) != frameMetaV1Size {
			t.Fatalf("frame %d: meta length = %d, want %d", i, len(b), frameMetaV1Size)
		}
		m, err := decodeFrameMetaV1(b)
		if err != nil {
			t.Fatalf("frame %d: decode: %v", i, err)
		}
		if m.RTPTimestamp != rtpTS {
			t.Fatalf("frame %d: meta rtpTimestamp = %d, want marker-packet timestamp %d", i, m.RTPTimestamp, rtpTS)
		}
		if m.CodecEpoch != f.CodecEpoch || m.ContentID != f.ContentID ||
			m.EncodeSeq != f.EncodeSeq || m.SourceMonoUs != f.SourceMonoUs {
			t.Fatalf("frame %d: meta identity = %+v, want frame identity", i, m)
		}
		if m.Flags != 0 {
			t.Fatalf("frame %d: flags = %d, want 0", i, m.Flags)
		}
		if m.Hash64 == 0 || hashes[m.Hash64] {
			t.Fatalf("frame %d: hash %d zero or repeated", i, m.Hash64)
		}
		hashes[m.Hash64] = true
	}
}

// ---- 小助手 ----

func hexOf(b []byte) string {
	const digits = "0123456789ABCDEF"
	out := make([]byte, 0, len(b)*2)
	for _, x := range b {
		out = append(out, digits[x>>4], digits[x&0xF])
	}
	return string(out)
}

// waitFor 轮询谓词直至超时(小工具;避免引入 testify 依赖形态差异)。
func waitFor(t *testing.T, d time.Duration, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !pred() {
		if time.Now().After(deadline) {
			t.Fatal("condition never became true")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
