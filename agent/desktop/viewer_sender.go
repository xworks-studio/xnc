// viewer_sender.go — M3 Task 1:ViewerSender(每 viewer 一个发送器)。
//
// 一个 Publisher(viewer 会话)拥有一个 ViewerSender;发送器拥有信令面
// 以下的全部发送状态:90kHz RTP 时钟(mono→ticks 显式映射,严格拒绝
// 回退)、H264 分包器(MTU 1200,独立序列空间)、单帧时限队列、令牌桶
// pacing 与 WAIT_IDR 状态机。Publisher 保留 PeerConnection/track/信令面,
// WriteFrame 委派 Enqueue(session.go 零改动)。
//
// TWCC/SSRC 平价说明(与重构前的 Publisher 完全一致):transport-cc 出向
// 头扩展由 publisher PC 的拦截器链(newDesktopAPI →
// ConfigureTWCCHeaderExtensionSender)对本发送器写入 track 的每个 RTP 包
// 盖章;SSRC/payload-type 由 TrackLocalStaticRTP.writeRTP 按 binding 逐包
// 重写——因此这里的 packetizer 常量(pt=102/ssrc=0)是惰性的,per-viewer
// SSRC 实际来自 binding,per-viewer 序列空间来自 packetizer 的 sequencer。
//
// 队列语义(全局约束:发送侧排队目标 50ms、硬上限 100ms):
//   - 队列至多容纳一帧的包;新帧入队时旧帧余包整帧冲刷(立即写出),
//     绝不出现两帧叠压(有界内存)。
//   - 入队门:最后一包的 plan 发送时刻超过入队时刻 + maxQueueAge 的帧
//     整帧不入队(无任何部分包发出),并进入 waitIDR —— 抑制只丢不采样,
//     绝不「丢一帧 P 帧后继续发后续 P 帧」。
//   - 队列年龄(实际时刻)超过 maxQueueAge:冲刷在队包(立即写出以完成
//     在途帧,避免撕裂帧)、清空队列/重传状态、进入 waitIDR,并恰一次
//     触发合并关键帧回调。
//
// 合并关键帧回调是 Task 2(KeyframeCoordinator)将接管的 seam:溢出
// ("overflow")、pacer 拒收("pacer")、恢复("resume")与 Publisher 侧的
// connect/pli/fir 全部经同一回调汇出。
package desktop

import (
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

const (
	// rtpMTU 与重构前 Publisher 的 packetizer 一致(1200B RTP 载荷上限)。
	rtpMTU = 1200
	// maxQueueAge 是发送侧排队的硬上限(全局约束:目标 50ms/硬上限
	// 100ms)。超过即冲刷 + waitIDR + 恰一次合并关键帧请求。
	maxQueueAge = 100 * time.Millisecond
	// pacingBudgetFraction:令牌桶速率 = 当前预算的 85%(15% 余量留给
	// 重传等带外流量,同时把帧内突发铺开成平稳速率)。
	pacingBudgetFraction = 0.85
	// pacingBurstBytes 是令牌桶容量(3×MTU):每帧首批包立即送出,余下
	// 按速率铺开——首包延迟为零,整帧有界。
	pacingBurstBytes = 3 * rtpMTU
	// defaultPacingBudgetBps 是预算缺省值(bits/s;BudgetBps=0 时生效)。
	// 拥塞控制(后续任务)将按 TWCC 反馈驱动预算。
	defaultPacingBudgetBps = 20_000_000
)

// viewerSendState 是 ViewerSender 的发送状态机。
type viewerSendState uint8

const (
	// stateWaitIDR:等待本 viewer 可解码起点的 IDR(初态、溢出/不连续/
	// 恢复之后)。期间 delta 一律抑制。
	stateWaitIDR viewerSendState = iota
	// stateLive:正常发送(delta 与 IDR 均出)。
	stateLive
	// statePaused:暂停(Pause):帧丢弃,不触发关键帧请求。
	statePaused
	// stateClosed:已关闭:帧丢弃,Close 幂等。
	stateClosed
)

func (s viewerSendState) String() string {
	switch s {
	case stateWaitIDR:
		return "wait_idr"
	case stateLive:
		return "live"
	case statePaused:
		return "paused"
	case stateClosed:
		return "closed"
	}
	return "unknown"
}

// frameEpoch 是 v2 身份的代际对(capture 重建 / 编码器重置)。全零 =
// 无身份(v1 帧/未指明),epoch 门控退化为纯关键帧门控。
type frameEpoch struct {
	capture, codec uint64
}

func (e frameEpoch) known() bool { return e.capture != 0 || e.codec != 0 }

// ViewerSenderConfig 组装一个发送器;WritePacket 必须非 nil。
type ViewerSenderConfig struct {
	// WritePacket 是 RTP 包出口(Publisher 注入 track.WriteRTP;测试注入
	// fake sink)。须线程安全(Enqueue 与 pace 泵并发调用)。
	WritePacket func(*rtp.Packet) error
	// KeyRequest 是合并关键帧回调(可为 nil:测试/无源拓扑)。reason 为
	// 稳定串(overflow/pacer/resume),上游(host)记账去重。
	KeyRequest func(reason string)
	// BudgetBps 是当前 pacing 预算(bits/s;0 → defaultPacingBudgetBps)。
	BudgetBps int
	// Now 是时钟注入点(默认 time.Now;单测注入 manualClock 确定性驱动
	// 队列年龄与令牌桶截止)。注入非真实时钟时勿 start() 常驻泵。
	Now func() time.Time
	Log *slog.Logger
}

// ViewerSender 拥有一个 viewer 的全部发送侧状态(见文件头)。mu 之外的
// 字段构造后不可变。
type ViewerSender struct {
	log      *slog.Logger
	nowFn    func() time.Time
	writePkt func(*rtp.Packet) error
	keyReq   func(reason string)

	clock      *rtpClock // 90kHz 显式映射(mono→ticks,严格拒绝回退)
	packetizer rtp.Packetizer
	bucket     tokenBucket
	interval   frameIntervalStats

	mu            sync.Mutex
	state         viewerSendState
	epoch         frameEpoch // live 期间正在发送的帧代际
	pendingEpoch  frameEpoch // Discontinuity 后等待的代际(0 = 不校验)
	queue         []queuedPacket
	queueEnqueued time.Time // 当前在队帧的入队时刻(年龄判据)
	queueBytes    int       // 在队帧的 AU 字节累计
	stats         viewerStatsN
	pumpStarted   bool

	notify chan struct{} // 唤醒 pace 泵(容量 1,合并重复唤醒)
	done   chan struct{}
	closeO sync.Once
	wg     sync.WaitGroup
}

// queuedPacket 是一个已排队 RTP 包:计划的发送时刻 + 帧边界记账。
type queuedPacket struct {
	pkt      *rtp.Packet
	sendAt   time.Time
	frameEnd bool // 本帧最后一包
	auBytes  int  // 本帧 AU 字节数(仅 frameEnd 有效)
}

// viewerStatsN 是 mu 保护下的计数器集(Stats 快照用)。
type viewerStatsN struct {
	framesSent      uint64 // 完整送出的帧数
	packetsSent     uint64
	bytesSent       uint64 // 送出帧的 AU 字节累计
	preKeyDropped   uint64 // waitIDR 期间抑制的 delta
	epochDropped    uint64 // 旧/异代 epoch 抑制的帧
	pausedDropped   uint64
	closedDropped   uint64
	deadlineDropped uint64 // 截止超 100ms 未入队的帧
	overflowFlushes uint64 // 队列年龄超限冲刷次数
	keyRequests     uint64 // 合并关键帧请求触发次数
}

// ViewerStats 是 Stats() 的快照形态。
type ViewerStats struct {
	State           string
	FramesSent      uint64
	PacketsSent     uint64
	BytesSent       uint64
	PreKeyDropped   uint64
	EpochDropped    uint64
	PausedDropped   uint64
	ClosedDropped   uint64
	DeadlineDropped uint64
	OverflowFlushes uint64
	KeyRequests     uint64
	QueuePackets    int
	QueueBytes      int
}

// tokenBucket 是 per-viewer 令牌桶:速率恒为预算的 85%,容量 pacingBurstBytes。
// 债务语义:reserve 可把 tokens 打为负(按赤字折算截止时刻),refill 按
// 耗时偿还并对容量封顶。
type tokenBucket struct {
	rate   float64 // bytes/s(已含 85% 折扣)
	burst  float64 // 容量 bytes
	tokens float64
	last   time.Time
}

func newTokenBucket(budgetBps int, now time.Time) tokenBucket {
	rate := float64(budgetBps) / 8 * pacingBudgetFraction
	if rate <= 0 {
		rate = float64(defaultPacingBudgetBps) / 8 * pacingBudgetFraction
	}
	return tokenBucket{rate: rate, burst: pacingBurstBytes, tokens: pacingBurstBytes, last: now}
}

func (b *tokenBucket) refill(now time.Time) {
	if d := now.Sub(b.last); d > 0 {
		b.tokens += float64(d) * b.rate / float64(time.Second)
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}
}

// reserve 为一帧的全部包计算计划发送时刻。若最后一包的截止超过
// now+maxQueueAge,则整帧拒收(不消耗任何令牌)——「超过 100ms 的包
// 不入队」的入队门。sizes 为各包的线缆字节数(MarshalSize)。
func (b *tokenBucket) reserve(sizes []int, now time.Time) (deadlines []time.Time, ok bool) {
	b.refill(now)
	deadlines = make([]time.Time, len(sizes))
	tokens := b.tokens
	for i, n := range sizes {
		if deficit := float64(n) - tokens; deficit > 0 {
			deadlines[i] = now.Add(time.Duration(deficit / b.rate * float64(time.Second)))
		} else {
			deadlines[i] = now
		}
		tokens -= float64(n)
	}
	if last := deadlines[len(deadlines)-1]; last.Sub(now) > maxQueueAge {
		return nil, false
	}
	b.tokens = tokens
	return deadlines, true
}

// newViewerSender 建一个发送器(不启动常驻泵;Publisher 随后 start())。
func newViewerSender(cfg ViewerSenderConfig) (*ViewerSender, error) {
	if cfg.WritePacket == nil {
		return nil, errors.New("desktop: ViewerSender requires WritePacket")
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &ViewerSender{
		log:      log,
		nowFn:    nowFn,
		writePkt: cfg.WritePacket,
		keyReq:   cfg.KeyRequest,
		// 惰性常量:pt/ssrc 会被 TrackLocalStaticRTP 按 binding 重写
		// (见文件头 TWCC/SSRC 平价说明);序列空间自此私有。
		clock:      newRTPClock(rand.Uint32()),
		packetizer: rtp.NewPacketizer(rtpMTU, 102, 0, &codecs.H264Payloader{}, rtp.NewRandomSequencer(), 90000),
		bucket:     newTokenBucket(cfg.BudgetBps, nowFn()),
		state:      stateWaitIDR,
		notify:     make(chan struct{}, 1),
		done:       make(chan struct{}),
	}, nil
}

// start 启动常驻 pace 泵(幂等;注入非真实时钟时勿调用)。
func (s *ViewerSender) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pumpStarted {
		return
	}
	s.pumpStarted = true
	s.wg.Add(1) // 持 mu 计数:与 Close 的 Wait 严格有序
	go s.paceLoop()
}

// Enqueue 接收一帧(Annex-B AU;入队后不得改动 AU 字节——分包器会拷贝
// 载荷,包对象在送出前归发送器所有)。状态机门控 → 分包 → 入队门 →
// 立即送出到期包,余下交给 pace 泵。返回错误仅限致命路径(RTP 时戳回退
// /分包器产出空/写出口失败),与旧 Publisher.WriteFrame 契约一致。
func (s *ViewerSender) Enqueue(f Frame) error {
	if len(f.AU) == 0 {
		return nil
	}
	s.mu.Lock()
	now := s.nowFn()
	// 入口 drain 的写失败同样是发送面死亡:上抛(帧泵据此收线)+ 已转
	// closed(Task 2 carry:此前被静默吞掉,泵会继续投喂死发送器)。
	keyReason, derr := s.drainLocked(now)
	if derr != nil {
		s.mu.Unlock()
		s.fireKey(keyReason)
		return derr
	}

	fe := frameEpoch{f.CaptureEpoch, f.CodecEpoch}
	// 自检出不连续(0x020B 丢失的兜底):live 期间帧自带 epoch 前进,
	// 旧参考链即刻作废,等待新 epoch 的 IDR(与 Discontinuity 同一转移,
	// 不触发关键帧请求——host 重建后的首帧即 IDR)。
	if s.state == stateLive && fe.known() && s.epoch.known() && fe != s.epoch {
		s.discontinuityLocked(fe)
	}

	proceed := false
	switch s.state {
	case stateClosed:
		s.stats.closedDropped++
	case statePaused:
		s.stats.pausedDropped++
	case stateWaitIDR:
		switch {
		case !s.epochAcceptsLocked(fe):
			s.stats.epochDropped++
		case !f.Key:
			s.stats.preKeyDropped++
		default:
			s.epoch, s.pendingEpoch = fe, frameEpoch{}
			s.state = stateLive
			proceed = true
		}
	case stateLive:
		proceed = true
	}
	if !proceed {
		s.mu.Unlock()
		s.fireKey(keyReason)
		return nil
	}

	// 90kHz 时戳(严格单调)。
	hadPrev := s.clock.started
	prevMono := s.clock.last
	ts, err := s.clock.Timestamp(f.PresentMonoUs)
	if err != nil {
		s.mu.Unlock()
		s.fireKey(keyReason)
		return err
	}
	pkts := s.packetizer.Packetize(f.AU, 0)
	if len(pkts) == 0 {
		s.mu.Unlock()
		s.fireKey(keyReason)
		return errors.New("desktop: H264 payloader produced no RTP packets")
	}
	sizes := make([]int, len(pkts))
	for i, p := range pkts {
		p.Timestamp = ts
		p.Marker = i == len(pkts)-1
		sizes[i] = p.MarshalSize()
	}

	// 单帧队列:上一帧余包(若已超龄则 drainLocked 已冲刷并转入
	// waitIDR,本帧不会走到这里)此刻整帧冲刷出让队列。
	if len(s.queue) > 0 {
		_ = s.flushQueueLocked("superseded")
	}

	// 入队门:最后一包截止超 maxQueueAge → 整帧不入队。抑制只丢不采样:
	// 进入 waitIDR 并恰一次合并请求,绝无「丢 P 帧后继续发后续 P 帧」。
	deadlines, ok := s.bucket.reserve(sizes, now)
	if !ok {
		s.stats.deadlineDropped++
		s.state = stateWaitIDR
		if keyReason == "" {
			keyReason = "pacer"
		}
		s.mu.Unlock()
		s.fireKey(keyReason)
		return nil
	}
	for i := range pkts {
		s.queue = append(s.queue, queuedPacket{
			pkt:      pkts[i],
			sendAt:   deadlines[i],
			frameEnd: i == len(pkts)-1,
			auBytes:  len(f.AU),
		})
	}
	if len(s.queue) == len(pkts) {
		s.queueEnqueued = now
	}
	s.queueBytes += len(f.AU)

	if hadPrev {
		if d := f.PresentMonoUs - prevMono; d <= 1_000_000 {
			s.interval.add(time.Duration(d)*time.Microsecond, s.log)
		}
	}
	// 立即送出 burst 容量内的到期包;余下交给 pace 泵。
	keyReason2, err2 := s.drainLocked(now)
	if keyReason == "" {
		keyReason = keyReason2
	}
	s.mu.Unlock()
	s.fireKey(keyReason)
	s.poke()
	return err2
}

// Discontinuity 镜像 desktoppipe 0x020B(M1 Task 4 的 wire 侧):epoch
// 前进(capture 重建/编码器重置)。丢弃在队包(旧代数据无效,不发)、
// 清空队列状态、进入 waitIDR 等待所携 epoch 的首个 IDR;旧 epoch 帧自此
// 全部抑制。不触发关键帧请求(host 重建后的首个帧即 IDR)。
func (s *ViewerSender) Discontinuity(captureEpoch, codecEpoch uint64) {
	s.mu.Lock()
	s.discontinuityLocked(frameEpoch{captureEpoch, codecEpoch})
	s.mu.Unlock()
}

// Pause 暂停发送:新帧丢弃(计数),不触发关键帧请求;在途帧的余包
// 仍会按节奏送完(队列不弃)。幂等。
func (s *ViewerSender) Pause() {
	s.mu.Lock()
	if s.state == stateLive || s.state == stateWaitIDR {
		s.state = statePaused
	}
	s.mu.Unlock()
}

// Resume 恢复发送:进入 waitIDR(暂停期间的增量已被抑制,恢复须以新
// IDR 重建参考链)并恰一次合并关键帧请求加速恢复;幂等。
func (s *ViewerSender) Resume() {
	s.mu.Lock()
	resumed := false
	if s.state == statePaused {
		s.state = stateWaitIDR
		resumed = true
	}
	s.mu.Unlock()
	if resumed {
		s.fireKey("resume")
	}
}

// Stats 返回发送器统计快照(计数器均为累计值)。
func (s *ViewerSender) Stats() ViewerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ViewerStats{
		State:           s.state.String(),
		FramesSent:      s.stats.framesSent,
		PacketsSent:     s.stats.packetsSent,
		BytesSent:       s.stats.bytesSent,
		PreKeyDropped:   s.stats.preKeyDropped,
		EpochDropped:    s.stats.epochDropped,
		PausedDropped:   s.stats.pausedDropped,
		ClosedDropped:   s.stats.closedDropped,
		DeadlineDropped: s.stats.deadlineDropped,
		OverflowFlushes: s.stats.overflowFlushes,
		KeyRequests:     s.stats.keyRequests,
		QueuePackets:    len(s.queue),
		QueueBytes:      s.queueBytes,
	}
}

// Close 关闭发送器:丢队列、停泵、等泵退出;幂等。
func (s *ViewerSender) Close() {
	s.mu.Lock()
	s.shutdownLocked()
	s.mu.Unlock()
	s.wg.Wait()
}

// ---- 内部 ----

// stateSnapshot 是同包测试观测点。
func (s *ViewerSender) stateSnapshot() viewerSendState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// fireKey 在锁外触发合并关键帧回调(reason 为空不触发)。回调可能下行
// 到 pipe(RequestKeyframe),绝不在持 mu 时调用。
func (s *ViewerSender) fireKey(reason string) {
	if reason == "" {
		return
	}
	s.mu.Lock()
	s.stats.keyRequests++
	s.mu.Unlock()
	if s.keyReq != nil {
		s.keyReq(reason)
	}
}

func (s *ViewerSender) poke() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// epochAcceptsLocked 判定 fe 是否为当前等待的代际:pendingEpoch 未知或
// fe 无身份(v1)时退化为纯关键帧门控。
func (s *ViewerSender) epochAcceptsLocked(fe frameEpoch) bool {
	if !s.pendingEpoch.known() || !fe.known() {
		return true
	}
	return fe == s.pendingEpoch
}

// discontinuityLocked 执行 epoch 不连续转移(丢队列 + waitIDR +
// pendingEpoch=fe);不触发关键帧请求。closed 时不转移。
func (s *ViewerSender) discontinuityLocked(fe frameEpoch) {
	if s.state == stateClosed {
		return
	}
	s.dropQueueLocked()
	s.pendingEpoch = fe
	s.state = stateWaitIDR
}

// dropQueueLocked 丢弃全部在队包(旧代数据无效)。
func (s *ViewerSender) dropQueueLocked() {
	s.queue = nil
	s.queueBytes = 0
}

// flushQueueLocked 把在队包立即写出(非 paced 冲刷)并清空队列;调用方
// 持 mu。返回首个写错误(仍尽最大努力写出余下包)。
func (s *ViewerSender) flushQueueLocked(cause string) error {
	var firstErr error
	for i := range s.queue {
		if err := s.writeLocked(s.queue[i]); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.dropQueueLocked()
	if firstErr != nil {
		s.log.Warn("desktop viewer flush failed", "cause", cause, "err", firstErr)
	}
	return firstErr
}

// ageOverflowLocked 是「队列年龄超限」的统一转移(drainLocked 的检测点
// 与单测直接驱动同一入口):冲刷在队包(立即写出以完成在途帧)、清空
// 队列/重传状态、进入 waitIDR。返回应触发的合并关键帧 reason(仅
// live→waitIDR 的转移触发;已在 waitIDR 时返回 ""——等待中的请求已合并)。
func (s *ViewerSender) ageOverflowLocked(now time.Time) string {
	_ = now // 冲刷即立即写出;now 仅用于日志/一致性扩展
	if s.state != stateLive {
		_ = s.flushQueueLocked("overflow")
		return ""
	}
	s.stats.overflowFlushes++
	_ = s.flushQueueLocked("overflow")
	s.state = stateWaitIDR
	return "overflow"
}

// writeLocked 写出一个包并结算统计;调用方持 mu。
func (s *ViewerSender) writeLocked(q queuedPacket) error {
	if err := s.writePkt(q.pkt); err != nil {
		return err
	}
	s.stats.packetsSent++
	if q.frameEnd {
		s.stats.framesSent++
		s.stats.bytesSent += uint64(q.auBytes)
	}
	return nil
}

// drainLocked 送出截止时刻已到的在队包;若当前在队帧年龄超过
// maxQueueAge 则先走 ageOverflow 转移。返回(待触发的合并请求 reason,
// 首个写错误)。写错误视为发送面死亡:弃队列并转入 closed。
func (s *ViewerSender) drainLocked(now time.Time) (string, error) {
	if len(s.queue) == 0 {
		return "", nil
	}
	if now.Sub(s.queueEnqueued) > maxQueueAge {
		return s.ageOverflowLocked(now), nil
	}
	t0 := time.Now()
	i, sent, sentBytes := 0, 0, 0
	for ; i < len(s.queue); i++ {
		if s.queue[i].sendAt.After(now) {
			break
		}
		if err := s.writeLocked(s.queue[i]); err != nil {
			// 出口已死(PC 关闭形态):弃队列、转 closed;错误上抛
			// (Enqueue 路径)或记日志(pace 泵路径)。
			n := len(s.queue)
			s.dropQueueLocked()
			s.shutdownLocked()
			return "", fmt.Errorf("desktop: write RTP packet %d/%d: %w", i+1, n, err)
		}
		sent++
		sentBytes += s.queue[i].pkt.MarshalSize()
	}
	if i > 0 {
		s.queue = append([]queuedPacket(nil), s.queue[i:]...)
		if len(s.queue) == 0 {
			s.queueBytes = 0
		}
		if d := time.Since(t0); d > 20*time.Millisecond {
			s.log.Info("desktop write_rtp slow", "ms", d.Milliseconds(), "packets", sent, "bytes", sentBytes)
		}
	}
	return "", nil
}

// drainNow 是泵体的一轮:drain + 锁外触发合并请求(单测同入口驱动)。
func (s *ViewerSender) drainNow() error {
	s.mu.Lock()
	now := s.nowFn()
	reason, err := s.drainLocked(now)
	s.mu.Unlock()
	s.fireKey(reason)
	return err
}

// paceLoop 是常驻泵:deadline 驱动(到下一包截止或队列年龄上限即醒),
// 绝不忙等;队列空时挂起在 notify/done 上。
func (s *ViewerSender) paceLoop() {
	defer s.wg.Done()
	for {
		if err := s.drainNow(); err != nil {
			s.log.Warn("desktop viewer sender stopped", "err", err)
			return // drainLocked 已转 closed 并 close(done)
		}
		s.mu.Lock()
		if s.state == stateClosed {
			s.mu.Unlock()
			return
		}
		var sleep time.Duration
		if len(s.queue) > 0 {
			next := s.queue[0].sendAt
			if age := s.queueEnqueued.Add(maxQueueAge); age.Before(next) {
				next = age // 最晚在超龄时刻醒来做冲刷判定
			}
			sleep = next.Sub(s.nowFn())
		}
		if sleep > 0 {
			timer := time.NewTimer(sleep)
			s.mu.Unlock()
			select {
			case <-timer.C:
			case <-s.notify:
				timer.Stop()
			case <-s.done:
				timer.Stop()
				return
			}
			continue
		}
		if len(s.queue) > 0 {
			// 有到期包(drainNow 与此刻之间时钟前移):直接再 drain。
			s.mu.Unlock()
			continue
		}
		notify, done := s.notify, s.done
		s.mu.Unlock()
		select {
		case <-notify:
		case <-done:
			return
		}
	}
}

// shutdownLocked 置 closed、丢队列、解除泵等待;幂等。
func (s *ViewerSender) shutdownLocked() {
	s.state = stateClosed
	s.dropQueueLocked()
	s.closeO.Do(func() { close(s.done) })
}
