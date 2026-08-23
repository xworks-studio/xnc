# M2-Slice2 稳定性打磨 + shell-host 令牌语义 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** ①Slice1 终审 triage 的稳定性项全部落地(健康分回满/重建风暴退避/web latch/GDI away 提示/SAS 限流);②exec/shell 迁往 xnc-shell.exe 并获得正确令牌语义(user 默认/SYSTEM 显式,NO_ACTIVE_SESSION 快速失败);③worker 监督完整化(崩溃退避/crash-loop 降参);④XIAOXIN 实机门全过。

**Architecture:** xnc-shell.exe = 新 Go module(移植 agent/session 的 ConPTY 封装与多 shell exec 引擎,pipe 协议沿用 M0 XNIP + stdin secret);xnc-core 增 CreateShell/KillShell RPC(0x0120/0x0121,令牌=用户令牌或 SYSTEM@会话,profile 白名单);agent 的 exec/shell handler 改走 coreclient(带 `system` 参数经 SESSION_OPEN 由 server 透传,capability 校验 server 侧=Slice3 完整票据,本片先参数+RBAC owner 门)。

**Tech Stack:** Go(新 module shellhost)、C++(core RPC/监督)、无新依赖。

**Spec:** spec §8(xnc-shell)、§6.3(CreateShell)、§8.4(NO_ACTIVE_SESSION)、§12(迁移)、§15.2(退避/crash-loop);Slice1 终审 triage 项。

## Global Constraints

- 令牌语义(spec §8.4):user = `WTSQueryUserToken(活动会话)`;system = SYSTEM@会话(与 desktop 同法);**无活动用户会话 → NO_ACTIVE_SESSION 稳定码,不隐式提权**;system 需显式请求
- profile 白名单:POWERSHELL/PWSH/CMD/BASH,core 自解析路径,**绝不接受路径参数**(spec §8.2)
- 退避(spec §15.2):worker 崩溃 1s,2s,4s…封顶 60s;60s 内 5 次 → 停自动重启,按类型锁死降参(desktop:--backend=gdi --encoder=software)重启
- 健康分:CaptureReset 成功(base frame 取得)→ 回满 100;-40(create 失败)仅在 gate 稳定(DEFAULT≥2s)时记;重建风暴:同 reason 重建 ≥3 次/10s → 指数退避(1s/2s/4s 封顶 10s)+ `reset_storm` 日志
- pipe_secret 经 stdin 继承句柄(先例);命令行零敏感字段;kill 一律 pid/handle 限定(事故守则)
- 遗留行为:legacy screen 不动;agent 现有 exec/shell 直连路径移除改为 core 路由(dev 构建);CLI `exec/shell --system` 落地(spec §18.8)
- 全程 selftest/Go 测试绿;实测数字诚实入 results

---

### Task 1: Slice1 triage 稳定性项打包

**Files:**
- Modify: `native/desktop/backend_ladder.h/.cpp`(reset 成功→health=100;create-fail −40 仅 gate 稳定 ≥2s 记)、`native/desktop/pipeline.cpp/.h`(重建风暴退避:同 reason ≥3/10s → 1s/2s/4s 封顶 10s + reset_storm 日志)、`native/desktop/xnc-desktop.cpp`(--help 补 --desktop-watch 常开注记)、GDI away 期 STATE(`gdi_stale_secure_desktop` 提示,后端=GDI 且 watch=WINLOGON 时每 5s 一条 STATE)
- Modify: `web/src/pages/DesktopLive.tsx`(SAS pending latch:新点击取消旧 20s 定时器)、`agent/desktop/session.go`(secure_attention in-flight 去重:同会话并发 SAS ≤1,后到直接回复 busy)、`agent/desktop/session_loopback_test.go`(blocking fake 钉异步性)

**验收:** 各项单测/自测新断言绿;`XNC_FORCE_DXGI_HEALTH` 场景重跑:降级→回升后健康分=100;风暴注入(合成连续 reset)→退避间隔序列断言。

### Task 2: xnc-shell.exe(新 Go module)

**Files:**
- Create: `shellhost/`(go.mod module xnc/shellhost;main.go --pipe/--secret-stdin/--profile/--mode interactive|oneshot/--cols/--rows/--cwd/--env/--command/--timeout;conpty_windows.go+exec 引擎自 agent/session **移植复制**,原文件不动;pipe_server_windows.go:XNIP 服务端+stdin secret+M0 握手;消息:`MSG_SHELL_BEGIN=0x0122` `[u16 cols][u16 rows][char profile[16]]`、`MSG_SHELL_DATA=0x0123` 双向 `[u8 stream stdin|stdout|stderr][u32 len][bytes]`、`MSG_SHELL_RESIZE=0x0124` `[u16][u16]`、`MSG_SHELL_EXIT=0x0125` `[u32 exit_code]`、`MSG_SHELL_KILL=0x0126`、`MSG_SHELL_STATE=0x0127` `[char code[24]]`)
- Test: `shellhost/*_test.go`(profile 解析/参数构造矩阵[与 agent/session 现有行为一致:pwsh -NoLogo -NonInteractive -ExecutionPolicy Bypass 等]/pipe 消息编解码黄金字节/集成:oneshot 模式真跑 `cmd /c echo` 经 pipe 往返[winio in-proc])

**验收:** `go vet/test` 绿;oneshot 本机直连测试出 `hello` 输出+exit 0;杀树语义(pkill)。

### Task 3: core CreateShell/KillShell RPC + 监督完整化

**Files:**
- Modify: `native/core/pipe_server.cpp/.h`(0x0120 `[u32 wts][u8 token_kind][u8 profile][u8 mode][u16 cols][u16 rows][u16 cwdLen][cwd utf8][u16 envLen][env][u16 cmdLen][cmd utf8][u32 timeoutSec]` → resp `[u32 pid][u16 nameLen][pipeName][32B secret]`;0x0121 `[u32 pid]`;token: user=WTSQueryUserToken,system=复用 SessionSystemToken;会话校验 live;profile 白名单自解析;stdin secret spawn)、`native/core/supervisor` 逻辑(退避表 1s..60s;crash-loop 5/60s 判定+降参重启 desktop;事件日志)
- Test: `native/core/selftest.cpp`(payload 编解码/白名单拒/无会话→NO_ACTIVE_SESSION[stub wts]/退避表)

**验收:** selftest 绿;XIAOXIN live:经 --sas-probe 风格诊断客户端发 0x0120 user 令牌 → xnc-shell 进程 `tasklist /V` 显示 Session 1 用户 LABS;oneshot whoami 经 pipe 回 LABS;system 令牌回 SYSTEM。

### Task 4: agent exec/shell 改走 core + CLI --system

**Files:**
- Modify: `agent/session/exec.go`、`shell.go`(进程创建改 coreclient.CreateShell + shellpipe 客户端[新 `agent/shellpipe/client.go`,镜像 desktoppipe 模式];保留 1MB 兜底/杀树语义经 0x0126)、`agent/coreclient/client.go`(+CreateShell/KillShell)、`proto/session.go`(ExecParams/ShellParams +`System bool`)
- Modify: `server/internal/api/exec_handlers.go、shell_handlers.go`(+`system` body 字段:RBAC **owner-only**(Slice3 换票据 capability),默认 false;审计字段 system=true)、`cli/cmd_exec.go、cmd_shell.go`(+`--system` flag 透传)
- Test: rpc/handler 单测(fake core);loopback:oneshot 经全链(agent session engine→fake core→fake shell)

**验收:** 单测绿;dev 拓扑 XIAOXIN-DEV:`xnc exec XIAOXIN-DEV whoami` → `labs\xnc...`?实际=用户令牌运行 whoami 输出 `xnc-win\xnc`?——**预期 LABS**(console 用户);`--system` → `nt authority\system`;`xnc shell` 交互 resize 冒烟(手动/pipeprobe)。

### Task 5: XIAOXIN E2E 验收门

**Files:** `scripts/e2e-m2s2.sh`;results 文档。

**验收门:** ①exec user 令牌(whoami=console 用户)②exec --system(owner RBAC:operator 403/owner 202)+whoami=SYSTEM ③无会话路径(单测覆盖+代码走查记录,logoff 实机门=Slice3)④交互 shell 经 ConPTY 全双工+resize(pipeprobe 冒烟)⑤desktop 崩溃监督:运行中 taskkill desktop → ≤60s 退避重启 → viewer 恢复(重连或 generation++)⑥crash-loop 降参:连杀 5 次 → 锁死 gdi/software 参数重启(日志证据)⑦Slice2 打磨回归:健康分回满/风暴退避/storm 日志/GDI away 提示/SAS busy。全部数字入 results;失败如实+结论。

## Self-Review 记录
- spec §8/§12 迁移落地;§6.3 CreateShell 与计划 payload 对齐;RBAC owner-only 为 Slice3 票据前的最低线(裁决记录)
- T2 独立先行可行;T3 依赖 T2 消息契约;T4 依赖 T3;T5 汇总
- Slice3(下一计划):SCM 服务化/screen 退役/agent 降权/server lease/SAS 票据/多显示器/logoff 门/V1 DoD 盘点
