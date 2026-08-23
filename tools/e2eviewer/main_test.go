package main

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
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
