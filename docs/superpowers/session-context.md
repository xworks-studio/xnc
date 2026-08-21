# XNC v2 — 会话上下文快照（Session Context Snapshot）

> 本文档保存了跨会话的项目全貌，供新会话或 fork 后快速恢复上下文。
> 最后更新：2026-08-21

---

## 项目概要

**XNC (eXtended Node Control)** — Windows 节点统一运维入口。节点仅需出站 443 长连接，即获得状态观测、命令执行、交互终端、脚本/文件分发和远程桌面。不是穿透工具，是合规友好的反向连接平面。

- **设计文档**：`docs/superpowers/specs/2026-08-19-xnc-v2-unified-session-design.md`
- **技术规格**：`spec.md`（v2 重写后，64 节）
- **路线图**：`docs/superpowers/plans/2026-08-19-xnc-v2-roadmap.md`
- **产品定位**：内部工具（≤10 人）→ 3 个月稳定后商业化

## 架构（v2 统一会话模型）

```
开发机/Web UI ──HTTPS──▶ control.xnc.app（阿里云 SRV）
                           └─ caddy TLS + xnc-server + PostgreSQL（compose 三容器）
                                ▲
Windows 节点 ──出站 WSS───────┘（XNCAgent Windows 服务）
```

**核心设计原则**：
- 控制连接纯 JSON（10 个消息），一切数据流走统一会话模式（双侧一次性 token + 60s Opening TTL + 帧粘合）
- 会话帧规则：text = 控制 JSON / binary = 不透明流字节
- 全 Go 单语言：proto/（唯一定义点）、server/、agent/、cli/、mockagent/、shellsmoke/
- Agent 以 LocalSystem 运行 = 管理员权限

## 完成状态

| Phase | 内容 | 状态 |
|-------|------|------|
| 1 | 连接面（enrollment、设备身份、控制连接、心跳、CLI 基础） | ✅ 合并+生产 |
| 2 | exec 会话（统一会话管理器、ExecManager、xnc exec/run） | ✅ 合并+生产 |
| 3 | shell 会话（ConPTY、xnc shell、win32 键盘协议转义） | ✅ 合并+生产 |
| 4 | 数据通道（file upload/download、tunnel、RDP/mstsc） | ✅ 合并+生产 |
| 5 | 多用户（RBAC、Membership、审计查询、Node disable） | ✅ 合并+生产 |
| 6 | 桌面预览（screen 会话 + GDI helper） | 计划就绪 `2026-08-21-xnc-v2-phase6-screen.md` |
| 7 | Web UI（React SPA、xterm.js Terminal、用户管理） | ✅ 合并+生产 |
| 8 | Linux agent | 远期 |

**风险门**：Gate A（1000 节点负载）✅ / Gate B（ConPTY spike）✅ / Gate C（websocket 吞吐+mstsc）✅

## 生产环境

| 组件 | 地址/位置 | 说明 |
|------|-----------|------|
| SRV | control.xnc.app → 47.243.209.52 | 阿里云 Ubuntu 24.04，Docker 29 + compose |
| xnc-server | 容器 deploy-xnc-server-1 | Caddy TLS 终止 → :8080 |
| PostgreSQL | 容器 deploy-postgres-1 | 16-alpine，healthy |
| 节点 | LABS-TB16G7 | Windows 11 Pro（开发机） |
| Admin | admin@xnc.app | 密码在 deploy/machines.env → SRV_ADMIN_PASSWORD |
| 部署工具 | deploy/deploy_srv.py | push/up/verify 子命令 |
| Agent 部署 | deploy/agent_tb16g7.ps1 | PS Remoting 停→拷→启 |

## 关键文件位置

```
proto/session.go          — 所有会话 payload 类型（KindExec/Shell/File/Tunnel/Screen）
server/internal/api/      — REST 端点 + rbac.go（requireMinRole）
server/internal/session/  — 会话管理器（manager.go + pump.go）
agent/session/            — exec.go / shell.go / file.go / tunnel.go
agent/agent.go            — Agent.Run 装配（OnReady → engine.Register）
cli/cmd_*.go              — 全部 CLI 命令
web/src/pages/            — React 页面（Login/Nodes/NodeDetail/Terminal/Users）
scripts/e2e_phase*.sh     — 各 Phase E2E 脚本
deploy/machines.env       — 测试设备凭据（gitignored）
```

## 开发流程纪律

每个 Phase 按以下流程执行：
1. **设计检查**（有缺口 → brainstorming；无缺口 → 直接下一步）
2. **writing-plans** 产出任务级实现计划
3. **Subagent-Driven** 执行（fresh implementer per task + task review + fix loop）
4. **Final whole-branch review** + fix wave
5. **合并回 main** + 生产部署 + 真机验收

## 会话协议要点（供实现者参考）

- **控制面消息**（10 个）：CHALLENGE / CHALLENGE_RESPONSE / HELLO / HELLO_ACK / HEARTBEAT / HEARTBEAT_ACK / SESSION_OPEN / SESSION_REFUSED / SESSION_CLOSE / ERROR
- **会话建立**：REST POST → 202 {sessionId, token, expiresAt, websocketUrl} → agent 拨 /api/agent/session?token= → client 拨 /api/session/{id}?token= → server 粘合
- **exec**：binary [1B 前缀][块]（0x01 stdout / 0x02 stderr）+ text EXEC_RESULT
- **shell**：binary 原始 VT 双向 + text SHELL_BEGIN/SHELL_RESIZE
- **file**：text FILE_BEGIN/RESULT/ERROR + binary 64KB 块
- **tunnel**：纯 binary 零 text（连不上 → text ERROR RDP_NOT_AVAILABLE）
- **SetReadLimit(1MiB)** 三处必须（server sessionws + cli dialSession + agent engine）——默认 32KB 会杀 64KB 文件块

## CLI 命令树

```
xnc login [--server URL] [--email E]
xnc whoami / status / version / --version
xnc cluster list / create / delete
xnc cluster member list / add / remove
xnc token create <cluster> [--ttl 30m] [--max-uses N]
xnc node list [--cluster c] [--status s] / show / disable / enable
xnc exec <node> [--timeout N] [--cwd PATH] [--] <command...>
xnc run <node> (--file x.ps1 | -) [--timeout N]
xnc shell <node> [--cols N] [--rows N]
xnc upload <node> <local> <remote> / download <node> <remote> <local>
xnc rdp <node> [--local-port N]
xnc audit list [--node n] [--user u] [--action a] [--since 7d]
```

## 已完成的 UX 优化（2026-08-21 测试轮）

- audit list 人类可读（JOIN users/nodes → email/name 而非 UUID）
- exec --json 静默模式（envelope-only，管道友好）
- node list 加 LAST SEEN 列（相对时间 "2m ago"）
- member list 加 USER_ID 列
- upload/download 进度指示（stderr）
- Web UI：Last seen 相对时间 / Disable 确认 / Terminal Reconnect 按钮
- `xnc --version` flag

## 测试环境

| 设备 | 系统 | 凭据位置 | 用途 |
|------|------|----------|------|
| SRV | Ubuntu 24.04 | machines.env SRV_* | 公网 server |
| NODE_MAIN (LABS-TB16G7) | Win 11 Pro | machines.env NODE_MAIN_* | 主测试节点（PS Remoting） |
| 本机 (LABS-XIAOXIN) | Win 11 | — | 开发机（Docker Desktop + Go + Node.js） |

- 本机无 make；sqlc v1.31 已装；Node.js v24 + npm 11
- E2E 用 `bash scripts/e2e_phaseN.sh`（Git Bash）
- 生产部署用 `py deploy/deploy_srv.py push && py deploy/deploy_srv.py up`

## 下一步

Phase 6（桌面预览）计划已就绪，待执行：
- 文件：`docs/superpowers/plans/2026-08-21-xnc-v2-phase6-screen.md`
- 8 个任务：proto → server 端点 → agent Screen Handler → helper.exe → 注册 → CLI → Web UI → E2E
- 执行方式：用户选择了 Subagent-Driven

Phase 6 完成后：Phase 8（Linux，远期）或稳定运行期（3 个月后商业化决策）。
