# 桌面流 QoS 修复：带宽估计源 / 重置闩死 / 旁观者暂停恢复 — 设计

日期：2026-09-06
状态：已实施（2026-09-06）
证据：本文件 §1；agent 日志（XIAOXIN，2026-09-06 会话）+ 浏览器 getStats 实测

## 1. 背景：三个已确诊缺陷

生产实测（真实视频会话 + agent 日志 + 劫持 pc 的 getStats）：

**A. 带宽估计源干涸 → 500kbps 地板锁死（用户"不清晰不流畅"的直接原因）**
- 浏览器对 pion→Chrome 流**不暴露 `availableIncomingBitrate`**（SDP 的
  rtcp-fb transport-cc 与 extmap(4) 双向协商齐全、agent 侧 TWCC 反馈计数
  正常增长，字段仍缺失——Chrome 侧行为，非我方可修）。
- 网页 viewer_feedback 的 estimatedBps 永久落到 goodput 兜底 ≈ 自身发码率
  （自指）。控制器目标 = 85%×est，升档需 est > 1.18×当前码率——自指源下
  **永不可达**；日志铁证：视频播放期间 `set_video_config` 的
  bitrate_bps=500000（工作区下限）纹丝不动，fps 5↔10 每 12s 锯齿，
  max_w 1728→1440→1152 两级降。

**B. 编码器重置确认永不到达 → reset-grace 闩死（整流冻结）**
- `desktop qos: congestion cut held (encoder reset in flight)` 每 3s 重复
  15+ 分钟，"confirmed by new-generation frame" 不再出现。
- 下层证据：`desktop encoder idr timeout` WARN ×3；冻结会话 publisher 收线
  统计 frames=81（与浏览器 decoded=81 分毫不差）、preKeyDropped=492；
  后续会话 preKeyDropped=20147、pli=204——native 编码器 IDR 生产在超时
  （根因在 native/desktop host，本周期调查不改码）。
- agent 侧可独立修的：grace 无限期挂起拥塞控制的设计——**确认超时必须有
  逃逸阀**。

**C. PauseSpectator 无恢复通道（reload 后冻结的另一形态）**
- 新 viewer 冷启动 est≈0 < 35%×controller est → 暂停且**永不恢复**
  （qos_controller.go 注释自认"恢复通道是后续任务"）。
- 放大器：viewer 表项按 lastSeen 2 分钟修剪——会话 detach 不删表项，
  reload 后新 viewer 与**陈旧 controller 表项**比对，暂停触发概率极高。

## 2. 修复设计

### 2.1 缺陷 A：agent 侧 GCC 带宽估计（pion 现成件）

用 pion 官方 sender-side 带宽估计（TWCC 反馈驱动，delay-gradient + loss
的完整 GCC），替换"浏览器 getStats 汇总"这一干涸源：

- **装配**（agent/desktop/transport.go，`RegisterDefaultInterceptors`/
  `ConfigureTWCCHeaderExtensionSender` 既有调用点）：

```go
ccInterceptor, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
    return gcc.NewSendSideBWE(
        gcc.SendSideBWEInitialBitrate(<该流当前生效码率：streamQoS.Current().Bitrate；流未建时 bitrateForWidth(1920) 缺省>),
        gcc.SendSideBWEMaxBitrate(qosMaxBitrateBps)) // 15M 对齐工作区
})
ccInterceptor.OnNewPeerConnection(func(id string, est cc.BandwidthEstimator) {
    est.OnTargetBitrateChange(func(bps int) { <会话回调：上报 streamQoS> })
})
ir.Add(ccInterceptor)
```

  （`pion/interceptor/pkg/cc` + `pkg/gcc`；与官方 bandwidth-estimation
  示例同构，装配顺序与现有代码兼容。）
- **est 上报路径**：estimator 是 per-PC（= per viewer 会话）。
  `OnNewPeerConnection` 的 id 与会话的对应关系经 Publisher 建联处接线；
  `OnTargetBitrateChange` → `streamQoS.AgentEstimate(sessionID, bps)`。
- **控制器 est 选择**（qos_controller.go Observe 输入侧）：
  viewer 表项新增 `agentEst`（最近一次 GCC 目标码率，带 10s 过期）；
  决策用的 est = `agentEst > 0 ? agentEst : fb.EstimatedBps`。
  浏览器 est 从主源降为兜底（未来 Chrome 修复了字段也自动让位）。
- **初始/上限**：InitialBitrate = 现行 `bitrateForWidth(W)`（保持起步
  行为）；MaxBitrate = qosMaxBitrateBps。
- 既有升/降档阶梯、85% 目标、pacing 预算**全部不动**——只是把 est 的
  真相来源换成能看见带宽的 GCC。

### 2.2 缺陷 B：reset-grace 确认超时逃逸阀（agent 侧）

qos_controller 增加在途重置的确认时限（默认 3s，时钟注入可测）：

- 超时未确认 → 经 KeyframeCoordinator 发 **urgent** 关键帧请求
  （epoch 变更类，绕过常规冷却）：agent 侧至多重发 2 次 urgent 请求，
  加上 native 编码器重建自身的一次恢复尝试（隐式的首次机会），
  共 3 次恢复机会。
- 仍未确认 → **强制释放 grace**（拥塞控制恢复），日志 WARN
  `encoder reset confirmation timeout; grace force-released`
  （挂起计数清零、后续拥塞照常剪码）——单次 native 失败不再劫持整条
  流的拥塞控制。
- native 编码器 IDR 超时根因（idr timeout / 20K pre-key drops）另立
  调查任务（§2.4），不在本周期改 native。

### 2.3 缺陷 C：PauseSpectator 恢复通道 + 表项即时清理

- **表项即时清理**：`streamQoS.detach(sessionID)` 同时从控制器 viewer
  表删除该表项（会话 WS 收线即删，不等 2 分钟 lastSeen 修剪）——reload
  冷启动不再与陈旧 controller est 比对。
- **接任解暂停**：controller 离场/被修剪后接任者若处于暂停态 →
  立即恢复其发送（新 controller 不可能仍是 spectator）。
- **est 恢复解暂停**：暂停中的 spectator 连续 5s 上报
  `est > 50%×controller est` → 恢复发送（带迟滞，防抖）；
  新增 Action `actionResumeSpectator`（幂等，重复恢复无害）。

### 2.4 native IDR 调查（只查不修）

timeboxed 调查任务：`desktop encoder idr timeout` 的触发链（PLI→
RequestKeyframe→native host 的 ForceIDR/rebuild 路径），pre-key 丢弃
风暴的机制；产出 = 根因报告 + （若根因明确且修复小）独立小修提案，
不与本周期三个 Go 侧修复混提交。

## 3. 范围与非目标

**范围**：agent/desktop 的 Go 侧（transport.go / qos_controller.go /
session.go / qos_min.go 邻域）；全部带单测（qos_controller_test 扩展 +
session_loopback 回归）。

**非目标**：native/desktop 改码（只调查）；web/server/proto 改动（零）；
QoS 阶梯参数调优（保持现行值，先修"看不见带宽"）；Chrome 字段缺失的
上游问题。

## 4. 测试计划

- qos_controller 单测：
  - est 选择：agentEst 新鲜 → 用之；过期/为 0 → 浏览器 est 兜底。
  - 逃逸阀：注入时钟推进 3s 未确认 → urgent 请求 ×2 → 第 3 次超时
    force-release；正常确认路径不受影响（既有用例回归）。
  - 恢复：detach 删表项；接任解暂停；5s est 恢复解暂停（含防抖）。
- session_loopback 回归：既有媒体回路全绿。
- 真机验收（XIAOXIN 视频会话）：`set_video_config` 的 bitrate 不再钉
  500000（可随 GCC est 爬升）；reload 后流不冻结；暂停可自愈。

## 5. 发布面

全部改动在 agent（Go）→ 随安装器发版（手动触发 release workflow）到达
节点；server/web 不动。发版前可先实验机手装未发布构建验证（AGENTS §5）。
