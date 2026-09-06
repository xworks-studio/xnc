# native 编码器 IDR 超时调查报告（只查不修）

日期：2026-09-06（调查执行 2026-09-05/06）
任务：Task 4（`.superpowers/sdd/2026-09-06-desktop-qos-fix/task-4-brief.md`；
设计 §2.4 `docs/superpowers/specs/2026-09-06-desktop-qos-bandwidth-fix-design.md:97-102`）
性质：**只读调查**。本报告不改动任何代码；文末修复提案均为「未实施」的提案。
结论速览：两条独立的 native 缺陷链分别解释「重建后 15+ 分钟无新代帧」与
「20K pre-key 丢弃 + pli=204」：(D) DXGI 重建后静态桌面无帧死锁；
(C) `idr_in_flight` 闩在活跃内容下无界，吞掉一切按需 IDR 请求（叠加 B1
硬件 rung force-key HRESULT 不检查、E GOP 兜底 best-effort）。

---

## 1. 证据

### 1.1 生产证据（2026-09-06 XIAOXIN 会话；设计 §1 缺陷 B，
`docs/superpowers/specs/2026-09-06-desktop-qos-bandwidth-fix-design.md:21-29`）

| # | 证据 | 出处（代码定位） |
|---|------|------------------|
| E1 | agent WARN `desktop encoder idr timeout reasons=[pli]` ×3 | 打点：`agent/desktop/keyframe_coordinator.go:305`（`emitState`；周期到期入口 `expireLocked` keyframe_coordinator.go:286-298） |
| E2 | 冻结会话收线统计 `frames=81`（浏览器 decoded=81 分毫不差）、`preKeyDropped=492` | 日志点：`agent/desktop/transport.go:435-438`（`desktop publisher closed`）；preKeyDropped 计数点：`agent/desktop/viewer_sender.go:404` |
| E3 | 后续会话 `preKeyDropped=20147`、`pli=204` | 同上；pli 计数点：`agent/desktop/qos_min.go:60`（RTCP PLI 分派） |
| E4 | max_w 重置后「新代帧确认」15+ 分钟未到，`congestion cut held (encoder reset in flight)` 每 3s 重复 | grace 置位/解除：`agent/desktop/qos_controller.go:634-664`（`FrameObserved` 只认 `codecEpoch > graceCodecEpoch`，qos_controller.go:659）；held 日志：`agent/desktop/session.go:155-157` |
| E5 | 同窗口拥塞剪码被 grace 挂起 → max_w 降档触发重置 | 决策点：`agent/desktop/qos_controller.go:562-576`；下发：`agent/desktop/session.go:271`（`src.SetVideoConfig`）→ `agent/desktop/core_windows.go:389-397` |

### 1.2 preKeyDropped 究竟数什么（把 20K 数字钉死）

`viewer_sender.go:391-409`：发送器处于 `stateWaitIDR` 时（初态、溢出/不连续/
恢复/pacer 拒收之后，viewer_sender.go:100-113、473-479），到达的**非 key 帧**
逐帧 `preKeyDropped++`（:404）。因此 20147 意味着：

- host → agent 的帧管道一直在流（两万帧到达了 agent），但整个期间**没有一帧
  `f.Key=true` 的 AU 到达本发送器**（key 帧若到达会立即转 live，:405-408；
  若被 epoch 门拒则计入 `epochDropped` 而非 preKeyDropped，:397-400；若被
  准入门拒则计入 `deadlineDropped`，:470-479）。
- host 侧订阅队列是 kLive（否则 delta 在 host 端就被 WAIT_IDR 机器丢掉，
  `native/desktop/subscribers.h:201-205`，根本到不了 agent）。
- 即：**native host 在数分钟内一条 IDR AU 都没有发布**（`is_idr` 判定
  NAL type 5，`native/desktop/media_pipeline_v2.cpp:761`；key 位随 AU 下发
  `native/desktop/rt_pipe_server.cpp:903`）。这与设计文档的判断一致
  （设计 §1「native 编码器 IDR 生产在超时」）。

pli=204（E3）：浏览器冻结期间持续回 RTCP PLI（qos_min.go:59-61 计数），
经协调器（session.go:560-562）→ host（见 §2），全部无 IDR 回应。

---

## 2. 链路：请求 → native 处理 → IDR 产出 → agent 观测

### 2.1 请求面（agent → host）

1. PLI/FIR：`Publisher.rtcpLoop`（qos_min.go:50-74）→ `fireKeyRequest("pli")`
   → `coord.Request(reason, keyRequestIsUrgent(reason))`（session.go:560-562；
   urgent 只认 connect，qos_min.go:90-92）。
2. **KeyframeCoordinator 合并**（keyframe_coordinator.go:166-207）：常规请求
   250ms 冷却（:39）；在途周期只并入 reason（:175-179）；pacer 有 2×宽限
   退避（:180-184）。发出 → `sink.RequestKeyframe(fire)`（:201）。
3. sink = 会话动态 Source：`pipeSource.RequestKeyframe`（core_windows.go:399）
   → `desktoppipe.Sub.RequestKeyframe`（`agent/desktoppipe/client.go:504-508`，
   0x0104 KEYFRAME_REQ，reason 截 31B）。
4. host 收包：`native/desktop/rt_pipe_server.cpp:657-669` —
   `table_.MarkNeedsKeyframe(sub_id, "explicit")` + INFO
   `rt keyframe_req sub=%u client_reason=...`（:662）。reason 只进记账，
   不影响行为（subscribers.h:321-326）。

### 2.2 native IDR 生产（host 媒体环线程）

媒体环单线程顺序（media_pipeline_v2.cpp:553-582）：收集输出 → 外部请求 →
**reset 命令（RunReset，:564-567）→ reconfigure（:571-574）→ PollIdrRequest
（:575）** → 采集（AcquireOnce，:577）→ **提交（仅 `stream_inited` 时，
:579 `if (im_.stream_inited) TrySubmit()`）** → beat。

- **合并请求 → 武装**：`PollIdrRequest`（media_pipeline_v2.cpp:842-855）读
  `sink().PendingIdrReason()`（RtServer 实现 rt_pipe_server.cpp:993-999；
  表层 `subscribers.h:328-340`：任一订阅 `needs_keyframe` 即非空）。三重门
  （:845-847）：`!have_key`（首 IDR 前不武装）、**`idr_in_flight`（在途
  IDR 未落地时不再武装）**、`kIdrMinIntervalMs=500ms` 节流（:131）。过门 →
  `mbox.ArmIdr(reason)` + `idr_in_flight=true`（:848-851）+ INFO
  `idr_request reason=...`（:853）。
- **武装 → 提交**：`TrySubmit`（:1094-1224）在 FPS/槽位门后
  `force = mbox.TakeIdr(...)`（:1171；邮箱深度 1，`media_pipeline_v2.h:220-235`
  「下一次提交恰好消费一次」）→ 转换 NV12（:1175）→ `session->Submit(id,
  lease, force)`（:1192）→ INFO `idr_submitted seq=... reason=...`（:1221）。
- **编码器 rung 的 force 语义（一次性、无条件消费）**：
  - 硬件 rung `MfGpuEncoder`：`UnitSubmitTexture` 里
    `codec_api->SetValue(&CODECAPI_AVEncVideoForceKeyFrame, &v)` —
    **HRESULT 不检查、不打日志**（mf_gpu_encoder.cpp:922-927）。
  - 软件 rung `MfCpuEncoder`→`MfSoftEncoder`：`ForceNextIdr` 只置
    `force_pending_`（mf_encoder.cpp:679-682）；`SubmitNv12` 在提交输入时
    恰好一次 SetValue，**即使失败也无条件消费**（mf_encoder.cpp:610-624，
    失败有日志 `force_key_set_rejected`）；CPU 会话侧入口
    mf_gpu_encoder.cpp:1755。这是文档化的 E2 契约（mf_encoder.h:19-26）。
- **产出 → 发布**：`CollectOutputs`/`AcceptForPublication`（:636-705，退役
  epoch 丢弃 :663-670）→ `PublishNow`：`is_idr = NalHasType(au,5)`（:761）、
  首个 IDR 收割 SPS/PPS 前缀（:762-768，新会话即清空缓存 InitStream
  :1275-1277）、置 `kAuFlagKey`（:796）→ sink `OnAu`。**`is_idr` 时清
  `idr_in_flight` + INFO `idr_delivered`（:818-828）——这是活跃流上唯一的
  清除点。**
- **发布 → 订阅者 → agent**：`RtServer::OnAu` v2 分支 `PushAuV2`
  （rt_pipe_server.cpp:934-960）：WAIT_IDR 订阅只放行目标 epoch 的 IDR
  （subscribers.h:201-211）；kLive 的 IDR 永不丢（:227-239）。0x0205 帧的
  key 位/身份到 agent（core_windows.go:288-309）→ 帧泵
  `idrObservingSource` 喂 `coord.OnIDR`（keyframe_coordinator.go:370-376；
  session.go:594-603）→ 在途请求清除 / 超时 WARN（keyframe_coordinator.go:216-228、305）。

### 2.3 max_w 重置（重建）链

QoS 降档 max_w → `SET_VIDEO_CONFIG`（rt_pipe_server.cpp:716-769）→
`p->SetMaxWidth(max_w)`（:743）→ `mbox.RequestReset("resolution")` +
`media_v2_set_max_w`（media_pipeline_v2.cpp:1841-1846）。`RunReset`
（:1497-1667，单环线程执行）：phase2 停提交/清内容（:1551-1555，
`stream_inited=false`）→ phase3 **拆会话 + `latest.Reset()`**（:1564-1574）→
phase4 重建后端（:1580-1629）→ phase5 epoch 双 +1（:1634-1636）→
**phase7 `mbox.ArmIdr("rebuild")`**（:1649-1650）→ phase8
`OnState("capture_rebuilt")`（:1658）→ RtServer 广播 HOST_HELLO 并对每个
订阅 `OnRebuildDiscontinuity(floor)`（rt_pipe_server.cpp:1010-1038，
floor = 最后扇出的 epoch 对 :924-926；订阅进入 pending_rebuild_/WAIT_IDR，
subscribers.h:249-255）。

**恢复 IDR 的唯一载体**：重置后第一个 `CaptureStatus::kFrame`（AcquireOnce
:929-940）触发 `InitStream`（:1227-1292，按新 max_w 重算缩放、建新会话、
清 SPS/PPS）→ `stream_inited=true` → 首次 `TrySubmit` 消费 "rebuild" 武装
（force）→ 新会话首输出（新编码器天然 IDR + force）以新 epoch 发布 →
订阅 0x020B+IDR 恢复。**注意：RunReset 不清除 `idr_in_flight`/`have_key`/
`last_initiated_idr_ms`（:1497-1667 全文无一处），这三个状态跨重置存活。**

---

## 3. 触发条件树（候选分支 + 代码证据 + 可能性）

```
「请求的 IDR 未产出」（E1/E2/E3）
├─ A. 请求没到 host
│   ├─ A1 agent 合并/冷却丢弃（冷却窗内，keyframe_coordinator.go:195）
│   └─ A2 pipe 写失败（有独立 WARN "desktop keyframe request failed"
│        keyframe_coordinator.go:202；生产未见）                      【低】
├─ B. 到了 host、force 已提交，但编码器没吐 IDR AU（一次性消费被白吃）
│   ├─ B1 硬件 MFT 拒绝/忽略 ForceKeyFrame SetValue —— HRESULT 不检查
│   │     （mf_gpu_encoder.cpp:922-927，软件 rung 有日志 :620-622 对照）
│   │     → AU 是 delta，idr_in_flight 不清（见 C）                  【中高】
│   ├─ B2 编码器吐的是「非 IDR 的 I 帧」（CRA/开放 GOP，NAL type 1），
│   │     NalHasType(...,5) 判非 key（media_pipeline_v2.cpp:761）
│   │     → 同样进 C 闩死；源码无法判定，需运行时证据（§5）           【中，源码死端】
│   └─ B3 TakeIdr 之后、Submit 之前失败（Convert 设备移除 :1181-1187 /
│        hw Submit kNotReady→StrikeHw+reset :1193-1204）——force 白吃，
│        但随后的 reset 走新会话首 IDR，自愈                          【低（瞬态）】
├─ C. idr_in_flight 闩死：武装的 IDR 永不落地时的饥饿
│   ├─ 清除点只有两个：PublishNow 见到 is_idr（:818-828）；
│   │   KeepaliveFeed 的 34-feed 出界（:1078-1087, idr_feed_bound
│   │   pipeline.h:108-112）
│   ├─ KeepaliveFeed 只在 kNoChange（静态屏）运行（:964-967→1032）；
│   │   **活跃内容（视频在放）下永不运行 → 闩一旦置上就永久生效**，
│   │   PollIdrRequest 的 idr_in_flight 门（:845）把之后所有
│   │   PLI/sub_join/queue_overflow 请求全部吞掉（pending 不消费但
│   │   永不出队）
│   └─ 进入闩的入口 = B1/B2 任意一次（一次即够）                      【高（机制确凿；
│        入口概率见 B1/B2）】
├─ D. 重建后无新代帧（E4，15+ 分钟）
│   ├─ D1 DXGI 重建后静态桌面无帧死锁：
│   │   - phase3 `latest.Reset()`（:1574）后 Snapshot 恒 false
│   │     （gpu_surface.h:178-181）→ KeepaliveFeed 在 :1055 早退，
│   │     **重置把 keepalive 的喂帧源也杀了**；
│   │   - 唯一复活输入 = 真 kFrame，而 DXGI 静态屏 WAIT_TIMEOUT→kNoChange
│   │     （dxgi_capture.cpp:885-889）或 LastPresentTime==0 被当光标帧丢
│   │     （dxgi_capture.cpp:905-910）；
│   │   - 对照：GDI rung 显式保证「重建后首帧必为帧」（gdi_capture.cpp:125
│   │     `have_frame_ = false` 注释 "first frame after (re)create is
│   │     always a frame"），**DXGI 无任何等价机制**（Rebuild→Init 只重置
│   │     标记 dxgi_capture.h:322-329，内容门完全交给 OS 的 present 事件）；
│   │   - 死锁表现：stream_inited=false → TrySubmit 不跑（:579）→
│   │     "rebuild" 武装悬空 → 无任何新 epoch AU → agent 侧
│   │     FrameObserved 永不满足（qos_controller.go:659）→ grace 永挂；
│   │     PLI 照常进表、PollIdrRequest 甚至会把 idr_in_flight 也闩上
│   │     （武装后无提交可消费）。用户一碰鼠标（屏幕 present）即自愈
│   │     —— 与「间歇性」吻合。冻结会话无输入（用户看着冻结画面放弃
│   │     操作）恰是触发土壤。                                      【高（对 E4）】
│   ├─ D2 phase4 重建循环卡死（backends 反复失败，循环无总上限
│   │     media_pipeline_v2.cpp:1594-1629；或 DesktopWatch 卡 NonDefault
│   │     :1581-1585）——会持续打 `capture_reset_rebuild_failed`/recovering
│   │     日志，可鉴别                                               【中低】
│   ├─ D3 InitStream 反复失败（max_w 缩放出奇数高 ：1246-1253）→
│   │     HandleInitFailure 重置循环 → 3 次后 FATAL（:1385-1403）→
│   │     run 结束、stream_end —— 会话应收线，与 15 分钟存活矛盾      【低】
│   └─ D4 媒体环 FATAL（identity_mismatch 等）同上 ends the run        【低】
└─ E. 周期 GOP IDR 兜底缺位（放大器，不单独致病）
    - GOP=fps×10 best-effort（mf_gpu_encoder.cpp:786-787；mf_encoder.cpp:363；
      设计意图「IDR 都是按需强制的」mf_encoder.cpp:355）——若被 MFT 拒绝，
      按需路径成为唯一 IDR 源，C 闩死即「零 IDR」而非「每 10s 一条」。
      与 E3 的 20K 连续 delta（≈67 个 GOP 周期无一 IDR）高度一致        【中高】
```

另记两个**良性**偏差（不解释分钟级冻结，但解释零星 WARN）：
- agent 宽限 `max(250ms, 2×帧距)`（keyframe_coordinator.go:268-273）短于
  native `kIdrMinIntervalMs=500ms`（media_pipeline_v2.cpp:131）——密集请求下
  第二条必然先在 host 排队 500ms，agent 侧可先超时打 WARN。
- 重置在途期间媒体环整段在 RunReset 里，PollIdrRequest 不运行（环序 :564-575）。

---

## 4. 指认（对 2026-09-06 生产时间线）

**E4（max_w 重置后 15+ 分钟无新代帧、grace 永挂）→ 分支 D1（DXGI 重建后
静态桌面无帧死锁）。** 理由：① 重置的恢复 IDR 需要一个 post-reset kFrame
作载体（§2.3），而唯一能喂帧的 LatestSurface 已被 phase3 `latest.Reset()`
（media_pipeline_v2.cpp:1574）杀死，keepalive 在 :1055 早退——重置自断了
保活路径；② 拥塞降档发生时远端桌面很可能静止（用户面对冻结/低码率画面），
DXGI 静态屏只回 kNoChange（dxgi_capture.cpp:885-889、905-910）；③ GDI rung
有显式「重建后首帧必为帧」保证而 DXGI 没有（gdi_capture.cpp:125 vs
dxgi_capture.h:322-329），说明该保证在 DXGI 侧依赖 OS 行为、未被源码兜住；
④ 该分支下零新代帧、会话长存、PLI 照收——与 E4 全部吻合，且天然「间歇」
（一有输入即愈）。D2/D3/D4 会留下显著 host 日志且多为终态，与 15 分钟存活
的会话矛盾。

**E1+E2+E3（idr timeout WARN、81 帧冻结、20147 pre-key / pli=204）→ 分支
C（idr_in_flight 无界闩）叠加 B1（硬件 rung force-key HRESULT 不检查）/
B2（非 IDR I 帧误判）与 E（GOP 兜底缺位）。** 理由：① E3 要求 host 数分钟
零 IDR 发布而 delta 持续（§1.2 推导）——活跃内容下唯一能全量吞掉按需 IDR
的就是 :845 的 `idr_in_flight` 门（其出界器只在静态屏 keepalive 里，
:1078-1087 ← :964-967）；② 闩的进入条件恰是 B1/B2 这类「一次性 force 被
消费却无 IDR AU」（mf_gpu_encoder.cpp:922-927 连日志都没有，属静默失败）；
③ GOP=fps×10 若被 MFT 拒绝（E，:786）则无周期 IDR 自愈，闩死即零 IDR——
20147 帧 ≈ 67 个 GOP 周期无一条 key 是 GOP 兜底存在的反证；④ **会话 A 冻结
(492) 后 reload 的会话 B 依旧从零冻结 (20147)**：编码器会话与闩都在共享
host 进程里，跨 viewer 会话存活——reload 不清 host 状态，与 B 依旧冻结
吻合；任何一次 capture 重置（新会话首 AU 必 IDR）才会解开。

次序推断：会话 A 期间一次 B1/B2 类失败闩上 → 冻结；随后的拥塞降档触发
max_w 重置，若桌面已静止则重置本身又落入 D1（无帧 → 无新代帧确认），
两项叠加把 grace 闩到 15+ 分钟。

---

## 5. 修复提案（均未实施）

三个互相独立的小修，合计 ≤50 行，均在 native/desktop：

1. **给 `idr_in_flight` 加提交数出界（修分支 C，~12 行，
   media_pipeline_v2.cpp）**：Impl 加 `uint32_t idr_forced_submits`；
   `TrySubmit` 在 `force` 为真且 Submit kOk 后 `++`；`PollIdrRequest` 的
   `idr_in_flight` 门之前加：武装后已随 ≥`idr_feed_bound` 次提交仍无 IDR
   → 清 `idr_in_flight`（INFO `idr_emergence_exhausted`，对齐 keepalive
   的既有出界语义 :1078-1087）；武装与 `idr_delivered` 时清零。活跃流下
   闩不再永久，最坏退化为「每 ~1 个 lookahead 深度重试一次」。
2. **硬件 rung 的 force-key 失败可见（修 B1 观测面，~4 行，
   mf_gpu_encoder.cpp:922-927）**：检查 `SetValue` HRESULT，失败打
   INFO `gpu_force_key_set_rejected hr=0x%08x`（与软件 rung
   mf_encoder.cpp:620-622 对齐）。本身不改行为，但把「静默白吃 force」
   变成可判定的生产证据。
3. **DXGI 重建后首帧行内容兜底（修 D1，~6 行，dxgi_capture.cpp:705-715 与
   905-910 两处）**：`have_base_frame_==false`（或 surface 路径
   `have_surface_base_==false`）时，`LastPresentTime==0` 但携带可 QI 纹理/
   AccumulatedFrames>0 的首帧按内容处理（对齐 GDI rung 的
   gdi_capture.cpp:125 契约）。若运行时证据显示 DDA 在静态屏直接
   WAIT_TIMEOUT（无帧可收），则此修不够，需重置完成后强制一次全量拷贝
   （>50 行，另行立项）。

**不实施/需补充证据**：分支 B2（编码器吐非 IDR I 帧）与 E（GOP 被拒）是
源码层死端——取决于生产编码器（QSV/NVENC/AMF）运行时行为。鉴别所需的
host 侧日志（一次冻结窗口即可）：

- `idr_request reason=...` 之后**有无** `idr_submitted`（无 → 请求被 C 闩
  或 have_key 门拦下）、`idr_submitted` 之后**有无** `idr_delivered`
  （无 → B1/B2）；
- `diag_media_v2 ... keyframes=` 列冻结不动而 `encoded=` 持续增长（零 IDR
  实锤，media_pipeline_v2.cpp:1750-1770）；
- `force_key_set_rejected` / （提案 2 落地后的）`gpu_force_key_set_rejected`；
- `rt epoch change without IDR`（rt_pipe_server.cpp:957）与
  `rt keyframe_req ... client_reason=pli` 的到达频率；
- 重置窗口内有无 `dxgi surface base frame`（dxgi_capture.cpp:942）/`gdi base
  frame` —— 无即 D1；`capture_reset_rebuild_failed`/`media_v2_stream_init_failed`
  —— 有则转 D2/D3。

agent 侧的对应缓解（reset-grace 确认超时逃逸阀 + urgent 重钥）已在本周期
Task 2 落地（qos_controller.go:152-158、session.go:302-317，commit
0bd75fc）：它保证 grace 不再无限劫持拥塞控制，但**解不开** native 的 IDR
饥饿本身——urgent 请求到达 host 后仍撞上同一批门（:845），这正是本报告
主张 native 侧小修的原因。
