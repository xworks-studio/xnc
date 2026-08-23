# M2-Slice1 桌面与会话可靠性 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** xnc-desktop 在 UAC 安全桌面、锁屏、分辨率变化、显示器拓扑变化、ACCESS_LOST 下连续可用;GDI 回退梯子落地;SAS(secure attention)经 capability 路径可触发;XIAOXIN 实机验收「UAC 可见可点、锁屏可解、变化自愈」。

**Architecture:** DesktopWatch(OpenInputDesktop 500ms 轮询)驱动 DEFAULT⇄WINLOGON 状态机 → 统一 CaptureReset(单一重建路径:desktop switch/分辨率/拓扑/ACCESS_LOST/GPU reset 全走它,generation++ + DISPLAY_CHANGED 广播);输入线程在桌面切换时**主动重绑**(现有重试一次保留为兜底);GDI 后端(BitBlt→CPU NV12→软编)进编码梯子;健康分与 30s DXGI probe 回升;xnc-core 增加 WTS 会话监控(ActiveSessionChanged → drain/respawn)与 SendSAS RPC(capability 门控)。

**Tech Stack:** 既有栈;无新依赖;GDI = GetDC/BitBlt/GetDIBits(user32/gdi32,纯 Win32)。

**Spec:** spec §7.3(DesktopSupervisor)、§7.5(CaptureReset)、§7.6(GDI/健康分)、§6.5(WTS)、§11.6(SAS)、§15.3 失败矩阵;先例:2026-08 revert 记录(UAC 冻结为旧 user-token helper 的已接受现状——本片以 SYSTEM-in-session 重新实证)。

## Global Constraints

- **证据先行**:UAC/锁屏下 DDA 行为(续帧/停帧/需重绑)必须先在 XIAOXIN 实测(计划 T1 探针),实现依据证据而非假设(NumLock 先例)
- 统一 CaptureReset 契约(spec §7.5):复位期间发 `SESSION_STATE{recovering}`;>3s 无 base frame → 降级(GDI 或 failed);generation++ 且每次记 reason;复位后 Force IDR 一次
- GDI 上限 15fps;健康分:初始 100,WAIT_TIMEOUT 0,ACCESS_LOST −10,Create 失败 −40,GPU removed −50,连续无有效帧 −10,<60 触发 GDI;30s probe 恢复 DXGI + IDR(spec §7.6 逐条)
- SAS:`SECURE_ATTENTION` 控制消息(需 capability input.secure_attention)→ agent → xnc-core SendSAS(SendSAS/sas.dll,LocalSystem 服务);**不走伪装键盘**(spec §11.6)
- 输入:桌面切换后注入线程主动 SetThreadDesktop 至当前 input desktop(重试一次保留);Winlogon 桌面注入依赖 SYSTEM-in-session 令牌
- dev 拓扑沿用(XIAOXIN console-run dev agent;logoff 场景会杀 dev agent → 注销/登录门顺延 M2-Slice3 服务化后)
- 验收数字全部实测入 results;失败如实记录(诚实性审查先例)
- 不碰生产路径/legacy screen;selftest/Go 全绿持续;凭据不入日志

---

### Task 1: UAC/锁屏 DDA 行为探针 + DesktopWatch 状态机

**Files:**
- Create: `native/desktop/desktop_watch.h/.cpp`(OpenInputDesktop/GetThreadDesktop 名字轮询 500ms;状态机 DEFAULT/TRANSITION(≤2s)/WINLOGON;回调 onTransition)
- Create: `scripts/uac-probe.ps1`(session1:schtasks 交互式启动一个需提权进程触发 UAC,或直接 `Start-Process -Verb RunAs cmd -ArgumentList '/c exit'`;`-Lock` 变体:rundll32 user32.dll,LockWorkStation;`-UnlockHint` 输出解锁提示)
- Modify: `xnc-desktop.cpp`(DesktopWatch 启停 + 状态日志 + `--desktop-watch` 诊断开关)、`desktop_selftest.cpp`(状态机转移表驱动,含 TRANSITION 超时)

**验收:** ①selftest 状态机绿 ②XIAOXIN 实测:diag-dump 模式 60s 内脚本触发 UAC→取消、锁屏→解锁,日志记录 DDA 帧行为(captured/timeouts 曲线)与桌面名变化序列;结论(续帧/停帧/是否需重绑)写入 report——T2 的实现依据。

### Task 2: 统一 CaptureReset + DISPLAY_CHANGED + 恢复自愈

**Files:**
- Create: `native/desktop/capture_reset.h`(统一入口 `RequestReset(reason)`,合并去抖 ≤100ms)
- Modify: `dxgi_capture.cpp`(ACCESS_LOST/尺寸变化路径改投 RequestReset;重建后 base frame 全量)、`pipeline.cpp/.h`(reset 流程编排:停采→重建→hello 重发(w/h 变化→DISPLAY_CHANGED+generation++)→ForceIDR;恢复中 STATE 事件)、`rt_pipe_server`(DISPLAY_CHANGED 0x010A `[u32 gen][u32 w][u32 h][char reason[24]]`)、desktoppipe/agent/web 词汇跟进
- Modify: `desktoppipe/client.go`(+0x010A 解析→HelloCh/EventCh)、`agent/desktop/session.go`(+display_changed state 转发 control 通道)、web `DesktopLive.tsx`(+toast)

**验收:** selftest(合成 capture:分辨率变化→reset→新 hello;ACCESS_LOST×2→RequestReset 去抖);XIAOXIN:运行中改分辨率(Set-DisplayResolution 1920x1080 ↔ 原)→viewer 收 DISPLAY_CHANGED、2s 内新帧、generation 递增;旋转变化同理(可选)。

### Task 3: GDI 后端 + 健康分梯子 + probe 回升

**Files:**
- Create: `native/desktop/gdi_capture.h/.cpp`(GetDC(NULL)/BitBlt(SRCCOPY|CAPTUREBLT)/GetDIBits→紧凑 BGRA;实现同一 ICapture;15fps 上限;自愈:detect 静止用 CRC 行采样)——参考 dda.c 时代 GDI 知识与 spec §7.6,**不参考外部代码**
- Modify: `pipeline`/`xnc-desktop`(后端选择梯子:DXGI 健康分<60→GDI;GDI 模式 30s DXGI probe 成功→回升+IDR)、`build.bat`(+gdi32.lib)
- selftest:GDI 帧格式(合成 DC?不可——GDI 路径 selftest 仅覆盖选层/健康分决策表;真 GDI 走 T6 实测)、健康分表驱动

**验收:** 决策表 selftest 绿;XIAOXIN:`--backend gdi` 手动模式跑通采集+编码(diag-dump 产物过 nalcheck);健康分强制降级路径(env `XNC_FORCE_DXGI_HEALTH=N` 诊断钩子)→观察日志切换+回升。

### Task 4: xnc-core WTS 监控 + SendSAS

**Files:**
- Create: `native/core/wts_monitor.h/.cpp`(WTSRegisterSessionNotificationEx + 500ms 轮询兜底;console 变化→回调;活动会话变化→现有 capture child drain/terminate+标记,下次 StartCapture 重建)
- Modify: `native/core/pipe_server.cpp`(+`MSG_SAS=0x0110` `[char reason[24]]`:仅当 agent 会话曾以 capability SAS 标记 ATTACH?——简化裁决:0x0110 payload 附 `[u32 ticket_hint]`?M2-Slice3 票据接入前:console 模式仅允许 `--allow-sas` 显式开关,服务模式默认拒绝并记审计日志——**SAS 门控最低线**)、`selftest`
- agent 侧:M1 StartCapture RPC 已有;本任务只加 core 能力;agent 转发在 T5

**验收:** selftest(消息校验矩阵:无 --allow-sas→拒+日志;开关→路径可达[SendSAS 调用 stub 化注入]);XIAOXIN:console `--allow-sas` + 锁屏场景触发 SAS→锁屏出现(T6 门内复用)。

### Task 5: agent/desktop 会话胶水(SAS + 状态词汇)

**Files:**
- Modify: `agent/desktop/session.go`(+control `{"type":"secure_attention"}` → core 0x0110;+desktop state/display_changed → control `state`/`display_changed` 词汇[与 web 对齐])、`session_loopback_test.go`
- Modify: `web/src/pages/DesktopLive.tsx`(Ctrl+Alt+Del 按钮→secure_attention;state toast)
- e2eviewer(+`--sas` step)

**验收:** loopback 断言(fake core 记 0x0110);web tsc 绿。

### Task 6: XIAOXIN E2E 验收门

**Files:** `scripts/e2e-m2s1.sh`(基于 e2e-slice3 模式:探针/场景驱动/产物核对);results 文档。

**验收门(全实测):** ①UAC:session1 触发提权弹窗→viewer 看到安全桌面(依 T1 证据:直见或 reset 后见)→input 点击「是」→提权进程运行(probe 佐证) ②锁屏:SAS→锁屏可见→解锁(labs 空密码:点击登录/回车)→桌面恢复,generation 变化但流不断 ③分辨率:1920↔原生切换→DISPLAY_CHANGED+2s 内新帧 ④降级:强制健康分→GDI 生效(帧继续,码率/fps 降)→probe 回升 DXGI ⑤SAS 门控:无 --allow-sas→0x0110 被拒+审计日志 ⑥回归:slice3 输入门(移动/打字抽样)在上述切换后仍通过(桌面切换后输入连续性)。数字如实入 results;失败场景记录并给结论(哪些进 M2-Slice2/3)。

## Self-Review 记录
- spec §7.3/§7.5/§7.6/§11.6 落地;§15.3 矩阵中 logoff/新登录门顺延 Slice3(服务化前提);多显示器切换=Slice3(§13.2 V1 单屏)
- T1 证据驱动 T2(重绑与否依实测);SAS 门控以 --allow-sas 为最低线(票据 capability 接入=Slice3,记偏差)
- 依赖:T1→T2;T3 独立可与 T1 并行(仍串行派发);T4/T5 独立;T6 汇总
