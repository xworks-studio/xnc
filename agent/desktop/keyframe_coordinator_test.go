// keyframe_coordinator_test.go — M3 Task 2:KeyframeCoordinator 的确定性
// 单测。时钟注入(manualClock,复用 viewer_sender_test.go 的定义)驱动
// 250ms 常规冷却与 IDR 宽限;不 start() 常驻 watcher(与 ViewerSender 泵
// 同律:注入非真实时钟时勿启动 goroutine),超时路径经公共 API 的惰性
// 检查点(Request/OnIDR/Pending)驱动——生产侧 watcher 走同一 expire 入口
// (单独一条真实时钟用例覆盖)。
package desktop

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// ---- 测试基建:fakeKeySink / fakeStateSink / fakeCoordinator ----

// fakeKeySink 记录到达底层 sink(host 面)的每条请求。
type fakeKeySink struct {
	mu   sync.Mutex
	reqs []string
	err  error
}

func (f *fakeKeySink) RequestKeyframe(reason string) error {
	f.mu.Lock()
	f.reqs = append(f.reqs, reason)
	f.mu.Unlock()
	return f.err
}

func (f *fakeKeySink) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

// fakeStateSink 记录协调器发出的状态回调(code)。
type fakeStateSink struct {
	mu     sync.Mutex
	codes  []string
	recovs []bool
}

func (f *fakeStateSink) onState(code string, recoverable bool) {
	f.mu.Lock()
	f.codes = append(f.codes, code)
	f.recovs = append(f.recovs, recoverable)
	f.mu.Unlock()
}

func (f *fakeStateSink) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.codes...)
}

// fakeCoordinator 是真 KeyframeCoordinator + 假 sink/state + 假时钟。
type fakeCoordinator struct {
	coord *KeyframeCoordinator
	clk   *manualClock
	sink  *fakeKeySink
	state *fakeStateSink
}

func newFakeCoordinator(framePeriod time.Duration) *fakeCoordinator {
	f := &fakeCoordinator{
		clk:   newManualClock(),
		sink:  &fakeKeySink{},
		state: &fakeStateSink{},
	}
	f.coord = newKeyframeCoordinator(KeyframeCoordinatorConfig{
		Sink:        f.sink,
		OnState:     f.state.onState,
		FramePeriod: framePeriod,
		Now:         f.clk.Now,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return f
}

// ---- Step 1(brief 精确形状):合并 + 冷却 ----

// TestKeyframeCoordinatorMergesBurstInsideCooldown:250ms 内到达的
// PLI/FIR/overflow 合并成恰一次 Host 请求;reason 集进 pending,无状态发出。
func TestKeyframeCoordinatorMergesBurstInsideCooldown(t *testing.T) {
	f := newFakeCoordinator(33 * time.Millisecond)
	f.coord.Request("pli", false)
	f.clk.advance(10 * time.Millisecond)
	f.coord.Request("fir", false)
	f.clk.advance(10 * time.Millisecond)
	f.coord.Request("overflow", false)

	if reqs := f.sink.requests(); len(reqs) != 1 || reqs[0] != "pli" {
		t.Fatalf("host requests = %v, want exactly [pli] (burst merged)", reqs)
	}
	if p := f.coord.Pending(); len(p) != 3 || p[0] != "fir" || p[1] != "overflow" || p[2] != "pli" {
		t.Fatalf("pending = %v, want [fir overflow pli]", p)
	}
	if codes := f.state.snapshot(); len(codes) != 0 {
		t.Fatalf("unexpected states %v during cooldown", codes)
	}
}

// TestKeyframeCoordinatorCooldownElapsesAfterIDR:IDR 清 pending 后,冷却
// 窗口内的新请求仍被合并;恰在 250ms 边界处放行。
func TestKeyframeCoordinatorCooldownElapsesAfterIDR(t *testing.T) {
	f := newFakeCoordinator(33 * time.Millisecond)
	f.coord.Request("pli", false)
	f.coord.OnIDR(1, 1) // 匹配更新 → pending 清空
	if p := f.coord.Pending(); len(p) != 0 {
		t.Fatalf("pending after IDR = %v, want none", p)
	}

	f.clk.advance(100 * time.Millisecond)
	f.coord.Request("fir", false) // 冷却内 → 抑制
	if reqs := f.sink.requests(); len(reqs) != 1 {
		t.Fatalf("cooldown did not suppress: %v", reqs)
	}

	f.clk.advance(149 * time.Millisecond) // 累计 249ms,仍内
	f.coord.Request("fir", false)
	if reqs := f.sink.requests(); len(reqs) != 1 {
		t.Fatalf("cooldown boundary off (249ms should suppress): %v", reqs)
	}

	f.clk.advance(1 * time.Millisecond) // 恰 250ms
	f.coord.Request("fir", false)
	if reqs := f.sink.requests(); len(reqs) != 2 || reqs[1] != "fir" {
		t.Fatalf("cooldown must elapse at 250ms: %v", reqs)
	}
}

// TestKeyframeCoordinatorPacerBackoff(IDR 恢复死亡螺旋修正):发送器
// pacer 循环的撞门拒收可以在 host 未产出上一条请求的 IDR 时再次触发,而
// host kIdrMinIntervalMs=500 + 编码深度令 pacer 请求的 IDR ≥500ms 不可得
//(生产日志:同一秒两条 client_reason=pacer)。因此非 urgent 的 pacer
// 请求在 2×IDR 宽限(33ms 帧距 → 宽限 250ms → 退避 500ms)内不开启新
// 周期;其余 reason 照常冷却合并;退避过后 pacer 恢复正常打 host。
func TestKeyframeCoordinatorPacerBackoff(t *testing.T) {
	f := newFakeCoordinator(33 * time.Millisecond) // 宽限 250ms → 退避 500ms
	f.coord.Request("pacer", false)                // t=0 打 host
	if reqs := f.sink.requests(); len(reqs) != 1 || reqs[0] != "pacer" {
		t.Fatalf("host requests = %v, want [pacer]", reqs)
	}

	// t=100ms:在途周期内(pending 非空)→ 并入,不打 host。
	f.clk.advance(100 * time.Millisecond)
	f.coord.Request("pacer", false)
	if reqs := f.sink.requests(); len(reqs) != 1 {
		t.Fatalf("pending merge duplicated host request: %v", reqs)
	}

	// t=300ms:宽限(250ms)已过 → 超时清空周期;退避窗内(lastPacer=
	// 100ms,退避至 600ms)的新 pacer 请求整体丢弃 —— 不打 host、不进
	// pending(在途/排队中的上一条已覆盖它)。
	f.clk.advance(200 * time.Millisecond)
	if p := f.coord.Pending(); len(p) != 0 {
		t.Fatalf("pending after grace = %v, want expired", p)
	}
	f.coord.Request("pacer", false)
	if reqs := f.sink.requests(); len(reqs) != 1 {
		t.Fatalf("pacer backoff failed to suppress: %v", reqs)
	}
	if p := f.coord.Pending(); len(p) != 0 {
		t.Fatalf("backoff must not open a cycle: pending=%v", p)
	}

	// 其他 reason 不受 pacer 退避影响:pli 在 t=300ms(距上次打 host
	// 300ms ≥ 250ms 冷却)照常打 host。
	f.coord.Request("pli", false)
	if reqs := f.sink.requests(); len(reqs) != 2 || reqs[1] != "pli" {
		t.Fatalf("pli must not inherit the pacer backoff: %v", reqs)
	}

	// t=700ms:退避(600ms)已过 → pacer 恢复正常开新周期。
	f.clk.advance(400 * time.Millisecond)
	f.coord.Request("pacer", false)
	if reqs := f.sink.requests(); len(reqs) != 3 || reqs[2] != "pacer" {
		t.Fatalf("pacer must fire after the backoff window: %v", reqs)
	}
	// 恰两次超时状态:t=250ms 的 pacer 周期与 t=550ms 的 pli 周期(测试
	// 不喂 IDR,各自宽限到点恰一次)。
	if codes := f.state.snapshot(); len(codes) != 2 || codes[0] != stateEncoderIDRTimeout || codes[1] != stateEncoderIDRTimeout {
		t.Fatalf("states = %v, want two encoder_idr_timeout (pacer cycle, pli cycle)", codes)
	}
}

// ---- Step 1(裁决 2):urgent 绕过冷却、但绝不复制在途请求 ----

// TestKeyframeCoordinatorUrgentBypassesCooldownNeverDuplicates:
//   - 在途请求(pending 非空)时,urgent 请求并入 reason 集但不再次打 host;
//   - 无在途请求时,urgent 绕过 250ms 冷却立即打 host。
func TestKeyframeCoordinatorUrgentBypassesCooldownNeverDuplicates(t *testing.T) {
	f := newFakeCoordinator(33 * time.Millisecond)
	f.coord.Request("pli", false) // t0 打一次

	f.clk.advance(10 * time.Millisecond)
	f.coord.Request("connect", true) // 新订阅 urgent,但 pli 在途 → 只并入
	if reqs := f.sink.requests(); len(reqs) != 1 {
		t.Fatalf("urgent duplicated an already-pending request: %v", reqs)
	}
	if p := f.coord.Pending(); len(p) != 2 {
		t.Fatalf("pending = %v, want [connect pli]", p)
	}

	f.coord.OnIDR(1, 1) // 清空在途

	f.clk.advance(5 * time.Millisecond) // 距上次打 host 仅 15ms(冷却内)
	f.coord.Request("epoch-change", true)
	if reqs := f.sink.requests(); len(reqs) != 2 || reqs[1] != "epoch-change" {
		t.Fatalf("urgent must bypass cooldown: %v", reqs)
	}
	if codes := f.state.snapshot(); len(codes) != 0 {
		t.Fatalf("unexpected states %v", codes)
	}
}

// ---- Step 2:OnIDR 仅被匹配/更新的 IDR 清除 ----

// TestKeyframeCoordinatorOnIDRClearsOnlyMatchingOrNewer:过时 IDRs(同
// epoch 更小 encodeSeq / 更旧 epoch)不清 pending;同 epoch 更新 seq 或更新
// epoch 清除。v1(身份全零)退化为任意 IDR 清除。
func TestKeyframeCoordinatorOnIDRClearsOnlyMatchingOrNewer(t *testing.T) {
	f := newFakeCoordinator(33 * time.Millisecond)
	f.coord.OnIDR(5, 100) // 闲时观测:请求快照基准 (5,100)
	f.coord.Request("pli", false)

	f.coord.OnIDR(5, 99) // 同 epoch 更旧 seq(请求前已在途的 IDR)
	if p := f.coord.Pending(); len(p) != 1 {
		t.Fatalf("stale-seq IDR cleared pending: %v", p)
	}
	f.coord.OnIDR(4, 500) // 更旧 epoch
	if p := f.coord.Pending(); len(p) != 1 {
		t.Fatalf("older-epoch IDR cleared pending: %v", p)
	}
	f.coord.OnIDR(5, 100) // 恰同一条(重复观测)
	if p := f.coord.Pending(); len(p) != 1 {
		t.Fatalf("same IDR cleared pending: %v", p)
	}
	f.coord.OnIDR(5, 101) // 同 epoch 更新 seq → 清除
	if p := f.coord.Pending(); len(p) != 0 {
		t.Fatalf("matching-newer IDR did not clear pending: %v", p)
	}

	// 新周期:快照 (5,101);更新 epoch 直接清除。
	f.coord.Request("pli", false)
	f.coord.OnIDR(6, 1)
	if p := f.coord.Pending(); len(p) != 0 {
		t.Fatalf("newer-epoch IDR did not clear pending: %v", p)
	}
}

// TestKeyframeCoordinatorV1IdentityAnyIDRClears:v1 帧(身份全零)无从分辨
// 新旧 → 任意 IDR 清除(向后兼容:旧 host / 无身份 fake)。
func TestKeyframeCoordinatorV1IdentityAnyIDRClears(t *testing.T) {
	f := newFakeCoordinator(33 * time.Millisecond)
	f.coord.Request("overflow", false)
	f.coord.OnIDR(0, 0)
	if p := f.coord.Pending(); len(p) != 0 {
		t.Fatalf("v1 IDR did not clear pending: %v", p)
	}
}

// ---- Step 2:encoder_idr_timeout ----

// TestKeyframeCoordinatorTimeoutEmitsStateOnce:请求后 250ms(或两帧周期,
// 取大者)无匹配 IDR → 恰一次 encoder_idr_timeout(recoverable),pending
// 清空(周期结束,后续请求可重试);晚到 IDR 不再触发状态。
func TestKeyframeCoordinatorTimeoutEmitsStateOnce(t *testing.T) {
	f := newFakeCoordinator(33 * time.Millisecond) // 宽限 = max(250ms, 66ms) = 250ms
	f.coord.Request("pli", false)

	f.clk.advance(249 * time.Millisecond)
	if p := f.coord.Pending(); len(p) != 1 {
		t.Fatalf("pending expired early at 249ms: %v", p)
	}
	if codes := f.state.snapshot(); len(codes) != 0 {
		t.Fatalf("timeout emitted early: %v", codes)
	}

	f.clk.advance(2 * time.Millisecond) // 251ms > 250ms 宽限
	if p := f.coord.Pending(); len(p) != 0 {
		t.Fatalf("pending should expire unsatisfied: %v", p)
	}
	codes := f.state.snapshot()
	if len(codes) != 1 || codes[0] != "encoder_idr_timeout" {
		t.Fatalf("states = %v, want exactly [encoder_idr_timeout]", codes)
	}
	if !f.state.recovs[0] {
		t.Fatalf("encoder_idr_timeout must be recoverable")
	}

	// 不重复发;周期结束后新请求(冷却已过)可再次打 host。
	f.coord.Pending()
	if codes = f.state.snapshot(); len(codes) != 1 {
		t.Fatalf("timeout emitted more than once: %v", codes)
	}

	// 晚到 IDR(该周期已超时收尾,pending 空)不再触发状态,仅更新基准。
	f.coord.OnIDR(1, 42)
	if codes = f.state.snapshot(); len(codes) != 1 {
		t.Fatalf("late IDR re-emitted state: %v", codes)
	}

	f.coord.Request("pli", false) // 251ms ≥ 250ms 冷却 → 重试
	if reqs := f.sink.requests(); len(reqs) != 2 {
		t.Fatalf("retry after timeout should fire: %v", reqs)
	}
	// 第二周期宽限内到达的匹配 IDR 静默清除,无新状态。
	f.clk.advance(100 * time.Millisecond)
	f.coord.OnIDR(1, 43)
	if p := f.coord.Pending(); len(p) != 0 {
		t.Fatalf("second cycle not cleared by fresh IDR: %v", p)
	}
	if codes = f.state.snapshot(); len(codes) != 1 {
		t.Fatalf("in-grace IDR must not emit state: %v", codes)
	}
}

// TestKeyframeCoordinatorTimeoutScalesWithFramePeriod:低帧率流的两帧周期
// 超过 250ms 时,宽限按两帧周期放宽(1s 帧距 → 2s 宽限)。
func TestKeyframeCoordinatorTimeoutScalesWithFramePeriod(t *testing.T) {
	f := newFakeCoordinator(time.Second) // 宽限 = max(250ms, 2s) = 2s
	f.coord.Request("pli", false)

	f.clk.advance(1900 * time.Millisecond)
	if p := f.coord.Pending(); len(p) != 1 {
		t.Fatalf("pending expired before two frame periods: %v", p)
	}
	if codes := f.state.snapshot(); len(codes) != 0 {
		t.Fatalf("timeout emitted early at low fps: %v", codes)
	}
	f.clk.advance(101 * time.Millisecond) // 2001ms > 2s
	if p := f.coord.Pending(); len(p) != 0 {
		t.Fatalf("pending should expire at two frame periods: %v", p)
	}
	if codes := f.state.snapshot(); len(codes) != 1 || codes[0] != "encoder_idr_timeout" {
		t.Fatalf("states = %v, want [encoder_idr_timeout]", codes)
	}
}

// ---- sink 失败面 ----

// TestKeyframeCoordinatorSinkErrorKeptPending:host 请求失败仅记日志,协调器
// 状态不回滚(pending 保持;宽限到期自然超时收尾)。
func TestKeyframeCoordinatorSinkErrorKeptPending(t *testing.T) {
	f := newFakeCoordinator(33 * time.Millisecond)
	f.sink.err = errors.New("pipe down")
	f.coord.Request("pli", false)
	if reqs := f.sink.requests(); len(reqs) != 1 {
		t.Fatalf("sink not called: %v", reqs)
	}
	if p := f.coord.Pending(); len(p) != 1 || p[0] != "pli" {
		t.Fatalf("pending after sink error = %v, want [pli]", p)
	}
}

// ---- 并发安全(裁决 4)----

// TestKeyframeCoordinatorConcurrentAccess:多 goroutine 并发 Request/
// OnIDR/Pending 无死锁无 panic,Host 请求至少发生一次。
func TestKeyframeCoordinatorConcurrentAccess(t *testing.T) {
	f := &fakeCoordinator{clk: newManualClock(), sink: &fakeKeySink{}, state: &fakeStateSink{}}
	f.coord = newKeyframeCoordinator(KeyframeCoordinatorConfig{
		Sink: f.sink, OnState: f.state.onState, Now: f.clk.Now,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				f.coord.Request("pli", g%2 == 0)
				f.coord.OnIDR(1, uint64(i))
				_ = f.coord.Pending()
			}
		}(g)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("coordinator deadlocked under concurrent access")
	}
	if len(f.sink.requests()) == 0 {
		t.Fatal("no host request fired under concurrency")
	}
}

// ---- 常驻 watcher(真实时钟;生产路径)----

// TestKeyframeCoordinatorWatcherEmitsTimeout:start() 后无 IDR 时,watcher
// 在宽限到点自行发出 encoder_idr_timeout(不依赖下一次触点);Close 收线。
func TestKeyframeCoordinatorWatcherEmitsTimeout(t *testing.T) {
	f := &fakeCoordinator{clk: newManualClock(), sink: &fakeKeySink{}, state: &fakeStateSink{}}
	f.coord = newKeyframeCoordinator(KeyframeCoordinatorConfig{
		Sink: f.sink, OnState: f.state.onState,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	f.coord.start()
	defer f.coord.Close()
	f.coord.Request("pli", false)

	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if codes := f.state.snapshot(); len(codes) == 1 && codes[0] == "encoder_idr_timeout" {
				return // watcher 在无后续触点的情况下自行到期
			}
		case <-deadline:
			t.Fatalf("watcher never emitted timeout; states=%v", f.state.snapshot())
		}
	}
}

// ---- 帧汇出点观测(idrObservingSource)----

// TestIDRObservingSourceFeedsKeyFramesOnly:装饰器透传全部 Source 行为,
// 仅对 key 帧回调一次身份(codecEpoch/encodeSeq),delta 不回调。
func TestIDRObservingSourceFeedsKeyFramesOnly(t *testing.T) {
	src := newFakeSource()
	var mu sync.Mutex
	var obs [][2]uint64
	wrapped := idrObservingSource{Source: src, onIDR: func(epoch, seq uint64) {
		mu.Lock()
		obs = append(obs, [2]uint64{epoch, seq})
		mu.Unlock()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type res struct {
		f  Frame
		ok bool
	}
	out := make(chan res, 2)
	go func() {
		f, ok := wrapped.RecvFrame(ctx)
		out <- res{f, ok}
		f2, ok2 := wrapped.RecvFrame(ctx)
		out <- res{f2, ok2}
	}()
	src.frameCh <- Frame{Key: false, CodecEpoch: 9, EncodeSeq: 9, AU: []byte{1}}
	src.frameCh <- Frame{Key: true, CodecEpoch: 3, EncodeSeq: 7, AU: []byte{2}}
	r1, r2 := <-out, <-out
	if !r1.ok || !r2.ok {
		t.Fatalf("passthrough broken: %+v %+v", r1, r2)
	}
	if r1.f.Key || r1.f.CodecEpoch != 9 || !r2.f.Key || r2.f.CodecEpoch != 3 {
		t.Fatalf("frames not passed through unchanged: %+v %+v", r1.f, r2.f)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(obs) != 1 || obs[0] != [2]uint64{3, 7} {
		t.Fatalf("observations = %v, want exactly [(3,7)] (key frames only)", obs)
	}
}
