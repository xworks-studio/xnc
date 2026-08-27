// qos_controller_test.go — M3 Task 3:QoS 决策表单测(纯逻辑,manualClock
// 确定性驱动;复用 viewer_sender_test.go 的 manualClock)。决策表来源:
// brief Step 1 + controller 裁决 4 ——
//
//	immediate 30% downshift on queueAge>100ms(绕过 1/s 限速)
//	no upshift before 10 stable seconds(up ≤1/3s after stability)
//	hidden viewers excluded from global decisions
//	controller (priority viewer) dominates
//	spectator paused below 35% of controller bandwidth
package desktop

import (
	"encoding/json"
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

// queueAge>100ms → 立即 30% 降码率(不要求稳定窗口、不触发 1/s 限速)。
func TestQoSImmediateDownshiftOnQueueAge(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fb("s1", true, 8_000_000, 5)) // controller 就位(初始下发)

	cfg, ok := configOf(t, c.Observe(fb("s1", true, 8_000_000, 150)))
	if !ok || cfg.Bitrate != 2_300_000*7/10 {
		t.Fatalf("queueAge=150ms should cut bitrate to 70%%: got %+v ok=%v", cfg, ok)
	}
	// 立即通道绕过 1/s 限速:100ms 后再次拥塞 → 再降 30%。
	clk.advance(100 * time.Millisecond)
	cfg, ok = configOf(t, c.Observe(fb("s1", true, 8_000_000, 200)))
	if !ok || cfg.Bitrate != 2_300_000*7/10*7/10 {
		t.Fatalf("immediate cut bypasses the 1/s limiter: got %+v ok=%v", cfg, ok)
	}
}

// 非拥塞降档(带宽估计跌落,目标 85%×est < 当前)受 1/s 限速。
func TestQoSDownRateLimitedPerSecond(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fb("s1", true, 8_000_000, 5)) // initial: 2.3M

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

// 10s 稳定前不升档;稳定后 up ≤1/3s;目标 = 85% × est,上限 15Mbps;
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
	if !ok || cfg.Bitrate != 6_800_000 {
		t.Fatalf("upshift to 85%% of estimate at 10s stable: got %+v ok=%v", cfg, ok)
	}
	// up ≤1/3s:11s 时 est 升(→ target 8.5M)但窗口内 → 抑制。
	clk.advance(1 * time.Second)
	if acts := c.Observe(fb("s1", true, 10_000_000, 5)); len(acts) != 0 {
		t.Fatalf("up within 3s must be suppressed, got %+v", acts)
	}
	clk.advance(2100 * time.Millisecond) // t=13.1s
	cfg, ok = configOf(t, c.Observe(fb("s1", true, 10_000_000, 5)))
	if !ok || cfg.Bitrate != 8_500_000 {
		t.Fatalf("up after 3s window: got %+v ok=%v", cfg, ok)
	}
	// 上限钳制:est=100Mbps → target 15M。
	clk.advance(4 * time.Second)
	cfg, ok = configOf(t, c.Observe(fb("s1", true, 100_000_000, 5)))
	if !ok || cfg.Bitrate != 15_000_000 {
		t.Fatalf("bitrate ceiling 15Mbps: got %+v ok=%v", cfg, ok)
	}
	// 拥塞:立即 30% 降 + 稳定窗口重置(10s 内不再升档)。
	clk.advance(100 * time.Millisecond)
	cfg, _ = configOf(t, c.Observe(fb("s1", true, 100_000_000, 300)))
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
	c, _ := newQoSTestController()
	if acts := c.Observe(fb("s1", false, 8_000_000, 500)); len(acts) != 0 {
		t.Fatalf("hidden viewer feedback must not act, got %+v", acts)
	}
	if c.ControllerID() != "" {
		t.Fatalf("hidden viewer must not become controller, got %q", c.ControllerID())
	}
	c.Observe(fb("s1", true, 8_000_000, 5)) // s1 = controller,初始已下发
	if acts := c.Observe(fb("s1", false, 1_000, 500)); len(acts) != 0 {
		t.Fatalf("hidden controller feedback must not act, got %+v", acts)
	}
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
	// controller 的低延迟反馈照常驱动升档(10s 稳定后)。
	clk.advance(10 * time.Second)
	cfg, ok := configOf(t, c.Observe(fb("ctrl", true, 8_000_000, 5)))
	if !ok || cfg.Bitrate != 6_800_000 {
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

	congest := func() VideoConfig {
		clk.advance(100 * time.Millisecond)
		cfg, ok := configOf(t, c.Observe(fb("s1", true, 1_000, 500)))
		if !ok {
			t.Fatalf("congestion step missing an action")
		}
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
	if acts := c.Observe(fb("s1", true, 1_000, 500)); len(acts) != 0 {
		t.Fatalf("all rungs at floor: congestion is a no-op, got %+v", acts)
	}

	// 恢复:干净样本重标稳定窗;10s 后先升码率到 target(est=30M → 15M)。
	clk.advance(20 * time.Second)
	if acts := c.Observe(fb("s1", true, 30_000_000, 5)); len(acts) != 0 {
		t.Fatalf("first clean sample only marks stability, got %+v", acts)
	}
	clk.advance(10 * time.Second)
	got, _ := configOf(t, c.Observe(fb("s1", true, 30_000_000, 5)))
	if got.Bitrate != 15_000_000 || got.FPS != 5 || got.MaxW != 1280 {
		t.Fatalf("recovery raises bitrate first: got %+v", got)
	}
	// 码率到顶后恢复 fps(5→10→15→20→30;60 越过初始 30 上限),再 height
	// (720→900 得 1600、→1080 得 1920;1440 档推导 2560>1920 不再升)。
	var steps []VideoConfig
	for i := 0; i < 7; i++ {
		clk.advance(4 * time.Second)
		cfg, ok := configOf(t, c.Observe(fb("s1", true, 30_000_000, 5)))
		if !ok {
			break
		}
		steps = append(steps, cfg)
	}
	want := []VideoConfig{
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
	// 码率 5 步 + fps 4 步打到底。
	for i := 0; i < 9; i++ {
		clk.advance(100 * time.Millisecond)
		c.Observe(fb("s1", true, 1_000, 500))
	}
	clk.advance(100 * time.Millisecond)
	cfg, ok := configOf(t, c.Observe(fb("s1", true, 1_000, 500)))
	if !ok || cfg.MaxW != 1920 {
		t.Fatalf("aspect mapping: first height step 1080 -> maxW 1920, got %+v", cfg)
	}
}

// 离开的 controller(长时间无反馈)被修剪,主导权让位给在场的 viewer
// (有界状态:陈旧 viewer 表项不无限累积)。
func TestQoSStaleControllerPruned(t *testing.T) {
	c, clk := newQoSTestController()
	c.Observe(fb("s1", true, 8_000_000, 5))
	clk.advance(3 * time.Minute)
	// s1 三分钟无反馈 → 修剪;s2 的反馈应接管并驱动全局。
	cfg, ok := configOf(t, c.Observe(fb("s2", true, 2_000_000, 5)))
	if !ok || c.ControllerID() != "s2" || cfg.Bitrate != 1_700_000 {
		t.Fatalf("stale controller must yield: ctrl=%q got %+v ok=%v", c.ControllerID(), cfg, ok)
	}
}

// viewer_feedback 信令帧形态(session.go 解析目标的纯校验)。
func TestViewerFeedbackJSONShape(t *testing.T) {
	raw := `{"type":"viewer_feedback","visible":true,"estimatedBps":4200000,` +
		`"queueMs":12.5,"decodeQueue":2,"rttMs":38.2}`
	var f viewerFeedbackFrame
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !f.Visible || f.EstimatedBps != 4_200_000 || f.QueueMs != 12.5 ||
		f.DecodeQueue != 2 || f.RTTMs != 38.2 {
		t.Fatalf("parsed feedback mismatch: %+v", f)
	}
}

// 被网络暂停的旁观者不得晋升 controller(review IMPORTANT 2):controller
// 离场后,接任者跳过 paused viewer;全部可见 viewer 都被暂停 → 空位
// (frozen config:无任何决策,持留最后生效配置),后到的好 viewer 可接管。
func TestQoSPausedSpectatorNotPromotedToController(t *testing.T) {
	c, _ := newQoSTestController()
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
