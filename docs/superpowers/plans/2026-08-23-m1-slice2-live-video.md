# M1-Slice2 实时视频到浏览器 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 浏览器(及 Go E2E viewer)经 dev server + TURN relay 实时观看 XIAOXIN 桌面:desktop session kind 全链路(server→agent→core→desktop→pipe→Pion RTP→viewer),含「静止桌面新订阅者按需 IDR」语义(Slice1 承接项)。

**Architecture:** 视频 = xnc-desktop 实时 pipe(订阅者模型,ENCODED_FRAME 推送)→ agent desktop 会话 → Pion(iceTransportPolicy=relay,H264 RTP)→ TURN(coturn,dev 非 TLS)→ dev server 会话中继(信令)→ viewer(浏览器页 + Go 自动化 viewer)。输入/光标/RPC-protobuf = Slice3。本片验证「看」;「控」在 Slice3。

**Tech Stack:** Go(Pion webrtc/turn 客户端;agent/server 扩展)、C++(xnc-desktop pipe 模式)、coturn(docker compose dev)、React/TS(desktop 实验页)。

**Spec:** docs/superpowers/specs/2026-08-22-xnc-agent-refactor-spec.md(§9.4 消息、§10 桌面会话协议、§7.5 IDR 合并语义、§20 M1);承接:2026-08-22-m1-slice1-capture-spine-results.md「M1-Slice2 承接」节。

## Global Constraints

- IDR 语义(spec §7.5 + Slice1 承接):订阅者 needsKeyframe 合并为一次 ForceNextIdr;**静止桌面新订阅者 = 管线重喂缓存帧产出新 IDR(按需 warm-up,上限 2×窗口或 2s,绝不重复 force)**;主动 IDR 最小间隔 500ms;IDR 请求带 reason 记账
- Pipe 消息沿用 M0 XNIP 帧;desktop pipe 消息 = Slice1 计划 §9.4 集合的**固定二进制子集**(protobuf 迁移 Slice3):`MSG_ATTACH=0x0102` `[u32 sub_id][u32 max_fps][u32 max_w][u32 bitrate]`、`MSG_DETACH=0x0103` `[u32 sub_id]`、`MSG_KEYFRAME_REQ=0x0104` `[u32 sub_id][char reason[32]]`、`MSG_FRAME=0x0105 event` `[u32 sub_id_target=0 广播][u64 mono_us][u8 key][u32 len][payload]`、`MSG_HOST_HELLO=0x0106` `[u32 gen][u32 w][u32 h][u32 fps][u32 max_subs]`、`MSG_STATE=0x0107` `[char code[32]][u8 recoverable]`
- 每订阅者发送队列深度 ≤3:满 → 丢 delta → needsKeyframe;控制消息不丢
- WebRTC:iceTransportPolicy=relay,dev 用非 TLS turn:`turn:<devhost>:3478?transport=tcp`(TLS/443 生产化 = M2);凭据 = dev 静态用户(lt-cred-mech),M2 换 REST
- dev 拓扑:XIAOXIN 运行 **console-run dev agent**(不装服务、不碰生产 XNCAgent 状态);dev server + coturn 跑 LABS-DEV docker;LABS-DEV 只构建+浏览器,不采集
- 不改生产路径:server 现有 kind 行为、legacy screen、生产 agent 发布流程零影响;`agent/screen-helper/**` 依旧只读
- 帧上限沿 proto.MaxSessionFrameBytes=8MiB;turn 凭据/密钥不入日志
- 每 C++ 改动保 selftest 绿;每 Go 改动保 `go test ./...` + vet 绿

## 参考实现索引

| 参考 | 位置 | 用途 |
|---|---|---|
| M1-Slice1 desktop 管线 | native/desktop/* | pipe 模式复用 Pipeline/MfSoftEncoder/DxgiCapture |
| core pipe_server | native/core/pipe_server.cpp | pipe 服务端模式抄结构 |
| agent screen 会话 | agent/session/screen.go、screen_windows.go | 会话 handler 形态、helper 生命周期(**只参考 spawn 生命周期思路,desktop 的 spawn 走 core RPC**) |
| 统一会话/server pump | server/internal/session、proto/session.go | desktop kind 接入点;pump 已透传 text+binary |
| server screen_handlers | server/internal/api/screen_handlers.go | REST/参数校验/RBAC 样板 |
| Web ScreenPreview | web/src/pages/ScreenPreview.tsx | 页面骨架(本片换 RTCPeerConnection) |

---

### Task 1: 开发拓扑 bring-up(dev server + console-run dev agent)

**Files:**
- Modify: `deploy/docker-compose.yml`(+coturn 服务)、`deploy/docker-compose.dev.yml`(端口/凭据 env)——或新增 `deploy/docker-compose.slice2.yml` 若侵入过大
- Modify: `agent/cmd/xnc-agent/main.go`(+`run-dev-console` 子命令:不起 SCM、状态目录 `%TEMP%\xnc-dev-agent-<pid>`、server URL 来自 `--server` flag/env、enrollment token 位置参数,复用现有 enroll/connect 逻辑)
- Create: `scripts/dev-topology.md`(端口/凭据/一键步骤备忘)

**Interfaces:**
- `xnc-agent.exe run-dev-console --server http://<devhost>:<port> --token <enroll-token> [--name XIAOXIN-DEV]`(进程前台运行,Ctrl+C 退出,状态全在 temp,退出即清)
- dev compose 暴露:server HTTP/WS(现有端口)+ coturn 3478 tcp/udp + relay 端口段 49160-49200/udp
- 验收(全手动+命令记录):①dev compose up,XIAOXIN 上经 exec 跑 run-dev-console(后台 schtasks session1)②dev server 节点列表见 dev 节点在线 ③`xnc exec`(指 --server dev)穿透该 dev agent 跑 `hostname` ④生产 XNCAgent(control.xnc.app)不受扰(两状态目录隔离证明)
- 先调查再动手:agent 现有 CLI/状态目录/server 覆盖能力(main_windows.go、connect/client.go、enroll)——如已有等价机制(环境变量/flag)直接用,不重复造

### Task 2: xnc-desktop 实时 pipe 模式 + 订阅者/按需 IDR

**Files:**
- Create: `native/desktop/rt_pipe_server.h/.cpp`、`native/desktop/subscribers.h`
- Modify: `xnc-desktop.cpp`(`--console-rt --pipe <name> --secret <hex> --max-subs 4`,与 --console-diag 并存)、`desktop_selftest.cpp`、`build.bat`

**Interfaces(消息即 Global Constraints 的固定二进制集):**
- `RtServer::Serve(ICapture&, MfSoftEncoder&, opts)` 线程模型:capture/encode 沿用 Pipeline 逻辑抽出共用(或 Pipeline 加 sink 回调:每 AU + key 标志 + mono_us 交给 RtServer 分发——**选 sink 回调方案,diag-dump 与 rt 共用一条管线**)
- 订阅者:sub_id 自增;attach 即 needsKeyframe;发送队列 3,溢出丢 delta 标记 needsKeyframe;合并触发按需 IDR(warm-up 重喂缓存帧)
- HOST_HELLO 在 attach 后立即发;STATE 事件透传(dxgi_access_denied 等)
- selftest:合成 capture + 真 encoder,两个 fake pipe 客户端(winio 或 C++ 客户端函数直接注入):attach→收到 HOST_HELLO+IDR;静止后第二个 attach→新 IDR(reason=sub_join);队列溢出路径
- XIAOXIN 验证:`--console-diag` 加 `--pipe` 可选参数时可被 Go 测试客户端连接(沿用 diag-deploy 部署)

### Task 3: agent 接线(core RPC + desktop pipe 客户端)

**Files:**
- Modify: `agent/coreclient/client.go`(+`StartCapture(wtsSession uint32) (hostPid, pipeName string, secret []byte, err)`——发 0x0100 固定 payload `[u32 wts][u32 pad]`,解析 resp `[u32 pid][u16 nameLen][name][32B secret]`;+`StopCapture`)+ `native/core/pipe_server.cpp`(实现 0x0100:校验+diag-spawn 同路径 spawn desktop,pipe 名/secret 生成与回传;desktop 退出→事件)
- Create: `agent/desktoppipe/client.go`(连 xnc-desktop pipe:握手(复用 M0 双向 HMAC)+ATTACH+FRAME 泵,回调 `OnFrame(key bool, monoUs uint64, au []byte)`/`OnHello`/`OnState`)

**Interfaces(供 T4/T6):**
- `desktoppipe.Dial(pipe, secret, subOpts) (*Sub, error)`;`Sub.FrameCh() <-chan Frame`;`Sub.RequestKeyframe(reason)`;`Sub.Close()`
- core StartCapture 幂等(已运行则返回既有 pipe/secret,同 secret 不重复 spawn)
- selftest:Go fake desktop pipe server(复用 coreclient 测试基建)+ 真 core(需提权时 skip 门,同 M0 模式)

### Task 4: agent Pion 传输 + Go E2E viewer

**Files:**
- Create: `agent/desktop/transport.go`(Pion:relay-only ICE、H264 packetization-mode1、RTP 时间戳=mono_us@90kHz、track 写入)、`agent/desktop/session.go`(desktop 会话 handler:kind=desktop,信令 OFFER/ANSWER 经会话 WS 文本帧中转——定义 JSON 信令词汇 `{type:offer/answer/ice, ...}`)、`agent/desktop/qos_min.go`(TWCC 开;PLI→RequestKeyframe;本片仅透传统计)
- Create: `tools/e2eviewer/main.go`(Go module,同 nalcheck 模式:Pion viewer——建 offer(带 turn 配置)、收 track、统计 RTP 包/关键帧标记/首帧时延、写 .h264 落盘(解 RTP 还原 AU)、`--expect-first-frame-ms`/`--expect-keyframes N` 断言,退出码 0/1)
- Modify: `agent/go.mod`(+pion webrtc/rtcp 依赖)、`go.work`

**Interfaces:**
- `desktop.NewSession(...)` 挂进 session engine kind 表(desktop);SESSION_OPEN params 增加 `signaling:"webrtc"`、`turn:{urls,username,credential}`(经 server 下发)
- viewer 侧 SDP 交换:stdin/文件 或 `--offer-file/--answer-file`(E2E 由脚本粘合)
- 验收(本机):单机回环——desktop --console-diag(本机会话采集自己)→ agent desktop 会话(本机 console-run dev agent)→ e2eviewer 连 dev server:首帧 <2s、变化期间持续帧、PLI(发 RTCP PLI)→ 新 IDR

### Task 5: server desktop kind + TURN 凭据

**Files:**
- Modify: `proto/session.go`(KindDesktop + params)、`server/internal/api/`(POST /api/nodes/{id}/desktop:RBAC operator+、单会话、返回 websocketUrl+turn 配置)、`server/internal/session/manager.go`(desktop kind 治理:idle 无订阅 5min 关)
- Modify: `deploy/*`(coturn 静态凭据 env 进 server 配置下发)

**Interfaces:** REST 返回 `{sessionId, token, websocketUrl, turn:{urls,username,credential}}`;agent SESSION_OPEN params 同步携带 turn;审计 desktop.open/close。desktop WS = 现有 pump 透传(信令 JSON text 帧)。

### Task 6: web 桌面页 + XIAOXIN E2E 验收门

**Files:**
- Create: `web/src/pages/DesktopLive.tsx` + 路由 `/desktop/:nodeId`(RTCPeerConnection relay-only、`<video>` 渲染、统计浮层:fps/首帧时延/PLI 按钮/ICE 状态;信令经会话 WS JSON)
- Create: `scripts/e2e-slice2.sh`(XIAOXIN dev agent + dev server + e2eviewer 全链路;变化驱动 session1 schtasks;静止晚期 attach 场景(第二个 viewer 在静止期加入))
- Test: e2eviewer 断言 + 浏览器页手动核验(截图/录屏入 results)

**验收门(XIAOXIN):** ①首帧 p95<2s ②变化期 ≥20fps 有效帧 ③静止期第二 viewer 加入→3s 内收到 IDR 入流 ④PLI→2s 内 IDR ⑤8MiB/队列/退订清理无残留进程。结果入 `docs/superpowers/plans/2026-08-23-m1-slice2-live-video-results.md`。

## Self-Review 记录
- 承接闭环:Slice1「静止入流」= T2 按需 IDR + T6 门③;M1 spec §10.2 信令时序本片以「dev server 会话 WS 中转 OFFER/ANSWER」最小实现(turn 非 TLS 为 dev 简化,TLS=M2)
- Slice3 留:输入/DataChannel input+mouse、光标通道、protobuf codegen、BuildChildCommandLine must-fix、lease
- 依赖顺序:T1→T3(需 dev agent)→T6;T2 独立可先行;T4 依赖 T3;T5 依赖 T1
