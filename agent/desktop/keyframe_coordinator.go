// keyframe_coordinator.go — M3 Task 2:KeyframeCoordinator(关键帧请求合并点)。
//
// 一个 desktop 会话(viewer)的全部关键帧请求源在此汇成一条到 host 的请求
// 面:RTCP PLI/FIR(qos_min.go 的 rtcpLoop 经 Publisher.OnKeyRequest)、
// 连接就绪(transport.go 的 Connected 钩子,reason="connect")、viewer 发送
// 器的 overflow/pacer/resume(viewer_sender.go 的 KeyRequest seam)。Task 3
// 的 QoS 动作将接入同一 API。
//
// 语义(裁决 2/3):
//   - Request(reason, urgent) 合并:常规请求受 250ms 冷却约束(冷却窗内的
//     并发请求并入 pending reason 集,不再打 host);urgent(新订阅 connect、
//     epoch 变更)绕过冷却,但绝不复制一条在途请求(pending 非空时只并入)。
//     250ms 是 Go 侧合并;native 的 kIdrMinIntervalMs=500 原样保留(裁决 3),
//     urgent native 路径(sub_join/rebuild)本就绕过它。
//   - OnIDR(codecEpoch, encodeSeq) 用已发布帧的身份(Task 1 透传)清
//     pending:仅当观测到的 IDR 相对请求快照「匹配或更新」(更新 epoch,或
//     同 epoch 更新 encodeSeq)——请求发出前已在途的过时 IDR 不清。帧汇出
//     点(session.go 的帧泵)经 idrObservingSource 喂养。
//   - Pending() 报告在途请求的 reason 集(排序快照)。
//   - 超时:请求发出后 max(250ms, 两帧周期) 内无匹配 IDR → 发一次
//     encoder_idr_timeout 状态(recoverable)并结束该周期(pending 清空,
//     后续请求可重新打 host)。观测触点(Request/OnIDR/Pending)惰性检查;
//     生产侧 start() 的常驻 watcher 在无触点时也按时发(deadline 驱动,
//     绝不忙等)。
//
// 线程安全:Request/OnIDR/Pending 可从任意 goroutine 并发调用(rtcpLoop、
// PC 状态回调、发送器回调、帧泵);回调(sink/OnState)一律在锁外触发。
package desktop

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// defaultKeyframeCooldown 是常规关键帧请求的合并窗口(裁决 2)。
const defaultKeyframeCooldown = 250 * time.Millisecond

// keyRequestReasonPacer 与 viewer_sender.go 入队门拒收路径的 fireKey
// ("pacer")同义 —— 发送器内部 pacer 循环的稳定 reason 串。
const keyRequestReasonPacer = "pacer"

// stateEncoderIDRTimeout 是「请求的关键帧未在宽限内产出」的稳定状态码
// (经 OnState 回调下发;viewer 对未知 state code 按既有词汇规则忽略)。
const stateEncoderIDRTimeout = "encoder_idr_timeout"

// keyframeSink 是 host 侧请求出口(Source 的子集;测试注入 fake)。
type keyframeSink interface {
	RequestKeyframe(reason string) error
}

// KeyframeCoordinatorConfig 组装协调器;Sink 必须非 nil。
type KeyframeCoordinatorConfig struct {
	// Sink 是底层请求出口(生产侧为会话的动态 Source)。
	Sink keyframeSink
	// Ctx 可选:常驻 watcher 的收线上下文(生产侧传会话 ctx——与帧/状态
	// 泵同一生命周期模型;Close 仍可显式收线,测试用)。
	Ctx context.Context
	// OnState 可选:状态回调(session 侧写 {"type":"state"} 帧)。
	OnState func(code string, recoverable bool)
	// FramePeriod 是标称帧距(fps 推导);IDR 宽限 = max(cooldown, 2×)。
	// 0 → 仅用 cooldown。
	FramePeriod time.Duration
	// Cooldown 覆盖常规合并窗口;0 → 250ms。
	Cooldown time.Duration
	// Now 是时钟注入点(默认 time.Now;单测注入 manualClock 确定性驱动
	// 冷却与宽限)。注入非真实时钟时勿 start() 常驻 watcher。
	Now func() time.Time
	Log *slog.Logger
}

// keyframeID 是一帧关键帧的身份对(codecEpoch, encodeSeq)——与请求快照
// 比对判定「匹配或更新」。全零 = 无身份(v1 帧),退化为任意 IDR 清除。
type keyframeID struct {
	epoch, seq uint64
}

func (id keyframeID) known() bool { return id.epoch != 0 || id.seq != 0 }

// satisfies 判定观测 IDR 是否满足请求快照 req:更新 epoch,或同 epoch 的
// 更新 encodeSeq。任一侧身份未知(v1)时无从分辨新旧 → 一律满足。
func (id keyframeID) satisfies(req keyframeID) bool {
	if !id.known() || !req.known() {
		return true
	}
	if id.epoch != req.epoch {
		return id.epoch > req.epoch
	}
	return id.seq > req.seq
}

// KeyframeCoordinator 见文件头。mu 之外字段构造后不可变。
type KeyframeCoordinator struct {
	log       *slog.Logger
	nowFn     func() time.Time
	sink      keyframeSink
	onState   func(code string, recoverable bool)
	cooldown  time.Duration
	twoFrames time.Duration // 两帧周期(0 = 不放宽)
	ctxDone   <-chan struct{}

	mu          sync.Mutex
	pending     map[string]struct{} // 在途请求周期并入的 reason 集
	lastRequest time.Time           // 最近一次实际打 host 的时刻
	lastPacer   time.Time           // 最近一次 pacer 请求进入在途周期的时刻(退避基准)
	requested   keyframeID          // 该周期的请求快照(打 host 时的 lastIDR)
	deadline    time.Time           // 该周期的 IDR 宽限
	timedOut    bool                // 该周期已发过 encoder_idr_timeout
	lastIDR     keyframeID          // 最近观测到的 IDR 身份
	started     bool

	notify chan struct{} // 唤醒 watcher 重整定时(容量 1,合并重复唤醒)
	done   chan struct{}
	closeO sync.Once
	wg     sync.WaitGroup
}

// newKeyframeCoordinator 建一个协调器(不启动常驻 watcher;生产侧随后
// start())。
func newKeyframeCoordinator(cfg KeyframeCoordinatorConfig) *KeyframeCoordinator {
	if cfg.Sink == nil {
		panic("desktop: KeyframeCoordinator requires Sink")
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	cooldown := cfg.Cooldown
	if cooldown <= 0 {
		cooldown = defaultKeyframeCooldown
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	var ctxDone <-chan struct{}
	if cfg.Ctx != nil {
		ctxDone = cfg.Ctx.Done()
	}
	return &KeyframeCoordinator{
		log:       log,
		nowFn:     nowFn,
		sink:      cfg.Sink,
		onState:   cfg.OnState,
		cooldown:  cooldown,
		twoFrames: 2 * cfg.FramePeriod,
		ctxDone:   ctxDone,
		pending:   make(map[string]struct{}),
		notify:    make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
}

// Request 合并一条关键帧请求(brief API)。urgent(新订阅/epoch 变更)绕过
// 冷却,但 pending 非空时只并入 reason 集(绝不复制在途请求)。
//
// pacer 专项退避(IDR 恢复死亡螺旋修正):常规 250ms 冷却为 PLI 设计;而
// 发送器 pacer 循环的撞门拒收可以在 host 尚未产出上一个请求的 IDR 时再次
// 触发 —— host 侧 kIdrMinIntervalMs=500 + 编码器深度令 pacer 请求的 IDR
// 物理上 ≥500ms 不可得(生产日志:同一秒内两条 client_reason=pacer 请求,
// 第二条只能排到 min_interval 边界被服务)。因此非 urgent 的 pacer 请求在
// 距上一条 pacer 请求 2×IDR 宽限(≥500ms)内不开启新周期(在途/排队中的
// 上一条已覆盖它);其余 reason(pli/fir/connect/overflow/resume)语义不变。
func (c *KeyframeCoordinator) Request(reason string, urgent bool) {
	if reason == "" {
		return
	}
	c.mu.Lock()
	now := c.nowFn()
	emit := c.expireLocked(now)
	fire := ""
	switch {
	case len(c.pending) > 0:
		c.pending[reason] = struct{}{} // 并入在途周期
		if reason == keyRequestReasonPacer {
			c.lastPacer = now
		}
	case reason == keyRequestReasonPacer && !urgent &&
		elapsed(now, c.lastPacer) < 2*c.grace():
		// pacer 退避窗内:丢弃(上一条 pacer 请求仍在 host 侧产出/排队,
		// 重发只会制造 encoder_idr_timeout 噪声;退避过后发送器的下一次
		// 转移仍会重新触发)。
	case urgent || elapsed(now, c.lastRequest) >= c.cooldown:
		c.pending[reason] = struct{}{}
		c.lastRequest = now
		if reason == keyRequestReasonPacer {
			c.lastPacer = now
		}
		c.requested = c.lastIDR
		c.deadline = now.Add(c.grace())
		c.timedOut = false
		fire = reason
	} // else: 冷却窗口内且无在途 → 丢弃(上游记账靠 pending 集)
	c.mu.Unlock()

	c.emitState(emit)
	if fire != "" {
		c.poke() // watcher:新周期有了 deadline
		if err := c.sink.RequestKeyframe(fire); err != nil {
			c.log.Warn("desktop keyframe request failed", "reason", fire, "err", err)
		} else {
			c.log.Debug("desktop keyframe request fired", "reason", fire)
		}
	}
}

// OnIDR 汇报一个已发布的 IDR 身份(帧汇出点喂养;Task 1 的身份透传)。
// 匹配/更新的 IDR 清空在途请求;过时 IDR 仅更新观测基准。
func (c *KeyframeCoordinator) OnIDR(codecEpoch, encodeSeq uint64) {
	c.mu.Lock()
	emit := c.expireLocked(c.nowFn())
	id := keyframeID{codecEpoch, encodeSeq}
	cleared := false
	if len(c.pending) > 0 && id.satisfies(c.requested) {
		c.pending = make(map[string]struct{})
		c.timedOut = false
		cleared = true
	}
	c.lastIDR = id
	c.mu.Unlock()

	if cleared {
		c.poke() // watcher:周期已满足,可回停车位
	}
	c.emitState(emit)
}

// Pending 返回在途请求的 reason 排序快照(空 = 无在途)。触点上顺带执行
// 惰性到期检查(与 watcher 同一入口)。
func (c *KeyframeCoordinator) Pending() []string {
	c.mu.Lock()
	emit := c.expireLocked(c.nowFn())
	out := make([]string, 0, len(c.pending))
	for r := range c.pending {
		out = append(out, r)
	}
	c.mu.Unlock()
	sort.Strings(out)
	c.emitState(emit)
	return out
}

// start 启动常驻 watcher(幂等;真实时钟专属——注入 manualClock 时勿启动,
// 单测经触点的惰性检查驱动同一到期入口)。
func (c *KeyframeCoordinator) start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return
	}
	c.started = true
	c.wg.Add(1) // 持 mu 计数:与 Close 的 Wait 严格有序
	go c.watchLoop()
}

// Close 停 watcher;幂等。
func (c *KeyframeCoordinator) Close() {
	c.closeO.Do(func() { close(c.done) })
	c.wg.Wait()
}

// ---- 内部 ----

// grace 是一个请求周期的 IDR 宽限:max(cooldown, 两帧周期)。低帧率流的
// IDR 物理上不可能在一个帧周期内出现,宽限按帧距放宽。
func (c *KeyframeCoordinator) grace() time.Duration {
	if c.twoFrames > c.cooldown {
		return c.twoFrames
	}
	return c.cooldown
}

// elapsed 计算 now-t(零值 t 得大间隔:首次必可打)。
func elapsed(now, t time.Time) time.Duration {
	if t.IsZero() {
		return time.Duration(1 << 62)
	}
	return now.Sub(t)
}

// expireLocked 是超时到期的统一入口:宽限已过且尚未发过 → 记一次
// encoder_idr_timeout 并结束该周期(pending 清空,后续请求可重试)。
// 返回待发出的状态(nil = 无);调用方须在锁外下发。调用方持 mu。
func (c *KeyframeCoordinator) expireLocked(now time.Time) []string {
	if len(c.pending) == 0 || c.timedOut || now.Before(c.deadline) {
		return nil
	}
	c.timedOut = true
	reasons := make([]string, 0, len(c.pending))
	for r := range c.pending {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	c.pending = make(map[string]struct{})
	return reasons
}

// emitState 在锁外下发一次超时状态(OnState 回调可写 WS;绝不持 mu 调用)。
func (c *KeyframeCoordinator) emitState(reasons []string) {
	if reasons == nil {
		return
	}
	c.log.Warn("desktop encoder idr timeout", "reasons", reasons)
	if c.onState != nil {
		c.onState(stateEncoderIDRTimeout, true)
	}
}

func (c *KeyframeCoordinator) poke() {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// watchLoop 是常驻 watcher:有在途周期时定时到 deadline,否则挂起在
// notify/done 上;绝不忙等。到期经 expireLocked 同一入口(真实时钟)。
func (c *KeyframeCoordinator) watchLoop() {
	defer c.wg.Done()
	for {
		c.mu.Lock()
		var timer *time.Timer
		var timerC <-chan time.Time
		if len(c.pending) > 0 && !c.timedOut {
			timer = time.NewTimer(time.Until(c.deadline)) // 真实时钟(见 start 约束)
			timerC = timer.C
		}
		notify, done, ctxDone := c.notify, c.done, c.ctxDone
		c.mu.Unlock()

		if timerC == nil {
			select {
			case <-notify:
			case <-done:
				return
			case <-ctxDone:
				return
			}
			continue
		}
		select {
		case <-timerC:
			c.mu.Lock()
			emit := c.expireLocked(time.Now())
			c.mu.Unlock()
			c.emitState(emit)
		case <-notify:
			timer.Stop()
		case <-done:
			timer.Stop()
			return
		case <-ctxDone:
			timer.Stop()
			return
		}
	}
}

// idrObservingSource 在帧汇出点(source → publisher 帧泵)喂养协调器的
// IDR 观测:透传全部 Source 行为,仅对 key 帧回调一次身份。session.go 以
// 它包裹帧泵的源,使 OnIDR 看到与发送面相同的帧流(连接就绪前的帧也在
// 观测内——请求快照基准需要它们)。
type idrObservingSource struct {
	Source
	onIDR func(codecEpoch, encodeSeq uint64)
}

func (s idrObservingSource) RecvFrame(ctx context.Context) (Frame, bool) {
	f, ok := s.Source.RecvFrame(ctx)
	if ok && f.Key && s.onIDR != nil {
		s.onIDR(f.CodecEpoch, f.EncodeSeq)
	}
	return f, ok
}
