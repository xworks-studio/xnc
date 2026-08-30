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
//   - 入队门(增量帧):最后一包的 plan 发送时刻超过入队时刻 +
//     maxQueueAge 的帧整帧不入队(无任何部分包发出),并进入 waitIDR
//     —— 抑制只丢不采样,绝不「丢一帧 P 帧后继续发后续 P 帧」。
//   - 关键帧豁免(C2,final-fixwave):真实 1080p 桌面 IDR 数十至数百
//     KB、native 无 VBV/IDR 尺寸钳制,100ms 门在控制器预算下只容 ~28KB
//     (2.3Mbps)/~9KB(500k 下限)—— 修前恢复 IDR(PLI/溢出/恢复/epoch)
//     整帧被拒 → waitIDR 棘轮 = 观众永久卡死。IDR 绝不拒收:头段按令牌
//     节奏铺开,尾段截止钳到入队 +100ms(有界突发,与冲刷同类);令牌
//     债务钳到 50ms 目标窗口等值(后续 delta 照常入门)。
//   - 恢复窗豁免(IDR 恢复死亡螺旋修正):C2 只保住恢复 IDR 本身,但 IDR
//     落地时债务钳在 50ms 地板,紧随其后的增量帧(残差膨胀 + 地板余债)
//     常撞 100ms 门 → 立刻再转 waitIDR + "pacer" 请求 → host 侧
//     kIdrMinIntervalMs=500 + 编码深度令新 IDR ≥500ms 不可得 → 每周期
//     ~15 帧被抑制、只交付 1 IDR → 再撞门,~1s 循环(生产实测 fps=2、
//     preKeyDropped=656、encoder_idr_timeout[pacer] 每秒一条)。恢复 IDR
//     的参考链本可承载这些帧,拒收它们等于作废刚到的 IDR。因此关键帧
//     准入后的最初 recoveryGraceDeltas 个撞门增量按关键帧同款有界突发
//     准入(截止钳 +100ms、债务钳地板);任一增量正常过门即认定恢复
//     完成并清零豁免 —— 正常门控与溢出恢复路径尽快原样恢复。
//   - 队列年龄(实际时刻)超过 maxQueueAge(增量帧):冲刷在队包(立即
//     写出以完成在途帧,避免撕裂帧)、清空队列、进入 waitIDR,并恰一次
//     触发合并关键帧回调。进行中的关键帧不被年龄界冲刷(冲刷会转
//     waitIDR,恢复自残)—— 其截止已钳到 100ms 视界,帧间年龄界照旧
//     (硬上限对增量帧始终成立)。
//   - (重传不在本层:发送器无 per-sender 重传状态 —— NACK 重发在
//     publisher PC 的 pion 拦截器链里,per-PC 一份。)
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
	// pacingBudgetFraction(M4 pacing 修正):令牌桶速率 = 预算 × 1.05。
	//
	// 历史:曾是 0.85(15% 余量留给重传)。这在「预算 = 缺省 20M」时无害,
	// 但 M3 起 SetBudget 的预算来源 = QoS 决策码率(qosTargetBitrate 已经
	// 是 0.85×est —— 15% 余量在那里已经记过一次),而同一码率也是编码器
	// 目标:编码器在持续运动下按目标产出时,令牌桶只以 0.85×目标 的速率
	// 放行 → 结构性欠载。令牌债务每帧加深 ~15%,数帧内撞上 100ms 入队门
	// → 增量帧整帧拒收 → waitIDR → 合并关键帧请求 → 恢复 IDR(豁免入队
	// 门、债务钳回 50ms 地板)→ 再数帧又撞门,循环往复:
	//
	//   确定性复现(manualClock 全链驱动,2.3Mbps/30fps、编码器按目标
	//   9583B/帧):330 帧内 36 次入队门拒收 + 36 个恢复 IDR(每第 9 帧
	//   一丢);帧完成间隔 33×7+67ms 的丢帧节拍;接收侧帧尾包经「合帧
	//   冲刷」成批突发(本机直连复测:549 个 0ms 到达间隔 + 17 个 >500ms
	//   饥饿间隙),正常间隔被拉成 34/62ms 交替对 —— 观众端 ?framediag
	//   读到的「间隔对交替 (n,n+1) 且档位随时间爬升」的突发-饥饿源。
	//   QoS 拥塞通道(queueMs>100)对这种自伤排队再砍码率/fps 沿
	//   {30,20,15,10,5} 下行,交替对档位随之爬升(复测 fps10 档:完成
	//   间隔 70/119/211ms 循环)。
	//
	// 修正:令牌桶按预算全额 × 1.05 放行 —— 15% 链路余量保留在 QoS 目标
	// (0.85×est)里不再双扣;×1.05 覆盖逐包线缆开销(令牌桶按
	// MarshalSize 计费,含 12B RTP 头 + FU 头 ≈1.2%,另有拦截器后盖的
	// transport-cc 扩展字节不在计费内)与编码器 VBV 窗口的过冲抖动。
	// 全额+开销余量下:债务不再结构性增长(0 拒收/0 IDR,间隔平整
	// 33ms),wire 速率 ≤ 1.05×0.85×est < est,不越估计带宽。
	pacingBudgetFraction = 1.05
	// pacingBurstBytes 是令牌桶容量(3×MTU):每帧首批包立即送出,余下
	// 按速率铺开——首包延迟为零,整帧有界。
	pacingBurstBytes = 3 * rtpMTU
	// pacingDebtFloor(C2 final-fixwave):关键帧债务钳制 —— 豁免入队门
	// 的 IDR 最多给令牌桶留下「50ms 排队目标窗口」等值的债务,保证随后
	// 的增量帧仍在 100ms 门内可入队、按节奏铺开(不为一次关键帧长期
	// 还债,否则恢复 IDR 之后 delta 接连被拒,流退化为 IDR-only)。
	pacingDebtFloor = 50 * time.Millisecond
	// recoveryGraceDeltas(IDR 恢复死亡螺旋修正):关键帧准入后、首个正常
	// 过门的增量帧之前,至多这么多个撞门增量帧按关键帧同款有界突发准入
	// (见文件头「恢复窗豁免」)。2 = 覆盖恢复 IDR 后的残差膨胀帧 + 地板
	// 余债帧(生产 500k 地板预算下实测:IDR、膨胀 P1、P2 三帧后门控自然
	// 恢复);再多会实质弱化 100ms 门对真实过载的节流。
	recoveryGraceDeltas = 2
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
	// MakeFrameMeta 组装一帧的 frame-meta 遥测记录(可为 nil = 无遥测;
	// M3 Task 4)。在 Enqueue 把帧 admitted 入队时调用,rtpTS 恰是将盖章
	// 在该帧全部包上的 per-viewer 90kHz 时戳。持发送器锁调用:须快速
	// 返回(只做记录组装,不做 I/O)。
	MakeFrameMeta func(f Frame, rtpTS uint32) *FrameMetaV1
	// OnFrameSent 在一帧最后一包成功写出后恰一次调用(frameEnd 边界)。
	// 合帧冲刷完成的帧同样计入(它们确实被完整送出);被抑制/丢弃/弃包
	// 的帧永不回调。持发送器锁调用:必须非阻塞、失败只许自行计数。
	OnFrameSent func(m *FrameMetaV1)
}

// ViewerSender 拥有一个 viewer 的全部发送侧状态(见文件头)。mu 之外的
// 字段构造后不可变。
type ViewerSender struct {
	log        *slog.Logger
	nowFn      func() time.Time
	writePkt   func(*rtp.Packet) error
	keyReq     func(reason string)
	makeMeta   func(Frame, uint32) *FrameMetaV1 // frame-meta 组装(入队时;nil = 无遥测)
	onFrameSnt func(*FrameMetaV1)               // frame-meta 汇出(最后一包写出后)

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
	queueHolds    bool      // 在队帧必须完整送出(关键帧,或恢复窗豁免增量:年龄界不冲刷,冲刷会转 waitIDR = 恢复自残)
	queueBytes    int       // 在队帧的 AU 字节累计
	recoveryGrace int       // 关键帧后剩余的恢复窗豁免名额(正常过门即清零;0 = 正常门控)
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
	frameEnd bool         // 本帧最后一包
	auBytes  int          // 本帧 AU 字节数(仅 frameEnd 有效)
	meta     *FrameMetaV1 // 本帧溯源记录(入队时绑定身份+时戳;frameEnd 包成功写出后经 OnFrameSent 汇出)
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
	deadlineDropped uint64 // 截止超 100ms 未入队的增量帧(关键帧豁免,C2)
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

// tokenBucket 是 per-viewer 令牌桶:速率恒为预算的 pacingBudgetFraction 倍
// (M4 起 ×1.05,见其注释),容量 pacingBurstBytes。
// 债务语义:reserve 可把 tokens 打为负(按赤字折算截止时刻),refill 按
// 耗时偿还并对容量封顶。
type tokenBucket struct {
	rate   float64 // bytes/s(预算 × pacingBudgetFraction)
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

// plan 为一帧的全部包计算计划发送时刻(不改余额;返回影子余额)。
// sizes 为各包的线缆字节数(MarshalSize)。
func (b *tokenBucket) plan(sizes []int, now time.Time) (deadlines []time.Time, tokens float64) {
	b.refill(now)
	deadlines = make([]time.Time, len(sizes))
	tokens = b.tokens
	for i, n := range sizes {
		if deficit := float64(n) - tokens; deficit > 0 {
			deadlines[i] = now.Add(time.Duration(deficit / b.rate * float64(time.Second)))
		} else {
			deadlines[i] = now
		}
		tokens -= float64(n)
	}
	return deadlines, tokens
}

// reserve(增量帧)为一帧的全部包计算计划发送时刻。若最后一包的截止
// 超过 now+maxQueueAge,则整帧拒收(不消耗任何令牌)——「超过 100ms 的
// 包不入队」的入队门。
func (b *tokenBucket) reserve(sizes []int, now time.Time) (deadlines []time.Time, ok bool) {
	deadlines, tokens := b.plan(sizes, now)
	if last := deadlines[len(deadlines)-1]; last.Sub(now) > maxQueueAge {
		return nil, false
	}
	b.tokens = tokens
	return deadlines, true
}

// reserveKeyframe(C2 final-fixwave;IDR 死亡螺旋修正:恢复窗豁免增量
// 复用同一语义):关键帧豁免 100ms 入队门 —— 绝不拒收。头段截止按令牌
// 节奏(plan 原值),尾段钳到 now+maxQueueAge:整帧铺开仍受 100ms 视界
// 约束(超出部分以有界突发送出,与冲刷已接受的突发同类),配
// drainLocked 的「进行中关键帧不被年龄界冲刷」保证 AU 必然完整送出。
// 余额照记(长期速率公平),但债务钳到 pacingDebtFloor 等值 —— 后续
// 增量帧照常入门、按节奏铺开。
func (b *tokenBucket) reserveKeyframe(sizes []int, now time.Time) []time.Time {
	deadlines, tokens := b.plan(sizes, now)
	gate := now.Add(maxQueueAge)
	for i := range deadlines {
		if deadlines[i].After(gate) {
			deadlines[i] = gate
		}
	}
	if floor := -b.rate * float64(pacingDebtFloor) / float64(time.Second); tokens < floor {
		tokens = floor
	}
	b.tokens = tokens
	return deadlines
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
		log:        log,
		nowFn:      nowFn,
		writePkt:   cfg.WritePacket,
		keyReq:     cfg.KeyRequest,
		makeMeta:   cfg.MakeFrameMeta,
		onFrameSnt: cfg.OnFrameSent,
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
		if !s.epochAcceptsLocked(fe) {
			// M4 修正(livelock 加固):等待期间 host 又前进了一代 ——
			// 重定位到更新的 epoch(其恢复 IDR 不被旧目标永久压制:修
			// 前 pendingEpoch 停在旧代,新代关键帧永远 epochDropped,
			// 观众饿死而 host 徒劳产 IDR)。旧代帧照旧抑制。
			if !epochNewer(fe, s.pendingEpoch) {
				s.stats.epochDropped++
				break
			}
			s.pendingEpoch = fe
		}
		if !f.Key {
			s.stats.preKeyDropped++
		} else {
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

	// 入队门:增量帧的最后一包截止超 maxQueueAge → 整帧不入队(抑制只
	// 丢不采样:进入 waitIDR 并恰一次合并请求,绝无「丢 P 帧后继续发
	// 后续 P 帧」);关键帧走 reserveKeyframe 豁免(C2,见其注释)——
	// 恢复 IDR 永远可交付。恢复窗豁免(IDR 死亡螺旋修正,见文件头):
	// 关键帧准入后的最初 recoveryGraceDeltas 个撞门增量同走关键帧豁免
	// (有界突发 + 债务地板)—— 拒收它们会把刚落地的恢复 IDR 作废并
	// 立刻再转 waitIDR(host kIdrMinIntervalMs=500 令下一个 IDR ≥500ms
	// 不可得,~15 帧/周期被抑制 = fps≈2 的死亡螺旋);任一增量正常过门
	// 即恢复完成,豁免清零,真实过载照常入门拒收。
	exempt := false
	var deadlines []time.Time
	if f.Key {
		deadlines = s.bucket.reserveKeyframe(sizes, now)
		s.recoveryGrace = recoveryGraceDeltas
	} else if d, ok := s.bucket.reserve(sizes, now); !ok {
		if s.recoveryGrace > 0 {
			deadlines = s.bucket.reserveKeyframe(sizes, now)
			s.recoveryGrace--
			exempt = true
		} else {
			s.stats.deadlineDropped++
			s.state = stateWaitIDR
			if keyReason == "" {
				keyReason = "pacer"
			}
			s.mu.Unlock()
			s.fireKey(keyReason)
			return nil
		}
	} else {
		deadlines = d
		s.recoveryGrace = 0
	}
	// frame-meta(M3 Task 4,修正轮):身份在本帧 admitted 入队时绑定
	//(此刻 per-viewer 时戳已定),随队列槽位携带——任何写出顺序(入口
	// drain/合帧冲刷/pacing 泵)下归属都不可能错位;只有最后一包真正
	// 写出的帧才在 writeLocked 汇出。
	var meta *FrameMetaV1
	if s.makeMeta != nil {
		meta = s.makeMeta(f, ts)
	}
	s.queueHolds = f.Key || exempt // 单帧队列:此刻队列必空(上方已冲刷/丢弃)
	for i := range pkts {
		s.queue = append(s.queue, queuedPacket{
			pkt:      pkts[i],
			sendAt:   deadlines[i],
			frameEnd: i == len(pkts)-1,
			auBytes:  len(f.AU),
			meta:     meta,
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

// SetBudget 更新 pacing 预算(M3 Task 3 裁决 2:预算来源 = QoS controller
// 的当前码率决策;无反馈时保持构造值 = defaultPacingBudgetBps)。速率即
// 时生效,令牌余额保留(在队帧的计划不受扰动)。bps<=0 忽略。
func (s *ViewerSender) SetBudget(bps int) {
	if bps <= 0 {
		return
	}
	s.mu.Lock()
	if r := float64(bps) / 8 * pacingBudgetFraction; r > 0 {
		s.bucket.rate = r
	}
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

// epochNewer 报告 a 是否严格新于 b(字典序:capture 先、codec 后;任一
// 无身份(v1 帧/未指明)则不参与重定位)。waitIDR 期间用它把等待目标
// 前移到 host 的最新代(M4 livelock 加固,见 Enqueue)。
func epochNewer(a, b frameEpoch) bool {
	if !a.known() || !b.known() {
		return false
	}
	if a.capture != b.capture {
		return a.capture > b.capture
	}
	return a.codec > b.codec
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
	s.queueHolds = false
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
// 与单测直接驱动同一入口;C2 后仅普通增量帧可入):冲刷在队包(立即写出以
// 完成在途帧)、清空队列、进入 waitIDR。返回应触发的合并关键帧 reason
// (仅 live→waitIDR 的转移触发;已在 waitIDR 时返回 ""——等待中的请求
// 已合并)。
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

// writeLocked 写出一个包并结算统计;调用方持 mu。frameEnd 包成功写出即
// 经 OnFrameSent 汇出该帧的 frame-meta(回调必须非阻塞;失败只许自行
// 计数,绝不影响发送面)。
func (s *ViewerSender) writeLocked(q queuedPacket) error {
	if err := s.writePkt(q.pkt); err != nil {
		return err
	}
	s.stats.packetsSent++
	if q.frameEnd {
		s.stats.framesSent++
		s.stats.bytesSent += uint64(q.auBytes)
		if q.meta != nil && s.onFrameSnt != nil {
			s.onFrameSnt(q.meta)
		}
	}
	return nil
}

// drainLocked 送出截止时刻已到的在队包;若当前在队帧(普通增量帧)年龄超过
// maxQueueAge 则先走 ageOverflow 转移。C2:进行中的关键帧不被年龄界冲刷
// —— 其截止已钳到入队 +100ms(reserveKeyframe),这里跳过冲刷判定让
// AU 必然完整送出(冲刷虽也整帧写出,但会转 waitIDR = 恢复自残);恢复
// 窗豁免的增量(IDR 死亡螺旋修正)同享此保证(同一钳制语义);帧间年龄
// 界照旧。返回(待触发的合并请求 reason,首个写错误)。写错误视为发送面
// 死亡:弃队列并转入 closed。
func (s *ViewerSender) drainLocked(now time.Time) (string, error) {
	if len(s.queue) == 0 {
		return "", nil
	}
	if now.Sub(s.queueEnqueued) > maxQueueAge && !s.queueHolds {
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
