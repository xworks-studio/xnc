// intent_test.go — 意图状态机单测(M2-Slice3 Task 2 验收):
//
//  1. 捕获死亡 → reattach 成功(fake core:首次 Start 源,死亡后第二次
//     Start 成功)→ 新源发布、代际递增、退避序列 1s;
//  2. core 持续不可用 → 退避序列 1s,2s,4s…cap 30s → 90s 窗口耗尽放弃;
//  3. server 会话关闭(ctx 取消)→ 立即放弃(不再尝试)。
//
// 时钟/睡眠注入:virtual clock 快进,无需真实等待。
package desktop

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// intentCore 是 fake core starter:脚本化 Start 结果序列。
type intentCore struct {
	mu      sync.Mutex
	results []func() (Source, error) // 每次 Start 弹一个;耗尽重复最后一个
	starts  int
	stops   int
}

func (c *intentCore) Start(_ context.Context, _ uint32) (Source, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.starts++
	i := c.starts - 1
	if i >= len(c.results) {
		i = len(c.results) - 1
	}
	return c.results[i]()
}

func (c *intentCore) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stops++
	return nil
}

func (c *intentCore) snapshot() (starts, stops int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.starts, c.stops
}

// virtualClock 快进睡眠并记账退避序列。
type virtualClock struct {
	mu      sync.Mutex
	slept   []time.Duration
	base    time.Time
	woke    chan struct{} // 每次快睡眠即通知(测试等待推进)
	graceTx time.Time     // 虚拟当前
}

func newVirtualClock() *virtualClock {
	return &virtualClock{base: time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC), woke: make(chan struct{}, 64)}
}

func (v *virtualClock) sleep(_ context.Context, d time.Duration) bool {
	v.mu.Lock()
	v.slept = append(v.slept, d)
	v.base = v.base.Add(d)
	v.mu.Unlock()
	select {
	case v.woke <- struct{}{}:
	default:
	}
	return true
}

func (v *virtualClock) now() time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.base
}

func (v *virtualClock) sleeps() []time.Duration {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]time.Duration(nil), v.slept...)
}

func (v *virtualClock) advance(d time.Duration) { // give-up 时钟推进
	v.mu.Lock()
	v.base = v.base.Add(d)
	v.mu.Unlock()
}

// newTestIntent 组装注入时钟的意图。
func newTestIntent(t *testing.T, ctx context.Context, core Starter, wts uint32) (*CaptureIntent, *virtualClock) {
	t.Helper()
	vc := newVirtualClock()
	it := newCaptureIntent(ctx, core, wts, slog.Default())
	it.now = vc.now
	it.sleep = vc.sleep
	it.grace = 90 * time.Second
	return it, vc
}

// waitStarts 轮询 core.Start 调用数达 n。
func waitStarts(t *testing.T, core *intentCore, n int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if s, _ := core.snapshot(); s >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("core.Start calls = %d, want >= %d", mustStarts(core), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// startInitial 模拟 Handle 的首次 Start(经 core;使 core.Start 计数与
// 结果序列对齐),随后 adopt。
func startInitial(t *testing.T, ctx context.Context, core Starter, wts uint32, it *CaptureIntent) Source {
	t.Helper()
	src, err := core.Start(ctx, wts)
	if err != nil {
		t.Fatalf("initial core.Start: %v", err)
	}
	it.adopt(src)
	return src
}

// TestIntentReattachAfterDeath — 捕获死亡 → 一次退避(1s)后重挂成功。
func TestIntentReattachAfterDeath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, second := newFakeSource(), newFakeSource()
	core := &intentCore{results: []func() (Source, error){
		func() (Source, error) { return first, nil },
		func() (Source, error) { return second, nil },
	}}
	it, vc := newTestIntent(t, ctx, core, 0)
	first = startInitial(t, ctx, core, 0, it).(*fakeSource)
	dyn := it.source()

	// 消费者视角:死亡前正常收帧。
	first.frameCh <- Frame{Key: true, PresentMonoUs: 1, AU: synthAU(true, 1)}
	if f, ok := dyn.RecvFrame(ctx); !ok || !f.Key {
		t.Fatalf("RecvFrame before death = %v %v", f, ok)
	}
	// 源死亡(frame 流终结)→ 意图上报 → 1s 退避 → 第二次 Start 成功。
	first.Close()
	got := make(chan Frame, 1)
	go func() {
		f, ok := dyn.RecvFrame(ctx)
		if ok {
			got <- f
		}
	}()
	waitStarts(t, core, 2, 5*time.Second)
	second.frameCh <- Frame{Key: true, PresentMonoUs: 2, AU: synthAU(true, 2)}
	select {
	case f := <-got:
		if !f.Key || f.PresentMonoUs != 2 {
			t.Fatalf("post-reattach frame = %+v", f)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no frame after reattach")
	}
	if sleeps := vc.sleeps(); len(sleeps) != 1 || sleeps[0] != time.Second {
		t.Fatalf("backoff sequence = %v, want [1s]", sleeps)
	}
	if s, st := core.snapshot(); s != 2 || st != 1 {
		t.Fatalf("starts=%d stops=%d, want 2/1 (每个 Start 一个 Stop 配对)", s, st)
	}
	// 第二代死亡也应触发新一轮(deadGen 去重只对同代)。
	it.close()
}

// TestIntentCoreDownBackoffAndGiveUp — core 持续失败:退避 1,2,4,8,16,30,
// 30…;90s 窗口耗尽 → 放弃(done 关闭,等待者解除)。
func TestIntentCoreDownBackoffAndGiveUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := newFakeSource()
	core := &intentCore{results: []func() (Source, error){
		func() (Source, error) { return first, nil },
		func() (Source, error) { return nil, errors.New("core down") },
	}}
	it, vc := newTestIntent(t, ctx, core, 0)
	startInitial(t, ctx, core, 0, it)
	dyn := it.source()

	first.Close()
	// 驱动:等待者阻塞,退避持续累计;累计睡眠超过 90s(1+2+4+8+16+30+30
	// = 91s)后下一次 now() 已过 deadline → 放弃。
	blocked := make(chan struct{})
	go func() {
		close(blocked)
		_, ok := dyn.RecvFrame(ctx)
		if ok {
			t.Error("RecvFrame must not succeed while core is down")
		}
	}()
	<-blocked
	// 等退避累计跨过 90s:7 次睡眠(1+2+4+8+16+30+30=91s)后第 7 次
	// Start 失败 → now(91s) > deadline(90s) → give-up。
	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(vc.sleeps()) >= 7 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("backoff sleeps = %v", vc.sleeps())
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitStarts(t, core, 7, 5*time.Second)
	select {
	case <-it.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("give-up not signaled; sleeps=%v starts=%d", vc.sleeps(), mustStarts(core))
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second}
	if g := vc.sleeps(); len(g) != len(want) {
		t.Fatalf("sleeps = %v, want exactly %v", g, want)
	}
	for i, d := range want {
		if got := vc.sleeps(); got[i] != d {
			t.Fatalf("sleep[%d] = %v, want %v (full: %v)", i, got[i], d, got)
		}
	}
	if s, st := core.snapshot(); s != 8 || st != 1 {
		t.Fatalf("starts=%d stops=%d, want 8/1 (initial + 7 attempts / initial source only)", s, st)
	}
}

func mustStarts(c *intentCore) int {
	s, _ := c.snapshot()
	return s
}

// TestIntentSessionClosedGivesUp — server 会话关闭(ctx 取消):死亡上报后
// 不再重试(sleep 返回 false 路径经真实 ctx)。
func TestIntentSessionClosedGivesUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	first := newFakeSource()
	core := &intentCore{results: []func() (Source, error){
		func() (Source, error) { return first, nil },
		func() (Source, error) { return nil, errors.New("core down") },
	}}
	it := newCaptureIntent(ctx, core, 0, slog.Default()) // 真实睡眠:ctx 取消必须打断
	it.grace = 90 * time.Second
	startInitial(t, ctx, core, 0, it)
	dyn := it.source()

	first.Close()
	done := make(chan struct{})
	go func() {
		_, _ = dyn.RecvFrame(ctx) // 阻塞于 waitNext
		close(done)
	}()
	// 给重试 goroutine 起跑时间(1s 退避睡眠中),随后取消会话。
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("waiter not released after session close")
	}
	time.Sleep(1500 * time.Millisecond) // 覆盖 1s 退避窗口
	if s, _ := core.snapshot(); s != 1 {
		t.Fatalf("core.Start calls = %d after session close, want 1 (no reattach)", s)
	}
}

// TestIntentClosePairsStartStop — 会话正常收线:初始 Start 恰由 close 结清。
func TestIntentClosePairsStartStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := newFakeSource()
	core := &intentCore{results: []func() (Source, error){
		func() (Source, error) { return first, nil },
	}}
	it, _ := newTestIntent(t, ctx, core, 0)
	startInitial(t, ctx, core, 0, it)
	it.close()
	if s, st := core.snapshot(); s != 1 || st != 1 {
		t.Fatalf("starts=%d stops=%d, want 1/1", s, st)
	}
	if !first.isClosed() {
		t.Fatalf("source not closed on intent close")
	}
}
