# 桌面流 QoS 三缺陷修复 — 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复三缺陷：A) agent 侧 GCC 带宽估计替换干涸的浏览器估计源（治 500kbps 地板锁死）；B) reset-grace 确认超时逃逸阀（治整流冻结）；C) PauseSpectator 恢复通道 + viewer 表项即时清理（治 reload 冻结）。

**Architecture:** pion 现成件 `interceptor/pkg/cc` + `pkg/gcc`（SendSideBWE）做 per-会话带宽估计，`OnTargetBitrateChange` 回调注入 streamQoS，控制器 est 选择 agent 优先/浏览器兜底；逃逸阀与恢复是 qos_controller 纯逻辑扩展（时钟注入可测）。全部 agent Go 侧，native 只调查不改。

**Tech Stack:** Go（pion/webrtc v4.2.18 + interceptor v0.1.47，模块缓存已含 gcc/cc 包）；既有测试面 qos_controller_test.go（manualClock）+ session_loopback_test.go。

**Spec:** `docs/superpowers/specs/2026-09-06-desktop-qos-bandwidth-fix-design.md`（含诊断证据链，实施者必读 §1）

## Global Constraints

- 代码注释中文、提交信息英文 conventional commits；每任务独立提交。
- 控制器（qos_controller.go）保持纯逻辑无 I/O（时钟注入；线程安全由外层 streamQoS.mu 串行）——新增字段/方法不得违反。
- QoS 阶梯参数（500k–15M、85%、1.18×、12s 稳定窗等）**全部保持现值**——本周期只修"est 真相来源"与"闩死"，不做参数调优。
- web/server/proto 零改动；native 零改动（Task 4 只调查）。
- agent 测试：`cd agent && go test ./desktop/ -count=1`（Windows 本机直接跑）；构建 `cd agent && go build ./...`。
- 既有用例（qos_controller_test 全量、session_loopback、reattach_loopback）必须保持绿——行为变化只允许新增路径。

---

### Task 1: agent 侧 GCC 估计器 + 控制器 est 选择（缺陷 A）

**Files:**
- Modify: `agent/desktop/transport.go`（cc/gcc 拦截器装配 + per-会话估计回调）
- Modify: `agent/desktop/session.go`（streamQoS.AgentEstimate 入口）
- Modify: `agent/desktop/qos_controller.go`（viewer 表项 agentEst + 有效 est 选择）
- Test: `agent/desktop/qos_controller_test.go`（追加 est 选择用例）
- Test: `agent/desktop/transport_test.go`（如需装配冒烟；既有用例回归）

**Interfaces:**
- Consumes: `newDesktopAPI`（transport.go:120-125 现有 RegisterDefaultInterceptors + ConfigureTWCCHeaderExtensionSender 装配点）；`streamQoS`（session.go:125，mu 串行）；`bitrateForWidth`；`qosMaxBitrateBps`（15M）。pion：`github.com/pion/interceptor/pkg/cc`、`pkg/gcc`（SendSideBWE/InitialBitrate/MaxBitrate/OnTargetBitrateChange；官方示例 `webrtc/v4@v4.2.18/examples/bandwidth-estimation-from-disk/main.go`）。
- Produces: `(*streamQoS).AgentEstimate(sessionID string, bps int)`（session.go 新方法，安全并发）；控制器 Observe 的有效 est 语义：`agentEst 新鲜(≤10s) → agentEst，否则 fb.EstimatedBps`——Task 2/3 的用例依赖此语义。

**关键设计决策（已定，勿改）：**
- **per-会话拦截器注册表**：为保证 `OnTargetBitrateChange` 回调能闭包捕获 sessionID（共享 registry 的 OnNewPeerConnection id 与我们会话 id 无对应关系，多 viewer 时无法归位），采用 **每个 viewer 会话独立 `interceptor.Registry` + `webrtc.NewAPI(...)`**；该 registry 里 Add：cc 拦截器（工厂闭包捕获 sessionID 与回调）+ 既有默认件（NACK/SenderReport/TWCC 头扩展，即对每会话复刻 newDesktopAPI 的装配）。共享 api（若现行代码是单例）保留给非桌面路径不动。
- **est 作用域**：agentEst 记在 viewer 表项（per-viewer），决策用 controller 的有效 est；旁观者 35% 暂停比较也用各自有效 est（替换现 `fb.EstimatedBps`）。
- **InitialBitrate**：取该流 `streamQoS.Current().Bitrate`（流未建时 `bitrateForWidth(1920)`）；**MaxBitrate**：`qosMaxBitrateBps`。

- [ ] **Step 1: 控制器 est 选择（先写失败测试）**

qos_controller_test.go 追加（沿用既有 manualClock 风格）：

```go
// TestAgentEstimatePrecedence — 有效 est 选择（设计 §2.1）:agentEst 新鲜
//（≤qosAgentEstTTL=10s）→ 用之；过期/为 0 → 浏览器 est 兜底。控制器的
// 全部 est 消费面（降档证据、升档目标、controllerBps）都走有效 est。
func TestAgentEstimatePrecedence(t *testing.T) {
	c := newTestController(t, VideoConfig{Bitrate: 2_000_000, FPS: 30}, 1920, 1200)
	now := c.now()

	// agent est 注入：2Mbps 流,GCC 说有 8M 可用 → 升档目标应按 8M 推导。
	c.AgentEstimate("s1", 8_000_000)
	c.Observe(ViewerFeedback{SessionID: "s1", Visible: true, EstimatedBps: 600_000})
	// 浏览器 est 600k(自指 goodput)被 agent est 8M 覆盖:非拥塞拍 +
	// 稳定窗后应升档(85%×8M=6.8M,单步 +1M → 3M),而非被 600k 拖到地板。
	for i := 0; i < 40; i++ { // 稳定窗 + 多个 3s 升档步
		now = now.Add(1 * time.Second)
		c.Observe(ViewerFeedback{SessionID: "s1", Visible: true, EstimatedBps: 600_000})
	}
	if c.Current().Bitrate <= 2_000_000 {
		t.Fatalf("bitrate = %d, want ramp above initial with agent est 8M", c.Current().Bitrate)
	}

	// agent est 过期:时钟推进 >10s 无新 agent est → 回落浏览器 est。
	now = now.Add(11 * time.Second)
	c.Observe(ViewerFeedback{SessionID: "s1", Visible: true, EstimatedBps: 600_000})
	// 此拍起有效 est = 600k:0.85×600k < 2M(已升到的码率)构成降档水平
	// 判据——不为断言具体档位,断言 controllerBps 已回落到浏览器值。
	if c.ControllerBps() != 600_000 {
		t.Fatalf("ControllerBps = %d, want fallback to browser est 600k", c.ControllerBps())
	}
}
```

（`newTestController`/`ControllerBps` 若不存在，按既有测试的构造方式建 helper；`ControllerBps()` 为本任务新增导出观测方法，返回最近一拍 decide 用的有效 est。）

Run: `cd agent && go test ./desktop/ -run TestAgentEstimatePrecedence -count=1`
Expected: FAIL——AgentEstimate/agentEst 不存在。

- [ ] **Step 2: 控制器实现**

qos_controller.go：`qosViewer` 增 `agentEst uint64; agentEstAt time.Time`；`QoSController` 增常量 `qosAgentEstTTL = 10 * time.Second`。新方法（外层 streamQoS.mu 下调用，保持纯逻辑）：

```go
// AgentEstimate 记录该 viewer 会话的 agent 侧 GCC 目标码率(pion
// SendSideBWE OnTargetBitrateChange;设计 §2.1)。浏览器侧
// availableIncomingBitrate 对 pion 发送流不暴露(2026-09-06 生产实测),
// 此值是 est 的主真相源;TTL 内新鲜,过期回落浏览器 est。
func (c *QoSController) AgentEstimate(sessionID string, bps int) {
	if sessionID == "" || bps <= 0 {
		return
	}
	if v := c.viewers[sessionID]; v != nil {
		v.agentEst, v.agentEstAt = uint64(bps), c.now()
	}
}

// effectiveEst 该 viewer 的有效带宽估计:agent est 新鲜则用之,否则
// 浏览器 est 兜底(设计 §2.1)。
func (v *qosViewer) effectiveEst(now time.Time) uint64 {
	if v.agentEst > 0 && now.Sub(v.agentEstAt) <= qosAgentEstTTL {
		return v.agentEst
	}
	return v.bps
}
```

Observe 改造：`v.bps = fb.EstimatedBps` 保留（原始浏览器值存档）；旁观者暂停比较 `fb.EstimatedBps` → `v.effectiveEst(now)`；`decide(fb, now)` 开头 `c.controllerBps = v.effectiveEst` —— 具体：decide 内新增 `est := v.effectiveEst(now)`（需把 viewer 或 est 传入 decide），后续 `fb.EstimatedBps` 的全部消费点（headroom/decay/deep/target/lastEstBps）替换为 `est`。新增观测方法 `ControllerBps() uint64`。**注意**：`decide` 签名从 `decide(fb ViewerFeedback, now)` 调整为 `decide(fb ViewerFeedback, v *qosViewer, now)`（或等效传 est），既有调用点同步。

Run: Step 1 测试 PASS + `go test ./desktop/ -run 'TestQoS|TestController' -count=1` 既有全绿（既有用例全部走浏览器 est 无 agent 注入 → effectiveEst 兜底 = 原行为，逐字节等价）。

- [ ] **Step 3: streamQoS 入口 + 装配**

session.go（streamQoS，mu 串行）：

```go
// AgentEstimate 会话的 agent 侧 GCC 估计回调入口(transport 层
// OnTargetBitrateChange 直调;任意 goroutine 安全)。
func (q *streamQoS) AgentEstimate(sessionID string, bps int) {
	q.mu.Lock()
	q.ctrl.AgentEstimate(sessionID, bps)
	q.mu.Unlock()
}
```

transport.go：在 per-会话 PC 构造路径（若现行 newDesktopAPI 为共享单例：新增 `newSessionAPI(qos *streamQoS, sessionID string) *webrtc.API`，内部 MediaEngine + per-会话 Registry；装配顺序照官方示例——cc 拦截器 Add 在 ConfigureTWCCHeaderExtensionSender 与 RegisterDefaultInterceptors **之前**；cc 工厂闭包捕获 sessionID，回调 `qos.AgentEstimate(sessionID, bps)`；InitialBitrate 取 `qos.Current().Bitrate`（流未建回退 `bitrateForWidth(1920)`），MaxBitrate 取 `qosMaxBitrateBps`）。若装配中发现 pion cc 拦截器与 RegisterDefaultInterceptors 有重复注册冲突（NACK/TWCC 二次 Add 报错），按官方示例顺序调整并在提交信息注明；共享 newDesktopAPI 保留给非桌面路径。

- [ ] **Step 4: 回归 + 提交**

Run: `cd agent && go build ./... && go vet ./... && go test ./desktop/ -count=1`
Expected: 全绿。

```bash
git add agent/desktop/transport.go agent/desktop/session.go agent/desktop/qos_controller.go agent/desktop/qos_controller_test.go agent/desktop/transport_test.go
git commit -m "feat(agent): agent-side GCC bandwidth estimation replaces dry browser estimate source"
```

---

### Task 2: reset-grace 确认超时逃逸阀（缺陷 B）

**Files:**
- Modify: `agent/desktop/qos_controller.go`
- Modify: `agent/desktop/session.go`（应用 actionRequestKeyframe）
- Test: `agent/desktop/qos_controller_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的控制器结构；`resetPending/graceCodecEpoch/lastCodecEpoch/heldCuts`（qos_controller.go:277-280）；`FrameObserved`（:506）；KeyframeCoordinator 的 epoch 变更 urgent 请求路径（qos_min.go:90 注释：QoS 动作直接走 coordinator API）。
- Produces: 新 Action `actionRequestKeyframe`（Kind=3, ViewerID 空=流级）；`FrameObserved` 语义不变；常量 `qosResetConfirmTimeout = 3 * time.Second`、`qosResetConfirmRetries = 2`。

- [ ] **Step 1: 失败测试**

```go
// TestResetGraceEscape — 确认超时逃逸阀（设计 §2.2）:重置在途 3s 未被
// 新代帧确认 → urgent 关键帧请求（共 3 次机会:1+2 重发）→ 仍未确认则
// 强制释放 grace,拥塞控制恢复（后续拥塞拍照常剪码,不再 held）。
func TestResetGraceEscape(t *testing.T) {
	c := newTestController(t, VideoConfig{Bitrate: 4_000_000, FPS: 30}, 1920, 1200)
	now := c.now()
	// 建流 + 制造一次 height 降档（reset 置位）。
	c.Observe(ViewerFeedback{SessionID: "s1", Visible: true, EstimatedBps: 4_000_000})
	now = now.Add(2 * time.Second)
	// 连续强拥塞证据（发送侧真实）直至 max_w 降档触发 resetPending。
	for i := 0; i < 60 && !c.ResetPending(); i++ {
		now = now.Add(1 * time.Second)
		c.Observe(saturatingFeedback("s1", c))
	}
	if !c.ResetPending() {
		t.Fatal("expected a pending reset (max_w downshift)")
	}
	// 无新代帧确认:3s 超时 → 首个 urgent 请求;每再 3s 重发,共 2 次。
	var keyReqs int
	for i := 0; i < 7; i++ {
		now = now.Add(1 * time.Second)
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
	// 第 3 次超时后（约 9s）:grace 强制释放 —— 拥塞拍产生真实剪码动作
	//（SetVideoConfig 降档）而非 held。
	for i := 0; i < 5; i++ {
		now = now.Add(1 * time.Second)
		acts := c.Observe(saturatingFeedback("s1", c))
		for _, a := range acts {
			if a.Kind == actionSetVideoConfig {
				return // 拥塞控制已恢复
			}
		}
	}
	t.Fatal("grace not force-released: congestion cuts still held after 9s+")
}
```

（`newTestController`/`saturatingFeedback`/`ResetPending` 按既有测试 helper 风格补齐：saturatingFeedback 构造发送侧真实拥塞的 ViewerFeedback（DeadlineDroppedRate>1% 或桶债务>200ms——按 Sender 结构既有字段）；`ResetPending()` 新增只读观测方法。若既有用例已有等价 helper（重置风暴测试 M4 有既阵），复用之。）

Run: `cd agent && go test ./desktop/ -run TestResetGraceEscape -count=1`
Expected: FAIL——无逃逸阀,拥塞拍永远 held。

- [ ] **Step 2: 实现**

qos_controller.go：`QoSController` 增 `resetAt time.Time; resetRetries uint32`（resetPending 置位处记录 `resetAt=now` 并清零 retries——置位点在 max_w 降档决策处，找到既有 `resetPending = true` 赋值行同步两字段）。Observe 的拥塞分支 `if c.resetPending { heldCuts++; return nil }` 改为：

```go
		if c.resetPending {
			// 逃逸阀(设计 §2.2):确认超时先重发 urgent 关键帧请求
			//(≤qosResetConfirmRetries 次),再超时强制释放 grace——
			// 单次 native 编码器失败不得劫持整条流的拥塞控制
			//(2026-09-06 生产:held 15+ 分钟,整流冻结)。
			if now.Sub(c.resetAt) >= qosResetConfirmTimeout {
				if c.resetRetries < qosResetConfirmRetries {
					c.resetRetries++
					c.resetAt = now // 重臂:下一次超时再判
					c.heldCuts++
					return []Action{{Kind: actionRequestKeyframe}}
				}
				c.resetPending = false // force-release:拥塞控制恢复
				// 落回本拍拥塞证据,继续常规剪码(不 return,直落下方剪码路径)。
			} else {
				c.heldCuts++
				return nil
			}
		}
```

（actionRequestKeyframe 常量加到 action 枚举；`actionRequestKeyframe` 的应用在 session.go 的 Action 分派处：调 KeyframeCoordinator 的 urgent 请求接口——与 SET_VIDEO_CONFIG 分派同型,查既有 `case actionPauseSpectator` 分派点旁新增。FrameObserved 的正常确认路径不动（既有强新代帧语义保留）。）

Run: Step 1 PASS + 既有 M4 重置风暴用例（TestResetRecovery*/重置相关）全绿——正常确认在 3s 内到达时逃逸阀零介入。

- [ ] **Step 3: 回归 + 提交**

Run: `cd agent && go build ./... && go test ./desktop/ -count=1`

```bash
git add agent/desktop/qos_controller.go agent/desktop/qos_controller_test.go agent/desktop/session.go
git commit -m "feat(agent): reset-grace confirmation timeout escape - urgent rekey then force-release"
```

---

### Task 3: PauseSpectator 恢复通道 + viewer 表项即时清理（缺陷 C）

**Files:**
- Modify: `agent/desktop/qos_controller.go`
- Modify: `agent/desktop/session.go`（detach 清表 + actionResumeSpectator 分派）
- Test: `agent/desktop/qos_controller_test.go`（追加）

**Interfaces:**
- Consumes: `qosViewer.paused`；`promote()`/`prune()`（选举与修剪既有实现）；`streamQoS.detach`（session.go:188）；既有 `actionPauseSpectator` 分派点（暂停发送器 + 发稳定态信令）。
- Produces: `actionResumeSpectator`（Kind=4, ViewerID）；`(*QoSController).DetachViewer(sessionID)`（会话收线即删表项 + 必要时重选举）；恢复判据常量 `qosResumeFraction = 0.50`、`qosResumeSustain = 5 * time.Second`。

- [ ] **Step 1: 失败测试（三条路径）**

```go
// TestSpectatorResume — 恢复通道三路径（设计 §2.3）:
//  1. detach 即删表项:会话收线后 viewer 不再参与选举/比对
//     （reload 冷启动不再撞 2 分钟陈旧 controller 表项）。
//  2. 接任解暂停:controller 离场,暂停态 viewer 接任 → 立即恢复。
//  3. est 恢复解暂停:暂停中的 spectator 连续 5s est > 50%×controller
//     → 恢复（带迟滞）。
func TestSpectatorResume(t *testing.T) {
	// 路径 1:detach 删表项。
	c := newTestController(t, VideoConfig{Bitrate: 2_000_000, FPS: 30}, 1920, 1200)
	c.Observe(ViewerFeedback{SessionID: "ctrl", Visible: true, EstimatedBps: 4_000_000})
	c.DetachViewer("ctrl")
	if c.ControllerID() == "ctrl" {
		t.Fatal("detached viewer still controller")
	}

	// 路径 2+3:旁观者被暂停 → 接任/est 恢复 → Resume 动作恰好一次。
	c2 := newTestController(t, VideoConfig{Bitrate: 2_000_000, FPS: 30}, 1920, 1200)
	now := c2.now()
	c2.Observe(ViewerFeedback{SessionID: "ctrl", Visible: true, EstimatedBps: 4_000_000})
	now = now.Add(1 * time.Second)
	acts := c2.Observe(ViewerFeedback{SessionID: "spec", Visible: true, EstimatedBps: 100_000})
	if !hasAction(acts, actionPauseSpectator) {
		t.Fatal("expected spectator pause (100k < 35% of 4M)")
	}
	// 路径 3:est 恢复(>50%×4M)持续 5s → Resume。
	var resumed int
	for i := 0; i < 8; i++ {
		now = now.Add(1 * time.Second)
		for _, a := range c2.Observe(ViewerFeedback{SessionID: "spec", Visible: true, EstimatedBps: 3_000_000}) {
			if a.Kind == actionResumeSpectator {
				resumed++
			}
		}
	}
	if resumed != 1 {
		t.Fatalf("resume actions = %d, want exactly 1 (hysteresis)", resumed)
	}
	// 路径 2:controller 离场(detach),暂停未恢复的另一 viewer 接任 →
	// 接任即解暂停。
	c2.Observe(ViewerFeedback{SessionID: "spec2", Visible: true, EstimatedBps: 200_000})
	c2.DetachViewer("ctrl")
	acts = c2.Observe(ViewerFeedback{SessionID: "spec2", Visible: true, EstimatedBps: 200_000})
	_ = acts
	if !c2.ResumeApplied("spec2") { // 或断言 acts 含 Resume;spec2 接任后 paused 位必须为 false
		t.Fatal("handover must clear pause on the new controller")
	}
```

（helper `hasAction`/`ResumeApplied` 按既有测试风格落——`ResumeApplied` 也可换成导出的 `IsPaused(sessionID)` 观测方法,断言接任后 false。）

Run: `cd agent && go test ./desktop/ -run TestSpectatorResume -count=1`
Expected: FAIL。

- [ ] **Step 2: 实现**

qos_controller.go：

```go
// DetachViewer 会话收线即删 viewer 表项(设计 §2.3):修前只靠 lastSeen
// 2 分钟修剪——reload 后新 viewer 冷启动 est 与陈旧 controller 表项
// 比对,35% 暂停误触发率极高且无恢复。删除后必要时重选举(promote)。
func (c *QoSController) DetachViewer(sessionID string) {
	delete(c.viewers, sessionID)
	if c.controllerID == sessionID {
		c.controllerID = ""
		c.promote() // 剩余可见 viewer 接任;接任者若暂停态 → 解除
		if v := c.viewers[c.controllerID]; v != nil && v.paused {
			v.paused = false
			v.resumeSeq++ // 接任解暂停:由 Observe 下拍发 Resume?——
			// 否:直接由 detach 返回 Action 不合时序(外层在收线路径)。
			// 简化:置 paused=false + pendingResume 标记,下拍 Observe 补发。
		}
	}
}
```

（实现自由度：接任解暂停可在 DetachViewer 置标记、由下一个 Observe 拍返回 `actionResumeSpectator`——发送器恢复动作在 session 分派处执行；或导出 `PendingResumes()` 由 session 收线路径直接应用。**语义钉死**：接任者的 paused 必须为 false 且发送器恢复恰好一次。est 恢复解暂停：`qosViewer` 增 `resumeSince time.Time`,旁观者分支里 `if v.paused && effectiveEst > qosResumeFraction*controllerBps` 累计 `resumeSince`（首拍置位）,持续 ≥`qosResumeSustain` → `v.paused=false` + 返回 `actionResumeSpectator`(恰一次,幂等)。`actionResumeSpectator` 常量 + session.go 分派:调既有发送器恢复路径(PauseSpectator 的对偶——查 ViewerSender 的 pause/resume 接口;若无 resume 接口则新增最小实现)。）

session.go：`detach()`（session.go:188 附近）在既有清理后追加 `q.ctrl.DetachViewer(sessionID)`（mu 内）。

Run: Step 1 PASS + 既有全绿。

- [ ] **Step 3: 回归 + 提交**

Run: `cd agent && go build ./... && go test ./desktop/ -count=1`

```bash
git add agent/desktop/qos_controller.go agent/desktop/qos_controller_test.go agent/desktop/session.go
git commit -m "feat(agent): spectator resume paths - detach prunes viewer entry, handover and est recovery unpause"
```

---

### Task 4: native 编码器 IDR 超时调查（只查不修）

**Files:**
- Read: `native/desktop/`（IDR/ForceIDR/rebuild 路径）、`agent/desktop/core_windows.go`（RequestKeyframe 下发）、`agent/desktop/keyframe_coordinator.go`
- Create: `docs/superpowers/plans/2026-09-06-native-idr-investigation.md`（调查报告）

**Interfaces:**
- Consumes: 诊断证据（spec §1 缺陷 B）:`desktop encoder idr timeout` WARN、preKeyDropped 492→20147、pli=204、reasons=[pli]。
- Produces: 根因报告(触发链/机制/证据),若根因明确且修复 ≤50 行 → 独立小修提案章节(不实施)。

- [ ] **Step 1: 证据定位** — agent 源码找 `desktop encoder idr timeout` WARN 的产生点(grep agent/desktop),追 RequestKeyframe(reason) 的完整链:coordinator → core 管道消息 → native host 的 ForceIDR 处理(native/desktop 源码)。
- [ ] **Step 2: 机制分析** — 结合 preKeyDropped 的计数点(viewer_sender.go)与 reset/rebuild 路径(native 编码器重建后 SPS/PPS/首 IDR 的产出时序),给出 idr timeout 的触发条件树;对照 2026-09-06 生产时序(reset 后无新代帧)指认最可能分支。
- [ ] **Step 3: 报告落盘** — 写入上述 md(结构:证据/链路/触发条件树/指认/修复提案或不修理由),提交 `docs(native): encoder IDR timeout investigation report`。
- [ ] **Step 4: 提交**

---

### Task 5: 文档对齐 + 真机验收准备

**Files:**
- Modify: `AGENTS.md`（§7 运维要点或已知小缺口——三缺陷修复一句话记录 + GCC 估计器为新 est 真相源）
- Modify: `docs/superpowers/specs/2026-09-06-desktop-qos-bandwidth-fix-design.md`（状态行改"已实施"）

**Steps:**

- [ ] AGENTS.md §7 增补（3-4 行）:QoS est 真相源 = agent 侧 GCC(TWCC 驱动),浏览器 availableIncomingBitrate 对 pion 流不暴露(2026-09-06 实测);reset-grace 3s 逃逸阀;PauseSpectator 三恢复路径。
- [ ] `cd agent && go build ./... && go test ./... -count=1` 全绿终检。
- [ ] 提交 `docs: QoS bandwidth fix landed - GCC estimator, grace escape, spectator resume`。

---

## 自审记录

1. **Spec 覆盖**:§2.1→Task 1;§2.2→Task 2;§2.3→Task 3;§2.4→Task 4;§5→Task 5(发布面说明)。非目标未越界(native 只调查、参数不动、web/server/proto 零改动)。
2. **占位符扫描**:无 TBD;Task 1 Step 3 的装配顺序给了确定顺序与冲突处理指令;Task 3 的接任解暂停给了两个等效实现自由度但**语义钉死**(paused=false + 恢复恰一次)——是裁决留白而非占位;Task 4 是调查任务,产出物形态已定。
3. **类型一致性**:`AgentEstimate(sessionID string, bps int)` 在 Task 1 的 controller/streamQoS 两层同名同签;`actionRequestKeyframe`/`actionResumeSpectator` 枚举序(3/4)跨 Task 2/3 一致;`effectiveEst(now)` 语义在 Task 1(决策)与 Task 3(旁观者比较)共用;`qosResetConfirmTimeout/Retries`、`qosResumeFraction/Sustain` 常量名与用例引用一致。
