# XNC v2 架构设计 — 统一会话模型（Unified Session Model）

- 日期：2026-08-19
- 状态：已确认（诊断 → 方向决策 → §1 协议 / §2 工程结构分节批准）
- 与 spec.md 的关系：本设计**取代** spec.md 中第 9 节变更映射列出的章节；未列出的章节继续有效

---

# 1. 背景与决策总览

## 1.1 v1 的三处结构性别扭（owner 确认）

1. **协议/会话模型混乱**：五种数据通道、三种帧规则并存；shell 以自定义二进制帧头复用控制连接，而 tunnel / file / screen 各自拨独立 WS；exec 走控制连接的 JSON 消息。
2. **技术栈过重**：Go + Rust + TS 三语言；协议在 Go（Server）与 Rust（Agent）双实现，MockAgent 是第三份镜像，1 人团队长期维护不可持续。
3. **MVP 范围错位**：定位 Agent-First，第一版却并行交付全套 React Web UI。

同时确认**接受、不重构**的：中心 Server 中转一切数据流的合规模型（仅出站 443、禁穿透/VPN/入站）。

## 1.2 决策总览

| 决策点 | v1 | v2 |
| ---- | ---- | ---- |
| 协议模型 | 混合：控制连接复用 shell 二进制帧 + 各 kind 独立 WS + 三种帧规则 | **统一会话模型**：控制连接纯 JSON；一切数据流（含 exec）走同一会话模式 |
| Agent 语言 | Rust（tokio + portable-pty） | **Go**（全栈单语言，协议单一定义） |
| Web UI | 与 CLI 从 Phase 1 并行交付 | **整体后置 Phase 7**，CLI 先行 |
| 功能范围 | Phase 1-6 | **只重排不砍**，Web UI 独立成相 |
| 部署形态 | 二进制 systemd 托管 + PG 同机安装/容器 | **Docker Compose**：caddy + xnc-server + postgres 三容器（见 §4.5） |
| 合规模型 / 数据模型 / 认证 / DB / CLI 契约 | — | **全部不变**（见第 8 节） |

## 1.3 顺带修复的 v1 正确性隐患

v1 中 exec / shell 输出与心跳共享控制连接发送队列：10 MB 输出可能顶住心跳导致节点被误判离线。v2 控制连接只承载小 JSON 消息，该问题类整类消失。会话限额也天然按连接执行。

---

# 2. 连接模型

系统只有**两种连接**。

## 2.1 控制连接（agent ↔ server，常驻、每 agent 一条）

```text
wss://server/api/agent/connect
```

认证流程不变（v1 第 9/13 节）：WS 建立后 server 下发 CHALLENGE（nonce，60s 有效，单次）→ agent 用设备私钥签名 → server 验签 → agent 发 HELLO → server 回 HELLO_ACK → 进入 Ready。

之后控制连接上**只允许 JSON 文本帧**。收到二进制帧视为协议错误，立即断开。

消息 envelope：

```json
{"type": "...", "payload": {}}
```

无 requestId——控制面是通知性质；会话数据与终态全部走会话连接，client 经 REST 响应中的 sessionId 关联。

## 2.2 会话连接（按需短连，一条会话一条连接，两侧对称拨号）

```text
client 侧：POST /api/nodes/{id}/{kind}
          → {sessionId, token, expiresAt, websocketUrl}
          → 连接 websocketUrl

agent  侧：server 经控制连接下发 SESSION_OPEN {sessionId, kind, params, agentToken, wsUrl}
          → agent 拨 wss://server/api/agent/session?token={agentToken}

server：两侧都连上后双向粘合（byte stream forwarding）
```

双侧 token 均为：随机生成、60 秒有效、单会话、单用途、使用后失效（沿用 v1 第 24 节 Tunnel Token 语义，泛化到所有 kind）。token 经 URL query 传递（CLI/Agent 侧可用 `Authorization: Bearer` 头；浏览器 WebSocket 无法设头，统一用 query，风险由单次 + 60s + TLS 约束）。

建立时序（两侧到达顺序不限，先到者等待，TTL 60s）：

```text
Client                    Server                          Agent
  │ POST /nodes/{id}/{kind}  │                               │
  │────────────────────────▶ │                               │
  │                          │ SESSION_OPEN ────────────────▶│
  │ ◀─ {sessionId,token,wsUrl}                               │
  │                          │ ◀──── dial /agent/session ────│
  │ ── connect wsUrl ──────▶ │                               │
  │                          │ both up → glue                │
  │ ◀══════════ 会话数据双向粘合（直至任一侧关闭）═══════════▶│
```

agent 拒绝（不支持的 kind、本地资源不足）：控制连接回 `SESSION_REFUSED {sessionId, code}`，server 关闭 client 侧并返回对应错误。

## 2.3 会话生命周期（所有 kind 一致）

```text
Created  POST 返回，sessionId/token 生成
Opening  等两侧拨号（TTL 60s，超时清理）
Open     粘合开始
Closed   任一侧断开 / 会话超时 / 节点离线 / 主动关闭
```

关闭时 server **必经控制连接**下发 `SESSION_CLOSE {sessionId, reason}`，agent 据此执行清理：杀进程树、删临时文件、退 helper、关 TCP。这是 v2 新增的统一清理路径——v1 中每种会话各自处理清理是最易遗漏之处。

超时责任划分：

- exec：`params.timeoutSec` 由 **agent** 计时并 kill 进程树（单一计时器，server 不重复计时）。
- shell：idle 30min / max 8h 由 **server** 计时（server 拥有两侧），到点关双侧。
- Opening TTL 60s 由 server 计时。

## 2.4 统一帧规则（一条会话 WS 内部）

```text
text frame   = 会话级控制 JSON {type, payload}
binary frame = 不透明流字节，语义由 kind 决定
```

v1 的自定义帧头 `[1B frameType][16B sessionId]` **删除**——会话连接本身就是会话，无需复用标识。

---

# 3. 协议定义

## 3.1 控制面消息（全部 10 个，v1 约 24 个）

| 方向 | type | payload |
| ---- | ---- | ---- |
| server → agent | `CHALLENGE` | `{nonce}` |
| agent → server | `CHALLENGE_RESPONSE` | `{signature}` |
| agent → server | `HELLO` | `{nodeId, hostname, agentVersion, shellType}` |
| server → agent | `HELLO_ACK` | `{}` |
| 双向 | `HEARTBEAT` / `HEARTBEAT_ACK` | `{}`（30s 间隔，90s 无心跳判 offline，可配置） |
| server → agent | `SESSION_OPEN` | `{sessionId, kind, params, agentToken, wsUrl, expiresAt}` |
| agent → server | `SESSION_REFUSED` | `{sessionId, code, message}` |
| server → agent | `SESSION_CLOSE` | `{sessionId, reason}` |
| 双向 | `ERROR` | `{code, message}` |

`kind ∈ {exec, shell, file, screen, tunnel}`。

## 3.2 各 kind 会话词汇

总原则：text = 控制，binary = 数据。每种会话只有 0-3 个 text 类型。

**exec**

```text
SESSION_OPEN params: {command? | script?, timeoutSec=300, cwd?}
binary frame: [1 字节流标识][字节块]     0x01=stdout  0x02=stderr
text frame:   EXEC_RESULT {exitCode|null, timedOut, durationMs}
```

- script ≤ 256 KB，agent 落地 `%TEMP%\xnc-<sessionId>.ps1` 执行后必删（含失败路径）。
- 超时：agent 计时到点 kill 进程树，发 `EXEC_RESULT {exitCode: null, timedOut: true}` 后关会话。
- 取消：client 断开会话 WS → server 发 SESSION_CLOSE → agent kill 进程树（无需回传结果，连接已断）。
- 1 字节流前缀保住 CLI 契约：`--json` 的 data 含独立的 stdout / stderr 字段。

**shell**

```text
SESSION_OPEN params: {cols, rows, shell?}     shell 缺省按探测结果
text frame:   SHELL_BEGIN {shell}             实际 shell（pwsh / windows-powershell）
text frame:   SHELL_RESIZE {cols, rows}       client → agent
text frame:   ERROR {code: SHELL_START_FAILED}  创建失败，随后关连接
binary frame: 原始 VT 字节，双向，不区分方向（连接方向即语义）
```

ConPTY + pwsh.exe / powershell.exe 自动降级，Ctrl+C 经终端输入传递——全部沿用 v1 第 15-20 节。

**file**

```text
SESSION_OPEN params: {direction, path, size?, sha256?}
text frame:   FILE_BEGIN {direction, path, size, sha256}
text frame:   FILE_RESULT {bytes, sha256, ok}
text frame:   FILE_ERROR {code}
binary frame: 数据块（默认 64 KB/条）
```

sha256 双边校验、单文件 ≤ 256 MB、绝对路径无白名单（operator+ 权限 + 审计兜底）——沿用 v1 第 43/58 节。

**screen**（Phase 6 交付，协议现在锁定）

```text
SESSION_OPEN params: {fps=1, quality=60, maxWidth=1280}
text frame:   SCREEN_BEGIN {width, height, state}
text frame:   SCREEN_STATE {state}            capturing / locked / no_session
binary frame: 单帧完整 JPEG（WS message 即一帧）
```

helper + named pipe + GDI 方案沿用 v1 第 64 节。

**tunnel**

```text
SESSION_OPEN params: {target}                 枚举："rdp"
server 端解析白名单 → {host: "127.0.0.1", port: 3389}，随 SESSION_OPEN 下发
纯 binary，零 text 帧：建立即转发
agent 连不上目标 → binary 首帧前发 text ERROR {code: RDP_NOT_AVAILABLE} 后关闭
```

client 永远不传 host/port；Linux 未来 `target: "ssh"` → 22。mstsc 本地端口转发流程沿用 v1 第 21-22 节。

## 3.3 消息数量对比

v1：控制面 ~24 个类型混在一个通道。v2：控制面 10 个；会话面 text 类型 exec 1 / shell 3（含通用 ERROR）/ file 3 / screen 2 / tunnel 0，合计 9 个，且每个只在所属通道出现。

## 3.4 REST API（统一返回结构）

五个会话端点返回完全一致：

```text
POST /api/nodes/{id}/exec           {command|script, timeoutSec?, cwd?}
POST /api/nodes/{id}/shell          {cols?, rows?, shell?}
POST /api/nodes/{id}/files/upload   {path, size, sha256}
POST /api/nodes/{id}/files/download {path}
POST /api/nodes/{id}/screen         {fps?, quality?, maxWidth?}
POST /api/nodes/{id}/tunnel         {target}

→ 202 {sessionId, token, expiresAt, websocketUrl}
```

- v1 exec 响应中的 `requestId` 字段统一为 `sessionId`。
- v1 的 `POST /api/nodes/{id}/rdp` 并入 `POST /api/nodes/{id}/tunnel {target: "rdp"}`。
- 其余 REST（auth / clusters / members / enrollment-tokens / nodes / audit）全部不变。

## 3.5 错误码表变化

```text
新增    KIND_UNSUPPORTED          agent 不认识 SESSION_OPEN 的 kind（版本协商兜底）
删除    TUNNEL_START_FAILED       并入会话内 ERROR RDP_NOT_AVAILABLE
其余全部不变（v1 第 51 节）
```

---

# 4. 工程结构（全 Go）

## 4.1 仓库结构

```text
xnc/
├── go.work                 go workspace
├── proto/                  共享协议包：消息类型、kind 常量、会话词汇、错误码
├── server/                 xnc-server：REST + 控制面网关 + 统一会话管理器 + PostgreSQL
├── agent/                  xnc-agent：Windows Service + 会话引擎
├── cli/                    xnc：与 Server 同语言（本就 Go，不变）
├── mockagent/              测试替身：import agent 核心包 + 内存传输
├── deploy/                 Docker Compose 生产栈：compose + Caddyfile + .env.example
└── web/                    React（Phase 7 动工，目录预留）
```

`proto/` 是唯一定义点，server / agent / cli / mockagent 全部 import——**协议漂移在结构上不可能发生**。

## 4.2 Agent 内部结构

```text
agent/
├── cmd/service.go           宿主：golang.org/x/sys/windows/svc（服务名 XNCAgent，Automatic）
├── cmd/xnc-agent/main.go    install / upgrade 子命令
├── internal/identity/       Ed25519 设备身份；DPAPI（CryptProtectData）保护私钥
├── internal/enroll/         首次注册（流程不变，v1 第 8 节）
├── internal/connection/     控制连接：挑战认证、心跳、指数退避重连（1/2/5/10/30s，上限 30s）
├── internal/session/        统一会话管理器：SESSION_OPEN 分发 → 拨会话 WS → 统一清理
│   ├── exec/                子进程 + 超时 kill 进程树
│   ├── shell/               ConPTY + pwsh（shell_windows.go；未来 shell_unix.go build tag）
│   ├── file/
│   ├── screen/              Phase 6：helper 拉起 + named pipe
│   └── tunnel/              TCP 转发 + 白名单校验
```

## 4.3 Go 依赖选型

| 用途 | 选型 | 备注 |
| ---- | ---- | ---- |
| ConPTY | `x/sys/windows` CreatePseudoConsole 直接封装（约 200 行）或 `UserExistsError/conpty` | Phase 1 原型验证二选一 |
| Windows 服务 | `golang.org/x/sys/windows/svc` | Go 官方扩展库 |
| WebSocket | `coder/websocket` | 与 server 同库 |
| 设备身份 | 标准库 `crypto/ed25519` | |
| 私钥保护 | `x/sys/windows` CryptProtectData（DPAPI） | Local Machine Scope |
| CLI 参数 | `spf13/cobra` | install / upgrade 子命令 |

单二进制约 8-12 MB（v1 Rust 版 3-5 MB），运维场景无实质影响。

## 4.4 Server 变化

架构角色不变（REST / 控制面网关 / 会话粘合 / PostgreSQL）。唯一实质变化：v1 的 Session Router + Tunnel Gateway + File/Screen 各自的连接管理**合并为一个统一会话管理器**（一个类型、五种 kind 参数化）。控制面网关变薄为纯 JSON 转发 + 心跳。内存态 `map[NodeID]AgentConn` + `map[SessionID]Session` 不变，单实例约束不变。

## 4.5 部署（Docker Compose）

部署产物为 `deploy/` 下的一套 compose 栈，单台 Ubuntu 云服务器 `docker compose up -d` 即完成部署：

```text
deploy/
├── docker-compose.yml     caddy + xnc-server + postgres 三容器
├── Caddyfile              :443 TLS termination，Let's Encrypt 自动签发
└── .env.example           域名 / PG 凭据 / Bootstrap Admin 环境变量

Internet → :443 Caddy(容器) → xnc-server(容器) → PostgreSQL(容器)
```

要求：

- xnc-server 镜像：多阶段构建，distroless/static 基底（Go 静态二进制），CI 构建推送。
- Caddy 对 WSS 的透传：WebSocket Upgrade 自动处理；tunnel / screen 等流式路径配置 `flush_interval -1` 禁用响应缓冲，保证转发低延迟。
- 长连接：代理层不得设低于心跳判定窗口的 idle 超时（在线判定 90s，代理 idle timeout 需大于 90s 或禁用）。
- PostgreSQL 数据卷持久化；全部凭据经 `.env` 注入，不入库。
- 单实例约束不变：一套 compose 栈即单实例。
- E2E 测试的 docker-compose 与本生产栈同构（仅追加 mockagent / cli 服务），开发与生产环境一致性由同一配方保证。

---

# 5. 测试策略

```text
协议 conformance   同一套用例跑两遍：mockagent（内存传输）+ agent 真实会话引擎
MockAgent          不再是第三份协议实现——就是 agent 核心包 + 虚拟传输
Server             unit + testcontainers-go(PostgreSQL) + httptest
Agent              unit + build tag `windows` 集成（ConPTY / 服务 / DPAPI，真机 NODE_MAIN）
E2E                docker-compose：server + PostgreSQL + mockagent×N + cli
负载               mockagent × 1000 并发连接（不变，不占真机）
CLI                golden 测试：--json envelope 快照、退出码表逐条、选择器歧义
```

v1 的功能测试矩阵、测试设备矩阵（NODE_MAIN / SRV / NODE2019 / NODELINUX）、Scenario A-H 全部沿用，仅通道层实现按 v2 协议重写。

---

# 6. Phase 重排（只重排不砍）

```text
Phase 1  连接面      Bootstrap Admin、JWT login、enrollment、设备身份、控制连接、心跳、
                     node list（CLI）、xnc login/whoami/status/version
Phase 2  exec 会话   exec + run、超时取消、退出码契约、（CLI golden 测试随之建立）
Phase 3  shell 会话  ConPTY、resize、Ctrl+C、xnc shell（CLI 交互终端；Web Terminal 后置）
Phase 4  数据通道    file 会话（upload/download）、tunnel 会话、RDP + mstsc
Phase 5  多用户      Cluster、成员角色（owner/operator/viewer）、审计查询
Phase 6  桌面预览    screen 会话 + helper（三态验收沿用 v1 第 53 节 Phase 6）
Phase 7  Web UI      React 整体交付：登录、节点列表、Terminal、预览面板
Phase 8  Linux       远期不变（v1 第 60 节）
```

依赖关系说明：Web Terminal / 预览面板依赖的 shell / screen 协议在 Phase 3 / 6 已就绪，Phase 7 纯消费方，无返工风险。

各 Phase 验收 = v1 Scenario A-H 映射不变（Phase 1 → A/B/G，2 → C/H，3 → D，4 → E/H，5 → F）。

---

# 7. 设计原则保持

v1 第 1.7 与 55 节的三条核心公式不变：

```text
Remote Shell     = ConPTY over reverse authenticated session connection
Remote Desktop   = RDP over reverse authenticated TCP tunnel（统一会话的一种 kind）
Node Connectivity = outbound HTTPS/WSS only
```

第一版六个必备能力不变：认证、节点连接、消息路由（现为：控制面路由 + 会话粘合）、ConPTY、TCP Tunnel、Cluster ACL。

---

# 8. 不变项清单（防范围漂移）

```text
合规模型      仅出站 443、禁穿透/VPN/入站、Server 中转一切数据
认证模型      Enrollment Token（一次性）→ Ed25519 设备挑战；JWT + bcrypt；不用 mTLS
数据模型      User / Cluster / ClusterMember / Node / EnrollmentToken / AuditLog 不变
数据库        PostgreSQL（含 testcontainers），不引入 SQLite
部署          单台 Ubuntu + Docker Compose（caddy + xnc-server + postgres 三容器）；单实例约束
CLI 契约      命令树、--json envelope、退出码表、错误码表不变（skills/xnc 文档零改动）
Node 选择器    唯一名 / cluster/name / UUID / --cluster，歧义报错
桌面预览技术  helper + named pipe + GDI 不变，仅 Phase 后移
运维要求      会话限额（10/10/4/2/10）、背压原则、审计动作、日志脱敏不变
RDP / mstsc   本地随机端口 + 反向隧道流程不变
Exec 模型     进程外 pwsh 子进程，不引入 Runspace
```

---

# 9. 对 spec.md 的变更映射

实现计划的第一步是按本设计重写 spec.md。映射如下：

| spec.md 章节 | 处理 |
| ---- | ---- |
| §5.1 部署产物 | systemd 二进制 → 容器镜像 + compose 栈（本设计 §4.5） |
| §5.2 Agent 技术栈 | 重写为 Go 选型（本设计 §4.3） |
| §5.3 Client | Web UI 后置说明，CLI 不变 |
| §11 Control Protocol | 重写为控制连接纯 JSON（本设计 §2.1、§3.1） |
| §12 Agent 消息类型 | 替换为 9 个控制面消息（本设计 §3.1） |
| §14 Exec | 重写为会话模式（本设计 §3.2 exec） |
| §16 Shell Input 的 binary 帧头 | 删除；shell 会话 WS binary（本设计 §3.2 shell） |
| §23-26 Tunnel | 重写为统一会话 + target 枚举（本设计 §3.2 tunnel） |
| §27 REST API | 统一返回结构（本设计 §3.4） |
| §30-35 Agent 内部模块 | 替换为 Go 结构（本设计 §4.2） |
| §42-50 | 原则不变，仅引用改为统一会话管理器；错误码表按 §3.5 微调 |
| §46 部署 | 重写为 Docker Compose 栈（本设计 §4.5） |
| §52 MVP 页面 | 移至 Phase 7 |
| §53 开发顺序 | 替换为本设计 §6 |
| §58 场景数据流 | 通道描述按会话模式重写，语义不变 |
| §59 测试方法 | mockagent 改为复用 agent 包（本设计 §5），设备矩阵不变 |
| §61 决策记录 | 增补本设计 §1.2 的四项变更 |
| §62 假设 | "Rust 生态稳定性" 假设替换为 "Go ConPTY 封装" 假设（见 §10） |
| §63 闭环自查表 | 架构组件列更新（Session Router → 统一会话管理器），闭环保持 |
| §64 桌面预览 | 词汇挂到统一会话，技术方案不变 |
| 其余全部章节 | 不变 |

---

# 10. 风险与待验证

> ⚠️ 假设：Go 侧 ConPTY 封装（x/sys/windows CreatePseudoConsole 或 UserExistsError/conpty）满足生产稳定性。Phase 1 原型期（enrollment → 控制连接 → 心跳 → 一个 exec 会话）先行验证。推翻影响：§4.2/§4.3。

> ⚠️ 假设：coder/websocket 双向粘合在 RDP 流量（数 MB/s）下吞吐与延迟可接受。Phase 4 用 mstsc 实连验证。推翻影响：§2.2/§4.4。

> ⚠️ 已论证接受的代价：每次 exec 多一次 agent 侧会话拨号（+1 RTT，几十 ms 量级），远小于 pwsh 子进程冷启动（100-500ms），属噪音级。

其余 v1 假设（节点版本 ≥1809、单租户、RDP 已启用、1000 agent 心跳压测）继续有效。
