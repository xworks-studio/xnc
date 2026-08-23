// intent.go — 会话意图自愈(M2-Slice3 Task 2)。
//
// 「desktop 意图」= 本逻辑会话希望有一个活的桌面采集。桌面子进程死亡
// (logoff 杀 session/desktoppipe Done/Err)、core RPC 失败(pipe 重连)、
// WTS 会话变更 —— 全部表现为当前 Source 终结(Recv* 返回 false)。意图
// 监督者在「逻辑会话仍存活」期间(判据 = 会话 ctx 未取消,即 server 会话
// WS 仍在;另设 90s 重试窗口)自动 re-StartCapture:
//
//	退避 1s,2s,4s… 上限 30s;每次尝试记日志 reattach_attempt n reason=…
//	成功 → 发布新 Source(代际 gen++),全部消费者(帧/光标/状态/输入泵)
//	  经 intentSource 透明切到新源 —— WebRTC PC 不重建,viewer 无感续流;
//	放弃 → 会话 ctx 取消(server 逻辑会话关闭)或 90s 窗口耗尽,记
//	  reattach_gave_up 并解除全部等待者。
//
// logoff→logon 全链:logoff 杀死桌面子进程 → 意图重试;console 会话仍在
// (登录 UI 也是活动 console 会话,core StartCapture 校验放行)→ spawn 进
// 登录 UI 会话 → viewer 看见登录界面 → 登录后意图对新会话的源继续供帧。
// 每个 Start 与一个 Stop 配对(源死亡时即配对结清;close 收尾最后一个)。
package desktop

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	// intentGrace 是一次源死亡后的重试窗口(放弃上限)。
	intentGrace = 90 * time.Second
	// intentBackoffCap 是重试退避上限(1s,2s,4s…cap 30s)。
	intentBackoffCap = 30 * time.Second
)

// CaptureIntent 是一个逻辑会话的采集意图监督者。非重入安全:单会话单实例。
type CaptureIntent struct {
	ctx     context.Context
	starter Starter
	wts     uint32
	log     *slog.Logger

	// 可注入(确定性单测):时钟、退避睡眠、窗口。
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) bool
	grace time.Duration
	cap   time.Duration

	mu      sync.Mutex
	src     Source
	gen     uint64        // 当前源代际(从 1 起)
	deadGen uint64        // 已报告死亡的代际(去重:每代至多触发一轮重试)
	ready   chan struct{} // 每次发布关闭并更换(等待者被唤醒)
	done    chan struct{} // 给 up(give-up/close):等待者解除阻塞
	closed  bool

	// 回调(持 mu 读):发布新源 / 放弃重试。Handle 注入写 viewer 信令帧。
	onPublish func(Source)
	onGiveUp  func()
}

// newCaptureIntent 建意图监督者;随后 adopt 初始源(Handle 的首次 Start)。
func newCaptureIntent(ctx context.Context, starter Starter, wts uint32, log *slog.Logger) *CaptureIntent {
	if log == nil {
		log = slog.Default()
	}
	return &CaptureIntent{
		ctx: ctx, starter: starter, wts: wts, log: log,
		now:   time.Now,
		sleep: intentSleep,
		grace: intentGrace,
		cap:   intentBackoffCap,
		ready: make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// intentSleep 是默认退避睡眠(ctx 取消返回 false)。
func intentSleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// adopt 发布初始源(首次 Start 成功);gen=1。
func (it *CaptureIntent) adopt(src Source) {
	it.mu.Lock()
	defer it.mu.Unlock()
	it.publishLocked(src)
}

// publishLocked 发布新源并唤醒等待者;调用方持 mu。
func (it *CaptureIntent) publishLocked(src Source) {
	it.src = src
	it.gen++
	old := it.ready
	it.ready = make(chan struct{})
	close(old)
}

// source 返回透明跟随意图的 Source(全部 Recv* 在源终结时自动上报死亡
// 并阻塞至新源;SendInput/SubID/Hello/RequestKeyframe 委派当前源)。
func (it *CaptureIntent) source() Source { return intentSource{it: it} }

// current 返回 (当前源, 代际);源可为 nil(放弃后)。
func (it *CaptureIntent) current() (Source, uint64) {
	it.mu.Lock()
	defer it.mu.Unlock()
	return it.src, it.gen
}

// waitNext 阻塞至出现代际大于 lastGen 的新源;false = 会话结束/放弃。
func (it *CaptureIntent) waitNext(ctx context.Context, lastGen uint64) bool {
	for {
		it.mu.Lock()
		gen, ready := it.gen, it.ready
		it.mu.Unlock()
		if gen > lastGen {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-it.done:
			return false
		case <-ready:
		}
	}
}

// reportDead 上报第 gen 代源死亡(reason 进日志);每代至多触发一轮重试。
// 同时结清该源的 Start/Stop 配对并 Close 旧源。
func (it *CaptureIntent) reportDead(gen uint64, reason string) {
	it.mu.Lock()
	if it.closed || it.gen != gen || it.deadGen >= gen || it.src == nil {
		it.mu.Unlock()
		return
	}
	it.deadGen = gen
	old := it.src
	it.mu.Unlock()

	_ = old.Close()
	if err := it.starter.Stop(); err != nil {
		it.log.Warn("desktop intent: stop for dead source failed", "err", err)
	}
	if it.ctx.Err() != nil {
		return // 会话已收线:不再重试(close 随后置 closed)
	}
	go it.reattachLoop(reason)
}

// reattachLoop 退避重试 re-StartCapture(1s,2s,4s…cap;90s 窗口放弃)。
func (it *CaptureIntent) reattachLoop(reason string) {
	attempt := 0
	deadline := it.now().Add(it.grace)
	for {
		d := time.Second << uint(attempt) // 1s,2s,4s…
		if d > it.cap {
			d = it.cap
		}
		if !it.sleep(it.ctx, d) {
			return // 会话 ctx 取消
		}
		it.mu.Lock()
		closed := it.closed
		it.mu.Unlock()
		if closed {
			return
		}
		attempt++
		it.log.Info("reattach_attempt", "n", attempt, "reason", reason)
		src, err := it.starter.Start(it.ctx, it.wts)
		if err == nil {
			it.mu.Lock()
			it.publishLocked(src)
			cb := it.onPublish
			gen := it.gen
			it.mu.Unlock()
			it.log.Info("desktop capture reattached", "attempt", attempt, "gen", gen)
			if cb != nil {
				cb(src)
			}
			return
		}
		it.log.Warn("reattach failed", "n", attempt, "reason", reason, "err", err)
		if it.now().After(deadline) {
			it.mu.Lock()
			if !it.closed {
				it.closed = true
				close(it.done)
			}
			cb := it.onGiveUp
			it.mu.Unlock()
			it.log.Warn("reattach_gave_up", "attempts", attempt, "reason", reason)
			if cb != nil {
				cb()
			}
			return
		}
	}
}

// close 是会话终结路径:关闭当前源 + 结清最后一个 Start/Stop 配对,
// 解除全部等待者。幂等。
func (it *CaptureIntent) close() {
	it.mu.Lock()
	if it.closed {
		it.mu.Unlock()
		return
	}
	it.closed = true
	src := it.src
	close(it.done)
	it.mu.Unlock()
	if src != nil {
		_ = src.Close()
	}
	_ = it.starter.Stop()
}

// intentSource 把意图暴露成 Source:死亡自动上报 + 阻塞等待新源,
// 重挂对全部消费者透明(见文件头)。
type intentSource struct {
	it *CaptureIntent
}

func (s intentSource) RecvFrame(ctx context.Context) (Frame, bool) {
	for {
		src, gen := s.it.current()
		if src == nil {
			return Frame{}, false
		}
		f, ok := src.RecvFrame(ctx)
		if ok {
			return f, true
		}
		if ctx.Err() != nil {
			return Frame{}, false
		}
		s.it.reportDead(gen, "frame_stream_end")
		if !s.it.waitNext(ctx, gen) {
			return Frame{}, false
		}
	}
}

func (s intentSource) RecvState(ctx context.Context) (StateEvent, bool) {
	for {
		src, gen := s.it.current()
		if src == nil {
			return StateEvent{}, false
		}
		ev, ok := src.RecvState(ctx)
		if ok {
			return ev, true
		}
		if ctx.Err() != nil {
			return StateEvent{}, false
		}
		s.it.reportDead(gen, "state_stream_end")
		if !s.it.waitNext(ctx, gen) {
			return StateEvent{}, false
		}
	}
}

func (s intentSource) RecvCursor(ctx context.Context) (CursorEvent, bool) {
	for {
		src, gen := s.it.current()
		if src == nil {
			return CursorEvent{}, false
		}
		ev, ok := src.RecvCursor(ctx)
		if ok {
			return ev, true
		}
		if ctx.Err() != nil {
			return CursorEvent{}, false
		}
		s.it.reportDead(gen, "cursor_stream_end")
		if !s.it.waitNext(ctx, gen) {
			return CursorEvent{}, false
		}
	}
}

func (s intentSource) RecvDisplay(ctx context.Context) (DisplayChangedEvent, bool) {
	for {
		src, gen := s.it.current()
		if src == nil {
			return DisplayChangedEvent{}, false
		}
		ev, ok := src.RecvDisplay(ctx)
		if ok {
			return ev, true
		}
		if ctx.Err() != nil {
			return DisplayChangedEvent{}, false
		}
		s.it.reportDead(gen, "display_stream_end")
		if !s.it.waitNext(ctx, gen) {
			return DisplayChangedEvent{}, false
		}
	}
}

func (s intentSource) cur() Source {
	src, _ := s.it.current()
	return src
}

func (s intentSource) Hello() *HelloInfo {
	if src := s.cur(); src != nil {
		return src.Hello()
	}
	return nil
}

func (s intentSource) RequestKeyframe(reason string) error {
	if src := s.cur(); src != nil {
		return src.RequestKeyframe(reason)
	}
	return context.Canceled
}

func (s intentSource) SubID() uint32 {
	if src := s.cur(); src != nil {
		return src.SubID()
	}
	return 0
}

func (s intentSource) SendInput(payload []byte) error {
	if src := s.cur(); src != nil {
		return src.SendInput(payload)
	}
	return context.Canceled
}

func (s intentSource) Close() error { return nil } // 生命周期归 CaptureIntent 管
