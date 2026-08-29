// qos_controller.go — M3 Task 3:共享流 QoS 决策(纯逻辑,无 I/O)。
//
// 一条桌面采集(一个 Starter)的全部 viewer 共享一条编码流;本控制器消
// 费各 viewer 的网络观测(viewer_feedback 信令帧,session.go 解析后经
// Handler 级共享的 streamQoS 汇入)并产出 Action 列表:
//
//	SetVideoConfig —— 共享编码参数(host 侧 SET_VIDEO_CONFIG 管控消息;
//	  同时是 ViewerSender pacing 预算的真相来源,裁决 2)。
//	PauseSpectator —— 旁观者带宽不足,暂停该 viewer 的发送。
//
// 决策表(brief Step 1/2 + 裁决 4,全部被 qos_controller_test.go 钉死):
//   - controller(优先 viewer):先到先得;离场/隐藏/被修剪后由字典序最小
//     的可见 viewer 接任(确定性,同一集合必选出同一人)。只有它的反馈驱
//     动全局决策;隐藏 viewer 的反馈一律不计(也不触发旁观者暂停)。
//   - 拥塞(controller queueMs > 100):立即把码率砍到 70%(绕过 1/s 限速
//     与稳定窗);码率已在 500kbps 下限 → fps 沿 {60,30,20,15,10,5} 降一档;
//     fps 已在 5 → height 沿 {1440,1080,900,720} 降一档(max_w 推导见下)。
//     全部到底后拥塞不再有动作(default-safe)。
//   - 非拥塞降档(85%×est < 当前码率,如 TWCC 估计跌落)+ C1 迟滞
//     (final-fixwave):还须 headroom 比率(est/码率)较上一拍衰减 ≥5%
//     或 est 深于 0.95×85%×码率;≤1/s 一次。纯「85%×est < 码率」对
//     goodput 形反馈(est ≈ 0.85×码率)是恒真式 —— 修前每拍棘轮 ~15%
//     直到 500k 下限,升档(需 est > 1.18×码率)永不可达。
//   - 升档:稳定(无拥塞且有富余)≥10s 后,≤1/3s 一次;顺序 bitrate →
//     fps → height(先恢复最便宜的 knob,再动需要 codec epoch 重建的
//     height),码率每步至多 +15% 或 +1Mbps(spec §14.2 ramp;远目标
//     85%×est 需多个 3s 步逼近),且 fps/height 不越过初始值。
//   - fps 天花板(M4 模糊修正):XNC_DESKTOP_QOS_MAX_FPS(0=无,缺省)
//     同时钳住 fps 阶梯顶端与初始 fps —— host 以 60fps 起流时,天花板
//     30 把控制器与首份下发配置都钉在 30;60fps 下 QSV 的 VBV/每帧预算
//     = 码率/fps 直接减半(AU 被 ~半尺寸钳制 → 运动期粗量化 = 模糊,
//     2026-08-29 本地环测:2.3Mbps 下 fps60 的 IDR 全被钳在 9.9KB,
//     fps30 IDR 15.7-17.2KB)。fps 升不上去后,升档的富余自然流向
//     bitrate(升档顺序首位)与 height —— 画质优先于帧率。
//   - 旁观者暂停:旁观者 est < 35%×controller est → PauseSpectator(恰一
//     次,幂等;无自动恢复——恢复通道是后续任务)。controller 自身与隐藏
//     旁观者不受此规则约束。
//   - 首个 controller 出现时恰一次下发初始配置(驱动 pacing 预算接线:
//     「无反馈 → 保持 ViewerSender 缺省预算」的边界由 session 侧保证)。
//
// height → max_w 映射(文档化的契约):max_w = even(height × aspectW /
// aspectH),aspect 取建流时 HOST_HELLO 的 W/H(流几何变更后 native 侧
// 只缩不放,推导偏大也安全)。
//
// 有界状态:viewer 表按 lastSeen 修剪(2 分钟未见即删,controller 亦不例
// 外——离场的主导者让位),上限 64 条(超出的新 viewer 按字典序挤掉最旧
// 表项)。时钟注入(Now)使全部时间规则可确定性单测。
//
// reset-recovery grace(M4 修正):max_w 变更的决策走 host 的 resolution
// 重置(编码器重建、codec epoch 前进、首个恢复 IDR 之前无帧产出)。
// 重置在途(FrameObserved 尚未确认)期间拥塞剪码一律挂起(held,不累
// 积——阶梯是状态机,恢复确认后自然续降);且 reset-triggering 剪刀之
// 后立即通道不再完全绕过 qosDownMinInterval。修前:每条拥塞反馈都再剪
// 一档 max_w → 密集重置风暴 → 恢复 IDR 反复作废 → 观众饿死而 host 徒
// 劳产 IDR(M3-T6 合成矩阵的假编码器无重启延迟,从未暴露此链)。
package desktop

import (
	"time"
)

// fpsLadder / heightLadder 是 brief Step 2 的精确阶梯(降序)。
var (
	fpsLadder    = []uint32{60, 30, 20, 15, 10, 5}
	heightLadder = []uint32{1440, 1080, 900, 720}
)

const (
	// qosBandwidthFraction:目标码率 = 85% × 估计带宽(15% 余量给重传/
	// 信令;与 ViewerSender 的 pacingBudgetFraction 同源)。
	qosBandwidthFraction = 0.85
	// qosDownshiftFactor:拥塞立即通道的码率砍幅(30% downshift)。
	qosDownshiftFactor = 0.70
	// qosDownRatioDecayGuard / qosDownDeepRatio(C1 final-fixwave 迟滞):
	// 非拥塞降档除水平判据(85%×est < 码率)外还需降档证据 —— headroom
	// 比率(est/码率)较上一拍衰减 ≥5%(真实恶化的边沿;goodput 跟随时
	// 比率恒定,永不触发),或 est 深于 0.95×85%×码率(≥19% 深亏;出生
	// 即低的通路无边沿可言,持续深亏必须能降,一步收敛到 85%×est)。
	qosDownRatioDecayGuard = 0.95
	qosDownDeepRatio       = 0.95 * 0.85
	// qosUpStepAddBps(I2 final-fixwave):升档步幅上限 —— spec §14.2
	// 「每 3 秒最多增加 15% 或 1 Mbps」(15% 走整数 ×115/100)。远目标
	// 需多个 3s 步阶梯逼近,绝不一步直达 85%×est。
	qosUpStepAddBps = 1_000_000
	// qosSpectatorPauseFraction:旁观者暂停线(35% of controller est)。
	qosSpectatorPauseFraction = 0.35
	// qosMinBitrateBps / qosMaxBitrateBps:码率工作区 500kbps–15Mbps。
	qosMinBitrateBps = 500_000
	qosMaxBitrateBps = 15_000_000
	// qosQueueAgeMs:拥塞判据(发送侧排队目标 50ms/硬上限 100ms 的全局
	// 约束镜像)。
	qosQueueAgeMs = 100.0
	// qosStableWindow:升档前的稳定窗;qosDownMinInterval /
	// qosUpMinInterval:降/升档限速(立即通道绕过降档限速)。
	qosStableWindow    = 10 * time.Second
	qosDownMinInterval = 1 * time.Second
	qosUpMinInterval   = 3 * time.Second
	// qosViewerTTL:viewer 表项的修剪窗;qosMaxViewers:表大小上限。
	qosViewerTTL  = 2 * time.Minute
	qosMaxViewers = 64
)

// bitrateForWidth 是 native diag.h BitrateForDims 的 Go 镜像(初始码率的
// 信念来源:agent ATTACH 不携带码率,host 用同一 tiers 选缺省)。tiers:
// ≤1280→1.5M,≤1920→2.3M,≤2560→3.2M,更大→4.2M。
func bitrateForWidth(w uint32) uint32 {
	switch {
	case w <= 1280:
		return 1_500_000
	case w <= 1920:
		return 2_300_000
	case w <= 2560:
		return 3_200_000
	default:
		return 4_200_000
	}
}

// ViewerFeedback 是一条 viewer 网络观测(session.go 从 viewer_feedback
// 信令帧解析;SessionID 由会话侧填充——路由到正确的 viewer)。
type ViewerFeedback struct {
	SessionID    string
	Visible      bool
	EstimatedBps uint64
	QueueMs      float64
	DecodeQueue  float64 // 解码队列深度(帧)——记录,不参与判据
	RTTMs        float64
}

// actionKind 是 Action 的判别器。
type actionKind uint8

const (
	// actionSetVideoConfig:应用共享编码参数(Source.SetVideoConfig +
	// 各 viewer 发送器的 pacing 预算)。
	actionSetVideoConfig actionKind = 1 + iota
	// actionPauseSpectator:向 ViewerID 指定的 viewer 发稳定态
	// spectator_network_paused 并暂停其 ViewerSender。
	actionPauseSpectator
)

// Action 是一次决策的产物(Observe 返回;session 侧应用)。
type Action struct {
	Kind     actionKind
	Config   VideoConfig // actionSetVideoConfig
	ViewerID string      // actionPauseSpectator
}

// QoSControllerConfig 组装控制器。
type QoSControllerConfig struct {
	// Initial 是建流基线:Bitrate = bitrateForWidth(W),FPS = hello fps,
	// MaxW = 流宽(native 只缩不放,等价无约束)。升档不会越过它。
	Initial VideoConfig
	// MaxFPS 是 fps 天花板(M4 模糊修正;0 = 无 = M3 行为)。>0 时:
	// fps 阶梯顶端与 Initial.FPS 一并钳到 ≤MaxFPS —— host 60fps 起流也
	// 被钉死在 MaxFPS(首份下发配置即生效),升档富余流向 bitrate/
	// height。生产接线:env XNC_DESKTOP_QOS_MAX_FPS(qosManager)。
	MaxFPS uint32
	// AspectW/AspectH:height 阶梯 → max_w 的推导宽高比(HOST_HELLO W/H)。
	AspectW, AspectH uint32
	// Now 是时钟注入点(默认 time.Now;单测注入 manualClock)。
	Now func() time.Time
}

// qosViewer 是一个 viewer 的观测状态。
type qosViewer struct {
	visible  bool
	bps      uint64
	paused   bool
	lastSeen time.Time
}

// QoSController 见文件头。零时钟缺省 time.Now;Observe 可从任意 goroutine
// 并发(内部无锁时由外层 streamQoS 串行——本类型自身非线程安全,保持纯
// 粹以利单测)。
type QoSController struct {
	now              func() time.Time
	cur              VideoConfig
	initial          VideoConfig
	aspectW, aspectH uint32
	fpsSteps         []uint32 // fps 阶梯(实例副本;MaxFPS>0 时顶端被钳)

	viewers       map[string]*qosViewer
	controllerID  string
	controllerBps uint64
	emitted       bool // 初始配置已下发(controller 就位时恰一次)

	stableSince time.Time // 稳定窗锚点(零值 = 未在稳定期)
	lastDownAt  time.Time
	lastUpAt    time.Time

	// reset-recovery grace(M4 修正,见文件头):resetPending = 一条改变
	// max_w 的配置已下发、尚未被帧流确认(host 重置编码器期间)。此间拥
	// 塞剪码挂起;lastMaxWActAt 让 reset-triggering 剪刀即便在立即通道
	// 也遵守 qosDownMinInterval(修前立即通道完全绕过限速)。heldCuts 记
	// 数挂起次数(可观测性:streamQoS 观测并记日志)。
	resetPending  bool
	lastMaxWActAt time.Time
	heldCuts      uint32

	// C1 迟滞参考:上一拍 controller 观测的 (est, 该拍生效码率[动作前])。
	// 以动作前码率为参考,我们自己降档后 goodput 的等比例回落(比率回
	// 到 ~0.85)不构成新证据 —— 真实 dip 之后的棘轮同样止步。
	estRefKnown bool
	lastEstBps  uint64
	lastRefBps  uint32
}

// newQoSController 建控制器(决策入口只有 Observe)。
func newQoSController(cfg QoSControllerConfig) *QoSController {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	// fps 天花板(M4 模糊修正):初始 fps(=升档上限 + 起步值)与阶梯
	// 顶端一并钳到 ≤MaxFPS —— 60 起流的 host 在首份下发配置(首个
	// controller 反馈即发)就被降到天花板;阶梯同理没有 >MaxFPS 的档。
	init := cfg.Initial
	if cfg.MaxFPS > 0 && init.FPS > cfg.MaxFPS {
		init.FPS = cfg.MaxFPS
	}
	return &QoSController{
		now:      now,
		cur:      init,
		initial:  init,
		aspectW:  cfg.AspectW,
		aspectH:  cfg.AspectH,
		fpsSteps: fpsLadderFor(cfg.MaxFPS),
		viewers:  make(map[string]*qosViewer),
	}
}

// fpsLadderFor 返回 fps 阶梯的实例副本:ceiling=0 原样({60,30,20,15,10,5});
// ceiling=30 → {30,20,15,10,5}(60 档消失 —— 升档不再有 fps 去处,富余
// 由升档顺序自然流向 bitrate → height;降档从 30 起步沿钳后阶梯下行)。
func fpsLadderFor(ceiling uint32) []uint32 {
	if ceiling == 0 {
		return fpsLadder
	}
	out := make([]uint32, 0, len(fpsLadder))
	for _, v := range fpsLadder {
		if v <= ceiling {
			out = append(out, v)
		}
	}
	return out
}

// Current 返回当前生效的编码参数(应用侧观测点)。
func (c *QoSController) Current() VideoConfig { return c.cur }

// ControllerID 返回当前优先 viewer(空 = 无可见 viewer)。
func (c *QoSController) ControllerID() string { return c.controllerID }

// Observe 消费一条 viewer 反馈,返回本次决策的 Action 列表(可为空:
// 无反馈/无变化 → 无动作,default-safe)。
func (c *QoSController) Observe(fb ViewerFeedback) []Action {
	if fb.SessionID == "" {
		return nil
	}
	now := c.now()
	v := c.touchViewer(fb.SessionID, now)
	if v == nil {
		return nil // 表满且无可挤占的陈旧表项:忽略未知新 viewer
	}
	v.visible = fb.Visible
	v.bps = fb.EstimatedBps

	c.prune(now)
	c.promote()

	var acts []Action
	if !c.emitted && c.controllerID != "" {
		// controller 就位:恰一次下发初始配置(pacing 预算接线 + host
		// 同步;此后只在决策变化时下发)。
		c.emitted = true
		acts = append(acts, Action{Kind: actionSetVideoConfig, Config: c.cur})
	}

	isController := fb.SessionID == c.controllerID && fb.Visible
	switch {
	case isController:
		if more := c.decide(fb, now); len(more) > 0 {
			acts = append(acts, more...)
		}
	case !fb.Visible || c.controllerID == "":
		// 隐藏 viewer / 无 controller:全局决策与暂停都不做。
	default:
		// 旁观者:唯一可能的动作是带宽不足暂停(见文件头;恰一次)。
		if !v.paused && c.controllerBps > 0 &&
			float64(fb.EstimatedBps) < qosSpectatorPauseFraction*float64(c.controllerBps) {
			v.paused = true
			acts = append(acts, Action{Kind: actionPauseSpectator, ViewerID: fb.SessionID})
		}
	}
	return acts
}

// decide 是 controller 反馈的全局决策(降/升档 + 限速;见文件头决策表)。
// 调用前提:fb 来自当前可见的 controller。
func (c *QoSController) decide(fb ViewerFeedback, now time.Time) []Action {
	c.controllerBps = fb.EstimatedBps

	// C1 迟滞(final-fixwave):降档判据从恒真式改为「带证据的降」。
	// goodput 形反馈(est ≈ 0.85×码率 —— pacing 令牌桶速率的直接投影)
	// 令 0.85×est < 码率恒真;headroom 比率的衰减边沿 + 深亏线把「带宽
	// 证据」与「发送速率的影子」区分开(常数文档见上)。
	headroom := float64(fb.EstimatedBps) / float64(c.cur.Bitrate)
	decay := c.estRefKnown &&
		headroom < qosDownRatioDecayGuard*(float64(c.lastEstBps)/float64(c.lastRefBps))
	deep := headroom < qosDownDeepRatio
	// 参考值记录当拍动作前的码率(decide 自此至动作只读 c.cur)。
	c.lastEstBps, c.lastRefBps, c.estRefKnown = fb.EstimatedBps, c.cur.Bitrate, true

	target := qosTargetBitrate(fb.EstimatedBps)
	if fb.QueueMs > qosQueueAgeMs {
		// 拥塞:稳定窗作废;立即 30% 通道(绕过 1/s 限速)。
		c.stableSince = time.Time{}
		// reset-recovery grace:重置在途期间挂起(编码器重启本身就会推
		// 高排队年龄——此刻的拥塞证据是垃圾;剪码只会再触发一次重置,
		// 把上一代的恢复 IDR 作废)。挂起不累积:阶梯是状态机,确认后
		// 的下一条拥塞反馈自然续降。
		if c.resetPending {
			c.heldCuts++
			return nil
		}
		if next := c.stepDown(); next != c.cur {
			if !c.noteMaxWChange(next, now) {
				c.heldCuts++
				return nil
			}
			c.cur = next
			c.lastDownAt = now
			return []Action{{Kind: actionSetVideoConfig, Config: c.cur}}
		}
		return nil
	}

	if target < c.cur.Bitrate && (decay || deep) {
		// 带宽估计跌落(尚未排队):常规降档,≤1/s;仅当有降档证据
		//(比率衰减或深亏)—— 恒平的 goodput 形反馈不再触发。
		c.stableSince = time.Time{}
		if now.Sub(c.lastDownAt) < qosDownMinInterval {
			return nil
		}
		next := c.cur
		next.Bitrate = target
		c.cur = next
		c.lastDownAt = now
		return []Action{{Kind: actionSetVideoConfig, Config: c.cur}}
	}

	// 无拥塞且有富余(或恰好持平):稳定窗推进。
	if c.stableSince.IsZero() {
		c.stableSince = now
	}
	if now.Sub(c.stableSince) < qosStableWindow ||
		now.Sub(c.lastUpAt) < qosUpMinInterval {
		return nil
	}
	if next := c.stepUp(target); next != c.cur {
		// height 升档同样改变 max_w → 同样触发 host 重置:同受 grace 与
		// 最小间隔约束(升档本就要求 10s 稳定 + 3s 限速,这里是防御性
		// 的对称闭合)。
		if !c.noteMaxWChange(next, now) {
			return nil
		}
		c.cur = next
		c.lastUpAt = now
		return []Action{{Kind: actionSetVideoConfig, Config: c.cur}}
	}
	return nil
}

// noteMaxWChange 判定 next 是否为 reset-triggering 决策(max_w 变更):
// 是则记录时刻并置 grace(resetPending);若距上一次 reset-triggering 决
// 策不足 qosDownMinInterval(立即通道也不许完全绕过——密集 max_w 变更
// 就是重置风暴)则返回 false = 本拍挂起。非 max_w 决策(bitrate/fps 热
// 更新)恒 true。
func (c *QoSController) noteMaxWChange(next VideoConfig, now time.Time) bool {
	if next.MaxW == c.cur.MaxW {
		return true
	}
	if now.Sub(c.lastMaxWActAt) < qosDownMinInterval {
		return false
	}
	c.lastMaxWActAt = now
	c.resetPending = true
	return true
}

// FrameObserved 通知决策器「流又产出帧了」(session 帧泵逐帧喂入):host
// 重置后的新代帧流确认重置已完成 → 解除 reset-recovery grace。返回是否
// 解除了一次在途挂起(真值时 streamQoS 记一条恢复日志)。
func (c *QoSController) FrameObserved() bool {
	held := c.resetPending
	c.resetPending = false
	return held
}

// ResetConfirmed 无帧观测地解除 grace(host 未收到配置 → 无重置可能在
// 途;apply 侧 configSend=false 时调用)。
func (c *QoSController) ResetConfirmed() { c.resetPending = false }

// HeldCuts 返回 grace 挂起的拥塞剪码累计数(可观测性)。
func (c *QoSController) HeldCuts() uint32 { return c.heldCuts }

// stepDown 计算拥塞时的下一档(码率 70% → fps 降档 → height 降档;全在
// 底则返回当前值 = 无动作)。
func (c *QoSController) stepDown() VideoConfig {
	next := c.cur
	if br := uint32(float64(next.Bitrate) * qosDownshiftFactor); br > qosMinBitrateBps && br < next.Bitrate {
		next.Bitrate = br
		return next
	}
	if next.Bitrate > qosMinBitrateBps {
		next.Bitrate = qosMinBitrateBps
		return next
	}
	if fps, ok := ladderBelow(c.fpsSteps, next.FPS); ok {
		next.FPS = fps
		return next
	}
	if h, ok := ladderBelow(heightLadder, c.streamHeight()); ok {
		next.MaxW = maxWForHeight(h, c.aspectW, c.aspectH)
		return next
	}
	return c.cur // 全底:拥塞不再有动作
}

// stepUp 计算稳定后的上一档(顺序:码率 → fps → height;不越初始值)。
// I2(final-fixwave):码率步幅 ≤ max(+15%, +1Mbps)(spec §14.2 ramp)
// —— 远目标(85%×est,封顶 15M)由多个 3s 步阶梯逼近,绝不一步直达
// (修前 500k→12.75M 一跳把恢复变成新的突发)。
func (c *QoSController) stepUp(target uint32) VideoConfig {
	next := c.cur
	if target > next.Bitrate {
		step := next.Bitrate * 115 / 100 // +15%(整数下取整)
		if add := next.Bitrate + qosUpStepAddBps; add > step {
			step = add // 小码率区 +1Mbps 更大(spec 允许二者取大)
		}
		if step > target {
			step = target
		}
		next.Bitrate = step
		return next
	}
	if fps, ok := ladderAbove(c.fpsSteps, next.FPS, c.initial.FPS); ok {
		next.FPS = fps
		return next
	}
	if w, ok := ladderHeightAbove(c.streamHeight(), c.initial.MaxW, c.aspectW, c.aspectH); ok {
		next.MaxW = w
		return next
	}
	return c.cur
}

// streamHeight 由当前 max_w 与宽高比反推有效流高。
func (c *QoSController) streamHeight() uint32 {
	if c.aspectW == 0 || c.aspectH == 0 {
		return 0
	}
	return uint32(uint64(c.cur.MaxW) * uint64(c.aspectH) / uint64(c.aspectW))
}

// ---- 纯 helpers ----

// qosTargetBitrate:目标 = clamp(85% × est, 500kbps, 15Mbps)。整数
// 四舍五入(×85/100)——浮点截断会在等值边界抖出 ±1bps 的伪降档。
func qosTargetBitrate(estBps uint64) uint32 {
	t := (estBps*85 + 50) / 100
	if t < qosMinBitrateBps {
		t = qosMinBitrateBps
	}
	if t > qosMaxBitrateBps {
		t = qosMaxBitrateBps
	}
	return uint32(t)
}

// ladderBelow 返回严格小于 cur 的最大档位(降档)。
func ladderBelow(ladder []uint32, cur uint32) (uint32, bool) {
	for _, v := range ladder { // 降序:首个 < cur 即最大者
		if v < cur {
			return v, true
		}
	}
	return 0, false
}

// ladderAbove 返回严格大于 cur、且不超过 cap 的最小档位(升档;cap 为
// 初始值——恢复不越过建流基线)。
func ladderAbove(ladder []uint32, cur, cap uint32) (uint32, bool) {
	best, ok := uint32(0), false
	for _, v := range ladder {
		if v > cur && v <= cap && (!ok || v < best) {
			best, ok = v, true
		}
	}
	return best, ok
}

// maxWForHeight:max_w = even(height × W / H)(文档化映射;native 侧
// ScaledDims/GpuScaledDims 同样只缩不放 + 偶数对齐)。
func maxWForHeight(h, aspectW, aspectH uint32) uint32 {
	if aspectW == 0 || aspectH == 0 || h == 0 {
		return 0
	}
	w := uint32(uint64(h) * uint64(aspectW) / uint64(aspectH))
	if w%2 != 0 {
		w--
	}
	return w
}

// ladderHeightAbove:height 升档——推导 max_w 不超过初始宽(1440 档在
// 1080p 流上推导 2560 > 1920 → 不升)。
func ladderHeightAbove(curH, initialW, aspectW, aspectH uint32) (uint32, bool) {
	best, ok := uint32(0), false
	for _, h := range heightLadder {
		if h > curH && maxWForHeight(h, aspectW, aspectH) <= initialW &&
			(!ok || h < best) {
			best, ok = h, true
		}
	}
	if !ok {
		return 0, false
	}
	return maxWForHeight(best, aspectW, aspectH), true
}

// ---- viewer 表(有界)----

// touchViewer 取/建表项并刷新 lastSeen;表满时挤掉最旧表项(含 controller
// ——离场的主导者正是要被挤掉的对象);无肉可挤返回 nil。
func (c *QoSController) touchViewer(id string, now time.Time) *qosViewer {
	if v, ok := c.viewers[id]; ok {
		v.lastSeen = now
		return v
	}
	if len(c.viewers) >= qosMaxViewers {
		var oldestID string
		var oldest time.Time
		for k, v := range c.viewers {
			if oldestID == "" || v.lastSeen.Before(oldest) {
				oldestID, oldest = k, v.lastSeen
			}
		}
		if oldestID == "" || !oldest.Before(now) {
			return nil // 全部都是本刻新建(时钟停走的病态注入):拒绝扩张
		}
		if c.controllerID == oldestID {
			c.controllerID = ""
		}
		delete(c.viewers, oldestID)
	}
	v := &qosViewer{lastSeen: now}
	c.viewers[id] = v
	return v
}

// prune 修剪 TTL 之外的表项(controller 让位由 promote 重选)。
func (c *QoSController) prune(now time.Time) {
	for k, v := range c.viewers {
		if now.Sub(v.lastSeen) > qosViewerTTL {
			delete(c.viewers, k)
		}
	}
}

// promote 重选 controller:在位者可见且未被暂停则保持(先到先得);空位/
// 离场由字典序最小的可见且未暂停 viewer 接任(确定性)。被网络暂停的旁观
// 者绝不接任——其退化带宽估计会把共享流拖到底,而先到先得会让后到的好
// viewer 永远拿不回主导权(review IMPORTANT 2)。全部可见 viewer 都被暂停
// → 空位(frozen config:无决策、无新暂停,持留最后生效配置)。
func (c *QoSController) promote() {
	best := ""
	if v, ok := c.viewers[c.controllerID]; ok && v.visible && !v.paused {
		return
	}
	for k, v := range c.viewers {
		if v.visible && !v.paused && (best == "" || k < best) {
			best = k
		}
	}
	c.controllerID = best
	if best == "" {
		c.controllerBps = 0
	}
}
