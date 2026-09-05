# TURN 监控页 + DesktopLive 信息补齐 — 设计

日期：2026-09-06
状态：已与用户对齐（监控页登录可见、仅回环探测、轻量重组）
关联：`docs/2026-08-24-turn-pool-architecture.md`（TURN 池架构）、M3/M4 desktop
媒体管线（viewer_feedback/e2e 相关件）

## 1. 背景与目标

- TURN 池有 30s STUN 健康探测（`server/internal/api/turnpool.go`）但**无对外
  暴露**——运维看不到哪台 TURN 在线、当前是否有会话在用。
- DesktopLive（`/screen/:nodeId`）已在计算 rttMs/e2eP95Ms/availBps（viewer_feedback
  环，1s 节奏）但 **UI 只显示 fps/first frame/decoded/pli**——数据在、没展示；
  TURN 中继路径（选中的 candidate-pair 走哪台 TURN）也无呈现。
- 目标：一个登录可见的 Monitor 页回答"TURN 是否在用/各节点状态/浏览器到
  各 TURN 的时延"；DesktopLive 轻量重组，把既有指标与 TURN 中继信息亮出来。

## 2. 范围与非目标

**范围**：server 新增 `GET /api/turn/status`（用户 JWT）；web 新 Monitor 页
（App shell 内）+ 回环探测库；DesktopLive 统计面板补齐与状态栏分组。

**非目标**：
- coturn 侧指标（allocations 数、带宽——需 coturn admin API，远期）。
- 活跃会话明细（节点/用户级标识——聚合数足够，隐私不必要）。
- DesktopLive 深度改版（折叠面板/全屏——用户已裁定轻量）。
- agent/proto 改动（零）；监控页不接活跃会话的真实 candidate-pair RTT
  （用户已裁定仅回环探测）。
- ScreenPreview.tsx 孤儿组件不在本轮处理（无路由引用，另行清理）。

## 3. 设计

### 3.1 server：`GET /api/turn/status`（用户 JWT）

新 `server/internal/api/turn_handlers.go`；路由挂用户 JWT 组
（`r.Route("/api/turn", ...)` + `auth.Middleware`，与 /api/clusters 同型）。

响应：

```json
{
  "mode": "pool" | "urls" | "unconfigured",
  "icePolicy": "relay" | "all",
  "pool": [{"ip": "1.2.3.4", "port": 3478,
            "urls": ["turn:1.2.3.4:3478?transport=udp",
                     "turn:1.2.3.4:3478?transport=tcp"],
            "healthy": true}],
  "fallbackUrls": ["turn:xnc.app:3478?transport=tcp"],
  "username": "xncdev",
  "credential": "xncdev-secret",
  "activeDesktopSessions": 3
}
```

- `mode`：pool 配置且非空 → `pool`（此时 pool[] 为健康快照）；否则
  TurnURLs 完整 → `urls`（pool[] 空，fallbackUrls=TurnURLs）；两者皆缺 →
  `unconfigured`（pool/fallbackUrls 空、username/credential 空串）。
- `icePolicy` = `cfg.DesktopICEPolicy`（relay|all）。
- `username`/`credential` 与会话下发同源（`cfg.TurnUsername/TurnCredential`
  或池统一凭据）——**仅登录可见**（用户裁定），供浏览器回环探测分配
  relay 候选。
- `activeDesktopSessions`：session.Manager 新增
  `CountActive(kind string) int`（锁内遍历 `sessions` map 按 Kind 计数，
  与 `countByNodeLocked` 同型）；handler 传 `proto.KindDesktop`。
- `TurnPoolManager` 新增 `Status() []TurnServerStatus`（锁内快照拷贝：
  IP/Port/Healthy；URLs 经既有 `turnURLs()` 构造）。
- 无 pool 配置时 turnPool 为 nil（router 构造逻辑既有）——handler 判空。

### 3.2 web：Monitor 页（App shell 内，`/monitor`）

- 路由：main.tsx App children 加 `/monitor`；Sidebar nav 加 `Monitor` 项
  （Download 之后）。全部登录用户可见（与 Nodes/Clusters 同级）。
- **TURN 服务卡**：
  - 头部：mode 徽标（pool/urls/unconfigured）+ ICE 策略 + Re-probe 按钮。
  - 表：每行一个可探测目标（池成员逐台 + fallback URLs 逐条）：
    `目标 | udp/tcp | server 健康（池成员显示 healthy 布尔，fallback 显 —）
    | 浏览器 RTT`。
  - 未配置态（mode=unconfigured）显示占位说明，不渲染表。
- **用量卡**：`activeDesktopSessions` 数值 + 说明文案（desktop 会话默认
  全程经 TURN 中继；ICE 策略为 all 时可能直连）。10s 自动刷新
  （setInterval 重取 status；卸载清理），Re-probe 只重跑探测。
- **回环探测**（新 `web/src/lib/turnProbe.ts`）：
  - `probeTurn(target: {urls, username, credential}, opts?): Promise<number>`
    返回浏览器→TURN 单向时延 ms（1 位小数）。
  - 实现：两条 `RTCPeerConnection`（iceServers=[target]，`iceTransportPolicy:
    "relay"`），A 建 DataChannel，双方 onicecandidate 互喂 addIceCandidate；
    ICE 连通后（datachannel open 或 pc iceState connected）取 getStats 的
    selected candidate-pair `currentRoundTripTime`/2；整体 8s 超时 reject。
  - 串行执行（逐台跑，避免并发互相抬时延）；每台完成后行内显示。
  - RTCPeerConnection 经模块级可注入工厂暴露（默认
    `window.RTCPeerConnection`；测试注入假实现）。
- vitest（`web/src/pages/Monitor.test.tsx`）：mock fetch 三态（pool 模式
  表行/未配置占位/拉取失败）；注入假 probe 断言按钮触发与结果渲染
  （"12.3 ms"、超时 "timeout"）；凭据经 api() 自动带 JWT 不在测试面。

### 3.3 web：DesktopLive 轻量重组

- **统计面板补齐**（video 左下 overlay，`desktop-stats`）：现有
  fps/first frame/decoded/key/pli 基础上加 `e2e p95 | rtt | bitrate`。
  数据源全部既有：`sendFeedback`（1s 环）里把 rttMs/availBps 提升为
  state；`corrSnap.e2eP95Ms` 同环提升（无关联样本时显 —）。
- **TURN 中继信息**：`readFeedbackStats` 的 report 遍历中增收集
  local/remote candidate 与 selected candidate-pair（nominated 优先），
  解析选中 pair 的 local candidate：`candidateType==="relay"` → 取其
  `address`（即 TURN 服务器 IP）与 `relayProtocol`，显示
  `via TURN <ip> (udp)`；非 relay（host/srflx，all 策略直连）→ `direct`。
  解析逻辑提取纯函数 `turnRelayLabel(local, pair) → string`
  （新 `web/src/pages/desktop/turnLabel.ts`，纯 TS 可测）。
- **状态栏分组**：现有单行 screen-bar 改三组（flex 分隔样式，
  styles.css 加 `.screen-bar-group` + 分隔线）：
  1. 会话信息：名称、state、ice、dims、显示器选择、`h264/<mode>`
  2. 媒体统计：fps、e2e p95、rtt、via TURN/direct（统计面板的精华行内
     版，一眼可读）
  3. 操作：PLI、SAS、lease、chips、notice、error
- 布局机制不动；`?framediag` 诊断模式不动。
- vitest：`turnLabel.test.ts` 纯函数用例（relay+udp、relay+tcp、host、
  无 pair/缺字段 → —）。

## 4. API 契约小结

| 端点 | 鉴权 | 响应 |
|---|---|---|
| GET /api/turn/status | 用户 JWT | §3.1 JSON；未认证 401 |

无其他 API 变更；proto 不动。

## 5. 测试计划

- **server**（api 包，真 PG）：三模式响应形状（pool 配置经 TestEnv cfg
  注入 TurnPool；urls 模式默认；unconfigured 显式清空）；401 未认证；
  凭据字段在场；创建一个 desktop 会话后 activeDesktopSessions=1。
- **session 包**：CountActive(kind) 单测（空表 0、混 kind 计数）。
- **turnpool 包**：Status() 快照与池内容一致（既有测试风格）。
- **web**：§3.2/§3.3 所列。

## 6. 验收清单

1. 登录后侧栏见 Monitor；页面显示 mode/策略/池成员健康/会话计数，
   10s 自刷新。
2. Re-probe 后每台 TURN 行内出现浏览器 RTT（ms）或 timeout；未配置时
   显示占位。
3. DesktopLive 状态栏三组清晰；统计面板与状态栏显示 e2e p95/rtt/bitrate/
   via TURN <ip> (udp)（relay 会话）或 direct（all 策略直连）。
4. 未登录访问 /monitor 跳登录；/api/turn/status 未认证 401。
5. ci 五矩阵全绿。
