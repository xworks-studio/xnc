// qos_controller_test.go — M3 Task 3:QoS 决策表单测(纯逻辑,manualClock
// 确定性驱动;复用 viewer_sender_test.go 的 manualClock)。决策表来源:
// brief Step 1 + controller 裁决 4 ——
//
//	immediate 30% downshift on queueAge>100ms(绕过 1/s 限速)
//	no upshift before 10 stable seconds(up ≤1/3s after stability)
//	hidden viewers excluded from global decisions
//	controller (priority viewer) dominates
//	spectator paused below 35% of controller bandwidth
//
// final-fixwave 增补:C1(goodput 形反馈不得棘轮 + 真实恶化仍及时降)、
// I2(升档步幅 ≤ max(+15%, +1Mbps))、I1(迟到会话 attach 即继承当前配置)。
package desktop

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"
)

// newQoSTestController 建一个 1920x1080@30fps 流形态的控制器(初始码率走
// bitrateForWidth(1920) = 2.3Mbps,镜像 native diag.h BitrateForDims)。
func newQoSTestController() (*QoSController, *manualClock) {
	clk := newManualClock()
	c := newQoSController(QoSControllerConfig{
		Initial: VideoConfig{Bitrate: bitrateForWidth(1920), FPS: 30, MaxW: 1920},
		AspectW: 1920,
		AspectH: 1080,
		Now:     clk.Now,
	})
	return c, clk
}

// pastColdStart 把时钟推过 qosColdStartGuard(2026-08-30 起 controller
// 就位后的冷启动保护窗内降档证据不行动 —— 测降档/升档决策的用例在选举
// 后调用,回到保护窗语义之外的传统行为)。
func pastColdStart(clk *manualClock) {
	clk.advance(qosColdStartGuard + 100*time.Millisecond)
}

// configOf 提取本批动作里(至多一条)的 SetVideoConfig;无则零值 + false。
func configOf(t *testing.T, acts []Action) (VideoConfig, bool) {
	t.Helper()
	for _, a := range acts {
		if a.Kind == actionSetVideoConfig {
			return a.Config, true
		}
	}
	return VideoConfig{}, false
}

func fb(sid string, visible bool, estBps uint64, queueMs float64) ViewerFeedback {
	return ViewerFeedback{SessionID: sid, Visible: visible, EstimatedBps: estBps, QueueMs: queueMs}
}

// fbCadence:带 presentedFps 的反馈(0 = 旧 web 未上报形态)。
func fbCadence(sid string, estBps uint64, queueMs, presentedFps float64) ViewerFeedback {
	return ViewerFeedback{SessionID: sid, Visible: true, EstimatedBps: estBps,
		QueueMs: queueMs, PresentedFps: presentedFps}
}

// fbCongest:拥塞形态反馈(queueMs=500 越线 + 健康节奏 25fps —— 节奏门
// 之下 queueMs 构成拥塞证据;Fix 6 起无 presentedFps 的越线 queueMs 仅咨
// 询,不再驱动立即通道,故拥塞用例一律带健康节奏)。
func fbCongest(sid string, estBps uint64) ViewerFeedback {
	return fbCadence(sid, estBps, 500, 25)
}

// ---- 决策表(brief Step 1 五行 + 限速/阶梯补充)----

// 首个可见 viewer 成为 controller:恰一次下发初始配置(驱动 pacing 预算
// 接线 + host 同步);此后稳定且无决策变化 → 静默(default-safe)。
func TestQoSInitialEmitOnFirstControllerFeedback(t *testing.T) {
	c, _ := newQoSTestController()
	acts := c.Observe(fb("s1", true, 8_000_000, 5))
	cfg, ok := configOf(t, acts)
	if !ok || cfg != (VideoConfig{Bitrate: 2_300_000, FPS: 30, MaxW: 1920}) {
		t.Fatalf("first controller feedback should emit the initial config once, got %+v ok=%v", cfg, ok)
	}
	if c.ControllerID() != "s1" {
		t.Fatalf("controller = %q, want s1", c.ControllerID())
	}
	if acts := c.Observe(fb("s1", true, 2_705_882, 5)); len(acts) != 0 {
		t.Fatalf("steady-state feedback should be silent, got %+v", acts)
	}
}

// queueAge>100ms → 立即 30% 降码率(不要求稳定窗口)。Fix 3:立即通道
// 与常规降档共用 1/s 限速 —— 「立即」= 窗口内的第一次剪码不等稳定窗,
// 不是无限速(修前 1/s 反馈节奏下每秒 30% 连剪 = 无界棘轮)。
func TestQoSImmediateDownshiftOnQueueAge(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fb("s1", true, 8_000_000, 5)) // controller 就位(初始下发)
	pastColdStart(clk)

	cfg, ok := configOf(t, c.Observe(fbCongest("s1", 8_000_000)))
	if !ok || cfg.Bitrate != 2_300_000*7/10 {
		t.Fatalf("queueAge=150ms should cut bitrate to 70%%: got %+v ok=%v", cfg, ok)
	}
	// 100ms 后再次拥塞:同一 1/s 窗内 → 抑制(且非 grace 挂起:HeldCuts
	// 不动,挡住它的是限速本身)。
	clk.advance(100 * time.Millisecond)
	held0 := c.HeldCuts()
	if acts := c.Observe(fbCongest("s1", 8_000_000)); len(acts) != 0 {
		t.Fatalf("immediate cut must respect the 1/s limiter, got %+v", acts)
	}
	if c.HeldCuts() != held0 {
		t.Fatalf("suppression must be the rate limit, not the reset grace (held %d -> %d)", held0, c.HeldCuts())
	}
	// 窗口过后 → 再降 30%。
	clk.advance(1000 * time.Millisecond)
	cfg, ok = configOf(t, c.Observe(fbCongest("s1", 8_000_000)))
	if !ok || cfg.Bitrate != 2_300_000*7/10*7/10 {
		t.Fatalf("post-window congestion should cut again: got %+v ok=%v", cfg, ok)
	}
}

// 非拥塞降档(带宽估计跌落,目标 85%×est < 当前)受 1/s 限速。
func TestQoSDownRateLimitedPerSecond(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fb("s1", true, 8_000_000, 5)) // initial: 2.3M
	pastColdStart(clk)

	// est=2Mbps → target 1.7M < 2.3M → 降档。
	cfg, ok := configOf(t, c.Observe(fb("s1", true, 2_000_000, 5)))
	if !ok || cfg.Bitrate != 1_700_000 {
		t.Fatalf("bandwidth-est downshift to 85%% target: got %+v ok=%v", cfg, ok)
	}
	// +100ms:est=1Mbps(→ target 850k)但 1/s 窗内 → 抑制。
	clk.advance(100 * time.Millisecond)
	if acts := c.Observe(fb("s1", true, 1_000_000, 5)); len(acts) != 0 {
		t.Fatalf("down within 1s must be suppressed, got %+v", acts)
	}
	// +1.1s:允许 → 降到 850k。
	clk.advance(1100 * time.Millisecond)
	cfg, ok = configOf(t, c.Observe(fb("s1", true, 1_000_000, 5)))
	if !ok || cfg.Bitrate != 850_000 {
		t.Fatalf("down after 1s window: got %+v ok=%v", cfg, ok)
	}
}

// 10s 稳定前不升档;稳定后 up ≤1/3s;I2(final-fixwave):每步步幅
// ≤ max(+15%, +1Mbps),远目标(85%×est)由多个 3s 步逼近,上限 15Mbps;
// 拥塞重置稳定窗口。
func TestQoSNoUpshiftBeforeTenStableSeconds(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fb("s1", true, 8_000_000, 5)) // stableSince = t0

	clk.advance(9900 * time.Millisecond)
	if acts := c.Observe(fb("s1", true, 8_000_000, 5)); len(acts) != 0 {
		t.Fatalf("no upshift before 10 stable seconds, got %+v", acts)
	}
	clk.advance(100 * time.Millisecond) // t=10s
	cfg, ok := configOf(t, c.Observe(fb("s1", true, 8_000_000, 5)))
	if !ok || cfg.Bitrate != 3_300_000 { // 2.3M + max(15%,1Mbps) = 3.3M
		t.Fatalf("first upshift steps by max(+15%%,+1Mbps): got %+v ok=%v", cfg, ok)
	}
	// up ≤1/3s:11s 时 est 升(→ target 8.5M)但窗口内 → 抑制。
	clk.advance(1 * time.Second)
	if acts := c.Observe(fb("s1", true, 10_000_000, 5)); len(acts) != 0 {
		t.Fatalf("up within 3s must be suppressed, got %+v", acts)
	}
	clk.advance(2100 * time.Millisecond) // t=13.1s
	cfg, ok = configOf(t, c.Observe(fb("s1", true, 10_000_000, 5)))
	if !ok || cfg.Bitrate != 4_300_000 { // 3.3M + max(15%,1Mbps) = 4.3M
		t.Fatalf("up after 3s window steps by max(+15%%,+1Mbps): got %+v ok=%v", cfg, ok)
	}
	// 上限钳制:est=100Mbps → 多步 ramp 逼近 15M(绝不一步直达)。
	prev := cfg.Bitrate
	for i := 0; i < 40 && prev < 15_000_000; i++ {
		clk.advance(3100 * time.Millisecond)
		cfg, ok = configOf(t, c.Observe(fb("s1", true, 100_000_000, 5)))
		if !ok {
			break
		}
		limit := prev + qosUpStepAddBps
		if lim15 := prev * 115 / 100; lim15 > limit {
			limit = lim15
		}
		if cfg.Bitrate > limit || cfg.Bitrate > 15_000_000 {
			t.Fatalf("ramp step %d: %d -> %d exceeds max(+15%%,+1Mbps) or ceiling", i, prev, cfg.Bitrate)
		}
		prev = cfg.Bitrate
	}
	if prev != 15_000_000 {
		t.Fatalf("ramp must converge to the 15Mbps ceiling, got %d", prev)
	}
	// 拥塞:立即 30% 降 + 稳定窗口重置(10s 内不再升档)。
	clk.advance(100 * time.Millisecond)
	cfg, _ = configOf(t, c.Observe(fbCongest("s1", 100_000_000)))
	if cfg.Bitrate != 15_000_000*7/10 {
		t.Fatalf("congestion after upshift cuts 30%%: got %+v", cfg)
	}
	clk.advance(10 * time.Second)
	if acts := c.Observe(fb("s1", true, 100_000_000, 5)); len(acts) != 0 {
		t.Fatalf("stability window must restart after congestion (first clean sample only marks it), got %+v", acts)
	}
}

// 隐藏 viewer 的反馈不驱动任何全局决策(也不触发初始下发);隐藏的
// controller 失去主导权,可见 viewer 接管。
func TestQoSHiddenViewersExcludedFromGlobalDecisions(t *testing.T) {
	c, clk := newQoSTestController()
	if acts := c.Observe(fb("s1", false, 8_000_000, 500)); len(acts) != 0 {
		t.Fatalf("hidden viewer feedback must not act, got %+v", acts)
	}
	if c.ControllerID() != "" {
		t.Fatalf("hidden viewer must not become controller, got %q", c.ControllerID())
	}
	c.Observe(fb("s1", true, 8_000_000, 5)) // s1 = controller,初始已下发
	pastColdStart(clk)
	if acts := c.Observe(fb("s1", false, 1_000, 500)); len(acts) != 0 {
		t.Fatalf("hidden controller feedback must not act, got %+v", acts)
	}
	// s2 接管(选举重锚冷启动窗):先过窗,再以低 est 反馈驱动降档。
	c.Observe(fb("s2", true, 8_000_000, 5))
	pastColdStart(clk)
	cfg, ok := configOf(t, c.Observe(fb("s2", true, 2_000_000, 5)))
	if !ok || cfg.Bitrate != 1_700_000 || c.ControllerID() != "s2" {
		t.Fatalf("visible viewer must take over globals: ctrl=%q got %+v ok=%v",
			c.ControllerID(), cfg, ok)
	}
}

// controller(优先 viewer)主导:旁观者的拥塞反馈不降档;controller 自己
// 的反馈照常驱动升档。
func TestQoSControllerDominatesSpectatorFeedback(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fb("ctrl", true, 8_000_000, 5)) // controller = "ctrl"
	// 旁观者(更大 sessionID,带宽不触发暂停)拥塞 → 不降档。
	if acts := c.Observe(fb("spec", true, 7_000_000, 500)); len(acts) != 0 {
		t.Fatalf("spectator congestion must not downshift the shared stream, got %+v", acts)
	}
	// controller 的低延迟反馈照常驱动升档(10s 稳定后;I2 ramp 首步
	// 2.3M+max(15%,1Mbps)=3.3M)。
	clk.advance(10 * time.Second)
	cfg, ok := configOf(t, c.Observe(fb("ctrl", true, 8_000_000, 5)))
	if !ok || cfg.Bitrate != 3_300_000 {
		t.Fatalf("controller's own feedback still drives upshift: got %+v ok=%v", cfg, ok)
	}
	if c.ControllerID() != "ctrl" {
		t.Fatalf("controller must stay the priority viewer, got %q", c.ControllerID())
	}
}

// 旁观者带宽 < controller 带宽的 35% → PauseSpectator(恰一次,幂等);
// 隐藏的旁观者不被暂停;controller 自身不受 35% 规则约束。
func TestQoSSpectatorPauseBelow35PercentControllerBandwidth(t *testing.T) {
	c, _ := newQoSTestController()
	c.Observe(fb("ctrl", true, 10_000_000, 5)) // controller est = 10M

	// 36% → 不暂停。
	if acts := c.Observe(fb("spec1", true, 3_600_000, 5)); len(acts) != 0 {
		t.Fatalf("36%% of controller bandwidth must not pause, got %+v", acts)
	}
	// 34% → 暂停(spec2)。
	acts := c.Observe(fb("spec2", true, 3_400_000, 5))
	if len(acts) != 1 || acts[0].Kind != actionPauseSpectator || acts[0].ViewerID != "spec2" {
		t.Fatalf("34%% of controller bandwidth should pause spec2, got %+v", acts)
	}
	// 幂等:再次低带宽反馈 → 不重复暂停。
	if acts := c.Observe(fb("spec2", true, 3_000_000, 5)); len(acts) != 0 {
		t.Fatalf("pause is idempotent, got %+v", acts)
	}
	// 隐藏的旁观者不被暂停。
	if acts := c.Observe(fb("spec3", false, 100_000, 5)); len(acts) != 0 {
		t.Fatalf("hidden spectator must not be paused, got %+v", acts)
	}
	// controller 自身带宽低 → 不自暂停,主导权不变。
	c.Observe(fb("spec4", true, 9_900_000, 5))
	if got := c.ControllerID(); got != "ctrl" {
		t.Fatalf("controller unchanged, got %q", got)
	}
}

// 阶梯(brief Step 2):持续拥塞 → 码率 30% 连降至 500k 下限,再 fps
// {60,30,20,15,10,5} 逐级,再 height {1440,1080,900,720} 逐级(max_w 按
// 当前流宽高比推导:16:9 下 1080→900 得 1600、900→720 得 1280);恢复顺
// 序 bitrate → fps → height,并受初始上限约束(60fps/1440 档不越过初值)。
func TestQoSLadderEscalationAndRecovery(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fb("s1", true, 8_000_000, 5))
	pastColdStart(clk)
	gen := uint64(0) // 帧流 codec epoch 递增器(新代帧模拟)

	congest := func() VideoConfig {
		// M4 修正后的契约:决策按真实反馈节奏(≥1/s)驱动,且每步之间
		// 帧流持续(max_w 步后由新代帧确认重置完成——reset-recovery
		// grace;间隔同时满足 reset-triggering 剪刀的最小间隔)。
		clk.advance(1100 * time.Millisecond)
		cfg, ok := configOf(t, c.Observe(fbCongest("s1", 1_000)))
		if !ok {
			t.Fatalf("congestion step missing an action")
		}
		gen++
		c.FrameObserved(gen) // 新代帧确认(可能刚发生的 max_w 重置)
		return cfg
	}
	// 码率连降(立即通道)到 500k 下限:2.3M→1.61M→1.127M→789k→552k→500k。
	for _, want := range []uint32{1_610_000, 1_127_000, 788_900, 552_230, 500_000} {
		if got := congest(); got.Bitrate != want {
			t.Fatalf("bitrate ladder: got %d want %d", got.Bitrate, want)
		}
	}
	// 下限后码率不再降 → fps 逐级 30→20→15→10→5。
	for _, fps := range []uint32{20, 15, 10, 5} {
		got := congest()
		if got.FPS != fps || got.Bitrate != 500_000 {
			t.Fatalf("fps ladder step to %d: got %+v", fps, got)
		}
	}
	// fps 到底 → height 逐级:maxW 1920→1600(900)→1280(720)。
	for _, maxW := range []uint32{1600, 1280} {
		got := congest()
		if got.MaxW != maxW || got.FPS != 5 || got.Bitrate != 500_000 {
			t.Fatalf("height ladder step to maxW=%d: got %+v", maxW, got)
		}
	}
	// 全底后再拥塞 → 无新动作。
	clk.advance(100 * time.Millisecond)
	if acts := c.Observe(fbCongest("s1", 1_000)); len(acts) != 0 {
		t.Fatalf("all rungs at floor: congestion is a no-op, got %+v", acts)
	}

	// 恢复:干净样本重标稳定窗;10s 后先升码率 —— I2 ramp:500k+
	// max(15%,1Mbps)=1.5M,远目标(est=30M → 15M)由多个 3s 步逼近。
	clk.advance(20 * time.Second)
	if acts := c.Observe(fb("s1", true, 30_000_000, 5)); len(acts) != 0 {
		t.Fatalf("first clean sample only marks stability, got %+v", acts)
	}
	clk.advance(10 * time.Second)
	got, _ := configOf(t, c.Observe(fb("s1", true, 30_000_000, 5)))
	if got.Bitrate != 1_500_000 || got.FPS != 5 || got.MaxW != 1280 {
		t.Fatalf("recovery raises bitrate first (ramped first step 1.5M): got %+v", got)
	}
	// 码率 ramp 到顶(每步 ≤ +max(15%,1Mbps)),到顶后恢复 fps
	// (5→10→15→20→30;60 越过初始 30 上限),再 height(720→900 得
	// 1600、→1080 得 1920;1440 档推导 2560>1920 不再升)。
	var steps []VideoConfig
	for i := 0; i < 40; i++ {
		clk.advance(3500 * time.Millisecond)
		gen++
		c.FrameObserved(gen) // 恢复期帧流持续(上一步若改了 max_w,新代帧确认)
		cfg, ok := configOf(t, c.Observe(fb("s1", true, 30_000_000, 5)))
		if !ok {
			break
		}
		steps = append(steps, cfg)
	}
	want := []VideoConfig{
		{Bitrate: 2_500_000, FPS: 5, MaxW: 1280},
		{Bitrate: 3_500_000, FPS: 5, MaxW: 1280},
		{Bitrate: 4_500_000, FPS: 5, MaxW: 1280},
		{Bitrate: 5_500_000, FPS: 5, MaxW: 1280},
		{Bitrate: 6_500_000, FPS: 5, MaxW: 1280},
		{Bitrate: 7_500_000, FPS: 5, MaxW: 1280},
		{Bitrate: 8_625_000, FPS: 5, MaxW: 1280},
		{Bitrate: 9_918_750, FPS: 5, MaxW: 1280},
		{Bitrate: 11_406_562, FPS: 5, MaxW: 1280},
		{Bitrate: 13_117_546, FPS: 5, MaxW: 1280},
		{Bitrate: 15_000_000, FPS: 5, MaxW: 1280},
		{Bitrate: 15_000_000, FPS: 10, MaxW: 1280},
		{Bitrate: 15_000_000, FPS: 15, MaxW: 1280},
		{Bitrate: 15_000_000, FPS: 20, MaxW: 1280},
		{Bitrate: 15_000_000, FPS: 30, MaxW: 1280},
		{Bitrate: 15_000_000, FPS: 30, MaxW: 1600},
		{Bitrate: 15_000_000, FPS: 30, MaxW: 1920},
	}
	if len(steps) != len(want) {
		t.Fatalf("recovery produced %d steps, want %d: %+v", len(steps), len(want), steps)
	}
	for i, w := range want {
		if steps[i] != w {
			t.Fatalf("recovery step %d: got %+v want %+v", i, steps[i], w)
		}
	}
}

// max_w 推导用当前流宽高比(brief:document the mapping):2560x1440 流
// (16:9)首档 height 1080 → maxW = even(1080×2560/1440) = 1920。
func TestQoSMaxWAspectMapping(t *testing.T) {
	clk := newManualClock()
	c := newQoSController(QoSControllerConfig{
		Initial: VideoConfig{Bitrate: 2_300_000, FPS: 30, MaxW: 2560},
		AspectW: 2560, AspectH: 1440,
		Now: clk.Now,
	})
	c.Observe(fb("s1", true, 8_000_000, 5))
	pastColdStart(clk)
	// 码率 5 步 + fps 4 步打到底(1/s 限速节奏)。
	for i := 0; i < 9; i++ {
		clk.advance(1100 * time.Millisecond)
		c.Observe(fbCongest("s1", 1_000))
	}
	clk.advance(1100 * time.Millisecond)
	cfg, ok := configOf(t, c.Observe(fbCongest("s1", 1_000)))
	if !ok || cfg.MaxW != 1920 {
		t.Fatalf("aspect mapping: first height step 1080 -> maxW 1920, got %+v", cfg)
	}
}

// ---- M4 修正:reset-recovery grace(重置风暴 / 饿死回归)----

// TestQoSCongestionHeldWhileResetInFlight:首档 max_w 剪刀下发后(host 编
// 码器重置在途),持续拥塞反馈一律挂起 —— 不级联第二/第三个 max_w 重
// 置(修前:立即通道绕过限速,每条反馈再剪一档 → 重置风暴 → 恢复 IDR
// 反复作废 → 观众饿死)。grace 由新代帧观测解除;且 reset-triggering 剪
// 刀即便在 grace 解除后也遵守 1s 最小间隔(立即通道不再完全绕过)。
func TestQoSCongestionHeldWhileResetInFlight(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fb("s1", true, 8_000_000, 5))
	pastColdStart(clk)
	// 码率 5 步 + fps 4 步打到底(均非 max_w 决策:不受 grace 影响)。
	for i := 0; i < 9; i++ {
		clk.advance(1100 * time.Millisecond)
		c.Observe(fbCongest("s1", 1_000))
	}
	clk.advance(1100 * time.Millisecond)
	cfg, ok := configOf(t, c.Observe(fbCongest("s1", 1_000)))
	if !ok || cfg.MaxW != 1600 || cfg.FPS != 5 || cfg.Bitrate != 500_000 {
		t.Fatalf("first height step (maxW 1600) missing: got %+v ok=%v", cfg, ok)
	}
	held0 := c.HeldCuts()

	// 重置在途:反复拥塞(高速反馈模拟立即通道)→ 全部挂起,单一边界
	// 动作之后不再有第二个 max_w 重置。
	for i := 0; i < 5; i++ {
		clk.advance(100 * time.Millisecond)
		if acts := c.Observe(fbCongest("s1", 1_000)); len(acts) != 0 {
			t.Fatalf("congestion while reset in flight must be held, got %+v", acts)
		}
	}
	if held := c.HeldCuts() - held0; held != 5 {
		t.Fatalf("held cuts = %d, want 5 (observability counter)", held)
	}
	if got := c.Current().MaxW; got != 1600 {
		t.Fatalf("maxW cascaded to %d while reset in flight, want 1600", got)
	}

	// (b)grace 被新代帧观测解除后,<1s 的立即通道拥塞仍被最小间隔挡住。
	c.FrameObserved(1)
	clk.advance(100 * time.Millisecond)
	if acts := c.Observe(fbCongest("s1", 1_000)); len(acts) != 0 {
		t.Fatalf("immediate path must respect the min interval after a reset-triggering cut, got %+v", acts)
	}

	// 间隔过后:恰再降一档(1600→1280),并再次置 grace(下一档等确认)。
	clk.advance(2 * time.Second)
	cfg, ok = configOf(t, c.Observe(fbCongest("s1", 1_000)))
	if !ok || cfg.MaxW != 1280 {
		t.Fatalf("post-confirmation congestion should step exactly one height rung, got %+v ok=%v", cfg, ok)
	}
	if acts := c.Observe(fbCongest("s1", 1_000)); len(acts) != 0 {
		t.Fatalf("new maxW cut must re-arm the grace, got %+v", acts)
	}
}

// TestQoSGraceClearsOnFrameObservation:FrameObserved 解除 grace 后拥塞控
// 制恢复(下一档可降);ResetConfirmed(host 未收配置 → 无重置在途)同效。
func TestQoSGraceClearsOnFrameObservation(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fb("s1", true, 8_000_000, 5))
	pastColdStart(clk)
	for i := 0; i < 9; i++ {
		clk.advance(1100 * time.Millisecond)
		c.Observe(fbCongest("s1", 1_000))
	}
	clk.advance(1100 * time.Millisecond)
	if cfg, ok := configOf(t, c.Observe(fbCongest("s1", 1_000))); !ok || cfg.MaxW != 1600 {
		t.Fatalf("height step setup failed: %+v ok=%v", cfg, ok)
	}
	// 帧观测解除(grace 清空即恢复;返回值报告确有一次在途挂起被解除)。
	clk.advance(2 * time.Second)
	if !c.FrameObserved(1) {
		t.Fatal("FrameObserved should report clearing an in-flight hold")
	}
	if c.FrameObserved(1) {
		t.Fatal("second FrameObserved must be a no-op (no hold outstanding)")
	}
	if cfg, ok := configOf(t, c.Observe(fbCongest("s1", 1_000))); !ok || cfg.MaxW != 1280 {
		t.Fatalf("post-grace congestion should cut again, got %+v ok=%v", cfg, ok)
	}
	// host 不支持 SET_VIDEO_CONFIG:apply 侧走 ResetConfirmed,grace 不滞留
	// (720 已是 16:9 height 阶梯最底:全底后拥塞本就无动作)。
	c.ResetConfirmed()
	clk.advance(2 * time.Second)
	if acts := c.Observe(fbCongest("s1", 1_000)); len(acts) != 0 {
		t.Fatalf("ladder floor: congestion is a no-op, got %+v", acts)
	}
}

// TestQoSGraceOnlyClearedByNewerCodecEpoch(Fix 1):grace 的解除只认
// 「codec epoch 严格新于置位时刻已见代」的帧。修前 FrameObserved 对
// ANY 帧清位 —— 重置在途时旧代在飞帧(host 重启编码器前产出的尾巴,
// 与置位时刻同代)立刻清掉 grace → 拥塞剪码可再触发一次 max_w 重置 →
// 恢复 IDR 作废 → 重置风暴/livelock 复发(架构评审 6 系统性问题的
// 头号项)。本用例在无 Fix 1 时必败:第 (a) 步的旧代帧会清掉 grace,
// +2s 的拥塞反馈便会剪出第二档 max_w。
func TestQoSGraceOnlyClearedByNewerCodecEpoch(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fb("s1", true, 8_000_000, 5))
	pastColdStart(clk)
	// 帧流已在 codec epoch 5 上运行(grace 置位时 lastCodecEpoch=5)。
	c.FrameObserved(5)
	// 码率 5 步 + fps 4 步打到底(非 max_w 决策),再首档 height 剪刀。
	for i := 0; i < 9; i++ {
		clk.advance(1100 * time.Millisecond)
		c.Observe(fbCongest("s1", 1_000))
	}
	clk.advance(1100 * time.Millisecond)
	if cfg, ok := configOf(t, c.Observe(fbCongest("s1", 1_000))); !ok || cfg.MaxW != 1600 {
		t.Fatalf("height step setup failed: %+v ok=%v", cfg, ok)
	}

	// (a)旧代在飞帧(epoch 5,重置期间仍在产出)不得解除 grace:间隔
	// 已满足(2s > 1s),唯一的护栏就是 epoch 资格线。
	if c.FrameObserved(5) {
		t.Fatal("same-generation frame must not clear the reset grace")
	}
	clk.advance(2 * time.Second)
	if acts := c.Observe(fbCongest("s1", 1_000)); len(acts) != 0 {
		t.Fatalf("stale-generation frame cleared the grace: second max_w reset cascaded, got %+v", acts)
	}
	if got := c.Current().MaxW; got != 1600 {
		t.Fatalf("maxW cascaded to %d on stale-generation confirmation, want 1600", got)
	}

	// (b)新代首帧(epoch 6,host 重置后的恢复 IDR)解除 → 拥塞控制恢复。
	if !c.FrameObserved(6) {
		t.Fatal("new-generation frame should clear the in-flight grace")
	}
	if cfg, ok := configOf(t, c.Observe(fbCongest("s1", 1_000))); !ok || cfg.MaxW != 1280 {
		t.Fatalf("post-confirmation congestion should cut one height rung, got %+v ok=%v", cfg, ok)
	}
}

// 离开的 controller(长时间无反馈)被修剪,主导权让位给在场的 viewer
// (有界状态:陈旧 viewer 表项不无限累积)。
func TestQoSStaleControllerPruned(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fb("s1", true, 8_000_000, 5))
	pastColdStart(clk)
	clk.advance(3 * time.Minute)
	// s1 三分钟无反馈 → 修剪;s2 接管(选举重锚冷启动窗)后过窗,低 est
	// 反馈照常驱动全局。
	c.Observe(fb("s2", true, 8_000_000, 5))
	pastColdStart(clk)
	cfg, ok := configOf(t, c.Observe(fb("s2", true, 2_000_000, 5)))
	if !ok || c.ControllerID() != "s2" || cfg.Bitrate != 1_700_000 {
		t.Fatalf("stale controller must yield: ctrl=%q got %+v ok=%v", c.ControllerID(), cfg, ok)
	}
}

// viewer_feedback 信令帧形态(session.go 解析目标的纯校验)。
func TestViewerFeedbackJSONShape(t *testing.T) {
	raw := `{"type":"viewer_feedback","visible":true,"estimatedBps":4200000,` +
		`"queueMs":12.5,"decodeQueue":2,"rttMs":38.2,"presentedFps":4.9,"e2eP95Ms":83.4}`
	var f viewerFeedbackFrame
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !f.Visible || f.EstimatedBps != 4_200_000 || f.QueueMs != 12.5 ||
		f.DecodeQueue != 2 || f.RTTMs != 38.2 || f.PresentedFps != 4.9 ||
		f.E2EP95Ms != 83.4 {
		t.Fatalf("parsed feedback mismatch: %+v", f)
	}
	// 旧 web:字段缺席 → 0(节奏门旁路 = 今日行为)。
	old := `{"type":"viewer_feedback","visible":true,"estimatedBps":4200000,` +
		`"queueMs":12.5,"decodeQueue":2,"rttMs":38.2}`
	var g viewerFeedbackFrame
	if err := json.Unmarshal([]byte(old), &g); err != nil ||
		g.PresentedFps != 0 || g.E2EP95Ms != 0 {
		t.Fatalf("absent presentedFps must parse to 0: %+v err=%v", g, err)
	}
}

// ---- final-fixwave C1:goodput 形反馈不得单调棘轮 ----

// goodputOf 模拟「goodput 跟随发送速率」的 viewer:est 恒等于当前码率的
// 85%(pacing 令牌桶速率 = 0.85×budget 的直接投影)。修前 0.85×est <
// cur.Bitrate 对这种反馈是恒真式 —— 每拍(1/s 限速)降 ~28% 直到 500k
// 下限,且升档需 est > 1.18×bitrate 永不可达:持续运动 = 必然棘轮。
func goodputOf(c *QoSController) uint64 { return uint64(c.Current().Bitrate) * 85 / 100 }

// TestQoSGoodputShapedFeedbackDoesNotRatchet:est = cur.Bitrate×0.85 的
// goodput 形反馈连续 ≥10 拍不得触发任何降档(码率纹丝不动)。这是 C1 的
// 核心回归:降档判据必须区分「带宽证据」与「发送速率的影子」。
func TestQoSGoodputShapedFeedbackDoesNotRatchet(t *testing.T) {
	c, clk := newQoSTestController()
	// controller 就位即以 goodput 形上报(稳态形状,无跳变边沿)。
	if acts := c.Observe(fb("s1", true, goodputOf(c), 5)); len(acts) != 1 {
		t.Fatalf("first feedback should only emit the initial config, got %+v", acts)
	}
	for i := 0; i < 10; i++ {
		clk.advance(1 * time.Second) // web 侧 1s 反馈节奏
		if acts := c.Observe(fb("s1", true, goodputOf(c), 5)); len(acts) != 0 {
			t.Fatalf("observation %d: goodput-shaped feedback must not act, got %+v", i+1, acts)
		}
	}
	if got := c.Current().Bitrate; got != 2_300_000 {
		t.Fatalf("bitrate ratchered to %d under goodput-shaped feedback, want stable 2.3M", got)
	}
}

// TestQoSSharplyDegradingEstStillDownshiftsPromptly:真实恶化(est 对半跌)
// 必须当拍即降(C1 迟滞不得吞掉真证据);且降档后 goodput 形的等比例回落
// (est 跟到 0.85×新码率)不构成新的降档证据 —— 棘轮在真实 dips 之后同样
// 必须止步。
func TestQoSSharplyDegradingEstStillDownshiftsPromptly(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fb("s1", true, goodputOf(c), 5)) // 稳态:1.955M goodput 形
	pastColdStart(clk)
	clk.advance(1 * time.Second)
	// est 对半跌:977,500 → target 830,875,当拍即降。
	cfg, ok := configOf(t, c.Observe(fb("s1", true, 977_500, 5)))
	if !ok || cfg.Bitrate != 830_875 {
		t.Fatalf("halved est must downshift promptly to 85%% target: got %+v ok=%v", cfg, ok)
	}
	// 降后 goodput 跟随到 0.85×830,875:10 拍零动作(棘轮止步)。
	for i := 0; i < 10; i++ {
		clk.advance(1 * time.Second)
		if acts := c.Observe(fb("s1", true, goodputOf(c), 5)); len(acts) != 0 {
			t.Fatalf("post-dip observation %d: goodput-shaped feedback must not act, got %+v", i+1, acts)
		}
	}
	if got := c.Current().Bitrate; got != 830_875 {
		t.Fatalf("post-dip bitrate ratchered to %d, want stable 830875", got)
	}
}

// ---- final-fixwave I2:升档步幅上限(spec §14.2:每 3s 最多 +15% 或 +1Mbps)----

// TestQoSUpshiftRampStepCap:500k 下限、est=12M 稳定时,首拍升档落在
// cur+max(15%,1Mbps)=1.5M,第二拍 2.5M —— 远目标(10.2M)需多个 3s 步
// 阶梯逼近,绝不一步直达(修前一步 10.2M 会把恢复变成新的突发)。
func TestQoSUpshiftRampStepCap(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fb("s1", true, 8_000_000, 5))
	pastColdStart(clk)
	// 拥塞立即通道 ×0.7 连降 5 拍到 500k 下限(fps/height 未动;Fix 3
	// 起按 1/s 限速节奏推进)。
	for i := 0; i < 5; i++ {
		clk.advance(1100 * time.Millisecond)
		c.Observe(fbCongest("s1", 1_000))
	}
	if got := c.Current().Bitrate; got != 500_000 {
		t.Fatalf("floor setup: bitrate=%d, want 500k", got)
	}
	// est=12M 干净到达:首拍只标记稳定窗。
	clk.advance(10 * time.Second)
	if acts := c.Observe(fb("s1", true, 12_000_000, 5)); len(acts) != 0 {
		t.Fatalf("first clean sample only marks stability, got %+v", acts)
	}
	clk.advance(10 * time.Second)
	cfg, ok := configOf(t, c.Observe(fb("s1", true, 12_000_000, 5)))
	if !ok || cfg.Bitrate != 1_500_000 {
		t.Fatalf("first upshift must step by max(+15%%,+1Mbps) to 1.5M, got %+v ok=%v", cfg, ok)
	}
	clk.advance(3100 * time.Millisecond)
	cfg, ok = configOf(t, c.Observe(fb("s1", true, 12_000_000, 5)))
	if !ok || cfg.Bitrate != 2_500_000 {
		t.Fatalf("second upshift must step to 2.5M, got %+v ok=%v", cfg, ok)
	}
}

// ---- final-fixwave I1:迟到会话 attach 即继承当前配置 ----

// TestStreamQoSAttachAppliesCurrentConfig:controller 已决策(emitted)之后
// 才 attach 的会话,发送器预算立刻等于当前配置码率 × 85% —— 不等下一个
// Action(修前迟到 viewer 以 20Mbps 缺省预算狂奔到下一次决策)。
func TestStreamQoSAttachAppliesCurrentConfig(t *testing.T) {
	clk := newManualClock()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := newStreamQoS(QoSControllerConfig{
		Initial: VideoConfig{Bitrate: 2_300_000, FPS: 30, MaxW: 1920},
		AspectW: 1920,
		AspectH: 1080,
		Now:     clk.Now,
	}, log)
	q.observe(fb("s1", true, 8_000_000, 5))               // controller 就位,初始下发
	clk.advance(qosColdStartGuard + 100*time.Millisecond) // 过冷启动保护窗
	if acts := q.observe(fb("s1", true, 2_000_000, 5)); len(acts) == 0 {
		t.Fatal("est halving should downshift the shared stream")
	}
	pub, err := NewPublisher(PublisherConfig{Log: log})
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	defer pub.Close()
	q.attach("s2", context.Background(), nil, pub)
	want := float64(1_700_000) / 8 * pacingBudgetFraction
	if got := pub.vs.bucket.rate; got != want {
		t.Fatalf("late-join pacing rate=%v, want %v (current 1.7M config × 85%%)", got, want)
	}
}

// 被网络暂停的旁观者不得晋升 controller(review IMPORTANT 2):controller
// 离场后,接任者跳过 paused viewer;全部可见 viewer 都被暂停 → 空位
// (frozen config:无任何决策,持留最后生效配置),后到的好 viewer 可接管。
func TestQoSPausedSpectatorNotPromotedToController(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fb("ctrl", true, 10_000_000, 5)) // controller est = 10M
	// spec(34%)被暂停。
	acts := c.Observe(fb("spec", true, 3_400_000, 5))
	if len(acts) != 1 || acts[0].Kind != actionPauseSpectator {
		t.Fatalf("spec should be paused, got %+v", acts)
	}
	// controller 离场(隐藏):spec 可见但被暂停 → 不得接任(空位)。
	c.Observe(fb("ctrl", false, 10_000_000, 5))
	if got := c.ControllerID(); got != "" {
		t.Fatalf("paused spectator must not be promoted, controller=%q", got)
	}
	// 冻结配置:被暂停 viewer 持续反馈 → 无任何动作(不决策、不再暂停)。
	if acts := c.Observe(fb("spec", true, 1_000, 5)); len(acts) != 0 {
		t.Fatalf("frozen config: paused viewer feedback must not act, got %+v", acts)
	}
	if acts := c.Observe(fb("spec", true, 1_000, 500)); len(acts) != 0 {
		t.Fatalf("frozen config: congestion from a paused viewer must not act, got %+v", acts)
	}
	// 后到的好 viewer 接管并驱动决策(est=2M → target 1.7M < 当前 2.3M)。
	// 接管重锚冷启动窗:先过窗,再以低 est 反馈驱动降档。
	c.Observe(fb("viewer", true, 8_000_000, 5))
	pastColdStart(clk)
	cfg, ok := configOf(t, c.Observe(fb("viewer", true, 2_000_000, 5)))
	if !ok || cfg.Bitrate != 1_700_000 || c.ControllerID() != "viewer" {
		t.Fatalf("healthy viewer must take over globals: ctrl=%q got %+v ok=%v",
			c.ControllerID(), cfg, ok)
	}
	// 在位者若(防御性地)处于 paused 也不保持席位:ctrl 隐藏 → 空位复验。
	c.Observe(fb("viewer", false, 2_000_000, 5))
	if got := c.ControllerID(); got != "" {
		t.Fatalf("vacant seat expected, got %q", got)
	}
}

// ---- fps 天花板(M4 模糊修正:XNC_DESKTOP_QOS_MAX_FPS)----

// newQoSTestControllerMaxFPS 建带天花板的控制器(1920x1080 流形态,
// initial fps 可注入 —— 60 = host 高帧率起流的形态)。
func newQoSTestControllerMaxFPS(initialFPS, maxFPS uint32) (*QoSController, *manualClock) {
	clk := newManualClock()
	c := newQoSController(QoSControllerConfig{
		Initial: VideoConfig{Bitrate: bitrateForWidth(1920), FPS: initialFPS, MaxW: 1920},
		MaxFPS:  maxFPS,
		AspectW: 1920,
		AspectH: 1080,
		Now:     clk.Now,
	})
	return c, clk
}

// 天花板 30 钳掉阶梯顶:60 档消失,首份下发配置把 60 起流的 host 钉到
// 30(升档上限 = 钳后初始值)。
func TestQoSMaxFPSCeilingClampsLadderAndInitial(t *testing.T) {
	c, _ := newQoSTestControllerMaxFPS(60, 30)
	if got := fpsLadderFor(30); len(got) != 5 || got[0] != 30 {
		t.Fatalf("ceiling 30 ladder = %v, want top 30 with 5 steps", got)
	}
	if got := fpsLadderFor(0); len(got) != 6 || got[0] != 60 {
		t.Fatalf("ceiling 0 (off) ladder must be today's, got %v", got)
	}
	cfg, ok := configOf(t, c.Observe(fb("s1", true, 8_000_000, 5)))
	if !ok || cfg.FPS != 30 {
		t.Fatalf("first emitted config must lock a 60fps stream to the 30 ceiling: got %+v ok=%v", cfg, ok)
	}
	if c.Current().FPS != 30 {
		t.Fatalf("controller state must run at 30, got %d", c.Current().FPS)
	}
}

// 升档富余流向画质而非帧率:est 充裕时,天花板 30 的控制器沿 bitrate
// 爬到 15M 上限,期间与之后 fps 恒 30(60 不在阶梯上);bitrate 到顶后
// 走 height 阶梯 —— 绝不出现 fps>30 的决策。
func TestQoSMaxFPSUpshiftSpendsHeadroomOnBitrateNotFps(t *testing.T) {
	c, clk := newQoSTestControllerMaxFPS(60, 30)
	c.Observe(fb("s1", true, 8_000_000, 5)) // initial emit(已钳 30)
	clk.advance(10 * time.Second)           // 稳定窗
	prev := uint32(2_300_000)
	for i := 0; i < 40; i++ {
		clk.advance(3100 * time.Millisecond)
		acts := c.Observe(fb("s1", true, 100_000_000, 5)) // est=100M:一切皆可升
		cfg, ok := configOf(t, acts)
		if !ok {
			continue // 窗口内抑制
		}
		if cfg.FPS != 30 {
			t.Fatalf("step %d: fps must stay at the ceiling, got %+v", i, cfg)
		}
		if cfg.MaxW != 1920 {
			t.Fatalf("step %d: height must not move before bitrate tops out, got %+v", i, cfg)
		}
		if cfg.Bitrate <= prev {
			t.Fatalf("step %d: bitrate must climb, %d -> %d", i, prev, cfg.Bitrate)
		}
		prev = cfg.Bitrate
		if prev == 15_000_000 {
			break
		}
	}
	if prev != 15_000_000 {
		t.Fatalf("bitrate must reach the 15M cap under the fps ceiling, got %d", prev)
	}
	// bitrate 到顶后的下一步:fps 无档(30 已是阶梯顶)→ height 恢复
	// 1920x1080 流上 height 阶梯本就无更高档 → 无动作(全顶)。降档
	// 反向验证:拥塞到底后 fps 沿钳后阶梯 30→20(不是 60→30;Fix 3 起
	// 按 1/s 限速节奏推进)。
	sawFPS := uint32(30)
	for i := 0; i < 12; i++ { // 码率到底 + fps 连降
		clk.advance(1100 * time.Millisecond)
		acts := c.Observe(fbCongest("s1", 100_000_000))
		cfg, ok := configOf(t, acts)
		if !ok {
			continue
		}
		if cfg.FPS == 30 {
			continue // 仍在码率阶梯下行(1/s 一档,fps 未动)
		}
		if cfg.FPS != 20 && cfg.FPS != 15 && cfg.FPS != 10 && cfg.FPS != 5 {
			t.Fatalf("fps downshift must walk the clamped ladder, got %+v", cfg)
		}
		if cfg.Bitrate != qosMinBitrateBps {
			t.Fatalf("fps steps only after bitrate bottoms out, got %+v", cfg)
		}
		if cfg.FPS >= sawFPS {
			t.Fatalf("fps downshift must descend, %d -> %d", sawFPS, cfg.FPS)
		}
		sawFPS = cfg.FPS
	}
}

// 对照:无天花板时同一路径 fps 升到 60(M3 行为不变 = 缺省回归测试)。
func TestQoSNoCeilingKeepsFpsUpshiftTo60(t *testing.T) {
	c, clk := newQoSTestControllerMaxFPS(60, 0)
	c.Observe(fb("s1", true, 8_000_000, 5))
	clk.advance(10 * time.Second)
	prev := uint32(2_300_000)
	saw60 := false
	for i := 0; i < 40 && !saw60; i++ {
		clk.advance(3100 * time.Millisecond)
		acts := c.Observe(fb("s1", true, 100_000_000, 5))
		if cfg, ok := configOf(t, acts); ok {
			if cfg.FPS == 60 {
				saw60 = true
			}
			prev = cfg.Bitrate
		}
	}
	if !saw60 {
		t.Fatalf("no ceiling: fps upshift to 60 must still exist (bitrate=%d)", prev)
	}
}

// env 解析:空/0 = 无;坏值与 >240 拒绝(0 + err);合法值直通。
func TestQoSParseMaxFPS(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want uint32
		ok   bool
	}{
		{"", 0, true}, {"  ", 0, true}, {"0", 0, false},
		{"30", 30, true}, {"60", 60, true}, {"240", 240, true},
		{"241", 0, false}, {"-1", 0, false}, {"abc", 0, false}, {"65536", 0, false},
	} {
		got, err := qosParseMaxFPS(tc.in)
		if tc.ok && (err != nil || got != tc.want) {
			t.Fatalf("qosParseMaxFPS(%q) = %d,%v; want %d,nil", tc.in, got, err, tc.want)
		}
		if !tc.ok && err == nil {
			t.Fatalf("qosParseMaxFPS(%q) must error, got %d", tc.in, got)
		}
	}
}

// ---- M4 节奏门:稀疏流的 queueMs jitter 代理误报 ----
//
// 生产证据(2026-08-30,静态桌面会话):浏览器 controller-viewer 的
// jitterBufferDelay 代理在 ~5fps 稀疏流上恒读 ~半帧间隔(100-200ms+)
// → >100ms 拥塞判据每报必中 → 每秒 30% 剪码 → 分辨率阶梯走底 → 每次
// max_w 变更 = 重置 = 新 epoch = IDR(节点侧 keyframes 以 ~1/s 攀升,
// 观众看到 ~1fps)。门禁:queueMs 仅在呈现节奏健康(presentedFps ≥ 8)
// 或超灾难线(> 3×max(当前目标帧率的期望帧间隔,250ms))时构成拥塞。

// (a)稀疏流:presentedFps=5、queueMs=180(目标 fps=30)→ 连续多拍
// (生产节奏 1/s,30 拍)零降档 —— 码率/fps/max_w 纹丝不动,抑制计数
// 逐拍累积(可观测性)。est=2.7M 保持无带宽证据(恒平 headroom,不触
// C1;目标 2.295M < 2.3M 亦无升档)。
func TestQoSSparseCadenceQueueMsNotCongestion(t *testing.T) {
	c, clk := newQoSTestController()
	if acts := c.Observe(fbCadence("s1", 2_700_000, 5, 5)); len(acts) != 1 {
		t.Fatalf("first feedback should only emit the initial config, got %+v", acts)
	}
	pastColdStart(clk)
	for i := 0; i < 30; i++ {
		clk.advance(1 * time.Second) // web 1s 反馈节奏
		if acts := c.Observe(fbCadence("s1", 2_700_000, 180, 5)); len(acts) != 0 {
			t.Fatalf("sparse observation %d: proxy queueMs must not downshift, got %+v", i+1, acts)
		}
	}
	if got := c.Current(); got != (VideoConfig{Bitrate: 2_300_000, FPS: 30, MaxW: 1920}) {
		t.Fatalf("sparse stream must hold config, got %+v", got)
	}
	if got := c.CadenceHolds(); got != 30 {
		t.Fatalf("cadence holds = %d, want 30 (one per suppressed sample)", got)
	}
}

// (b)健康节奏:presentedFps=25、queueMs=180 → 立即 30% 通道照常(不等
// 稳定窗;Fix 3 起与常规降档共用 1/s 限速 —— 窗口内的第二次拥塞抑制,
// 窗口过后续降)。
func TestQoSHealthyCadenceQueueMsStillCongestion(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fbCadence("s1", 8_000_000, 5, 25))
	pastColdStart(clk)
	cfg, ok := configOf(t, c.Observe(fbCadence("s1", 8_000_000, 180, 25)))
	if !ok || cfg.Bitrate != 2_300_000*7/10 {
		t.Fatalf("healthy-cadence queueMs=180 must cut 30%%: got %+v ok=%v", cfg, ok)
	}
	clk.advance(100 * time.Millisecond)
	if acts := c.Observe(fbCadence("s1", 8_000_000, 200, 25)); len(acts) != 0 {
		t.Fatalf("second cut within 1s must be rate-limited, got %+v", acts)
	}
	clk.advance(1000 * time.Millisecond)
	cfg, ok = configOf(t, c.Observe(fbCadence("s1", 8_000_000, 200, 25)))
	if !ok || cfg.Bitrate != 2_300_000*7/10*7/10 {
		t.Fatalf("post-window congestion should cut again: got %+v ok=%v", cfg, ok)
	}
	if got := c.CadenceHolds(); got != 0 {
		t.Fatalf("healthy cadence must not count holds, got %d", got)
	}
}

// (c)灾难逃逸:presentedFps=2、queueMs=1500 → 即便节奏稀疏也剪(真
// 队列积压远超任何节奏的 3×期望帧间隔);门禁线之下(700ms < 3×250ms
// @30fps 目标)仍被抑制。
func TestQoSCatastrophicQueueEscapesCadenceGuard(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fbCadence("s1", 8_000_000, 5, 2))
	pastColdStart(clk)
	clk.advance(1 * time.Second)
	if acts := c.Observe(fbCadence("s1", 8_000_000, 700, 2)); len(acts) != 0 {
		t.Fatalf("queueMs=700 below the 750ms escape line must stay suppressed, got %+v", acts)
	}
	clk.advance(1 * time.Second)
	cfg, ok := configOf(t, c.Observe(fbCadence("s1", 8_000_000, 1500, 2)))
	if !ok || cfg.Bitrate != 2_300_000*7/10 {
		t.Fatalf("catastrophic queueMs=1500 at 2fps must cut 30%%: got %+v ok=%v", cfg, ok)
	}
	if got := c.CadenceHolds(); got != 1 {
		t.Fatalf("holds = %d, want 1 (the 700ms sample; escape does not count)", got)
	}
}

// (d)字段缺席(旧 web / Firefox 无 rVFC,presentedFps=0)→ 节奏未知:
// queueMs 是 jitter 代理,节奏未知时无法区分代理几何与真排队 —— 单独
// 的越线 queueMs 仅咨询,不剪码(Fix 6;拥塞确认通道 = 发送侧证据,
// Fix 2 的 TestQoSSenderCongestionEvidenceCutsImmediately)。带宽证据路径
// 照常可用(真拥塞最终推高 deadlineDropped/debt 或压低 est)。
func TestQoSAbsentPresentedFpsKeepsLegacyBehavior(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fb("s1", true, 8_000_000, 5)) // 旧 web 形态:无 presentedFps
	pastColdStart(clk)
	for i := 0; i < 3; i++ {
		// 节拍收紧到 100ms:保护窗推进后稳定窗已临近成熟,est=8M 的合法
		// 升档会在 ~2s 后插进来 —— 本用例只考察 advisory queueMs 不剪码,
		// 不考察升档时序。
		clk.advance(100 * time.Millisecond)
		if acts := c.Observe(fb("s1", true, 8_000_000, 180)); len(acts) != 0 {
			t.Fatalf("cadence-unknown queueMs alone must stay advisory, got %+v", acts)
		}
	}
	if got := c.Current().Bitrate; got != 2_300_000 {
		t.Fatalf("cadence-unknown proxy must not cut bitrate, got %d", got)
	}
	if got := c.CadenceHolds(); got != 3 {
		t.Fatalf("holds = %d, want 3 (one per suppressed sample)", got)
	}
}

// ---- Fix 2:发送侧拥塞证据(第一类真证据)----
//
// 架构评审:QoS 只信浏览器 jitter 代理 → 振荡类缺陷的根因。发送器
// (本机令牌桶/入队门)先于浏览器看到真排队:DeadlineDroppedRate(窗口
// 内入队门拒收占比)与 BucketDebtMs(债务折算毫秒)越过阈值即为真拥
// 塞,单独触发立即通道 —— 无需浏览器确认;浏览器 queueMs 降为次要
// 证据(节奏未知时须发送侧确认,见 (d))。

// (e)发送侧证据单独触发:queueMs 完全健康(5ms)+ DeadlineDroppedRate
// 5%(> 1% 阈值)→ 立即 30% 剪码(修前这拍不进拥塞通道,只能等带宽
// 估计跌落)。债务证据(250ms > 200ms)同款;阈值之下不单独触发。
func TestQoSSenderCongestionEvidenceCutsImmediately(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fbCadence("s1", 8_000_000, 5, 25))
	pastColdStart(clk)
	clk.advance(1100 * time.Millisecond)
	fb := fbCadence("s1", 8_000_000, 5, 25) // queueMs=5:浏览器侧毫无拥塞迹象
	fb.Sender.DeadlineDroppedRate = 0.05
	cfg, ok := configOf(t, c.Observe(fb))
	if !ok || cfg.Bitrate != 2_300_000*7/10 {
		t.Fatalf("sender deadline drops must cut 30%% without browser confirmation: got %+v ok=%v", cfg, ok)
	}
	// 债务证据:窗口内 0 拒收但债务 250ms > 200ms → 同样立即剪。
	clk.advance(1100 * time.Millisecond)
	fb = fbCadence("s1", 8_000_000, 5, 25)
	fb.Sender.BucketDebtMs = 250
	cfg, ok = configOf(t, c.Observe(fb))
	if !ok || cfg.Bitrate != 2_300_000*7/10*7/10 {
		t.Fatalf("sender bucket debt must cut 30%%: got %+v ok=%v", cfg, ok)
	}
	// 阈值之下(0.5% / 150ms)= 非拥塞证据,不单独触发。
	clk.advance(1100 * time.Millisecond)
	fb = fbCadence("s1", 8_000_000, 5, 25)
	fb.Sender.DeadlineDroppedRate = 0.005
	fb.Sender.BucketDebtMs = 150
	if acts := c.Observe(fb); len(acts) != 0 {
		t.Fatalf("sub-threshold sender evidence must not act, got %+v", acts)
	}
}

// (f)发送侧确认解锁节奏未知的 queueMs:presentedFps=0 + queueMs=180
// 单独仅咨询((d));同一拍带发送侧证据 → 拥塞成立,立即剪码。
func TestQoSSenderCongestionConfirmsCadenceUnknownQueueMs(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fb("s1", true, 8_000_000, 5))
	pastColdStart(clk)
	fb := fb("s1", true, 8_000_000, 180) // 旧 web 形态:无 presentedFps
	fb.Sender.DeadlineDroppedRate = 0.02
	cfg, ok := configOf(t, c.Observe(fb))
	if !ok || cfg.Bitrate != 2_300_000*7/10 {
		t.Fatalf("sender-confirmed queueMs must cut 30%%: got %+v ok=%v", cfg, ok)
	}
}

// 采集侧:窗口差分 DeadlineDroppedRate(1s 反馈节拍上的 pub.vs.Stats()
// 采样)。首拍只采样(零值);次拍 98 准入 + 2 拒收 → 2%。
func TestSenderCongestionSnapshotWindowRates(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pub, err := NewPublisher(PublisherConfig{Log: log})
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	defer pub.Close()
	e := &sessionEvents{pub: pub}

	if sc := e.senderCongestionSnapshot(); sc.DeadlineDroppedRate != 0 || sc.OverflowFlushes != 0 {
		t.Fatalf("first snapshot must only prime the window, got %+v", sc)
	}
	pub.vs.mu.Lock()
	pub.vs.stats.admitted += 98
	pub.vs.stats.deadlineDropped += 2
	pub.vs.mu.Unlock()
	sc := e.senderCongestionSnapshot()
	if sc.DeadlineDroppedRate < 0.0199 || sc.DeadlineDroppedRate > 0.0201 {
		t.Fatalf("deadlineDroppedRate = %v, want 0.02 (2 of 100 attempted)", sc.DeadlineDroppedRate)
	}
	if sc.OverflowFlushes != 0 {
		t.Fatalf("overflow flushes delta = %d, want 0", sc.OverflowFlushes)
	}
}

// ---- 2026-08-30 XIAOXIN 生产事故回归(冷启动保护 + 底部恢复) ----
//
// 事故时间线(生产日志):会话建立 2.3M/30fps → 第一条 viewer 反馈(同
// 毫秒,TURN relay transport-cc 爬坡中的 est=1.29M)即触发 deep 砍到
// 1.09M → est 随发送量下探继续跌 → 4 拍到 500k 地板 → 首个 IDR 的令牌
// 桶欠债令准入门结构性拒帧(每秒 [pacer] 重钥)→ fps/height 棱梯走底
// (3 次 resolution 重置 = 用户看到的 capture rebuilt 提示)→ 稳定窗被
// 残留证据无限作废,升档永不成形 → 观众 ~1fps。

// TestQoSColdStartGuardHoldsConfigThroughEstRamp:保护窗内,爬坡形态的
// 低 est(逐拍下探)+ 首帧 IDR 的桶欠债(sender 证据)一律不行动 ——
// 初始配置纹丝不动;窗后持续低 est 照常降档(保护是延迟,不是关闭)。
func TestQoSColdStartGuardHoldsConfigThroughEstRamp(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fbCadence("s1", 1_290_000, 5, 30)) // 事故首拍:relay est 爬坡中
	for i, est := range []uint64{900_000, 630_000, 700_000, 800_000, 1_100_000} {
		clk.advance(1 * time.Second)
		f := fbCadence("s1", est, 5, 30)
		f.Sender.BucketDebtMs = 250 // 首 IDR 铺开的令牌欠债(事故现场形态)
		if acts := c.Observe(f); len(acts) != 0 {
			t.Fatalf("cold-start tick %d (est=%d) must not act, got %+v", i+1, est, acts)
		}
	}
	if got := c.Current(); got != (VideoConfig{Bitrate: 2_300_000, FPS: 30, MaxW: 1920}) {
		t.Fatalf("cold-start guard must hold the initial config, got %+v", got)
	}
	// 窗后(t≥8s):持续低 est 的带宽证据照常剪码(85% 目标)。
	clk.advance(3500 * time.Millisecond)
	cfg, ok := configOf(t, c.Observe(fbCadence("s1", 1_100_000, 5, 30)))
	if !ok || cfg.Bitrate != qosTargetBitrate(1_100_000) {
		t.Fatalf("post-guard sustained low est must cut to the 85%% target, got %+v ok=%v", cfg, ok)
	}
}

// TestQoSStabilitySurvivesNoOpFloorEvidence:全底后残留的拥塞证据
// (结构性拒收的影子,无动作可能)不得作废稳定窗 —— 修前「证据存在即清
// 零」,升档锚点被每秒一拍地推迟,底部死锁。第 10 拍换成 no-op 证据后,
// 稳定窗仍按干净拍序列成熟,第 11 拍升档即刻启动。
func TestQoSStabilitySurvivesNoOpFloorEvidence(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fb("s1", true, 8_000_000, 5))
	pastColdStart(clk)
	gen := uint64(0)
	congest := func() {
		clk.advance(1100 * time.Millisecond)
		c.Observe(fbCongest("s1", 1_000))
		gen++
		c.FrameObserved(gen)
	}
	// 连降到底:(500k, 5fps, maxW 1280)。
	for i := 0; i < 11; i++ {
		congest()
	}
	if got := c.Current(); got.Bitrate != 500_000 || got.FPS != 5 || got.MaxW != 1280 {
		t.Fatalf("floor setup: got %+v", got)
	}
	// 9 拍干净(est=30M)锚定稳定窗,第 10 拍换成全底 no-op 拥塞证据
	//(stepDown 无路可走 → 无动作),第 11 拍干净 → 升档即刻启动。
	for i := 0; i < 9; i++ {
		clk.advance(1 * time.Second)
		c.Observe(fb("s1", true, 30_000_000, 5))
	}
	clk.advance(1 * time.Second)
	f := fbCadence("s1", 30_000_000, 5, 30)
	f.Sender.DeadlineDroppedRate = 0.05
	if acts := c.Observe(f); len(acts) != 0 {
		t.Fatalf("floor no-op evidence must not act, got %+v", acts)
	}
	clk.advance(1 * time.Second)
	if _, ok := configOf(t, c.Observe(fb("s1", true, 30_000_000, 5))); !ok {
		t.Fatal("upshift must fire once stability survives the no-op evidence tick")
	}
}

// ---- 缺陷 A:agent 侧 GCC 估计是 est 的主真相源 ----

// newAgentEstTestController 是 TestAgentEstimatePrecedence 的控制器构造
// (brief Step 1;brief 草稿名 newTestController 与 input_test.go 的既有
// helper 撞名,就近改名):manualClock 注入,initial.MaxW 缺省取 aspectW
// (镜像 qosManager 的建流参数)。返回时钟供测试推进节拍(brief 草稿里
// 的 now 局部变量在本套件里是 clk.advance 的机械改写,断言不变)。
func newAgentEstTestController(t *testing.T, initial VideoConfig, aspectW, aspectH uint32) (*QoSController, *manualClock) {
	t.Helper()
	clk := newManualClock()
	if initial.MaxW == 0 {
		initial.MaxW = aspectW
	}
	return newQoSController(QoSControllerConfig{
		Initial:  initial,
		AspectW:  aspectW,
		AspectH:  aspectH,
		Now:      clk.Now,
	}), clk
}

// TestAgentEstimatePrecedence — 有效 est 选择(设计 §2.1):agentEst 新鲜
// (≤qosAgentEstTTL=10s)→ 用之;过期/为 0 → 浏览器 est 兜底。控制器的
// 全部 est 消费面(降档证据、升档目标、controllerBps)都走有效 est。
func TestAgentEstimatePrecedence(t *testing.T) {
	c, clk := newAgentEstTestController(t, VideoConfig{Bitrate: 2_000_000, FPS: 30}, 1920, 1200)

	// agent est 注入:2Mbps 流,GCC 说有 8M 可用 → 升档目标应按 8M 推导。
	c.AgentEstimate("s1", 8_000_000)
	c.Observe(ViewerFeedback{SessionID: "s1", Visible: true, EstimatedBps: 600_000})
	// 浏览器 est 600k(自指 goodput)被 agent est 8M 覆盖:非拥塞拍 +
	// 稳定窗后应升档(85%×8M=6.8M,单步 +1M → 3M),而非被 600k 拖到地板。
	// 每 1s 节拍重申 agent est:GCC 的 OnTargetBitrateChange 只在目标变化
	// 时回调,爬坡期逐拍变化——测试以节拍重放这一形态。
	for i := 0; i < 40; i++ { // 稳定窗 + 多个 3s 升档步
		clk.advance(1 * time.Second)
		c.AgentEstimate("s1", 8_000_000)
		c.Observe(ViewerFeedback{SessionID: "s1", Visible: true, EstimatedBps: 600_000})
	}
	if c.Current().Bitrate <= 2_000_000 {
		t.Fatalf("bitrate = %d, want ramp above initial with agent est 8M", c.Current().Bitrate)
	}

	// agent est 过期:时钟推进 >10s 无新 agent est → 回落浏览器 est。
	clk.advance(11 * time.Second)
	c.Observe(ViewerFeedback{SessionID: "s1", Visible: true, EstimatedBps: 600_000})
	// 此拍起有效 est = 600k:0.85×600k < 2M(已升到的码率)构成降档水平
	// 判据——不为断言具体档位,断言 controllerBps 已回落到浏览器值。
	if c.ControllerBps() != 600_000 {
		t.Fatalf("ControllerBps = %d, want fallback to browser est 600k", c.ControllerBps())
	}
}

// ---- 缺陷 B:reset-grace 确认超时逃逸阀 ----

// saturatingFeedback 构造发送侧真实拥塞的反馈(brief Step 1:
// DeadlineDroppedRate 5% > 1% 阈值 = Fix 2 的第一类真证据,单独即可剪码;
// est 取当前码率的 goodput 形态,带宽证据路径保持安静)。「重置在途但永无
// 新代帧确认」的场景里,它是持续不变的拥塞拍。
func saturatingFeedback(sid string, c *QoSController) ViewerFeedback {
	f := fbCadence(sid, uint64(c.Current().Bitrate)*85/100, 5, 25)
	f.Sender.DeadlineDroppedRate = 0.05
	return f
}

// TestResetGraceEscape — 确认超时逃逸阀(设计 §2.2):重置在途 3s 未被
// 新代帧确认 → urgent 关键帧请求(共 3 次机会:1+2 重发)→ 仍未确认则
// 强制释放 grace,拥塞控制恢复(后续拥塞拍照常剪码,不再 held)。
//(brief 草稿的 now 局部变量是 clk.advance 的机械改写,断言不变 —— 与
// Task 1 的 TestAgentEstimatePrecedence 同一处理;控制器构造复用
// newAgentEstTestController,即 brief 草稿 newTestController 的既有等价
// helper。)
func TestResetGraceEscape(t *testing.T) {
	c, clk := newAgentEstTestController(t, VideoConfig{Bitrate: 4_000_000, FPS: 30}, 1920, 1200)
	// 建流 + 制造一次 height 降档(reset 置位)。
	c.Observe(ViewerFeedback{SessionID: "s1", Visible: true, EstimatedBps: 4_000_000})
	clk.advance(2 * time.Second)
	// 连续强拥塞证据(发送侧真实)直至 max_w 降档触发 resetPending。
	for i := 0; i < 60 && !c.ResetPending(); i++ {
		clk.advance(1 * time.Second)
		c.Observe(saturatingFeedback("s1", c))
	}
	if !c.ResetPending() {
		t.Fatal("expected a pending reset (max_w downshift)")
	}
	// 无新代帧确认:3s 超时 → 首个 urgent 请求;每再 3s 重发,共 2 次。
	var keyReqs int
	for i := 0; i < 7; i++ {
		clk.advance(1 * time.Second)
		acts := c.Observe(saturatingFeedback("s1", c))
		for _, a := range acts {
			if a.Kind == actionRequestKeyframe {
				keyReqs++
			}
		}
	}
	if keyReqs < 1 {
		t.Fatal("expected urgent keyframe request after 3s unconfirmed reset")
	}
	if keyReqs > 1+qosResetConfirmRetries {
		t.Fatalf("keyReqs = %d, want ≤ 1+2", keyReqs)
	}
	// 第 3 次超时后(约 9s):grace 强制释放 —— 拥塞拍产生真实剪码动作
	//(SetVideoConfig 降档)而非 held。
	for i := 0; i < 5; i++ {
		clk.advance(1 * time.Second)
		acts := c.Observe(saturatingFeedback("s1", c))
		for _, a := range acts {
			if a.Kind == actionSetVideoConfig {
				return // 拥塞控制已恢复
			}
		}
	}
	t.Fatal("grace not force-released: congestion cuts still held after 9s+")
}
