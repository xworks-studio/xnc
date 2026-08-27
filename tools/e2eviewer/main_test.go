package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"

	"xnc/agent/desktop"
)

func TestParseTurn(t *testing.T) {
	srv, err := parseTurn("xncdev:xncdev-secret@192.168.1.12:3478", nil)
	if err != nil {
		t.Fatalf("parseTurn: %v", err)
	}
	if len(srv) != 1 {
		t.Fatalf("servers = %d, want 1", len(srv))
	}
	s := srv[0]
	want := []string{"turn:192.168.1.12:3478?transport=tcp", "turn:192.168.1.12:3478"}
	if len(s.URLs) != 2 || s.URLs[0] != want[0] || s.URLs[1] != want[1] {
		t.Fatalf("urls = %v, want %v", s.URLs, want)
	}
	if s.Username != "xncdev" || s.Credential != "xncdev-secret" {
		t.Fatalf("creds = %q/%q", s.Username, s.Credential)
	}

	// 空 = 无 TURN(直连模式本机 ICE)。
	if s, err := parseTurn("", nil); err != nil || s != nil {
		t.Fatalf("empty spec: %v %v", s, err)
	}
	// 形态错误。
	for _, bad := range []string{"no-at-host", "xncdev@1.2.3.4:1", ":pass@1.2.3.4:1", "user:@1.2.3.4:1"} {
		if _, err := parseTurn(bad, nil); err == nil {
			t.Fatalf("parseTurn(%q) should fail", bad)
		}
	}
	// 显式 URL 覆盖(凭据仍来自 --turn)。
	srv, err = parseTurn("u:p@h:1", []string{"turn:h:1?transport=tcp"})
	if err != nil || len(srv) != 1 || srv[0].URLs[0] != "turn:h:1?transport=tcp" || srv[0].Username != "u" {
		t.Fatalf("explicit urls: %+v err=%v", srv, err)
	}
}

func TestSummaryEvaluate(t *testing.T) {
	c := &config{
		expectFirstFrameMs: 2000,
		expectKeyframes:    1,
		expectPliIdrMs:     2000,
		duration:           time.Second,
	}
	ok := &summary{FirstFrameMs: 800, Keyframes: 3}
	ok.evaluate(c)
	if !ok.AssertionsPassed || len(ok.Failures) != 0 {
		t.Fatalf("ok case failed: %+v", ok)
	}

	bad := &summary{FirstFrameMs: 3000, Keyframes: 0, PlisSent: 1, PliToIdrMaxMs: 0}
	bad.evaluate(c)
	if bad.AssertionsPassed || len(bad.Failures) != 3 {
		t.Fatalf("bad case: %+v (failures=%v)", bad, bad.Failures)
	}

	// PLI 已发且 IDR 到达但过慢。
	slow := &summary{FirstFrameMs: 100, Keyframes: 2, PlisSent: 1, PliToIdrMaxMs: 2500}
	slow.evaluate(c)
	if slow.AssertionsPassed || len(slow.Failures) != 1 {
		t.Fatalf("slow PLI case: %+v", slow)
	}

	// 跳过断言(0 值)。
	none := &summary{}
	none.evaluate(&config{})
	if !none.AssertionsPassed {
		t.Fatalf("no-assert config should pass: %+v", none)
	}

	// T6 静止/承接断言:--expect-frames-max 与 --expect-first-key。
	sc := &config{expectFramesMax: 10, expectFirstKey: true, duration: time.Second}
	quiet := &summary{FirstFrameMs: 900, FirstKey: true, Keyframes: 1, Frames: 4}
	quiet.evaluate(sc)
	if !quiet.AssertionsPassed {
		t.Fatalf("quiet static case failed: %+v", quiet)
	}
	noisy := &summary{FirstFrameMs: 900, FirstKey: true, Keyframes: 1, Frames: 40}
	noisy.evaluate(sc)
	if noisy.AssertionsPassed || len(noisy.Failures) != 1 {
		t.Fatalf("noisy static case should fail on frames-max: %+v", noisy)
	}
	deltaJoin := &summary{FirstFrameMs: 900, FirstKey: false, Keyframes: 1, Frames: 5}
	deltaJoin.evaluate(sc)
	if deltaJoin.AssertionsPassed || len(deltaJoin.Failures) != 1 {
		t.Fatalf("non-IDR first AU should fail expect-first-key: %+v", deltaJoin)
	}
	noFrames := &summary{}
	noFrames.evaluate(sc)
	if noFrames.AssertionsPassed || len(noFrames.Failures) != 1 {
		t.Fatalf("no frames should fail expect-first-key (not crash): %+v", noFrames)
	}
}

// 编译期锁 ICE server 形态(pion 版本升级时的兼容哨兵)。
var _ = webrtc.ICETransportPolicyRelay

// TestPliRetryDecision — pending-PLI 重发的判定表(M1-Slice3 承接:
// 1.5s 无 IDR 即重发,轮上限 3 发;frames>0 与否由调用侧状态隐含,
// 决策只看 episode)。
func TestPliRetryDecision(t *testing.T) {
	const retryAfter = 1500 * time.Millisecond
	cases := []struct {
		since    time.Duration
		attempts int
		want     bool
	}{
		{since: 0, attempts: 1, want: false},                       // 刚发完
		{since: 1499 * time.Millisecond, attempts: 1, want: false}, // 未到 1.5s
		{since: 1500 * time.Millisecond, attempts: 1, want: true},  // 到点,首轮
		{since: 10 * time.Second, attempts: 1, want: true},         // 迟到仍可重发
		{since: 10 * time.Second, attempts: 2, want: true},         // 第 2 次重发(共 3 发)
		{since: 10 * time.Second, attempts: 3, want: false},        // 轮上限:不再发
		{since: 10 * time.Second, attempts: 0, want: false},        // 无 episode(0=已复位)
		{since: 10 * time.Second, attempts: -1, want: false},       // 病态输入
	}
	for _, c := range cases {
		if got := pliRetryDecision(c.since, c.attempts, maxPliEpisodeSends, retryAfter); got != c.want {
			t.Errorf("pliRetryDecision(since=%v, attempts=%d) = %v, want %v",
				c.since, c.attempts, got, c.want)
		}
	}
	// 自定义上限/超时参数亦尊重。
	if !pliRetryDecision(2*time.Second, 1, 5, time.Second) {
		t.Errorf("maxSends=5 should allow retry at attempts=1")
	}
	if pliRetryDecision(2*time.Second, 1, 5, 5*time.Second) {
		t.Errorf("retryAfter=5s should not retry at since=2s")
	}
}

// TestCursorRecording — cursor 通道记录:计数全量、样本截 cap、stats 副本。
func TestCursorRecording(t *testing.T) {
	v := &viewer{start: time.Now()}
	for i := 0; i < cursorSampleCap+100; i++ {
		v.recordCursor(int32(i), int32(-i), i%2 == 0)
	}
	n, samples := v.cursorStats()
	if n != cursorSampleCap+100 {
		t.Errorf("cursorEvents = %d, want %d", n, cursorSampleCap+100)
	}
	if len(samples) != cursorSampleCap {
		t.Fatalf("samples = %d, want %d", len(samples), cursorSampleCap)
	}
	if samples[0].X != 0 || samples[0].Visible != 1 || samples[1].Visible != 0 {
		t.Errorf("sample[0:1] = %+v %+v", samples[0], samples[1])
	}
	if samples[len(samples)-1].X != cursorSampleCap-1 {
		t.Errorf("overflow samples should be dropped, not appended")
	}
	// stats 返回副本:改副本不影响内部。
	samples[0].X = 9999
	if _, again := v.cursorStats(); again[0].X == 9999 {
		t.Errorf("cursorStats must return a copy")
	}
}

// TestDisplayRecording:display_changed 事件全量计数 + 样本截 cap + 副本语义
// (M2-Slice1 Task 2)。
func TestDisplayRecording(t *testing.T) {
	v := &viewer{start: time.Now()}
	for i := 0; i < displaySampleCap+50; i++ {
		v.recordDisplay(uint32(i), uint32(i*10), uint32(i*5), "resolution")
	}
	n, samples := v.displayStats()
	if n != displaySampleCap+50 {
		t.Errorf("displayEvents = %d, want %d", n, displaySampleCap+50)
	}
	if len(samples) != displaySampleCap {
		t.Fatalf("samples = %d, want %d", len(samples), displaySampleCap)
	}
	if samples[0].Gen != 0 || samples[1].W != 10 || samples[0].Reason != "resolution" {
		t.Errorf("sample[0:1] = %+v %+v", samples[0], samples[1])
	}
	if samples[len(samples)-1].Gen != displaySampleCap-1 {
		t.Errorf("overflow samples should be dropped, not appended")
	}
	samples[0].Gen = 9999
	if _, again := v.displayStats(); again[0].Gen == 9999 {
		t.Errorf("displayStats must return a copy")
	}
}

// TestStateRecording:STATE 事件全量计数 + 样本截 cap + 副本语义
// (M2-Slice1 Task 6 门②/④ 证据)。
func TestStateRecording(t *testing.T) {
	v := &viewer{start: time.Now()}
	for i := 0; i < stateSampleCap+10; i++ {
		v.recordState("backend_changed", true)
	}
	v.recordState("capture_rebuilt", true)
	n, samples := v.stateStats()
	if n != stateSampleCap+11 {
		t.Errorf("stateEvents = %d, want %d", n, stateSampleCap+11)
	}
	if len(samples) != stateSampleCap {
		t.Fatalf("samples = %d, want %d", len(samples), stateSampleCap)
	}
	if samples[0].Code != "backend_changed" || !samples[0].Recoverable {
		t.Errorf("sample[0] = %+v", samples[0])
	}
	samples[0].Code = "mutated"
	if _, again := v.stateStats(); again[0].Code == "mutated" {
		t.Errorf("stateStats must return a copy")
	}
}

// TestDrainSasReplies(Task 6 关联):清空丢弃排队的迟到
// secure_attention_result(FIFO,空即止),后续等待只见新回执。
func TestDrainSasReplies(t *testing.T) {
	ch := make(chan sasReply, 8)
	if n := drainSasReplies(ch); n != 0 {
		t.Fatalf("empty drain = %d, want 0", n)
	}
	ch <- sasReply{ok: true}
	ch <- sasReply{ok: false, code: "SAS_DENIED"}
	if n := drainSasReplies(ch); n != 2 {
		t.Fatalf("drain = %d, want 2", n)
	}
	select {
	case <-ch:
		t.Fatal("channel should be empty after drain")
	default:
	}
}

// TestDisplayResumeDelta(M2-Slice1 Task 6 门③):displayResumeMs 是「事件→
// 下一帧」的差值(非绝对时刻);事件后无帧 = -1。run-3 gate-3 曾把绝对
// AU 时刻当差值(16758ms 假失败,真值 1052ms)。
func TestDisplayResumeDelta(t *testing.T) {
	v := &viewer{start: time.Now().Add(-10 * time.Second)}
	v.recordDisplay(1, 1920, 1080, "access_lost")
	time.Sleep(30 * time.Millisecond)
	v.recordAu(true)
	time.Sleep(25 * time.Millisecond) // distinct ms bucket: the AU is BEFORE event 2
	v.recordDisplay(2, 1024, 768, "access_lost")
	s := v.collect("server", "", 0)
	if len(s.DisplayResumeMs) != 2 {
		t.Fatalf("displayResumeMs = %v, want 2 entries", s.DisplayResumeMs)
	}
	if s.DisplayResumeMs[0] < 0 || s.DisplayResumeMs[0] > 5000 {
		t.Errorf("first delta = %dms, want a small positive delta", s.DisplayResumeMs[0])
	}
	if s.DisplayResumeMs[1] != -1 {
		t.Errorf("second delta = %dms, want -1 (no AU after the second event)", s.DisplayResumeMs[1])
	}
}

// ---- M3 Task 6:TURN/TCP congestion E2E 纯逻辑(先测后实现)----

// TestPercentile:nearest-rank 定义(sorted 输入;空 = 0)。
func TestPercentile(t *testing.T) {
	if got := percentile(nil, 0.5); got != 0 {
		t.Fatalf("percentile(nil) = %v, want 0", got)
	}
	one := []float64{42}
	if got := percentile(one, 0.5); got != 42 {
		t.Fatalf("percentile([42]) = %v, want 42", got)
	}
	v := []float64{10, 20, 30, 40} // 已排序
	// nearest-rank:idx = ceil(p·n) − 1 → p50 = 第 2 小(20)、p95 = 最大(40)。
	if got := percentile(v, 0.50); got != 20 {
		t.Errorf("p50 = %v, want 20", got)
	}
	if got := percentile(v, 0.95); got != 40 {
		t.Errorf("p95 = %v, want 40", got)
	}
	if got := percentile(v, 0.0); got != 10 {
		t.Errorf("p0 = %v, want 10", got)
	}
	if got := percentile(v, 1.0); got != 40 {
		t.Errorf("p100 = %v, want 40", got)
	}
}

// TestQueueAgeStats:接收侧排队年龄 = 每帧单向时延 − 运行最小值(时延
// 基线抵消:发送/接收时钟零点差与传播时延被减掉,余量即排队增长)。
func TestQueueAgeStats(t *testing.T) {
	p50, p95, max := queueAgeStats(nil)
	if p50 != 0 || p95 != 0 || max != 0 {
		t.Fatalf("empty = %v/%v/%v, want 0/0/0", p50, p95, max)
	}
	// 基线 100ms(时钟零点差),帧间排队增长 0/10/30/100ms。
	ages := []float64{100, 110, 130, 200}
	p50, p95, max = queueAgeStats(ages)
	if max != 100 {
		t.Errorf("max = %v, want 100", max)
	}
	if p50 != 10 {
		t.Errorf("p50 = %v, want 10 (ages [0 10 30 100], nearest-rank rank 2)", p50)
	}
	if p95 != 100 {
		t.Errorf("p95 = %v, want 100", p95)
	}
	// 最小值出现在中间(非首帧)同样成立:ages → [0, 0, 5]。
	p50, p95, max = queueAgeStats([]float64{200, 100, 105})
	if max != 5 || p50 != 0 || p95 != 5 {
		t.Errorf("mid-baseline case = %v/%v/%v, want 0/5/5", p50, p95, max)
	}
}

// TestRtpStep:16..不 —— 32 位回绕感知的帧边界步进;delta<=0 即回归
// (重复/回退;回绕经 int32 差值天然为正)。
func TestRtpStep(t *testing.T) {
	cases := []struct {
		prev, cur  uint32
		delta      int64
		regression bool
	}{
		{prev: 5, cur: 7, delta: 2, regression: false},
		{prev: 7, cur: 7, delta: 0, regression: true},   // 重复时戳
		{prev: 10, cur: 5, delta: -5, regression: true}, // 回退
		// 回绕:0xFFFFFFFE → 0x00000001 差 +3(int32 算术)。
		{prev: 0xFFFFFFFE, cur: 0x00000001, delta: 3, regression: false},
		{prev: 0xFFFFFFFF, cur: 0x00000000, delta: 1, regression: false},
	}
	for _, c := range cases {
		d, reg := rtpStep(c.prev, c.cur)
		if d != c.delta || reg != c.regression {
			t.Errorf("rtpStep(%d, %d) = (%d, %v), want (%d, %v)",
				c.prev, c.cur, d, reg, c.delta, c.regression)
		}
	}
}

// TestFrameMetaRecDecode:56 字节 FrameMetaV1 镜像解码(agent/desktop/
// frame_meta.go 布局;版本/长度不符拒绝)。
func TestFrameMetaRecDecode(t *testing.T) {
	b := make([]byte, 56)
	b[0] = 1 // version
	binary.LittleEndian.PutUint32(b[4:], 987654321)
	binary.LittleEndian.PutUint64(b[8:], 7)   // codecEpoch
	binary.LittleEndian.PutUint64(b[16:], 42) // contentId
	binary.LittleEndian.PutUint64(b[24:], 1001)
	binary.LittleEndian.PutUint64(b[32:], 0x1122334455667788)
	binary.LittleEndian.PutUint64(b[40:], 0xDEADBEEF) // hash(不校验,透传)

	rec, ok := decodeFrameMetaRec(b)
	if !ok {
		t.Fatal("decodeFrameMetaRec rejected a valid record")
	}
	if rec.RTPTimestamp != 987654321 || rec.CodecEpoch != 7 || rec.ContentID != 42 ||
		rec.EncodeSeq != 1001 || rec.SourceMonoUs != 0x1122334455667788 {
		t.Fatalf("rec = %+v", rec)
	}
	if _, ok := decodeFrameMetaRec(b[:55]); ok {
		t.Error("short record must be rejected")
	}
	if _, ok := decodeFrameMetaRec(append(append([]byte(nil), b...), 0)); ok {
		t.Error("long record must be rejected")
	}
	bad := append([]byte(nil), b...)
	bad[0] = 2
	if _, ok := decodeFrameMetaRec(bad); ok {
		t.Error("version != 1 must be rejected")
	}
}

// TestCountFrameMetaRegressions:身份单调性镜像(desktoppipe frameLedger
// .accept 的 codecEpoch 维):epoch 前进重基线;同 epoch 内 contentId
// 回退 / encodeSeq 不前进即回归;被拒记录不推进基线。
func TestCountFrameMetaRegressions(t *testing.T) {
	mk := func(epoch, content, seq uint64) frameMetaRec {
		return frameMetaRec{CodecEpoch: epoch, ContentID: content, EncodeSeq: seq}
	}
	// 单调前进:零回归。
	c, s, e := countFrameMetaRegressions([]frameMetaRec{
		mk(1, 10, 1), mk(1, 10, 2), mk(1, 12, 9), mk(1, 12, 10),
	})
	if c != 0 || s != 0 || e != 0 {
		t.Fatalf("monotonic = %d/%d/%d, want 0/0/0", c, s, e)
	}
	// contentId 回退(同 epoch)。
	c, s, e = countFrameMetaRegressions([]frameMetaRec{mk(1, 12, 5), mk(1, 10, 6)})
	if c != 1 || s != 0 || e != 0 {
		t.Fatalf("contentId regression = %d/%d/%d, want 1/0/0", c, s, e)
	}
	// encodeSeq 重复(同 epoch 同 content)。
	c, s, e = countFrameMetaRegressions([]frameMetaRec{mk(1, 10, 5), mk(1, 10, 5)})
	if c != 0 || s != 1 || e != 0 {
		t.Fatalf("encodeSeq regression = %d/%d/%d, want 0/1/0", c, s, e)
	}
	// epoch 前进重基线:content 回到低位亦合法。
	c, s, e = countFrameMetaRegressions([]frameMetaRec{mk(1, 12, 5), mk(2, 1, 1)})
	if c != 0 || s != 0 || e != 0 {
		t.Fatalf("epoch advance = %d/%d/%d, want 0/0/0", c, s, e)
	}
	// epoch 回退。
	c, s, e = countFrameMetaRegressions([]frameMetaRec{mk(2, 1, 1), mk(1, 9, 9)})
	if c != 0 || s != 0 || e != 1 {
		t.Fatalf("epoch regression = %d/%d/%d, want 0/0/1", c, s, e)
	}
	// 被拒记录不推进基线:回退后继续按旧基线判。
	c, s, e = countFrameMetaRegressions([]frameMetaRec{
		mk(1, 10, 5), mk(1, 8, 6), mk(1, 9, 6), // 9 仍 < 基线 10 → 又一条 contentId 回归
	})
	if c != 2 {
		t.Fatalf("baseline must not advance on rejected records: contentId = %d, want 2", c)
	}
}

// TestRecoveryLogic:恢复 = 接收间隙 ≥ 阈值后的首帧;恢复必须 IDR 起
// 步(丢后不允许 delta 续流)。阈值自适应:max(300ms, 6×p50 帧距)。
func TestRecoveryLogic(t *testing.T) {
	// 33ms 稳定帧距 → 阈值 300ms。
	aus := []auRec{{0, true}, {33, false}, {66, false}, {500, false}, {533, false}}
	if got := recoveryGapMs(aus); got != 300 {
		t.Fatalf("recoveryGapMs(33ms cadence) = %d, want 300", got)
	}
	// 200ms 帧距 → 阈值 6×200=1200ms。
	slow := make([]auRec, 0, 12)
	for i := 0; i < 12; i++ {
		slow = append(slow, auRec{TMs: int64(200 * i), Key: i == 0})
	}
	if got := recoveryGapMs(slow); got != 1200 {
		t.Fatalf("recoveryGapMs(200ms cadence) = %d, want 1200", got)
	}
	// 间隙 434ms ≥ 300 → 恢复点在 500ms;delta 起步 = 违例 1。
	if got := recoveryViolations(aus, 300); got != 1 {
		t.Fatalf("delta-first recovery = %d violations, want 1", got)
	}
	// IDR 起步 = 无违例。
	ok := []auRec{{0, true}, {33, false}, {66, false}, {500, true}, {533, false}}
	if got := recoveryViolations(ok, 300); got != 0 {
		t.Fatalf("IDR-first recovery = %d violations, want 0", got)
	}
	// 间隙 < 阈值不构成恢复。
	tight := []auRec{{0, true}, {33, false}, {300, false}}
	if got := recoveryViolations(tight, 300); got != 0 {
		t.Fatalf("sub-threshold gap counted as recovery: %d", got)
	}
	// 间隙恰等于阈值构成恢复(>=)。
	edge := []auRec{{0, true}, {333, false}}
	if got := recoveryViolations(edge, 300); got != 1 {
		t.Fatalf("gap == threshold must count: %d, want 1", got)
	}
}

// TestPausedIntervals:spectator_network_paused 稳定态 → 暂停区间
// [首个暂停样本, run 结束](M3 无解除词汇;后续不同 code 的样本不解除)。
func TestPausedIntervals(t *testing.T) {
	events, ms := pausedIntervals(nil, 5000)
	if events != 0 || ms != 0 {
		t.Fatalf("no samples = %d/%d, want 0/0", events, ms)
	}
	samples := []stateSample{
		{TMs: 0, Code: "capture_rebuilt"},
		{TMs: 2000, Code: spectatorPausedCode},
		{TMs: 2500, Code: spectatorPausedCode}, // 幂等重发:仍是同一区间
		{TMs: 3000, Code: "backend_changed"},   // 不解除暂停
	}
	events, ms = pausedIntervals(samples, 5000)
	if events != 2 {
		t.Errorf("events = %d, want 2 (each state frame observed)", events)
	}
	if ms != 3000 {
		t.Errorf("pausedMs = %d, want 3000 (first pause at 2000 to run end 5000)", ms)
	}
}

// TestEvaluateTask6Gates:--expect-queue-max-ms 与 --expect-recovery-idr。
func TestEvaluateTask6Gates(t *testing.T) {
	base := func() *summary {
		return &summary{FirstFrameMs: 100, Keyframes: 2, Frames: 10}
	}
	c := &config{expectQueueMaxMs: 100, expectRecoveryIdr: true, duration: time.Second}

	ok := base()
	ok.QueueAgeMaxMs = 99.9
	ok.RecoveryViolations = 0
	ok.evaluate(c)
	if !ok.AssertionsPassed {
		t.Fatalf("within gates should pass: %+v (%v)", ok, ok.Failures)
	}

	over := base()
	over.QueueAgeMaxMs = 150
	over.evaluate(c)
	if over.AssertionsPassed || len(over.Failures) != 1 {
		t.Fatalf("queue max 150 > 100 must fail exactly once: %+v (%v)", over, over.Failures)
	}

	deltas := base()
	deltas.RecoveryViolations = 2
	deltas.evaluate(c)
	if deltas.AssertionsPassed || len(deltas.Failures) != 1 {
		t.Fatalf("post-drop delta continuation must fail exactly once: %+v (%v)", deltas, deltas.Failures)
	}

	// 0/关 = 跳过。
	off := base()
	off.QueueAgeMaxMs = 5000
	off.RecoveryViolations = 7
	off.evaluate(&config{})
	if !off.AssertionsPassed {
		t.Fatalf("gates off should pass: %+v", off)
	}
}

// ---- M3 Task 6:loopback 合成反馈网络矩阵 ----
//
// 裁决 2 的 loopback/synthetic 模式:同一进程内「真实 desktop.Handler(会话
// WS 信令 + streamQoS 决策环)+ 真实 Publisher/ViewerSender(Pion 发送侧)
// + 真实 viewer PeerConnection ×2(controller + spectator)」,链路不受限
// (host 候选回环),由 harness 按档位注入 viewer_feedback(与 web
// DesktopLive 同一 1s 节奏/同一字段形态)驱动 QoS 决策环 —— 不依赖 OS 级
// 整形(RDP 下 netsh qos 不可靠,回环接口不整形),确定性可进 CI。真 TURN
// 互联网拓扑是 M4 的矩阵。
//
// 每档位断言(裁决 2):① queueAge 硬上限 100ms(接收侧);② spectator 被
// 限速时只有 spectator 暂停(state 帧 + 帧流停止),controller 保持 LIVE;
// ③ 每次恢复以 IDR 起步(接收间隙后首帧非 delta;PLI→IDR 闭环另证)。
// 另按档位断言 QoS 动作:无拥塞档不降档;1Mbps/250ms 档走拥塞立即降档。

// netCase 是一个 relay 形状的约束档位(15Mbps/30ms、5Mbps/100ms、
// 1Mbps/250ms —— run-network-matrix.ps1 编码同一张表)。
type netCase struct {
	name    string
	bps     uint64
	rttMs   float64
	queueMs float64 // controller 链路的排队反馈(>100 = agent 判拥塞)
	congest bool    // 本档位断言拥塞降档(码率阶梯前进了)
}

var networkMatrixCases = []netCase{
	{name: "15mbps-30ms", bps: 15_000_000, rttMs: 30, queueMs: 5, congest: false},
	{name: "5mbps-100ms", bps: 5_000_000, rttMs: 100, queueMs: 8, congest: false},
	{name: "1mbps-250ms", bps: 1_000_000, rttMs: 250, queueMs: 250, congest: true},
}

// matrixCaseInitialBitrate 是 1920 宽流的初始码率(agent bitrateForWidth
// (1920) = 2.3Mbps;qosManager 以 HOST_HELLO 维度播种)。
const matrixCaseInitialBitrate = 2_300_000

// matrixSource 是 desktop.Source 的矩阵 fake:30fps 合成帧(IDR ~2s GOP +
// 请求强制 IDR),记录 QoS 决策(SetVideoConfig)与关键帧请求。每个会话
// 一个实例(镜像真实拓扑:每会话一条 ATTACH,共享编码参数由 Handler 级
// streamQoS 决策)。
type matrixSource struct {
	mu      sync.Mutex
	cfgs    []desktop.VideoConfig
	keyReqs []string
	nextKey bool
	mono    uint64
	seq     uint64

	frameCh chan desktop.Frame
	done    chan struct{}
	once    sync.Once
}

func newMatrixSource() *matrixSource {
	return &matrixSource{
		frameCh: make(chan desktop.Frame, 32),
		done:    make(chan struct{}),
	}
}

// run 以 33ms 帧距产帧直至 done;消费落后即丢(镜像真实客户端背压)。
func (s *matrixSource) run() {
	tk := time.NewTicker(33 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-tk.C:
			s.mu.Lock()
			force := s.nextKey
			s.nextKey = false
			s.mono += 33_333
			s.seq++
			seq := s.seq
			s.mu.Unlock()
			key := force || seq%60 == 1
			select {
			case s.frameCh <- desktop.Frame{
				Key: key, PresentMonoUs: s.mono, AU: matrixAU(key),
				CaptureEpoch: 1, CodecEpoch: 1, ContentID: 1,
				EncodeSeq: seq, SourceMonoUs: s.mono - 1_000,
			}:
			case <-s.done:
				return
			default: // 消费侧落后:丢帧(真实客户端同款背压语义)
			}
		}
	}
}

func (s *matrixSource) RecvFrame(ctx context.Context) (desktop.Frame, bool) {
	select {
	case f, ok := <-s.frameCh:
		return f, ok
	case <-ctx.Done():
		return desktop.Frame{}, false
	case <-s.done:
		return desktop.Frame{}, false
	}
}

func (s *matrixSource) RecvState(ctx context.Context) (desktop.StateEvent, bool) {
	<-ctx.Done()
	return desktop.StateEvent{}, false
}

func (s *matrixSource) RecvCursor(ctx context.Context) (desktop.CursorEvent, bool) {
	<-ctx.Done()
	return desktop.CursorEvent{}, false
}

func (s *matrixSource) RecvDisplay(ctx context.Context) (desktop.DisplayChangedEvent, bool) {
	<-ctx.Done()
	return desktop.DisplayChangedEvent{}, false
}

func (s *matrixSource) Hello() *desktop.HelloInfo {
	return &desktop.HelloInfo{Gen: 1, W: 1920, H: 1080, Fps: 30, MaxSubs: 4}
}

func (s *matrixSource) RequestKeyframe(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keyReqs = append(s.keyReqs, reason)
	s.nextKey = true
	return nil
}

func (s *matrixSource) SetVideoConfig(cfg desktop.VideoConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfgs = append(s.cfgs, cfg)
	return nil
}

func (s *matrixSource) SubID() uint32            { return 7 }
func (s *matrixSource) SendInput(_ []byte) error { return nil }

func (s *matrixSource) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

func (s *matrixSource) videoConfigs() []desktop.VideoConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]desktop.VideoConfig(nil), s.cfgs...)
}

func (s *matrixSource) keyRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.keyReqs...)
}

var (
	matSPS = []byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x64, 0x00, 0x1f, 0xac, 0xd9, 0x40, 0x50, 0x05, 0xbb, 0x01, 0x6c, 0x80}
	matPPS = []byte{0x00, 0x00, 0x00, 0x01, 0x68, 0xeb, 0xec, 0xb2, 0x2c}
)

// matrixAU 产合成 Annex-B:key = SPS+PPS+IDR slice(~8KB,>MTU 走 FU-A),
// delta = P slice(~1.2KB);总码率 ~350kbps,远低于最低 pacing 预算
// (500kbps×85%)—— 档位差异全部由注入的 viewer_feedback 承载,与链路
// 实际吞吐解耦(确定性)。
func matrixAU(key bool) []byte {
	var au []byte
	if key {
		au = append(au, matSPS...)
		au = append(au, matPPS...)
	}
	hdr := byte(0x41)
	if key {
		hdr = 0x65
	}
	au = append(au, 0x00, 0x00, 0x00, 0x01, hdr, 0x88)
	n := 1200
	if key {
		n = 8000
	}
	for i := 0; i < n; i++ {
		au = append(au, 0xa5) // 绝不含起始码序列
	}
	return au
}

// matrixStarter 每会话产一个新 source(共享决策、独立帧流)。
type matrixStarter struct {
	mu   sync.Mutex
	srcs []*matrixSource
}

func (st *matrixStarter) Start(_ context.Context, _ uint32) (desktop.Source, error) {
	s := newMatrixSource()
	go s.run()
	st.mu.Lock()
	st.srcs = append(st.srcs, s)
	st.mu.Unlock()
	return s, nil
}

func (st *matrixStarter) Stop() error { return nil }

func (st *matrixStarter) sources() []*matrixSource {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]*matrixSource(nil), st.srcs...)
}

// loopPeer 是一个连到回环 Handler 的 viewer 会话(WS 信令 + 真 PC)。
type loopPeer struct {
	t   *testing.T
	ctx context.Context
	ws  *websocket.Conn
	v   *viewer
}

// startLoopPeer 拨 WS、建 viewer PC、跑 offer/answer/trickle 直至 Connected
// (runServer 的信令粘合的测试版;frame-meta/cursor 通道照常接线)。
func startLoopPeer(t *testing.T, ctx context.Context, wsURL, session string, log *slog.Logger) *loopPeer {
	t.Helper()
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("loopPeer %s: dial: %v", session, err)
	}
	ws.SetReadLimit(1 << 20)
	v, err := newViewer(&config{}, nil, false, log)
	if err != nil {
		t.Fatalf("loopPeer %s: viewer: %v", session, err)
	}
	if _, err := v.pc.CreateDataChannel("xnc-viewer-sctp", nil); err != nil {
		t.Fatalf("loopPeer %s: placeholder dc: %v", session, err)
	}
	readyCh := make(chan struct{}, 1)
	wsErr := make(chan error, 1)
	v.pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		switch dc.Label() {
		case dcLabelFrameMeta:
			dc.OnMessage(func(m webrtc.DataChannelMessage) { v.recordMetaMsg(m.Data) })
		case dcLabelCursor:
			dc.OnMessage(func(m webrtc.DataChannelMessage) {
				if x, y, vis, ok := decodeCursorWire(m.Data); ok {
					v.recordCursor(x, y, vis)
				}
			})
		}
	})
	// 入站信令泵(单读者;ready → offer 时序契约与 runServer 一致)。
	go func() {
		for {
			mt, r, err := ws.Reader(ctx)
			if err != nil {
				wsErr <- err
				return
			}
			if mt != websocket.MessageText {
				continue
			}
			b, err := io.ReadAll(io.LimitReader(r, 1<<20))
			if err != nil {
				wsErr <- err
				return
			}
			var f struct {
				Type        string                   `json:"type"`
				SDP         string                   `json:"sdp"`
				Candidate   *webrtc.ICECandidateInit `json:"candidate"`
				Code        string                   `json:"code"`
				Recoverable bool                     `json:"recoverable"`
			}
			if json.Unmarshal(b, &f) != nil {
				continue
			}
			switch f.Type {
			case "ready":
				select {
				case readyCh <- struct{}{}:
				default:
				}
			case "answer":
				if err := v.pc.SetRemoteDescription(webrtc.SessionDescription{
					Type: webrtc.SDPTypeAnswer, SDP: f.SDP}); err != nil {
					wsErr <- fmt.Errorf("set answer: %w", err)
					return
				}
			case "ice":
				if f.Candidate != nil {
					_ = v.pc.AddICECandidate(*f.Candidate)
				}
			case "state":
				v.recordState(f.Code, f.Recoverable)
			}
		}
	}()
	select {
	case <-readyCh:
	case err := <-wsErr:
		t.Fatalf("loopPeer %s: ws before ready: %v", session, err)
	case <-ctx.Done():
		t.Fatalf("loopPeer %s: ctx done before ready", session)
	}
	v.pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand == nil {
			return
		}
		ci := cand.ToJSON()
		b, _ := json.Marshal(map[string]any{"type": "ice", "candidate": ci})
		wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
		_ = ws.Write(wctx, websocket.MessageText, b)
		wcancel()
	})
	offer, err := v.pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("loopPeer %s: offer: %v", session, err)
	}
	if err := v.pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("loopPeer %s: set local: %v", session, err)
	}
	ob, _ := json.Marshal(map[string]any{"type": "offer", "sdp": offer.SDP})
	wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
	if err := ws.Write(wctx, websocket.MessageText, ob); err != nil {
		wcancel()
		t.Fatalf("loopPeer %s: send offer: %v", session, err)
	}
	wcancel()
	if err := v.waitConnected(20 * time.Second); err != nil {
		t.Fatalf("loopPeer %s: %v", session, err)
	}
	return &loopPeer{t: t, ctx: ctx, ws: ws, v: v}
}

// sendFeedback 注入一条合成 viewer_feedback(1s 节奏由调用方掌握)。
func (p *loopPeer) sendFeedback(bps uint64, queueMs, rttMs float64) {
	p.t.Helper()
	b, _ := json.Marshal(map[string]any{
		"type": "viewer_feedback", "visible": true,
		"estimatedBps": bps, "queueMs": queueMs, "decodeQueue": 0, "rttMs": rttMs,
	})
	p.v.fbSent.Add(1)
	wctx, wcancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer wcancel()
	if err := p.ws.Write(wctx, websocket.MessageText, b); err != nil {
		p.t.Fatalf("sendFeedback: %v", err)
	}
}

func (p *loopPeer) close() {
	_ = p.v.pc.Close()
	p.ws.CloseNow()
}

// pollUntil 有界轮询(25ms 粒度;超时带 what 指认)。
func pollUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout (%v) waiting for %s", d, what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// matrixCfgJSON 是报告里的 SET_VIDEO_CONFIG 决策记录。
type matrixCfgJSON struct {
	Bitrate uint32 `json:"bitrate"`
	FPS     uint32 `json:"fps"`
	MaxW    uint32 `json:"maxW"`
}

// matrixReport 是一个档位的完整矩阵报告(脚本汇总消费;也落盘留档)。
type matrixReport struct {
	Case           string          `json:"case"`
	ProfileBps     uint64          `json:"profileBps"`
	ProfileRttMs   float64         `json:"profileRttMs"`
	ProfileQueueMs float64         `json:"profileQueueMs"`
	Passed         bool            `json:"passed"`
	Failures       []string        `json:"failures,omitempty"`
	Controller     *summary        `json:"controller"`
	Spectator      *summary        `json:"spectator"`
	VideoConfigs   []matrixCfgJSON `json:"videoConfigs"`
	KeyRequests    []string        `json:"keyRequests,omitempty"`
	DurationMs     int64           `json:"durationMs"`
}

// TestNetworkMatrixCase — 裁决 2 的确定性档位门(CI 常规 go test;XNC_NET_CASE
// 可选单档,XNC_NET_REPORT_DIR 指定报告落盘目录,缺省 t.TempDir)。
func TestNetworkMatrixCase(t *testing.T) {
	sel := os.Getenv("XNC_NET_CASE")
	dir := os.Getenv("XNC_NET_REPORT_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("report dir: %v", err)
	}
	ran := false
	for _, tc := range networkMatrixCases {
		if sel != "" && sel != tc.name {
			continue
		}
		ran = true
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			runNetworkCase(t, tc, dir)
		})
	}
	if sel != "" && !ran {
		t.Fatalf("unknown XNC_NET_CASE %q (want one of 15mbps-30ms/5mbps-100ms/1mbps-250ms)", sel)
	}
}

// runNetworkCase 跑一个档位的完整场景(阶段见行内注释)并落盘报告。
func runNetworkCase(t *testing.T, tc netCase, dir string) {
	t.Helper()
	started := time.Now()
	log := slog.Default()

	st := &matrixStarter{}
	h := &desktop.Handler{Log: log, Starter: st}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(1 << 20)
		h.Handle(ctx, c, r.URL.Query().Get("s"), json.RawMessage(`{"signaling":"webrtc","iceTransportPolicy":"all"}`))
	}))
	defer srv.Close()
	go func() {
		<-ctx.Done()
		close(handlerDone)
	}()
	// 自带 cancel:Fatal 路径上它先于 defer cancel() 运行,handlerDone 的
	// 等待不会空转 5s(cancel 未跑 → done 永不关)。
	defer func() {
		cancel()
		select {
		case <-handlerDone:
		case <-time.After(5 * time.Second):
			t.Log("handler goroutines did not return within 5s of ctx cancel")
		}
	}()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/?s="

	// 会话名仅作标注(a-ctrl/b-spec);controller 仲裁是先到先得(见
	// Phase ② 的定身序)。
	ctrl := startLoopPeer(t, ctx, wsURL+"a-ctrl", "a-ctrl", log)
	defer ctrl.close()
	spec := startLoopPeer(t, ctx, wsURL+"b-spec", "b-spec", log)
	defer spec.close()
	ctrlSrc, specSrc := st.sources()[0], st.sources()[1]

	// anyVideoConfig:决策(setVideoConfig 经观察会话的源下发)可能在
	// ctrl 或 spec 的源上 —— 两条会话是两个 Handler goroutine,哪条先消费
	// 反馈存在竞态;观测取并集,断言才不依赖调度。
	anyVideoConfig := func(pred func(desktop.VideoConfig) bool) bool {
		for _, s := range []*matrixSource{ctrlSrc, specSrc} {
			for _, c := range s.videoConfigs() {
				if pred(c) {
					return true
				}
			}
		}
		return false
	}

	failures := []string{}
	fail := func(format string, args ...any) {
		failures = append(failures, fmt.Sprintf(format, args...))
	}

	// ① join:两 viewer 首帧必须 IDR(WAIT_IDR 承接语义)。
	pollUntil(t, 15*time.Second, "controller first frames", func() bool { return ctrl.v.frames.Load() >= 10 })
	pollUntil(t, 15*time.Second, "spectator first frames", func() bool { return spec.v.frames.Load() >= 10 })
	if !ctrl.v.firstKey.Load() {
		fail("controller first decoded AU was not an IDR")
	}
	if !spec.v.firstKey.Load() {
		fail("spectator first decoded AU was not an IDR")
	}

	// ② Phase A:先只发 controller 反馈 —— controllership 是「先到先得,
	// 在位者可见即保持」(qos_controller promote),不是字典序;spectator
	// 必须等 controller 定身。定身证明 = 初始配置落在 ctrl 的源上
	//(apply 按观察会话路由 SetVideoConfig;此刻只有 ctrl 发过反馈)。
	ctrl.sendFeedback(tc.bps, 5, tc.rttMs)
	pollUntil(t, 5*time.Second, "controller seated (initial SET_VIDEO_CONFIG on the controller source)", func() bool {
		for _, c := range ctrlSrc.videoConfigs() {
			if c.Bitrate == matrixCaseInitialBitrate {
				return true
			}
		}
		return false
	})
	// controller 定身后 spectator 入场(健康反馈一轮,1s 节奏)。
	spec.sendFeedback(tc.bps, 5, tc.rttMs)
	time.Sleep(1050 * time.Millisecond)
	ctrl.sendFeedback(tc.bps, 5, tc.rttMs)
	time.Sleep(200 * time.Millisecond) // 第二轮 settle(1M 档 est 跌落落 ctrlSrc)

	// ③ Phase B(spectator 限速至 30% 档位带宽 < 35% 暂停线)→ 只有
	// spectator 暂停:state 帧 + 帧流冻结;controller 保持 LIVE。
	specThrottleBps := uint64(float64(tc.bps) * 0.30)
	spec.sendFeedback(specThrottleBps, 5, tc.rttMs)
	pollUntil(t, 5*time.Second, "spectator_network_paused state frame", func() bool {
		_, samples := spec.v.stateStats()
		for _, s := range samples {
			if s.Code == spectatorPausedCode {
				return true
			}
		}
		return false
	})
	time.Sleep(200 * time.Millisecond) // 在途帧落地
	specFrozen := spec.v.frames.Load()
	ctrlLive := ctrl.v.frames.Load()
	time.Sleep(600 * time.Millisecond) // 观察窗
	if got := spec.v.frames.Load(); got != specFrozen {
		fail("spectator kept receiving after pause: %d -> %d frames", specFrozen, got)
	}
	if ctrl.v.frames.Load() <= ctrlLive {
		fail("controller stopped receiving while a spectator was throttled (must stay LIVE)")
	}
	// controller 自身不得收到暂停态(只有 spectator 暂停)。
	if _, samples := ctrl.v.stateStats(); len(samples) > 0 {
		for _, s := range samples {
			if s.Code == spectatorPausedCode {
				fail("controller received spectator_network_paused (only the spectator may pause)")
			}
		}
	}

	// ④ Phase C(档位拥塞):controller queueMs = 档位排队反馈。>100 →
	// 立即 30% 降档;<100 → 无降档(升档需 10s 稳定窗,本用例内不触发)。
	ctrl.sendFeedback(tc.bps, tc.queueMs, tc.rttMs)
	if tc.congest {
		// 1M 档先有 est 跌落(85%×1M=850k),拥塞再 30%(595k);
		// ≤700k 只能由拥塞立即通道到达(est 降档是幂等的 850k)。
		pollUntil(t, 5*time.Second, "congestion downshift (immediate 30% channel)", func() bool {
			return anyVideoConfig(func(c desktop.VideoConfig) bool { return c.Bitrate <= 700_000 })
		})
	}
	time.Sleep(300 * time.Millisecond)
	for _, src := range []*matrixSource{ctrlSrc, specSrc} {
		for _, c := range src.videoConfigs() {
			if c.Bitrate < 500_000 {
				fail("bitrate ladder broke the 500kbps floor: %+v", c)
			}
		}
		if !tc.congest {
			for _, c := range src.videoConfigs() {
				if c.Bitrate != matrixCaseInitialBitrate {
					fail("non-congested profile downshifted without congestion: %+v", c)
				}
			}
		}
	}

	// ⑤ PLI 恢复闭环:RTCP PLI → 协调器 → host 请求 → 新 IDR。
	kf0 := ctrl.v.keyframes.Load()
	if !ctrl.v.sendPLI(2 * time.Second) {
		fail("controller PLI send failed")
	} else {
		pollUntil(t, 5*time.Second, "PLI -> new IDR", func() bool { return ctrl.v.keyframes.Load() > kf0 })
	}

	// ⑥ 档位门(裁决 2):queueAge 硬上限 / 恢复 IDR 起步 / 回归零。
	cancel() // 收线:会话停止后 collect,ran 覆盖全程
	ctrl.close()
	spec.close()
	ctrlSum := ctrl.v.collect("loopback-ctrl", "", time.Since(ctrl.v.start))
	specSum := spec.v.collect("loopback-spec", "", time.Since(spec.v.start))
	if ctrlSum.QueueAgeMaxMs > 100 {
		fail("queue age max %.1fms > 100ms (sender hard bound)", ctrlSum.QueueAgeMaxMs)
	}
	if v := ctrlSum.RecoveryViolations; v > 0 {
		fail("%d recovery(ies) began with a delta frame on the controller", v)
	}
	if v := specSum.RecoveryViolations; v > 0 {
		fail("%d recovery(ies) began with a delta frame on the spectator", v)
	}
	if v := ctrlSum.RtpTsRegressions; v > 0 {
		fail("%d RTP timestamp regressions on the controller receive stream", v)
	}
	if specSum.PausedEvents < 1 {
		fail("no spectator_network_paused observation recorded")
	}
	if specSum.PausedMs < 500 {
		fail("paused interval = %dms, want >= 500ms of observed pause", specSum.PausedMs)
	}
	if ctrlSum.PausedEvents != 0 {
		fail("controller observed pause events (%d), want 0", ctrlSum.PausedEvents)
	}
	if ctrlSum.Frames < 60 {
		fail("controller received only %d frames (QoS loop must keep the stream flowing)", ctrlSum.Frames)
	}
	// frame-meta 回归(通道可用时;unordered 通道在回环上应保序)。
	if ctrlSum.FrameMetaCount >= 10 {
		if v := ctrlSum.ContentIdRegressions; v > 0 {
			fail("%d contentId regressions from frame-meta (%d records)", v, ctrlSum.FrameMetaCount)
		}
		if v := ctrlSum.EncodeSeqRegressions; v > 0 {
			fail("%d encodeSeq regressions from frame-meta (%d records)", v, ctrlSum.FrameMetaCount)
		}
	}
	if ctrl.v.plisSent.Load() > 0 && ctrlSum.PliToIdrMaxMs == 0 {
		fail("PLI sent but no IDR observed afterwards")
	}

	rep := &matrixReport{
		Case: tc.name, ProfileBps: tc.bps, ProfileRttMs: tc.rttMs, ProfileQueueMs: tc.queueMs,
		Passed:     len(failures) == 0,
		Failures:   failures,
		Controller: ctrlSum, Spectator: specSum,
		KeyRequests: ctrlSrc.keyRequests(),
		DurationMs:  time.Since(started).Milliseconds(),
	}
	// 决策记录取两会话源的并集(apply 按观察会话路由;见 anyVideoConfig)。
	for _, src := range []*matrixSource{ctrlSrc, specSrc} {
		for _, c := range src.videoConfigs() {
			rep.VideoConfigs = append(rep.VideoConfigs, matrixCfgJSON{Bitrate: c.Bitrate, FPS: c.FPS, MaxW: c.MaxW})
		}
	}
	jb, err := json.MarshalIndent(rep, "", " ")
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	path := filepath.Join(dir, "report-"+tc.name+".json")
	if err := os.WriteFile(path, jb, 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
	t.Logf("network matrix case %s: report=%s configs=%d ctrlFrames=%d specFrames=%d pausedMs=%d queueAgeMax=%.1fms",
		tc.name, path, len(rep.VideoConfigs), ctrlSum.Frames, specSum.Frames, specSum.PausedMs, ctrlSum.QueueAgeMaxMs)
	for _, f := range failures {
		t.Errorf("case %s: %s", tc.name, f)
	}
}
