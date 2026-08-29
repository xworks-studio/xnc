// viewer_sender_test.go — M3 Task 1:ViewerSender 状态机 / 单帧队列 / 令牌桶
// 的确定性单测。时钟注入(manualClock)+ 不启动常驻泵 goroutine,全部
// 转移经 Enqueue/drainNow 公共路径驱动;大 AU 走 FU-A 分包以覆盖真实
// pacing 路径(小 AU 单包用于状态机形状)。
package desktop

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// ---- 测试基建:manualClock / fakeRTPSink / fakeViewerSender ----

// manualClock 是可手动推进的时钟注入点(确定性驱动 queue age 与 token
// bucket 截止,不真实睡眠)。
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func newManualClock() *manualClock {
	return &manualClock{t: time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeRTPSink 记录写出的每个 RTP 包(顺序即写出顺序)。
type fakeRTPSink struct {
	mu   sync.Mutex
	pkts []*rtp.Packet
}

func (s *fakeRTPSink) write(p *rtp.Packet) error {
	s.mu.Lock()
	s.pkts = append(s.pkts, p)
	s.mu.Unlock()
	return nil
}

func (s *fakeRTPSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pkts)
}

func (s *fakeRTPSink) snapshot() []*rtp.Packet {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*rtp.Packet(nil), s.pkts...)
}

// fakeViewerSender 是真 ViewerSender + 假出口 + 假时钟 + 合并关键帧
// 请求记账(brief 的测试形状:newFakeViewerSender/sent/state/overflow)。
type fakeViewerSender struct {
	vs    *ViewerSender
	sink  *fakeRTPSink
	clk   *manualClock
	keyMu sync.Mutex
	keys  []string
}

func newFakeViewerSender() *fakeViewerSender {
	return newFakeViewerSenderWithBudget(0)
}

// newFakeViewerSenderWithBudget 以指定 pacing 预算(bits/s;0 = 默认,
// 即令所有测试 AU 立即送出)构造 fake。
func newFakeViewerSenderWithBudget(bps int) *fakeViewerSender {
	f := &fakeViewerSender{sink: &fakeRTPSink{}, clk: newManualClock()}
	vs, err := newViewerSender(ViewerSenderConfig{
		WritePacket: f.sink.write,
		KeyRequest:  f.recordKey,
		BudgetBps:   bps,
		Now:         f.clk.Now,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		panic(err)
	}
	f.vs = vs
	return f
}

func (f *fakeViewerSender) recordKey(reason string) {
	f.keyMu.Lock()
	f.keys = append(f.keys, reason)
	f.keyMu.Unlock()
}

func (f *fakeViewerSender) keyRequests() []string {
	f.keyMu.Lock()
	defer f.keyMu.Unlock()
	return append([]string(nil), f.keys...)
}

func (f *fakeViewerSender) sent() int { return f.sink.count() }

// Enqueue 直通被测发送器(brief 测试形状的入口)。
func (f *fakeViewerSender) Enqueue(fr Frame) error { return f.vs.Enqueue(fr) }

func (f *fakeViewerSender) state() viewerSendState { return f.vs.stateSnapshot() }

// overflow 直接驱动生产代码的「队列年龄超限」转移(drainLocked 的同一
// 检测点所调用的 ageOverflowLocked)——假时钟推进超 100ms 后执行真实
// 转移:冲刷在队包、进入 waitIDR、恰一次合并关键帧请求。
func (f *fakeViewerSender) overflow() {
	f.clk.advance(maxQueueAge + 5*time.Millisecond)
	f.vs.mu.Lock()
	reason := f.vs.ageOverflowLocked(f.clk.Now())
	f.vs.mu.Unlock()
	f.vs.fireKey(reason)
}

// ---- 帧助手(单 NAL → 恰一个 RTP 包;brief 的 delta/idr 形状) ----

func vsMono(i uint64) uint64 { return i * 33_000 } // 33ms 帧距 @90kHz 单调

// vsNAL 产单 NAL Annex-B AU(key:IDR slice(type 5);delta:P slice)。
// 单 NAL ≤ MTU → 恰一个 RTP 包。载荷不含起始码序列。
func vsNAL(key bool, i uint64, n int) []byte {
	hdr := byte(0x41) // non-IDR slice
	if key {
		hdr = 0x65 // IDR slice
	}
	au := []byte{0x00, 0x00, 0x00, 0x01, hdr, 0x88, byte(i), byte(i >> 8)}
	for j := 0; j < n; j++ {
		au = append(au, 0xa5)
	}
	return au
}

// vsBigIDRAU 产 SPS+PPS+大 IDR slice(>MTU 触发 FU-A 分片的多包帧)。
func vsBigIDRAU(n int) []byte {
	au := append([]byte(nil), synSPS...)
	au = append(au, synPPS...)
	au = append(au, 0x00, 0x00, 0x00, 0x01, 0x65, 0x88, 0x01, 0x00)
	for j := 0; j < n; j++ {
		au = append(au, 0xa5)
	}
	return au
}

func delta(i uint64) Frame { return Frame{PresentMonoUs: vsMono(i), AU: vsNAL(false, i, 600)} }
func idr(i uint64) Frame   { return Frame{Key: true, PresentMonoUs: vsMono(i), AU: vsNAL(true, i, 40)} }

// 带 v2 身份的帧(ruling 1 字段的直达构造)。
func deltaE(i, capture, codec uint64) Frame {
	f := delta(i)
	f.CaptureEpoch, f.CodecEpoch = capture, codec
	f.ContentID, f.EncodeSeq = 7, i
	f.SourceMonoUs = f.PresentMonoUs - 1_000
	return f
}

func idrE(i, capture, codec uint64) Frame {
	f := idr(i)
	f.CaptureEpoch, f.CodecEpoch = capture, codec
	f.ContentID, f.EncodeSeq = 7, i
	f.SourceMonoUs = f.PresentMonoUs - 1_000
	return f
}

// ---- Step 1:状态机(brief 精确形状) ----

// TestViewerSenderWaitIDRStateMachine 是 brief Step 1 的精确形状:
// delta before IDR 不发;IDR 开流;overflow 后 broken delta 被抑制;
// recovery IDR 恢复 live。
func TestViewerSenderWaitIDRStateMachine(t *testing.T) {
	s := newFakeViewerSender()
	s.Enqueue(delta(1))
	if s.sent() != 0 {
		t.Fatal("delta before IDR")
	}
	s.Enqueue(idr(2))
	if s.state() != stateLive {
		t.Fatal("IDR did not start stream")
	}
	s.overflow()
	s.Enqueue(delta(3))
	if s.sent() != 1 {
		t.Fatal("sent broken delta")
	}
	s.Enqueue(idr(4))
	if s.state() != stateLive {
		t.Fatal("recovery IDR failed")
	}

	// 溢出恰触发一次合并关键帧请求;恢复 IDR 不再触发。
	if reqs := s.keyRequests(); len(reqs) != 1 || reqs[0] != "overflow" {
		t.Fatalf("key requests = %v, want exactly [overflow]", reqs)
	}
	// 恢复后的 delta 正常发送(抑制已解除)。
	before := s.sent()
	if err := s.Enqueue(delta(5)); err != nil {
		t.Fatalf("post-recovery delta: %v", err)
	}
	if s.sent() != before+1 {
		t.Fatalf("post-recovery delta sent=%d, want %d (live resumes)", s.sent(), before+1)
	}
}

// ---- Ruling 4:epoch 感知的 WAIT_IDR ----

// TestViewerSenderDiscontinuitySuppressesOldEpoch:Discontinuity(0x020B
// 镜像)后旧 epoch 的帧(delta 与 IDR)全部抑制,新 epoch 的首个 IDR
// 恢复 live;epoch 路径不触发关键帧请求(host 重建首帧即 IDR)。
func TestViewerSenderDiscontinuitySuppressesOldEpoch(t *testing.T) {
	s := newFakeViewerSender()
	if err := s.Enqueue(idrE(1, 1, 1)); err != nil {
		t.Fatalf("initial idr: %v", err)
	}
	if s.state() != stateLive {
		t.Fatalf("state=%v, want live after initial IDR", s.state())
	}
	sent0 := s.sent()

	s.vs.Discontinuity(2, 1) // capture 重建:capture epoch 1→2
	if s.state() != stateWaitIDR {
		t.Fatalf("state after Discontinuity=%v, want waitIDR", s.state())
	}
	// 旧 epoch 的 delta 与 IDR 都被抑制。
	if err := s.Enqueue(deltaE(2, 1, 1)); err != nil {
		t.Fatalf("old-epoch delta: %v", err)
	}
	if s.sent() != sent0 {
		t.Fatalf("old-epoch delta was sent: %d -> %d", sent0, s.sent())
	}
	if err := s.Enqueue(idrE(3, 1, 1)); err != nil {
		t.Fatalf("old-epoch idr: %v", err)
	}
	if s.sent() != sent0 {
		t.Fatalf("old-epoch IDR was sent: %d -> %d", sent0, s.sent())
	}
	if s.state() != stateWaitIDR {
		t.Fatalf("state after old-epoch frames=%v, want waitIDR", s.state())
	}
	// 新 epoch 的 delta 仍抑制(preKey),首个 IDR 恢复 live。
	if err := s.Enqueue(deltaE(4, 2, 1)); err != nil {
		t.Fatalf("new-epoch delta: %v", err)
	}
	if s.sent() != sent0 {
		t.Fatalf("new-epoch delta before IDR was sent: %d -> %d", sent0, s.sent())
	}
	if err := s.Enqueue(idrE(5, 2, 1)); err != nil {
		t.Fatalf("new-epoch idr: %v", err)
	}
	if s.state() != stateLive {
		t.Fatalf("state after new-epoch IDR=%v, want live", s.state())
	}
	if s.sent() != sent0+1 {
		t.Fatalf("new-epoch IDR packets=%d, want %d", s.sent()-sent0, 1)
	}
	if reqs := s.keyRequests(); len(reqs) != 0 {
		t.Fatalf("epoch path fired key requests %v, want none", reqs)
	}
	st := s.vs.Stats()
	if st.EpochDropped != 2 {
		t.Fatalf("epochDropped=%d, want 2 (old-epoch delta+IDR)", st.EpochDropped)
	}
	if st.PreKeyDropped != 1 {
		t.Fatalf("preKeyDropped=%d, want 1 (new-epoch delta)", st.PreKeyDropped)
	}
}

// TestViewerSenderEpochJumpWithoutEvent:0x020B 丢失时的兜底 —— live 期间
// 帧自带 epoch 前进即自检出不连续(旧链作废),等待新 epoch 的 IDR。
func TestViewerSenderEpochJumpWithoutEvent(t *testing.T) {
	s := newFakeViewerSender()
	if err := s.Enqueue(idrE(1, 5, 3)); err != nil {
		t.Fatalf("initial idr: %v", err)
	}
	sent0 := s.sent()
	// 新 epoch 的 delta:自检出不连续 → 抑制。
	if err := s.Enqueue(deltaE(2, 6, 3)); err != nil {
		t.Fatalf("epoch-jump delta: %v", err)
	}
	if s.state() != stateWaitIDR {
		t.Fatalf("state after epoch jump=%v, want waitIDR", s.state())
	}
	if s.sent() != sent0 {
		t.Fatalf("epoch-jump delta was sent: %d -> %d", sent0, s.sent())
	}
	// 新 epoch 的 IDR 恢复 live;旧 epoch 帧仍被抑制。
	if err := s.Enqueue(idrE(3, 6, 3)); err != nil {
		t.Fatalf("new-epoch idr: %v", err)
	}
	if s.state() != stateLive {
		t.Fatalf("state after new-epoch IDR=%v, want live", s.state())
	}
	if err := s.Enqueue(deltaE(4, 5, 3)); err != nil {
		t.Fatalf("stale-epoch delta after recovery: %v", err)
	}
	if s.sent() != sent0+1 {
		t.Fatalf("stale-epoch delta after recovery was sent: %d -> %d", sent0+1, s.sent())
	}
}

// TestViewerSenderWaitIDRRetargetsNewerEpoch(M4 livelock 加固):已在
// waitIDR 等待 (capture,codec) 的 IDR 时 host 又重置了一代 → 等待目标
// 重定位到更新的 epoch,其恢复 IDR 被接受(修前:pendingEpoch 停在旧代,
// 新代关键帧永远 epochDropped —— 观众饿死而 host 徒劳产 IDR)。旧代帧
// 照旧抑制。
func TestViewerSenderWaitIDRRetargetsNewerEpoch(t *testing.T) {
	s := newFakeViewerSender()
	if err := s.Enqueue(idrE(1, 3, 2)); err != nil {
		t.Fatalf("initial idr: %v", err)
	}
	sent0 := s.sent()
	// 等待 (3,4) 的 IDR(0x020B 镜像)。
	s.vs.Discontinuity(3, 4)
	if s.state() != stateWaitIDR {
		t.Fatalf("state after Discontinuity=%v, want waitIDR", s.state())
	}
	// 旧代 (3,3) 的 IDR 仍被压制(epochDropped)……
	if err := s.Enqueue(idrE(2, 3, 3)); err != nil {
		t.Fatalf("old-epoch idr: %v", err)
	}
	if s.sent() != sent0 || s.state() != stateWaitIDR {
		t.Fatalf("old-epoch IDR must stay suppressed: sent %d->%d state=%v", sent0, s.sent(), s.state())
	}
	// ……而等待期间 host 又前进到 (3,5):更新一代的 IDR 必须被接受
	//(重定位),流恢复 live —— 修前它被旧目标 (3,4) 永久压制。
	if err := s.Enqueue(idrE(3, 3, 5)); err != nil {
		t.Fatalf("newer-epoch idr: %v", err)
	}
	if s.state() != stateLive {
		t.Fatalf("state after newer-epoch IDR=%v, want live (re-targeted)", s.state())
	}
	if s.sent() != sent0+1 {
		t.Fatalf("newer-epoch IDR was suppressed: %d -> %d", sent0, s.sent())
	}
	// 重定位同样适用于增量帧:live 于 (3,5),自检出到 (4,5) 的跳代 →
	// waitIDR 等待 (4,5);期间 host 又前进到 (4,6) 的 delta 把目标前移
	//(仍抑制 —— 非 key),随后 (4,6) 的 IDR 恢复。
	if err := s.Enqueue(deltaE(4, 4, 5)); err != nil {
		t.Fatalf("epoch-jump delta: %v", err)
	}
	if s.state() != stateWaitIDR {
		t.Fatalf("state after epoch jump=%v, want waitIDR", s.state())
	}
	if err := s.Enqueue(deltaE(5, 4, 6)); err != nil {
		t.Fatalf("newer-epoch delta: %v", err)
	}
	if s.state() != stateWaitIDR {
		t.Fatalf("state after newer-epoch delta=%v, want waitIDR", s.state())
	}
	if err := s.Enqueue(idrE(6, 4, 6)); err != nil {
		t.Fatalf("re-targeted idr: %v", err)
	}
	if s.state() != stateLive || s.sent() != sent0+2 {
		t.Fatalf("re-targeted IDR must recover: state=%v sent=%d", s.state(), s.sent()-sent0)
	}
	// 旧代帧自始至终被压制。
	if err := s.Enqueue(deltaE(7, 3, 4)); err != nil {
		t.Fatalf("old-epoch delta: %v", err)
	}
	if s.sent() != sent0+2 {
		t.Fatalf("old-epoch delta was sent: %d", s.sent()-sent0)
	}
}

// ---- Ruling 3:队列年龄 100ms 硬上限(真实检测路径) ----

// TestViewerSenderQueueAgeOverflowFlushesAndRequestsKeyOnce:小预算使包
// 留在队里,时钟推进超 100ms 后下一次 Enqueue 走真实检测:整帧冲刷
// (立即写出)、进入 waitIDR、恰一次合并请求、后续 delta 抑制。
func TestViewerSenderQueueAgeOverflowFlushesAndRequestsKeyOnce(t *testing.T) {
	s := newFakeViewerSenderWithBudget(2_000_000) // 85% → ~212KB/s:20KB 帧约 80ms 铺完(入队且有余包)
	// C2 后关键帧豁免年龄冲刷 —— 年龄溢出转移由增量帧触发:小 IDR 开流,
	// 在队的是 ~20KB 大 delta(FU-A 多包)。
	if err := s.Enqueue(idr(0)); err != nil {
		t.Fatalf("small idr: %v", err)
	}
	base := s.sent()
	au := vsNAL(false, 1, 20_000) // ~20KB 非 key AU → FU-A 多包
	f := Frame{PresentMonoUs: vsMono(1), AU: au}
	if err := s.Enqueue(f); err != nil {
		t.Fatalf("big delta: %v", err)
	}
	if s.state() != stateLive {
		t.Fatalf("state=%v, want live", s.state())
	}
	total := base + expectedPacketCount(t, au)
	if s.sent() >= total {
		t.Fatalf("pacing did not defer anything: sent=%d total=%d", s.sent(), total)
	}
	if q := s.vs.Stats().QueuePackets; q == 0 {
		t.Fatalf("queue empty after enqueue (pacing broken)")
	}

	// 队列年龄超限 → 下一次入队前的 drain 检出并转移。
	s.clk.advance(maxQueueAge + 5*time.Millisecond)
	if err := s.Enqueue(delta(2)); err != nil {
		t.Fatalf("delta after overflow: %v", err)
	}
	if s.sent() != total {
		t.Fatalf("overflow flush: sent=%d, want %d (frame burst out)", s.sent(), total)
	}
	if s.state() != stateWaitIDR {
		t.Fatalf("state after overflow=%v, want waitIDR", s.state())
	}
	if reqs := s.keyRequests(); len(reqs) != 1 || reqs[0] != "overflow" {
		t.Fatalf("key requests = %v, want exactly [overflow]", reqs)
	}
	if q := s.vs.Stats().QueuePackets; q != 0 {
		t.Fatalf("queue not cleared: %d packets", q)
	}
	st := s.vs.Stats()
	if st.OverflowFlushes != 1 {
		t.Fatalf("overflowFlushes=%d, want 1", st.OverflowFlushes)
	}
	// broken delta 被抑制;recovery IDR 恢复 live。
	if err := s.Enqueue(delta(3)); err != nil {
		t.Fatalf("suppressed delta: %v", err)
	}
	if s.sent() != total {
		t.Fatalf("broken delta sent after overflow: %d -> %d", total, s.sent())
	}
	if err := s.Enqueue(idr(4)); err != nil {
		t.Fatalf("recovery idr: %v", err)
	}
	if s.state() != stateLive {
		t.Fatalf("state after recovery IDR=%v, want live", s.state())
	}
	// 溢出后无第二条合并请求。
	if reqs := s.keyRequests(); len(reqs) != 1 {
		t.Fatalf("merged key request fired more than once: %v", reqs)
	}
}

// ---- Ruling 3:截止超 100ms 不入队;50ms 目标内正常入队 ----

// TestViewerSenderDeadlineBeyondHardMaxNotQueued:预算使最后一包的
// plan 截止超 100ms → 整帧不入队(无任何部分包发出)+ 抑制(只丢不
// 采样)+ 恰一次合并请求;对比:预算充足(截止 ~5ms,50ms 目标内)
// 时整帧入队送出。
func TestViewerSenderDeadlineBeyondHardMaxNotQueued(t *testing.T) {
	// ① 硬上限:100kbps → 85KB/s;~20KB 帧(去 burst 后 ~16.7KB)需
	// ~197ms > 100ms → 不入队。
	s := newFakeViewerSenderWithBudget(100_000)
	if err := s.Enqueue(idr(1)); err != nil { // 小 IDR 立即送出 → live
		t.Fatalf("small idr: %v", err)
	}
	sent0 := s.sent()
	big := Frame{PresentMonoUs: vsMono(2), AU: vsBigIDRAU(20_000)}
	if err := s.Enqueue(big); err != nil {
		t.Fatalf("over-deadline delta: %v", err)
	}
	if s.sent() != sent0 {
		t.Fatalf("frame with >100ms deadline was (partially) queued: %d -> %d", sent0, s.sent())
	}
	if s.state() != stateWaitIDR {
		t.Fatalf("state after not-queued frame=%v, want waitIDR (suppression, never sampling)", s.state())
	}
	if reqs := s.keyRequests(); len(reqs) != 1 || reqs[0] != "pacer" {
		t.Fatalf("key requests = %v, want exactly [pacer]", reqs)
	}
	if st := s.vs.Stats(); st.DeadlineDropped != 1 {
		t.Fatalf("deadlineDropped=%d, want 1", st.DeadlineDropped)
	}
	// 后续 delta 抑制直到 IDR。
	if err := s.Enqueue(delta(3)); err != nil {
		t.Fatalf("suppressed delta: %v", err)
	}
	if s.sent() != sent0 {
		t.Fatalf("delta after suppression was sent: %d -> %d", sent0, s.sent())
	}

	// ② 50ms 目标内:4Mbps → 3.4MB/s;~16.7KB 需 ~5ms → 正常入队送出。
	s2 := newFakeViewerSenderWithBudget(4_000_000)
	if err := s2.Enqueue(idr(1)); err != nil {
		t.Fatalf("small idr: %v", err)
	}
	before := s2.sent()
	if err := s2.Enqueue(Frame{PresentMonoUs: vsMono(2), AU: vsBigIDRAU(20_000)}); err != nil {
		t.Fatalf("in-budget delta: %v", err)
	}
	if s2.state() != stateLive {
		t.Fatalf("state=%v, want live (in-budget frame must queue)", s2.state())
	}
	// 时钟推到截止之后 drain:全部送出、无溢出、无请求。
	s2.clk.advance(50 * time.Millisecond)
	if err := s2.vs.drainNow(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := s2.sent(); got != before+expectedPacketCount(t, vsBigIDRAU(20_000)) {
		t.Fatalf("in-budget frame packets=%d, want %d", got-before, expectedPacketCount(t, vsBigIDRAU(20_000)))
	}
	if reqs := s2.keyRequests(); len(reqs) != 0 {
		t.Fatalf("in-budget path fired key requests %v, want none", reqs)
	}
}

// ---- Ruling 3:令牌桶 pacing(85% 预算) ----

// TestViewerSenderTokenBucketPacing:大 IDR 的包按令牌桶节奏分批送出
// (首 drain 只出 burst 部分),时钟推进逐步 drain 全部送出,且全程
// 无溢出、无关键帧请求、无年龄超限。
func TestViewerSenderTokenBucketPacing(t *testing.T) {
	s := newFakeViewerSenderWithBudget(2_000_000) // 85% → ~212KB/s:20KB 帧约 80ms 铺完
	au := vsBigIDRAU(20_000)
	total := expectedPacketCount(t, au)
	if err := s.Enqueue(Frame{Key: true, PresentMonoUs: vsMono(1), AU: au}); err != nil {
		t.Fatalf("big idr: %v", err)
	}
	first := s.sent()
	if first == 0 || first >= total {
		t.Fatalf("first drain sent=%d, want 0 < sent < total=%d", first, total)
	}
	// 推进 5ms:再送一批,但未到全部(节奏在铺开)。
	s.clk.advance(5 * time.Millisecond)
	if err := s.vs.drainNow(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	mid := s.sent()
	if mid <= first {
		t.Fatalf("no progress after 5ms: %d -> %d", first, mid)
	}
	if mid >= total {
		t.Fatalf("all packets out after 5ms: %d/%d (pacing not spreading)", mid, total)
	}
	// 推进到全部截止之后(帧计划铺完 ≈75ms,仍在 100ms 年龄限内):
	// 全部送出,状态 live,零请求。
	s.clk.advance(80 * time.Millisecond)
	if err := s.vs.drainNow(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if s.sent() != total {
		t.Fatalf("sent=%d, want %d", s.sent(), total)
	}
	if s.state() != stateLive {
		t.Fatalf("state=%v, want live", s.state())
	}
	if reqs := s.keyRequests(); len(reqs) != 0 {
		t.Fatalf("key requests=%v, want none", reqs)
	}
	if st := s.vs.Stats(); st.FramesSent != 1 || st.QueuePackets != 0 {
		t.Fatalf("stats: framesSent=%d queue=%d, want 1/0", st.FramesSent, st.QueuePackets)
	}
	// 序列号严格递增(独立序列空间,无重复)。
	pkts := s.sink.snapshot()
	for i := 1; i < len(pkts); i++ {
		if pkts[i].SequenceNumber != pkts[i-1].SequenceNumber+1 {
			t.Fatalf("sequence not consecutive at %d: %d -> %d",
				i, pkts[i-1].SequenceNumber, pkts[i].SequenceNumber)
		}
	}
	// 同帧所有包同一 RTP 时戳;仅最后一包 Marker。
	lastTS := pkts[0].Timestamp
	for i, p := range pkts {
		if p.Timestamp != lastTS {
			t.Fatalf("packet %d timestamp=%d, want %d (same AU)", i, p.Timestamp, lastTS)
		}
		if wantMarker := i == len(pkts)-1; p.Marker != wantMarker {
			t.Fatalf("packet %d marker=%v, want %v", i, p.Marker, wantMarker)
		}
	}
}

// ---- final-fixwave C2:控制器预算下的真尺寸 IDR 必须完整可交付 ----

// assertCompleteAUOrder 校验 sink 尾部 total 个包构成一个完整按序 AU:
// 序列号连续、同一 RTP 时戳、仅最后一包 Marker。
func assertCompleteAUOrder(t *testing.T, pkts []*rtp.Packet, total int) {
	t.Helper()
	tail := pkts[len(pkts)-total:]
	for i := 1; i < len(tail); i++ {
		if tail[i].SequenceNumber != tail[i-1].SequenceNumber+1 {
			t.Fatalf("sequence not consecutive at %d: %d -> %d",
				i, tail[i-1].SequenceNumber, tail[i].SequenceNumber)
		}
	}
	for i, p := range tail {
		if p.Timestamp != tail[0].Timestamp {
			t.Fatalf("packet %d timestamp=%d, want %d (same AU)", i, p.Timestamp, tail[0].Timestamp)
		}
		if wantMarker := i == len(tail)-1; p.Marker != wantMarker {
			t.Fatalf("packet %d marker=%v, want %v", i, p.Marker, wantMarker)
		}
	}
}

// TestViewerSenderKeyframeDeliveredUnderControllerBudget:2.3Mbps 预算
//(QoS 控制器接线后的真实形态)下 ~60KB 的恢复 IDR:修前 100ms 入队门
// 整帧拒收(铺完需 ~230ms)→ waitIDR + pacer 请求 → 观众永久卡死;修后
// 关键帧豁免入队门:头段按令牌节奏铺开、尾段在 100ms 视界处有界突发,
// 全部包按序送出(Marker 收尾)、状态全程 LIVE、零关键帧请求;随后 delta
// 正常入队按节奏送出(关键帧债务被钳到 50ms 目标窗口等值)。
func TestViewerSenderKeyframeDeliveredUnderControllerBudget(t *testing.T) {
	s := newFakeViewerSenderWithBudget(2_300_000)
	au := vsBigIDRAU(60_000)
	total := expectedPacketCount(t, au)
	if err := s.Enqueue(Frame{Key: true, PresentMonoUs: vsMono(1), AU: au}); err != nil {
		t.Fatalf("big idr: %v", err)
	}
	if s.state() != stateLive {
		t.Fatalf("state=%v, want live (keyframe admission must not reject)", s.state())
	}
	// 100ms 视界(+时钟余量)后全部送出。
	s.clk.advance(105 * time.Millisecond)
	if err := s.vs.drainNow(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if s.sent() != total {
		t.Fatalf("sent=%d, want %d (complete IDR AU under 2.3Mbps budget)", s.sent(), total)
	}
	assertCompleteAUOrder(t, s.sink.snapshot(), total)
	if reqs := s.keyRequests(); len(reqs) != 0 {
		t.Fatalf("key requests=%v, want none (IDR must be deliverable)", reqs)
	}
	if st := s.vs.Stats(); st.FramesSent != 1 || st.DeadlineDropped != 0 {
		t.Fatalf("stats: framesSent=%d deadlineDropped=%d, want 1/0", st.FramesSent, st.DeadlineDropped)
	}
	// 随后 delta:正常入队送出(不为关键帧爆偿债务)。
	if err := s.Enqueue(delta(2)); err != nil {
		t.Fatalf("post-idr delta: %v", err)
	}
	s.clk.advance(30 * time.Millisecond)
	if err := s.vs.drainNow(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if s.sent() != total+1 {
		t.Fatalf("post-idr delta: sent=%d, want %d (deltas pace normally after IDR)", s.sent(), total+1)
	}
	if s.state() != stateLive {
		t.Fatalf("state=%v, want live after delta", s.state())
	}
}

// TestViewerSenderKeyframeDeliveredAtFloorBudget:500k 码率下限时 ~60KB
// IDR 铺完需 ~1.1s —— 100ms 视界处有界突发收尾;队列年龄界绝不冲刷进行
// 中的关键帧(冲刷会转 waitIDR,恢复自残),帧间年龄界照旧对增量帧成立。
func TestViewerSenderKeyframeDeliveredAtFloorBudget(t *testing.T) {
	s := newFakeViewerSenderWithBudget(500_000)
	au := vsBigIDRAU(60_000)
	total := expectedPacketCount(t, au)
	if err := s.Enqueue(Frame{Key: true, PresentMonoUs: vsMono(1), AU: au}); err != nil {
		t.Fatalf("big idr: %v", err)
	}
	// 60ms:头段按令牌节奏部分送出(铺开仍在进行)。
	s.clk.advance(60 * time.Millisecond)
	if err := s.vs.drainNow(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if mid := s.sent(); mid == 0 || mid >= total {
		t.Fatalf("mid-drain sent=%d, want 0 < sent < total=%d (pacing head)", mid, total)
	}
	// 110ms:年龄已超 100ms —— 关键帧完成(全部按序送出)而非被冲刷;
	// 冲刷路径会转 waitIDR 并触发 overflow 请求,这里必须都没有。
	s.clk.advance(50 * time.Millisecond)
	if err := s.vs.drainNow(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if s.sent() != total {
		t.Fatalf("sent=%d, want %d (in-progress keyframe completes past the age bound)", s.sent(), total)
	}
	assertCompleteAUOrder(t, s.sink.snapshot(), total)
	if s.state() != stateLive {
		t.Fatalf("state=%v, want live (keyframe completion is not an overflow)", s.state())
	}
	if reqs := s.keyRequests(); len(reqs) != 0 {
		t.Fatalf("key requests=%v, want none", reqs)
	}
	if st := s.vs.Stats(); st.OverflowFlushes != 0 || st.FramesSent != 1 {
		t.Fatalf("stats: overflowFlushes=%d framesSent=%d, want 0/1", st.OverflowFlushes, st.FramesSent)
	}
	// 随后 delta 照常入门(债务已钳制)。
	if err := s.Enqueue(delta(2)); err != nil {
		t.Fatalf("post-idr delta: %v", err)
	}
	s.clk.advance(100 * time.Millisecond)
	if err := s.vs.drainNow(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if s.sent() != total+1 {
		t.Fatalf("post-idr delta: sent=%d, want %d", s.sent(), total+1)
	}
}

// ---- 单帧队列:新帧到达冲刷上一帧余包 ----

// TestViewerSenderOneFrameQueueCoalesces:上一帧仍有在队包(未超龄)时
// 新帧入队 → 旧帧余包整帧冲刷、新帧正常入队,状态保持 live(合并
// 不等于溢出)。
func TestViewerSenderOneFrameQueueCoalesces(t *testing.T) {
	s := newFakeViewerSenderWithBudget(3_000_000) // 85% → ~318KB/s:20KB 帧约 53ms 铺完
	au := vsBigIDRAU(20_000)
	total := expectedPacketCount(t, au)
	if err := s.Enqueue(Frame{Key: true, PresentMonoUs: vsMono(1), AU: au}); err != nil {
		t.Fatalf("big idr: %v", err)
	}
	if s.sent() >= total {
		t.Fatalf("expected pending remainder, sent=%d/%d", s.sent(), total)
	}
	s.clk.advance(10 * time.Millisecond)
	// 新 delta 到达:旧帧余包全部冲刷(立即写出)+ delta 入队,无溢出无请求。
	if err := s.Enqueue(delta(2)); err != nil {
		t.Fatalf("coalescing delta: %v", err)
	}
	if s.sent() != total {
		t.Fatalf("old frame remainder not flushed on coalesce: sent=%d, want %d", s.sent(), total)
	}
	// delta 的包按令牌桶节奏在大帧债务之后送出(40ms 预算内)。
	s.clk.advance(50 * time.Millisecond)
	if err := s.vs.drainNow(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if s.sent() != total+1 {
		t.Fatalf("after coalesce sent=%d, want %d (old frame + delta)", s.sent(), total+1)
	}
	if s.state() != stateLive {
		t.Fatalf("state=%v, want live (coalescing is not overflow)", s.state())
	}
	if reqs := s.keyRequests(); len(reqs) != 0 {
		t.Fatalf("coalescing fired key requests %v", reqs)
	}
	if st := s.vs.Stats(); st.OverflowFlushes != 0 {
		t.Fatalf("overflowFlushes=%d, want 0", st.OverflowFlushes)
	}
}

// ---- Pause/Resume/Close ----

// TestViewerSenderPauseResumeClose:暂停丢帧(不请求关键帧);恢复进入
// waitIDR 并恰一次请求;恢复 IDR 后 live;关闭后帧丢弃且幂等。
func TestViewerSenderPauseResumeClose(t *testing.T) {
	s := newFakeViewerSender()
	if err := s.Enqueue(idr(1)); err != nil {
		t.Fatalf("idr: %v", err)
	}
	sent0 := s.sent()

	s.vs.Pause()
	if s.state() != statePaused {
		t.Fatalf("state after Pause=%v", s.state())
	}
	if err := s.Enqueue(delta(2)); err != nil {
		t.Fatalf("paused delta: %v", err)
	}
	if s.sent() != sent0 {
		t.Fatalf("paused frame was sent: %d -> %d", sent0, s.sent())
	}
	if reqs := s.keyRequests(); len(reqs) != 0 {
		t.Fatalf("pause fired key requests %v, want none", reqs)
	}

	s.vs.Resume()
	if s.state() != stateWaitIDR {
		t.Fatalf("state after Resume=%v, want waitIDR", s.state())
	}
	if reqs := s.keyRequests(); len(reqs) != 1 || reqs[0] != "resume" {
		t.Fatalf("key requests after resume = %v, want [resume]", reqs)
	}
	if err := s.Enqueue(delta(3)); err != nil {
		t.Fatalf("post-resume delta: %v", err)
	}
	if s.sent() != sent0 {
		t.Fatalf("delta before post-resume IDR was sent: %d -> %d", sent0, s.sent())
	}
	if err := s.Enqueue(idr(4)); err != nil {
		t.Fatalf("post-resume idr: %v", err)
	}
	if s.state() != stateLive {
		t.Fatalf("state after post-resume IDR=%v, want live", s.state())
	}

	s.vs.Close()
	if s.state() != stateClosed {
		t.Fatalf("state after Close=%v", s.state())
	}
	if err := s.Enqueue(idr(5)); err != nil {
		t.Fatalf("closed enqueue must not error: %v", err)
	}
	if s.state() != stateClosed {
		t.Fatalf("state changed by closed enqueue: %v", s.state())
	}
	s.vs.Close() // 幂等
	s.vs.Pause()
	s.vs.Resume()
}

// TestViewerSenderCloseWithPumpDrainsGoroutine:常驻泵启动后 Close 能收
// 线(真实时钟 + 默认预算,一轮真实 Enqueue 后关闭,无死锁无泄漏)。
func TestViewerSenderCloseWithPumpDrainsGoroutine(t *testing.T) {
	f := &fakeViewerSender{sink: &fakeRTPSink{}, clk: newManualClock()}
	vs, err := newViewerSender(ViewerSenderConfig{
		WritePacket: f.sink.write,
		KeyRequest:  f.recordKey,
		Now:         time.Now, // 真实时钟(泵的 timer 与之一致)
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("newViewerSender: %v", err)
	}
	f.vs = vs
	vs.start()
	if err := vs.Enqueue(idr(1)); err != nil {
		t.Fatalf("idr: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.sent() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.sent() == 0 {
		t.Fatalf("pump never wrote packets")
	}
	done := make(chan struct{})
	go func() {
		vs.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Close did not return (pump goroutine stuck)")
	}
	vs.Close() // 幂等
}

// ---- Step 3:分包私有性 ----

// TestViewerSenderIndependentPacketization:两个 sender 吃同一 AU 字节,
// 各自产出独立 RTP 包(独立时戳基点/序列空间),载荷一致。
func TestViewerSenderIndependentPacketization(t *testing.T) {
	a := newFakeViewerSender()
	b := newFakeViewerSender()
	// 确定性基点(独立时戳基点 + 独立序列空间)。
	a.vs.mu.Lock()
	a.vs.clock = newRTPClock(10_000)
	a.vs.packetizer = rtp.NewPacketizer(rtpMTU, 102, 0, &codecs.H264Payloader{}, rtp.NewFixedSequencer(100), 90000)
	a.vs.mu.Unlock()
	b.vs.mu.Lock()
	b.vs.clock = newRTPClock(90_000)
	b.vs.packetizer = rtp.NewPacketizer(rtpMTU, 102, 0, &codecs.H264Payloader{}, rtp.NewFixedSequencer(20_000), 90000)
	b.vs.mu.Unlock()

	au := vsBigIDRAU(4_000)
	if err := a.Enqueue(Frame{Key: true, PresentMonoUs: 1_000_000, AU: au}); err != nil {
		t.Fatalf("sender A: %v", err)
	}
	if err := b.Enqueue(Frame{Key: true, PresentMonoUs: 1_000_000, AU: au}); err != nil {
		t.Fatalf("sender B: %v", err)
	}
	// 默认预算下分批送出:推进时钟并 drain 到全部包落地。
	for _, f := range []*fakeViewerSender{a, b} {
		f.clk.advance(10 * time.Millisecond)
		if err := f.vs.drainNow(); err != nil {
			t.Fatalf("drain: %v", err)
		}
	}
	pa, pb := a.sink.snapshot(), b.sink.snapshot()
	if len(pa) == 0 || len(pa) != len(pb) {
		t.Fatalf("packet counts differ: A=%d B=%d", len(pa), len(pb))
	}
	for i := range pa {
		// 独立基点:时戳必须不同(A base 10000 vs B base 90000)。
		if pa[i].Timestamp == pb[i].Timestamp {
			t.Fatalf("packet %d shares timestamp %d", i, pa[i].Timestamp)
		}
		if pa[i].SequenceNumber == pb[i].SequenceNumber {
			t.Fatalf("packet %d shares sequence %d", i, pa[i].SequenceNumber)
		}
		if string(pa[i].Payload) != string(pb[i].Payload) {
			t.Fatalf("packet %d payload mismatch (same AU must packetize identically)", i)
		}
	}
}

// expectedPacketCount 用同参数 packetizer 数出 AU 的期望包数(测试侧的
// 独立推导,避免用被测对象的 packetizer 自证)。
func expectedPacketCount(t *testing.T, au []byte) int {
	t.Helper()
	p := rtp.NewPacketizer(rtpMTU, 102, 0, &codecs.H264Payloader{}, rtp.NewFixedSequencer(0), 90000)
	n := len(p.Packetize(au, 0))
	if n == 0 {
		t.Fatalf("expectedPacketCount: no packets for %d byte AU", len(au))
	}
	return n
}

// ---- Task 2 carry(Task 1 遗留):入口 drain 写错误上抛 ----

// failableRTPSink 在置位后对每个写出返回错误(模拟 PC 关闭形态的出口死亡)。
type failableRTPSink struct {
	inner *fakeRTPSink
	fail  atomic.Bool
}

func (s *failableRTPSink) write(p *rtp.Packet) error {
	if s.fail.Load() {
		return errors.New("sink dead")
	}
	return s.inner.write(p)
}

// TestViewerSenderEntryDrainWriteErrorPropagates:入口 drain(Enqueue 开头
// 对到期在队包的冲写)遇写失败时,错误必须上抛(发送面死亡,pumpFrames
// 据此收线)且发送器转 closed——不得静默吞掉后继续被投喂。
func TestViewerSenderEntryDrainWriteErrorPropagates(t *testing.T) {
	sink := &failableRTPSink{inner: &fakeRTPSink{}}
	clk := newManualClock()
	vs, err := newViewerSender(ViewerSenderConfig{
		WritePacket: sink.write,
		Now:         clk.Now,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("newViewerSender: %v", err)
	}
	// 小预算铺开大 IDR:首 drain 出 burst,余包在队(到期时刻分布在未来)。
	if err := vs.Enqueue(Frame{Key: true, PresentMonoUs: vsMono(1), AU: vsBigIDRAU(20_000)}); err != nil {
		t.Fatalf("big idr: %v", err)
	}
	total := expectedPacketCount(t, vsBigIDRAU(20_000))
	if sent := sink.inner.count(); sent == 0 || sent >= total {
		t.Fatalf("expected deferred remainder, sent=%d/%d", sent, total)
	}
	// 推进 50ms(队列年龄 50ms < 100ms:不走 overflow,走到期包冲写路径),
	// 出口死亡后下一次 Enqueue 的入口 drain 必须把写错误上抛。
	clk.advance(50 * time.Millisecond)
	sink.fail.Store(true)
	err = vs.Enqueue(delta(2))
	if err == nil {
		t.Fatal("entry-drain write error was silently swallowed")
	}
	if !strings.Contains(err.Error(), "write RTP packet") {
		t.Fatalf("err = %v, want write RTP packet failure", err)
	}
	if st := vs.stateSnapshot(); st != stateClosed {
		t.Fatalf("state after entry-drain failure=%v, want closed", st)
	}
	// closed 后续帧静默丢弃(不报错),泵侧可凭首错收线。
	if err := vs.Enqueue(delta(3)); err != nil {
		t.Fatalf("closed enqueue must not error: %v", err)
	}
	if st := vs.Stats(); st.ClosedDropped != 1 {
		t.Fatalf("closedDropped=%d, want 1", st.ClosedDropped)
	}
	vs.Close() // 幂等,无泵也能安全收线
}

// TestViewerSenderPacingSustainedMotionKeepsUp 钉死 M4 pacing 修正
//(pacingBudgetFraction 0.85 → 1.05)所治的窗口:生产形态下 pacing 预算
//= QoS 决策码率 = 编码器目标(session.go SetPacingBudget 接线),持续
//运动令编码器按目标产出(≈ bitrate/fps 每帧)。旧 0.85 折扣令令牌桶
//结构性欠载 15%:债务逐帧加深,数帧内撞 100ms 入队门 → 拒收 → waitIDR
//→ 恢复 IDR 循环(直连复测:66 帧被抑制、27 个恢复 IDR/45s、549 个
// 0ms 到达突发 + 17 个 >500ms 饥饿间隙 —— ?framediag 读到的「间隔对
//交替且档位爬升」)。修正后令牌桶按预算 ×1.05 放行:持续运动下零拒收、
//零恢复 IDR、每帧在帧周期内完整铺出。
//
// 驱动形态 = 生产:manualClock 逐毫秒 drainNow(与常驻泵同一入口),
//帧以精确 spf 到达;帧大小 = bitrate/8/fps(编码器按目标)+ 初始 IDR
//(1.67×,本机 QSV 实测 IDR/增量比)。leg B 用缩放预算(×85/105)复现
//旧折扣的排放速率,钉死失败模式本身。
func TestViewerSenderPacingSustainedMotionKeepsUp(t *testing.T) {
	const (
		bps = 2_300_000 // 1920 宽档位(bitrateForWidth)—— 生产 pacing 预算
		fps = 30
	)
	spf := time.Second / time.Duration(fps)
	frameAU := func(key bool, i int) Frame {
		n := int(bps / 8 / fps)
		if key {
			n = n * 5 / 3
		}
		au := make([]byte, n)
		copy(au, []byte{0x00, 0x00, 0x00, 0x01, 0x65})
		for j := 5; j < n; j++ {
			au[j] = 0xa5
		}
		return Frame{Key: key, PresentMonoUs: uint64(i) * uint64(spf.Microseconds()), AU: au}
	}
	run := func(budget int) (drops uint64, keysSent uint64, incompleteArrivals int) {
		f := newFakeViewerSenderWithBudget(budget)
		defer f.vs.Close()
		var completed uint64
		f.vs.makeMeta = func(Frame, uint32) *FrameMetaV1 { return &FrameMetaV1{} }
		f.vs.onFrameSnt = func(*FrameMetaV1) { completed++ }
		for i := 0; i < 120; i++ {
			tArr := f.clk.Now().Add(spf)
			for f.clk.Now().Before(tArr) {
				f.clk.advance(time.Millisecond)
				_ = f.vs.drainNow()
			}
			f.clk.advance(tArr.Sub(f.clk.Now()))
			// 上一帧是否在本帧到达前完整送出。初始 IDR 的令牌债务在数
			// 帧内偿还(每帧 ~5% 盈余),恢复窗之后必须帧帧清空 —— 只
			// 统计稳态窗(前 50 帧的恢复尾不算)。
			if i >= 50 && f.vs.Stats().QueuePackets > 0 {
				incompleteArrivals++
			}
			key := i == 0 // 初态 waitIDR:首帧必须 IDR
			if err := f.Enqueue(frameAU(key, i)); err != nil {
				t.Fatalf("frame %d: %v", i, err)
			}
		}
		// 尾段:末帧的余包按节奏送完(生产中下一帧到达前完成;此处
		// 给足一个 maxQueueAge 视界,避免末帧滞队误计)。
		for i := 0; i < 120 && f.vs.Stats().QueuePackets > 0; i++ {
			f.clk.advance(time.Millisecond)
			_ = f.vs.drainNow()
		}
		st := f.vs.Stats()
		return st.DeadlineDropped, completed, incompleteArrivals
	}

	// leg A:生产预算(pacingBudgetFraction ×1.05)—— 持续运动零拒收、
	// 零恢复关键帧、每帧帧周期内完整铺出(120/120)。
	drops, sent, incomplete := run(bps)
	if drops != 0 {
		t.Fatalf("production budget: deadlineDropped=%d, want 0 (sustained motion must not trip the enqueue gate)", drops)
	}
	if sent != 120 {
		t.Fatalf("frames completed=%d, want 120", sent)
	}
	if incomplete != 0 {
		t.Fatalf("frames still queued at next arrival=%d, want 0 (pacing keeps up with the frame period)", incomplete)
	}

	// leg B:旧 0.85 折扣的排放速率(预算 ×85/105)—— 同内容必须复现
	// 入队门拒收(修正前的失败模式;数字形态见 pacingBudgetFraction 注释)。
	legacyDrops, _, _ := run(bps * 85 / 105)
	if legacyDrops == 0 {
		t.Fatal("legacy 0.85 drain rate: expected deadline drops under sustained at-target motion (failure mode gone missing)")
	}
}
