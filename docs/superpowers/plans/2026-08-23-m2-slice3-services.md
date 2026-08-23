# M2-Slice3 服务化与生产化 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** XNCCore 成为 Windows 服务(SCM)、dev 拓扑双服务化(解锁 **logoff 门**)、legacy screen 退役(快照走 xnc-desktop)、server 侧 lease+generation 撤销+SAS capability、多显示器枚举/切换;XIAOXIN 实机过 logoff 存活/登录恢复门,并完成 spec §53 V1 DoD 盘点。

**Architecture:** xnc-core 增 `--service`(SCM dispatch,服务名参数化);agent 增服务名/state 目录覆盖(XNCAgentDev);agent 会话意图持久(会话变更/worker 退出→自动 re-StartCapture);screen kind 移除,`xnc screen --snap` 经 core spawn `xnc-desktop --jpeg-single`;server 会话 manager 增 per-node lease 仲裁 + capability 集(SESSION_OPEN 下发,agent/core 强制);desktop 多输出枚举进 HOST_HELLO + SWITCH_DISPLAY。

**Tech Stack:** 既有;无新依赖。

**Spec:** spec §4.3(服务)、§6.5(WTS)、§7.9/§13.2(多显示器 V1 单编+切换)、§11.1(server lease)、§14(capability)、§15.4、§20 M2、§53 V1 DoD。

## Global Constraints

- **双服务 dev 隔离**:XNCAgentDev/XNCCoreDev 服务名+`C:\ProgramData\XNCAgentDev` 状态目录;XIAOXIN 生产 XNCAgent/目录零接触(验收含隔离证明)
- 服务:LocalSystem、Automatic、XNCCoreDev 依赖 XNCAgentDev;`--service <name>` 参数;console 模式保留(诊断);SCM 停止→Drain(释放按键/停采/断 pipe)
- logoff 自愈:agent 持「desktop 会话意图」;worker_exited/session_changed → 会话仍存在(server 逻辑会话 30s 窗)→ 自动 re-StartCapture;新登录→ 意图仍在→ 重建;**登录界面注入**:登录 UI 会话=console 会话(WTSConnected),TokenManager spawn 合法——锁屏/登录画面输入沿用已证通路
- lease 仲裁移 server:per-node 单 lease(60s TTL 续期=活跃输入即续);授予/拒绝经 SESSION_OPEN params + control 词汇;generation 变化→server 撤销重授;agent 强制 server 签发的 leaseId(本地表仅缓存);**迁移兼容**:agent 侧仲裁代码退役
- SAS capability:server 会话 capability 集(默认 operator={view,input};owner 全量;desktop 会话创建时按 RBAC 定 `secure_attention` 有无)→ SESSION_OPEN params 下发 → agent 转发 core 前 校验;core 维持 --allow-sas 双保险(服务模式默认 on?**裁决:服务模式=on,console=flag**——票据完整化前最低线由 server capability 把门)
- screen 退役:proto kind 移除+agent handler 移除;`xnc screen --snap` → server 端点保留路由到 desktop 快照(core spawn xnc-desktop --jpeg-single,等 pipe JPEG 帧回传);server screen 端点 404 迁移提示(M2 过渡)——**裁决:保留 REST 端点与 CLI 语法,实现换轨**
- 多显示器:dxgi 枚举全部 attached 输出→HOST_HELLO `displays[]`(index/origin/w/h/primary);SWITCH_DISPLAY control→SET_VIDEO_CONFIG→CaptureReset(rebuild 绑定新输出)+DISPLAY_CHANGED(reason=switch);V1 单输出编码
- kill 一律限定;凭据 stdin;诚实数字;全测试持续绿

---

### Task 1: xnc-core --service(SCM)
**Files:** `native/core/xnc-core.cpp`(+--service <name> 分支:SCM dispatch/Start/Stop/待停 Drain)、`native/core/service.cpp/.h`(SERVICE_TABLE/状态汇报/watchdog 兼容)、`scripts/dev-services.md`+`scripts/install-dev-services.ps1`(XNC*Dev 安装/卸载)。
**验收:** selftest 绿;XIAOXIN 装 XNCCoreDev→`Get-Service` Running;console 模式回归;Stop→Drain 日志(释放按键)。

### Task 2: agent 服务化 dev 支持 + 会话意图自愈
**Files:** `agent/svcapp/service_windows.go`(+服务名/state 目录/env 覆盖参数)、`agent/desktop/session.go`(+intent 持久:DesktopHostExited/session_changed→退避 re-StartCapture,逻辑会话存活期内)、`agent/desktop/core_windows.go`(core 掉线重连后重建)。
**验收:** 单测(意图状态机:exited→reattempt→成功/逻辑会话过期放弃);XIAOXIN:XNCAgentDev 服务在线(dev server)。

### Task 3: screen 退役 + --jpeg-single
**Files:** `native/desktop/xnc-desktop.cpp`(+--jpeg-single:采一帧→JPEG(GDI+ 轻量或 WIC?用现有 jpeg 路径参考 screen-helper jpeg_windows.go——**C++ WIC 最小编码**)→stdout/文件→exit)、`agent/session/screen.go` 改道(kind 保留名但走 core spawn 一次性 desktop)、proto 注释更新、server screen_handlers 路由不变。
**验收:** C++ 单测(WIC 编码首字节/JFIF 头);XIAOXIN:`xnc screen --snap` 出 JPEG(非空+JFIF 头);legacy helper 不再被 spawn。

### Task 4: server lease 仲裁 + capability 下发
**Files:** `server/internal/session/manager.go`(+per-node Lease{sessionID,expiresAt};授予/续期/撤销;generation 钩子)、`server/internal/api/desktop_handlers.go`(params+capabilities 下发)、`proto/session.go`(+DesktopParams.Capabilities[]/LeaseId)、`agent/desktop/input.go`(agent 仲裁退役→执行 server lease)、web/e2eviewer 词汇对齐。
**验收:** server 单测(授予/抢占拒绝/断连撤销/generation 撤销);agent loopback(server 签发 lease 生效/非持有拒);XIAOXIN 双 viewer 移交实测。

### Task 5: 多显示器枚举+切换
**Files:** `native/desktop/dxgi_capture.cpp`(枚举所有输出存表)、`rt_pipe_server`(HOST_HELLO displays[])、`MSG_SWITCH_DISPLAY=0x0128` `[u32 idx]`→CaptureReset 绑新输出、desktoppipe/agent/web(SWITCH_DISPLAY control→viewer 显示列表)。
**验收:** selftest(枚举表/切换消息/非法 idx 拒);XIAOXIN 单屏:displays=[1] 且 switch(0) 幂等、switch(9) 拒(日志);TB16G7 若多屏则实测切换(探测)。

### Task 6: logoff/logon 自愈门 + V1 DoD 盘点
**Files:** `scripts/e2e-m2s3.sh`(服务化拓扑:logoff(session1 schtasks `shutdown /l`?——logoff=`logoff 1`?用 logoff 命令)→agent 服务存活(节点在线)→登录 UI 会话画面可见(自愈 spawn)→输入注入空密码登录→桌面恢复全链)+ V1 DoD checklist 逐项盘点文档。
**验收门:** ①logoff 后 60s 节点仍在线+agent 服务 Running ②登录 UI 经 viewer 可见(自愈)③注入登录(空密码)成功④桌面恢复+输入连续 ⑤lease:logoff 撤销/重新授予 ⑥V1 DoD(§53)逐项:pass/部分(Slice 外依赖如实)/fail——结果入 results 文档。

## Self-Review 记录
- 依赖:T1+T2→T6;T3/T4/T5 相对独立;T4 动 agent 仲裁(与 T2 的 session.go 有交叠——串行派发避免冲突)
- 裁决已记:服务模式 SAS=on;screen 端点保语义换实现;agent 仲裁退役
- V1 DoD 预计「部分」项:插拔(物理)、4K、崩溃后 5s 恢复(实测值)
