# M1-Slice2 实时视频到浏览器 — 验收结果（Task 6）

**日期**: 2026-08-22/23 · **分支**: feat/m1-slice2-live-video · **节点**: LABS-XIAOXIN（dev agent XIAOXIN-DEV）+ LABS-DEV（dev server+coturn docker, LAN http://192.168.1.12:18080）

**链路**: e2eviewer（本机 Go/Pion）→ POST /api/nodes/{uuid}/desktop → 会话 WS（JSON 信令: ready/offer/answer/ice）→ dev agent（run-dev-console, session 1, `/RL HIGHEST` elevated）→ core XNIP pipe（0x0100 StartCapture, stdin secret）→ xnc-desktop --console-rt（DXGI+Mf 2880x1800@30）→ desktoppipe ATTACH → Pion publisher → coturn TURN relay（relay-only 双端）→ viewer。

## 验收门结果 — **12/13 门 PASS**（scripts/e2e-slice2.sh run-13, 2026-08-23 fix-round-1）

> 历史: run-11 曾 13/13 PASS, 但门④ 用的是 `--pli-interval` 断言——其 pliMu 覆盖缺陷
> （controller P1）把首个 PLI 的 warm-up 成本记到了下一个 PLI 的时间戳上; 裁决改
> `--pli-at` 单发后即现原形: ④ = 2351ms（run-12）/ 2392ms（run-13）> 2000ms, 两跑一致
> 非 variance, 如实记 FAIL。其余 12 门 run-13 全绿（run-12 亦然, 仅③伴飞 viewer 的
> 非门断言 frames 52>40 超限一次, 伴飞断言不计门）。

| 门 | 判据 | 实测（run-13） | 结果 |
|---|---|---|---|
| ① 首帧（变化场景 30s） | ≤5000ms（relay 含 TURN allocation） | **4267ms** | PASS |
| ① 关键帧（变化场景） | ≥2 | **2**（frames=60） | PASS |
| ① viewer 断言 | exit 0 | exit 0（keyframeReqs=1 救回一次被 WiFi 打散的首 IDR） | PASS |
| ② 静止 45s 帧数 | 解码 AU ≤10 | **0**（纯静止屏, rtpPackets=0） | PASS |
| ② viewer 全链路存活 | exit 0（会话+信令+relay ICE 完成） | exit 0 | PASS |
| ③ 静止期第二 viewer 首帧=IDR | firstKey=true | **true** | PASS |
| ③ 首帧到达 | ≤5000ms | **2515ms** | PASS |
| ③ viewer 断言 | exit 0 | exit 0（companion viewer 同 0） | PASS |
| ④ PLI→新 IDR（**单发**） | ≤2000ms | **2392ms**（run-12 复测 2351ms） | **FAIL** |
| ④ viewer 断言 | exit 0 | exit 1（同上一行断言; plisSent=1, keyframes=3, firstKey=true） | **FAIL** |
| ⑤ 退订清理 | 无残留 xnc-desktop.exe | tasklist 无此进程 | PASS |
| ⑤ 0x0100 spawn 证据 | core.log spawn+stop | **5 次 start_capture + stop_capture: terminating** | PASS |

**④ FAIL 分析（单发断言的真实成本构成, 留 controller 裁决）**：desktop 侧 = warm-up 重喂（run-12 core.log: PLI 到达 13:22:53 → `idr_delivered reason=explicit feeds=11` 13:22:55, 即计划「按需 warm-up 上限 2×窗口或 2s」的 2s 上限本身, **desktop 侧 ≤2s 判据仍成立**）; viewer 侧再加 WiFi/relay 传输 + AU 组装滞后 ~0.4s（samplebuilder 无 flush 回调, 稀疏 1s-tick 流上 AU 需等下一 AU 首包才 finalize）→ 2351/2392ms。旧 interval 断言的 1979ms 之所以「过」, 正是 pliMu 覆盖把首个 PLI 的 ~2.4s 成本记到了下一个 PLI 头上——修复后的单发断言暴露真实值。**待裁决**: viewer 侧上界校准（如 ≤3000ms）, 或与门① 的 <2s 同例顺延 M1-Slice3 有线门。

**门值出处（裁决留痕, 2026-08-23 controller review — 非计划原文）**：计划 Task 6 原门为「①首帧 p95<2s ②变化期 ≥20fps」。两条 Ruling（`.superpowers/sdd/2026-08-23-m1-slice2-live-video/progress.md`）: **① ≤5s = XIAOXIN WiFi+TURN relay 链路校准值**——计划 2s 是理想 LAN 假设, <2s 顺延 M1-Slice3 有线门; **② ≥20fps = 计划缺陷**——ping 驱动 1 行/s 的内容仅 ~2.7fps, 编码器内容自适应正确, 20fps 门需强驱动器, 顺延 M1-Slice3 输入注入后的交互驱动门, 以「静止 45s 解码 AU ≤10」校准门替代（计划并无 ≤10 数字; 本页此前误引为「计划原文」, 已改为裁决留痕）。④ ≤2s 为计划原值。

**逐场景 JSON 摘要**（原始产物 + 四份 .h264 dump 在 `.superpowers/sdd/2026-08-23-m1-slice2-live-video/e2e/`;run-12 产物另存于 `e2e-run12-artifacts/`）：

- ① 变化（session-1 ping 驱动）: firstFrame=4267ms firstKey=true rtp=1466 frames=60 keyframes=2 bytes=572KB/33s
- ② 纯静止（solid overlay 45s）: frames=0 rtp=0 —— §7.4 语义（静止=不编码不出包）在真实 Mf 编码器上的端到端体现。（该场景 JSON 里 `connected:false` **不是 ICE 失败**——ICE 连通已由 e2eviewer `waitConnected` 硬校验, 不连则 exit≠0; 该字段是 PC 旧快照语义: 仅在首帧到达时置 true, 0 帧的静止场景恒 false, 不代表连接断开）
- ③ 静止期加入: B firstFrame=2515ms **firstKey=true**（16 帧 108KB）; companion A: firstFrame=2509ms frames=37（2s-tick 静默流, keyframes=2）
- ④ PLI 恢复（单发 `--pli-at 8s`）: pliToIdrMax=**2392ms** > 2000（run-12: 2351ms, 两跑一致）——1s-tick 静默流, plisSent=1, keyframes=3, keyframeReqs=3（均发生在首帧落地前, 即加入首 IDR 的 WiFi 自救; 单发 PLI 的 IDR 无 retry, 单发即真实值）。desktop 侧 idr_delivered ≈2s（≤2s 判据成立）, 详见上表 ④ FAIL 分析
- ⑤: 见上表

### 场景驱动方式（与计划字面的差异及理由, 全部实测驱动）

- **②「静止」 = solid 黑屏 overlay**（`scripts/static-overlay.ps1 -TickMs 0`）: 「静止期新增 AU ≤10」为上表所引 controller 裁决校准门（计划无此数字）。实测发现 XIAOXIN 控制台有 ~1.2fps 背景动画（taskbar 托盘 WiFi 图标被自身流量驱动 + agent 控制台滚动日志, 截图证据 `e2e/console-shot*.png`）——只有确定性纯静止屏才能测「编码器静默」。纯静止屏下 AU=0：Mf 编码器对逐位相同的重喂帧即使 ForceIDR 也跳过不输出（native 侧为 M1 冻结, 无法改）。
- **③④「静止期」 = 2s/1s-tick overlay**（同一 ps1, TickMs>0）: 按需 IDR（sub_join/connect/PLI 武装的 force）**只能骑在下一帧真实变化上浮出**——纯静止屏上永远不出（core 代码注释即此语义, 实测证实）。tick 间隔 = IDR 时延上界, ④ 用 1s tick 满足 ≤2s 门。
- **WiFi 链路现实**: XIAOXIN 无线（实测 ~10% UDP 突发丢包, 大 IDR≈170 包极易打散）。e2eviewer 新增 `--keyframe-retry-after`（信令级 `keyframe-req` 重请, agent 词汇新增帧类型）——①实测救回一次首 IDR。④ 改单发后 retry 只覆盖加入首帧期（仅在 frames==0 时发出）, 单发 PLI 的 IDR 无 retry——单发即真实值（见上表 ④ FAIL 分析）; desktop 侧 idr_delivered(core.log) 均 ≤2s 不变。
- 防火墙弹窗处置: dev agent 每次换新 exe 触发 Windows Security 提示, 浮在 TopMost overlay 之上且动画（~1.2fps 来源之一）。脚本加 netsh allow 规则 + `scripts/dismiss-popup.ps1`（FindWindow+WM_CLOSE, 进程 MainWindowTitle 匹配不到服务宿主窗口——实测教训）。

## T3 承接项闭环（提权 StartCaptureCross → 以 E2E 证据替代）

计划批准的替代验证：XIAOXIN 上没有真实提权 Go dev 环境可跑 `TestStartCaptureCross`；
改为全链路运行证据闭环 —— 场景① 的每一帧都流经 core `0x0100 StartCapture`
（agent 会话 handler → coreclient.StartCapture → core spawn xnc-desktop，secret 经
stdin 继承句柄通道，绝不过 argv）。视频流动 = stdin-secret spawn 通道端到端工作。

实测证据（XIAOXIN `C:\xnc-dev\core.log`，secret 按设计全程不入日志）：

```
console pipe server starting (pipe=\\.\pipe\xnc-core-dev)
start_capture: spawned pid=4260 pipe=\\.\pipe\xnc-desktop-rt-8172 gen=1
stop_capture: terminating pid=4260 (graceful drain = Slice3)
capture child pid=4260 exited code=1, clearing state
```

agent 侧（dev-agent.log）：`desktop capture attached host_pid=4260 gen=1 wts=1`，
`desktop publisher closed session=… frames=… pli=… nack=…`（会话统计面）。
core 以 SYSTEM console 模式跑（TokenManager::SessionSystemToken 需要 SeTcb），
dev agent 以 LABS `/RL HIGHEST`（完整管理员令牌）跑 —— core pipe DACL 为
SYSTEM+Admins，非提升的管理员令牌（UAC 过滤）连不上 pipe：T6 实测确定的
dev 拓扑补充，已写入 e2e-slice2.sh 注释与本文件。

**T3 提权债务：closed-by-evidence**（e2e 而非单测；同一代码路径——StartCapture
握手、spawn、stdin secret、幂等复用（gen 递增）、StopCapture 终止——全部在
真实 SYSTEM core 上运行了 13 轮）。

## 过程中真实踩坑记录（run-1…run-13 全记录）

1. **run-1** `xnc put` 覆盖运行中的 dev agent exe 失败（FILE_NOT_FOUND=rename
   到被占用文件）：先停旧 agent 再部署。
2. **run-2** 节点 ID 解析静默失败：本机 `python` 是 WindowsApps 占位 stub
   （exit 49）→ 改纯 grep/sed；dev server 对重名节点自动加 `-2/-3…` 后缀 →
   匹配 `name(-N)?`。
3. **e2eviewer server 模式并发读 bug（T4 潜伏）**：`waitReady` 与信令泵并发
   `ws.Reader()`（coder/websocket 禁止）→ answer 永远到不了 viewer（PC 恒
   new）。server 模式 T4 从未 live 验证，本次实测暴露。修复：单泵读帧, ready
   经 channel。
4. **agent 会话主循环 json.Decoder 不排干 WS 帧（链路级真·主因）**：大 SDP
   分段到达时 Decoder 值尾停读 → `previous message not read to completion`
   → 会话在 answer 后 ~70ms 被 agent 收线。回环单分段测不出。修复：
   `io.ReadAll`+`Unmarshal`，加 `desktop session ended` 终因日志。**修复后
   全链路首绿**（connected=true firstFrame=5108 firstKey=true）。
5. **run-3/4 脚本 `set -euo pipefail` + grep 管道赋值静默退出**：helper 加
   `{ …; } || true`；agent 上线等待窗 90s→180s（schtasks /Run 启动延迟实测
   ~25s）。
6. **run-5 ②③④ 全挂 → WiFi**：XIAOXIN 无线, ~10% UDP 突发丢包, 2880x1800
   IDR≈170+ 包几乎必被打散（viewer rtpPackets>0 但 0 帧可组）。pion 无法走
   TURN/TCP（实测 tcp-only URL 仍 udp relay——pion/ice 无 TURN/TCP 客户端）；
   ATTACH 的 max_w/bitrate 上限在 native host 侧只记日志不生效（native 冻结
   亦无法加）。对策 = e2eviewer `--keyframe-retry-after` 信令级重请。
7. **run-6 纯黑 overlay → 零输出**：纯静止屏编码器零输出（§7.4+Mf 跳帧）,
   按需 IDR 无帧可骑 → 引入 tick overlay。
8. **run-7/8/9/10 迭代**：agent 控制台滚动日志污染（重定向到文件）；防火墙
   弹窗浮在 overlay 上且动画（netsh 规则+WM_CLOSE 关闭）；maximized 窗口不
   盖 taskbar（改 manual 全屏 bounds）；每 tick 实测 ~2 AU + attach warm-up
   突发 ~34 AU → ②与③拆窗（②用纯静止=0 AU, ③④用 tick 骑 IDR）。
9. **run-8 ① 首帧 15s 一次**（WiFi 瞬时恶化, retry 兜回）——重跑 4.3s 正常,
   记录为链路波动非代码问题。
10. **run-12/13（fix-round-1）单发 PLI 门④ 连续 2351/2392ms FAIL**——非链路
    variance（两跑一致, 其余 12 门全绿）: desktop warm-up 重喂 2s 上限 + viewer 侧
    传输/组装 ~0.4s。旧 interval 断言的 1979ms 靠 pliMu 覆盖把首 PLI 成本记到下一
    PLI 头上（controller P1 覆盖问题的实证——覆盖修掉前该门从未测过真值）。
    viewer 侧上界留 controller 裁决（见门值出处与 ④ FAIL 分析）。

## 浏览器页（Deliverable A）

`web/src/pages/DesktopLive.tsx` + 路由 `/desktop/:nodeId`（不在侧边导航,
实验页直达）。认证沿仓库既有模式（localStorage `xnc_token` 经 `api()`）。
功能：relay-only RTCPeerConnection（turn 配置取自 REST 202 响应）、recvonly
video transceiver、`<video autoplay muted playsinline>`、统计浮层（fps via
`requestVideoFrameCallback`，Firefox 回退 getStats framesDecoded；首帧 ms；
ICE connectionState；解码/关键帧计数）、PLI 按钮（浏览器 JS 无法发 RTCP PLI →
发 `{"type":"keyframe-req"}` 信令帧, agent 映射到与真 PLI 同一条
RequestKeyframe 路径, reason=viewer-pli）、卸载清理。

**浏览器手工核验 = owner 一键步骤**（本任务无浏览器驱动能力；gate 结束后
dev 栈保持运行）：

```
http://192.168.1.12:18080/desktop/070d0123-a1e7-4b6d-8d9f-eee882d37fa4
```

（先以 dev admin 登录 http://192.168.1.12:18080；Chrome/Edge 预期 OK——T4
已核 profile-level-id 42e01f 声明 vs 实际 high profile 的带内 SPS 解码。）

- web 构建: `npm run lint`（0 error, 无新增告警）+ `npm run build`
  （tsc -b + vite）绿；仓库无 web 测试基建（无 vitest）——按任务说明跳过
  页单测并记录。

## Go/脚本门

- `go vet ./...` + `go test ./... -count=1` 全模块绿：proto（含 ipc）、
  server（api/bootstrap/db/registry/session/tokens/auth）、agent（全部 12 包,
  desktop 含扩展后的 loopback 门 `-count=2`）、cli、e2eviewer、nalcheck。
- e2eviewer 扩展：`--expect-frames-max`（静止低产出断言）、`--expect-first-key`
  （首 AU=IDR 承接断言）、`--pli-at`（单发 PLI）、`--keyframe-retry-after`
  （信令级首 IDR 自救）+ evaluate 单测覆盖。
- agent 变更：run-dev-console `--desktop-core-pipe/--desktop-core-secret-hex`
  （dev seam flags, 等价设 env, secret 不入日志）；desktop 信令词汇加
  `keyframe-req`（loopback 测试覆盖 ④b 路径）；会话终因日志；WS 帧排干修复
  （runServer/`session.go` 各一处）。

## 遗留

- **门④ 单发 PLI viewer 侧 2351/2392ms > 2000ms**（fix-round-1 如实记录;desktop 侧
  idr_delivered ≤2s 成立）——viewer 侧上界留 controller 裁决: 校准（如 ≤3000ms）或
  顺延 M1-Slice3 有线门（同门① <2s 先例）。
- pion 无 TURN/TCP 客户端（`?transport=tcp` 被忽略, 实测）→ WiFi 链路上大
  IDR 依赖 NACK+信令重请兜底；生产 TLS/TCP 化 = M2。
- ATTACH 的 max_w/bitrate 上限 native 侧未生效（只记日志）——若 M2 要限分辨
  率/码率需 host 实现。
- dev server 节点表按运行累积（-N 后缀）, `docker compose down -v` 重置；
  拆除步骤: scripts/dev-topology.md §6（dev core/agent schtasks + compose
  down）。
- 首帧 p95 含 core spawn 冷路径（实测 4.3-5.1s）; 暖路径/重连优化 = Slice3。
