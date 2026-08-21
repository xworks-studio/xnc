# XNC v2 全 Phase 路线图（Roadmap）

- 日期：2026-08-19
- 上游依据：`docs/superpowers/specs/2026-08-19-xnc-v2-unified-session-design.md`（v2 设计）+ 重写后的 `spec.md`
- 本文档职责：**只定范围、依赖、验收、风险门**。每个 Phase 启动时经 writing-plans 产出任务级实现计划，挂在本文档索引下。

---

## 0. 当前状态

```text
v2 设计文档        ✅ 已确认并提交（9493acf / 746bf60）
spec.md            ⏳ 仍为 v1，Phase 1 Task 1-2 重写
Phase 1 实现计划    ✅ docs/superpowers/plans/2026-08-19-xnc-v2-phase1.md（16 tasks，待执行）
代码               ❌ 未开始（仓库处于 pre-development 状态）
```

## 1. 总览表

| Phase | 名称 | 交付核心 | 依赖 | 规模 | 详细计划 |
| ---- | ---- | ---- | ---- | ---- | ---- |
| 1 | 连接面 | enrollment、设备身份、控制连接、心跳、CLI 基础、compose 部署 | — | L | ✅ 已有 |
| 2 | exec 会话 | 统一会话管理器 + exec + run + CLI | P1 | L | 待写 |
| 3 | shell 会话 | ConPTY 交互终端 + `xnc shell` | P2（会话管理器） | M | 待写 |
| 4 | 数据通道 | file 会话 + tunnel 会话 + RDP/mstsc | P2（会话管理器） | M | 待写 |
| 5 | 多用户 | RBAC 全量执行、成员管理、审计查询 | P1（可与 P3/P4 并行，单人建议串行） | M | 待写 |
| 6 | 桌面预览 | screen 会话 + helper（GDI + named pipe） | P2（会话管理器） | M | 待写 |
| 7 | Web UI | React：登录/节点/终端/预览面板 | P3 + P6（消费其协议） | M | 待写 |
| 8 | Linux agent | 平台层 openpty + systemd + ssh tunnel | P2-P6 会话引擎稳定 | M | 待写（远期） |

规模符号：L ≈ 3-4 周（单人、AI 协作），M ≈ 1-2 周。仅用于排序决策，不做承诺。

商业化的多租户 / OIDC / 品牌化 / 许可**不在本路线图内**（spec §1.6：内部稳定运行约 3 个月无事故后再启动）。

## 2. 各 Phase 边界与验收

### Phase 1 — 连接面（计划已就绪）

**范围**：spec.md v2 重写；proto 包；server 基础（config/迁移/sqlc）；JWT 登录 + Bootstrap Admin；enrollment（幂等注册 + 审计写入）；控制连接（挑战 → HELLO → 心跳 → offline 判定）；nodes API；agent 核心包（identity/DPAPI、machineinfo、enroll、connect）+ Windows 服务宿主；mockagent；CLI（login/whoami/status/version、cluster list、token create、node list/show）；deploy compose 栈；E2E Scenario A/B/G。

**明确不含**：任何 SESSION_OPEN 会话、node disable/enable、audit 查询、agent upgrade、Web。

**验收**：`make build && make test && make e2e` 全绿；NODE_MAIN 真机 agent 上线；**Gate A：mockagent × 1000 心跳压测通过**（验证单实例容量假设，设计 §10）。

**出口条件**：压测数据记入本文件 §4，然后才启动 Phase 2 计划。

### Phase 2 — exec 会话

**范围**：
- server：**统一会话管理器**（SESSION_OPEN/REFUSED/CLOSE 下发、双侧短时 token、会话 WS 拨号粘合、Opening TTL 60s、统一清理路径）——这是 P3-P6 全部复用的核心资产；
- agent：会话引擎（拨号 + 分发）+ ExecManager（子进程、stdout/stderr 1 字节前缀流、agent 侧超时 kill 进程树、EXEC_RESULT）；
- CLI：`xnc exec`（--timeout/--cwd、退出码透传、流式输出）、`xnc run`（script payload ≤ 256KB、临时文件生命周期）；
- 审计：exec.start / exec.finish；协议 conformance 用例第一套（跑 mockagent + agent 双实现）。

**验收**：Scenario C（`xnc exec web-01 hostname` → `WEB-01`）+ Scenario H 的 run 部分（exitCode 透传、临时文件清理、超时矩阵）；CLI golden：退出码表逐条。

**关键设计约束**（来自 v2 设计 §2-§3，实现计划必须遵守）：exec 超时计时器**只在 agent**；client 断开 = 取消（server 发 SESSION_CLOSE）；会话 WS 内 text=控制 / binary=数据。

### Phase 3 — shell 会话

**范围**：ConPTY（Go 封装，**Gate B 前置验证**）；shell 会话词汇（SHELL_BEGIN/RESIZE + 原始 VT 双向流）；pwsh/powershell 降级；CLI `xnc shell`（Windows 终端 raw mode、resize 透传、Ctrl+C）；server 侧 idle 30min / max 8h 计时；会话限额（10/node）；audit shell.open/close。

**验收**：Scenario D（$x=42 状态保持、Ctrl+C 后会话存活、resize）；断连后 pwsh 进程无残留。

**Gate B 已通过（2026-08-20）**：采用 x/sys/windows 直接封装（勿引入第三方 conpty 库）；两个已验证不变量必须带入实现——lpValue 传 HPCON 句柄值（非指针），子进程 STARTUPINFO 必须置 STARTF_USESTDHANDLES。原风险门描述：Phase 3 计划前先做 2-3 天 spike——`x/sys/windows` 手封 CreatePseudoConsole 与 `UserExistsError/conpty` 二选一产出可用原型；若两者均不可行，**停下重新决策**（升级回 brainstorming，这是设计 §10 的显式假设）。

### Phase 4 — 数据通道（file + tunnel + RDP）

**范围**：FileManager（FILE_BEGIN/RESULT/ERROR、sha256 双边校验、256MB 上限、4/node）；TunnelManager（target 枚举 rdp → 127.0.0.1:3389 白名单、纯 binary 转发）；CLI `xnc upload/download`、`xnc rdp`（本地随机端口 + mstsc 启动）；audit file.*/rdp.*；**Gate C：mstsc 经隧道实连 + websocket 粘合吞吐验证**（设计 §10 第二假设）。

**验收**：Scenario E（RDP 通且节点无 3389 暴露）+ Scenario H 全量（sha256、故意损坏 → HASH_MISMATCH）；发布前手工清单中的 tunnel 项。

### Phase 5 — 多用户（Cluster / RBAC / 审计）

**范围**：三角色权限矩阵全量执行（Phase 1 只有 owner 逻辑）；membership 管理 API + CLI（`cluster member add/remove`）；`node disable/enable`；audit 查询 API + `xnc audit list`；错误码矩阵回归（viewer 全 403）。

**验收**：Scenario F + 审计完整性扫描（每动作有记录、日志 grep 无 token/密码/私钥）。

**注**：本 Phase 主要是 API/DB 层，不碰协议；若 P3/P4 任一被 Gate 阻塞，可提前本 Phase 填空档。

### Phase 6 — 桌面预览

**范围**：ScreenManager（Session 0 服务）+ helper exe（WTSQueryUserToken + CreateProcessAsUser + GDI BitBlt → JPEG + named pipe）；screen 会话（SCREEN_BEGIN/STATE、1fps JPEG、≤300KB/帧）；CLI `xnc screen --snapshot/--open`；权限 operator+；audit screen.*。

**验收**：Phase 6 验收场景（1fps 出帧、locked/no_session 状态而非黑屏、关闭无 helper 残留、RDP 会话不受扰动）。

### Phase 7 — Web UI

**范围**：React + TS + xterm.js：登录、Cluster/Node 列表与详情、Web Terminal（消费 shell 会话 WS）、预览面板（消费 screen 会话 WS）；静态资源嵌入 xnc-server 二进制（compose 栈不变）；手工冒烟清单。

**硬约束**：**零协议改动**——只消费既有 REST + 会话 WS；若发现协议缺口，回设计层面补 spec 而非现场发挥。

**验收**：spec §52 页面清单手工冒烟通过。

### Phase 8 — Linux agent（远期）

**范围**：平台层实现（openpty via build tags、systemd 单元、0600 文件凭据存储）；`sh -c` exec；tunnel 白名单 ssh → 22；编译目标 x86_64/aarch64 GNU。

**前置**：P2-P6 会话引擎在 Windows 上稳定运行；spec §60 的跨平台分层约束未被破坏。

## 3. 依赖图

```text
P1 连接面 ──▶ P2 exec(会话管理器) ──▶ P3 shell ──┐
   │                 ├──▶ P4 file/tunnel/RDP ────┼──▶ P7 Web UI
   │                 └──▶ P6 screen ────────────┘
   └──▶ P5 多用户（独立，可填空档）
P8 Linux（依赖会话引擎稳定，随时可插）
```

串行执行顺序建议：1 → 2 → 3 → 4 → 5 → 6 → 7（5 可机动）。P2 是唯一的" hub "：会话管理器在 P2 一次成型，P3/P4/P6 只加 kind。

## 4. 风险门与验证记录

| 门 | 时点 | 内容 | 状态 |
| ---- | ---- | ---- | ---- |
| Gate A | P1 出口 | mockagent × 1000 心跳：server CPU/内存基线、100 并发 exec 占位 | ✅ 2026-08-21 通过：1000/1000 在线稳定（10 波接入 0 失败）；心跳稳态 xnc-server CPU ≤2%、MEM ~75MiB，postgres ≤2.2%/52MiB；100 并发 exec 全部成功（exitCode 0），7s 完成，会话峰后 MEM 88MiB、无 panic。装置教训：Docker Desktop 端口代理经不起 1000 并发 SYN（需分波），dev 栈心跳超时已参数化（compose env XNC_HEARTBEAT_TIMEOUT），mockagent 增 --first 偏移支持多波共享身份目录 |
| Gate B | P3 计划前 | Go ConPTY spike（两方案择一，均不可行则回炉） | ✅ 2026-08-20 双方案 6/6 PASS（pwsh 7.6 + PS 5.1）；**选定方案 B：x/sys 直接封装**（v0.47 原生导出 ConPTY API，91 行 in-repo wrapper，零三方依赖）；两个关键不变量已文档化（lpValue 传句柄值非指针；STARTF_USESTDHANDLES 必需）。详见 docs/superpowers/spikes/gateb-conpty/ |
| Gate C | P4 内 | mstsc 实连 + websocket 粘合吞吐 | ✅ 2026-08-21 双项通过：吞吐（docker loopback 10MB upload 206-215ms / download 173-186ms，双边 sha256，评审复核）；生产真机（TB16G7 RDP 已启用 fDenyTS=0/3389 listener 通，文件 roundtrip 30B ok + diff identical；mstsc GUI 实连由用户手工确认——隧道路径已打通） |
| 稳定性门 | P7 后 | 内部日常运行约 3 个月无事故 + 审计可回溯 → 商业化立项决策 | ⏳ |

（各 Gate 的实测数据在对应 Phase 完成时追加到本节。）

部署记录：2026-08-20 SRV 生产栈上线（Docker 29 + compose：caddy TLS + xnc-server + PostgreSQL，control.xnc.app 公网 HTTPS 验证通过）；NODE_MAIN（LABS-TB16G7）以 XNCAgent 服务形式注册并在线（真实 NAT 出站路径，Scenario A/B 真机成立）。
部署记录（2026-08-20 晚）：Phase 3 合并后同栈滚动更新（server + agent 二进制刷新），生产 shellsmoke 对 LABS-TB16G7 全 PASS（BEGIN/echo/resize/ctrl-c/exit-close，真实公网 + 真 ConPTY）。

## 5. 跨 Phase 恒定轨道

```text
协议 conformance   P2 起建立，每 Phase 追加本 Phase 会话的用例（mockagent + agent 双跑）
skills/xnc 同步    CLI 每新增命令，核对 skills/xnc/references/cli.md 与实际行为一致（发布物纪律，spec §56）
真机矩阵           NODE_MAIN 全程；SRV 自 P1（真机部署验证）；NODE2019 在 P2-P4 补最低版本线
安全回归           每 Phase 收尾跑一次：日志脱敏 grep、错误码矩阵、token 一次性验证
计划纪律           每 Phase 启动 = brainstorming（如有设计缺口）→ writing-plans → 执行；出口 = 验收 + Gate + 本文件更新
```

## 6. 详细计划索引

| Phase | 计划文件 | 状态 |
| ---- | ---- | ---- |
| 1 | `docs/superpowers/plans/2026-08-19-xnc-v2-phase1.md` | ✅ 就绪待执行 |
| 2 | `docs/superpowers/plans/2026-08-20-xnc-v2-phase2-exec.md` | ✅ 就绪待执行 |
| 3 | `docs/superpowers/plans/2026-08-20-xnc-v2-phase3-shell.md`（Gate B 已通过） | ✅ 已完成并合并（2026-08-20，生产验收通过） |
| 4 | `docs/superpowers/plans/2026-08-21-xnc-v2-phase4-files-tunnel.md` | ✅ 就绪待执行 |
| 5 | `docs/superpowers/plans/2026-08-21-xnc-v2-phase5-multiuser.md` | ✅ 已完成并合并（2026-08-21，Scenario F 全量矩阵通过） |
| 6 | `<date>-xnc-v2-phase6-screen.md` | ⏳ |
| 7 | `<date>-xnc-v2-phase7-webui.md` | ⏳ |
| 8 | `<date>-xnc-v2-phase8-linux.md` | ⏳ 远期 |
