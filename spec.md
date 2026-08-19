# XNC (eXtended Node Control) — Technical Specification

## 1. 概述

### 1.1 一句话定位

XNC 是小团队和 AI Agent 的 Windows 节点统一运维入口：节点只需**出站 443 长连接**，即获得状态观测、命令执行、交互终端、脚本/文件分发和远程桌面。不是穿透工具，是合规友好的反向连接平面。

### 1.2 目标用户与角色（内部阶段 ≤10 人）

```text
运维 owner（1-2 人）   管全部节点：注册发 token、全量状态、处理问题
开发 operator（若干）   各管自己的 Windows 测试/构建机：exec、传文件、跑脚本
AI Agent（第一公民）   主要操作者：xnc --json + 退出码契约 + skills 驱动一切
```

产品定位：

* 第一阶段：公司内部运维工具（≤10 用户，数十节点）。
* 第二阶段：演进为对外商业化产品。
* MVP 按单租户内部工具设计，但数据模型与协议不得阻碍未来商业化（多租户隔离、品牌化、许可）。

### 1.3 现状痛点

无统一方案：

```text
不知道公司有几台 Windows、活没活着
出问题临时装远程软件 / ssh 跳板 / 人肉到机房
合规禁掉 frp 类穿透、VPN 拨入、公网开 3389/5985
```

常见工具要么不合规，要么没有命令行能力——这个空档就是 XNC 的位置。

### 1.4 差异化

| 对比 | XNC 优势 |
| ---- | ---- |
| frp / 花生壳类穿透 | 不开入站端口、不打洞，纯出站长连接，合规可过审 |
| 向日葵 / ToDesk / RustDesk | CLI + JSON + 退出码，Agent 可直接驱动；exec / 脚本 / 文件不只桌面 |
| Tailscale 组网 + WinRM | 无网络层依赖、不装 VPN，权限模型内建，单端口出站 |
| Ansible / SCCM（商业化后） | 零依赖部署（单 exe），NAT 后节点即插即用，面向 Agent 而非 YAML |

### 1.5 用户故事

```text
1. 作为运维，我要一眼看到所有节点在线状态，以便回答"有几台、活没活着"      → Phase 1
2. 作为运维，我要一条命令在任意节点执行命令/脚本，告别临时远程软件         → Phase 2
3. 作为开发，我要给自己测试机传文件跑脚本，快速部署构建产物               → Phase 4
4. 作为运维，我要对 AI Agent 说"查一下 web-01 为什么挂并修好"，外包重复诊断 → Agent-First 贯穿
5. 作为安全负责人，我要所有操作留审计且零入站端口，以便过合规              → Phase 5 + 架构原则
```

### 1.6 成功标准（内部阶段，四条递进）

```text
1. 节点全量注册、状态实时可观测
2. 日常操作默认走 XNC（临时远程 / 人肉降为例外）
3. 跑通多个"Agent 自主诊断 → 修复 → 验证"完整案例（商业化核心卖点验证）
4. 稳定运行约 3 个月无事故、审计可回溯，再启动商业化
```

### 1.7 部署合规约束

```text
仅允许出站长连接
禁入站暴露 / frp 式穿透 / VPN 拨入
Server 中转会话数据属于允许范围 → Control Server 可部署公网云（现有架构不变）
```

系统由一个中心服务器、若干 Windows 节点 Agent 以及客户端组成。用户可以自行创建节点集群、注册 Windows 节点，并通过中心服务器执行远程 PowerShell、交互式终端以及远程桌面。

系统优先复用 Windows 原生能力，不重新实现 PowerShell Remoting 或 RDP 协议。

核心设计原则：

* Windows 节点主动连接中心服务器。
* 节点无需暴露 WinRM、SSH 或 RDP 公网端口。
* 所有公网通信统一通过 HTTPS/WSS 443。
* PowerShell 交互使用 ConPTY + `pwsh.exe`。
* 一次性命令使用 PowerShell 子进程执行。
* Remote Desktop 使用 Windows 原生 RDP。
* Agent 只负责建立反向 TCP Tunnel，不实现桌面协议。
* 中心服务器采用单体架构。
* MVP 不引入消息队列、服务发现、微服务或复杂调度系统。
* CLI 面向 Agent 与脚本优先：--json、稳定退出码、无交互提示。

---

# 2. 目标

系统需要支持：

1. 用户登录中心服务器。
2. 用户创建和管理 Cluster。
3. 用户生成节点注册 Token。
4. Windows 节点安装 Agent Service。
5. Agent 使用 Token 完成首次注册。
6. Agent 注册后使用长期设备身份认证。
7. Agent 主动连接中心服务器并维持在线状态。
8. 用户查看节点在线状态。
9. 用户执行一次性 PowerShell 命令。
10. 用户建立交互式 PowerShell Terminal。
11. 用户通过中心服务器连接 Windows Remote Desktop。
12. 所有 Shell、Exec 和 RDP 操作经过用户权限检查。
13. 记录基本操作审计日志。
14. 用户上传/下载节点文件（最小文件传输）。
15. 用户执行本地 PowerShell 脚本（即传即执行即清理）。
16. CLI 机器可读输出，供 AI Agent 直接调用。
17. 用户在 Web 低开销实时预览节点桌面（只读，不扰动会话）。

---

# 3. 非目标

MVP 不实现以下功能：

* 不重新实现 PSRP。
* 不重新实现 WinRM。
* 不重新实现 RDP。
* 不实现浏览器原生 Remote Desktop。
* 不实现远程桌面视频编码。
* 不实现桌面预览的键鼠控制与视频级帧率（预览只读、低频 JPEG，见第 64 节）。
* 不实现 WebRTC。
* 不实现 Kubernetes 风格 Resource Model。
* 不实现复杂 RBAC。
* 不实现 Workflow Engine。
* 不实现 Job Scheduler。
* 不实现 Kafka、RabbitMQ、NATS。
* 不实现 Service Mesh。
* 不要求 Redis。
* 不支持 Central Server 多实例高可用。
* 不支持 Agent 间直接通信。
* 不实现完整远程文件管理系统（浏览 / 权限 / 断点续传），仅最小 upload / download。

后续版本可扩展这些能力，但不得影响 MVP 核心协议设计。

---

# 4. 系统组成

系统包含三个核心组件：

```text
Control Server
Windows Agent
Client
```

总体结构：

```text
                    ┌──────────────────────┐
                    │    Control Server    │
                    │                      │
 Web / CLI ────────▶│ Auth                 │
                    │ Cluster Management   │
                    │ Node Management      │
                    │ 统一会话管理器       │
                    └──────────┬───────────┘
                               │
                         HTTPS / WSS :443
                               │
                   Agent outbound connection
                               │
                    ┌──────────▼───────────┐
                    │    Windows Agent     │
                    │   Windows Service    │
                    │                      │
                    │ connection 控制连接  │
                    │ session 统一会话引擎 │
                    │ exec·shell·file·     │
                    │ screen·tunnel        │
                    └───────┬────────┬─────┘
                            │        │
                         ConPTY     TCP
                            │        │
                         pwsh.exe  localhost:3389
```

---

# 5. 技术栈

## 5.1 Control Server

选型：

```text
Go 1.26+
net/http + chi（路由）
coder/websocket 或 gorilla/websocket
pgx + sqlc（类型安全 SQL）
PostgreSQL
golang-jwt（MVP）→ OIDC（商业化阶段）
golang.org/x/crypto/bcrypt
```

部署产物：

```text
xnc-server 容器镜像（多阶段构建，distroless/static 基底）
deploy/ Docker Compose 栈（caddy + xnc-server + postgres 三容器）
```

单台 Ubuntu 云服务器 `docker compose up -d` 即完成部署，栈结构与运维要求见第 46 节。

数据库决策：

* 统一使用 PostgreSQL，不引入 SQLite 双轨。
* 开发与 CI 使用 PostgreSQL 容器（testcontainers-go），环境与生产一致。
* 未来要商业化，数据层无中途迁移风险。

---

## 5.2 Windows Agent

Agent 与 Server / CLI 同为 Go，协议定义共享仓库根 `proto/` 包（唯一定义点），协议漂移在结构上不可能发生。

选型：

| 用途 | 选型 | 备注 |
| ---- | ---- | ---- |
| ConPTY | `x/sys/windows` CreatePseudoConsole 直接封装（约 200 行）或 `UserExistsError/conpty` | Phase 1 原型验证二选一 |
| Windows 服务 | `golang.org/x/sys/windows/svc` | Go 官方扩展库 |
| WebSocket | `coder/websocket` | 与 server 同库 |
| 设备身份 | 标准库 `crypto/ed25519` | |
| 私钥保护 | `x/sys/windows` CryptProtectData（DPAPI） | Local Machine Scope |
| CLI 参数 | `spf13/cobra` | install / upgrade 子命令 |

内部结构：

```text
agent/
├── cmd/service.go           宿主：golang.org/x/sys/windows/svc（服务名 XNCAgent，Automatic）
├── cmd/xnc-agent/main.go    install / upgrade 子命令
├── internal/identity/       Ed25519 设备身份；DPAPI（CryptProtectData）保护私钥
├── internal/enroll/         首次注册（流程不变，见第 8 节）
├── internal/connection/     控制连接：挑战认证、心跳、指数退避重连（1/2/5/10/30s，上限 30s）
├── internal/session/        统一会话管理器：SESSION_OPEN 分发 → 拨会话 WS → 统一清理
│   ├── exec/                子进程 + 超时 kill 进程树
│   ├── shell/               ConPTY + pwsh（shell_windows.go；未来 shell_unix.go build tag）
│   ├── file/
│   ├── screen/              Phase 6：helper 拉起 + named pipe
│   └── tunnel/              TCP 转发 + 白名单校验
```

产物：

```text
xnc-agent.exe 单文件约 8-12 MB（v1 Rust 版 3-5 MB），运维场景无实质影响
无运行时依赖
```

Agent 默认安装为 Windows Service。

节点最低系统要求：

```text
Windows 10 1809+
Windows Server 2019+
```

原因：ConPTY 依赖较新版本的 Console API。

PowerShell 依赖：

* 优先使用 pwsh.exe（PowerShell 7+）。
* 未安装 pwsh.exe 时自动降级为 powershell.exe（Windows PowerShell 5.1，系统内置）。
* Agent 启动时探测实际可用 shell 并上报 Server。

建议服务名：

```text
XNCAgent
```

启动类型：

```text
Automatic
```

服务异常时由 Windows Service Control Manager 自动恢复。

---

## 5.3 Client

提供：

```text
Web UI
xnc CLI
```

Web UI：

```text
React + TypeScript (Vite)
xterm.js + @xterm/addon-fit
```

xnc CLI：

```text
Go 1.26+（与 Server 同语言）
单静态二进制，交叉编译 Windows / macOS / Linux
本地端口转发 + 启动 mstsc.exe
```

CLI 是 Server API 的客户端（REST + Shell/Tunnel WebSocket），不依赖 Agent 侧代码。

CLI 先行交付；Web UI 整体后置 Phase 7（Phase 1-6 全部经 CLI 交付与验收）。两者共用同一套 REST API 与 WebSocket 协议，Web UI 是纯消费方，无返工风险。

Windows Remote Desktop 使用：

```text
mstsc.exe
```

CLI 示例：

```text
xnc login

xnc clusters

xnc nodes

xnc shell web-01

xnc exec web-01 "Get-Service"

xnc rdp web-01
```

---

# 6. Cluster 模型

用户可以自行创建 Cluster。

例如：

```text
production
development
lab
```

一个 Node 必须属于一个 Cluster。

一个 Cluster 可以包含多个用户。

MVP 角色：

```text
owner
operator
viewer
```

权限：

| Role     | 查看节点 | Exec | Shell | RDP | Cluster 配置 |
| -------- | ---: | ---: | ----: | --: | ---------: |
| owner    |  Yes |  Yes |   Yes | Yes |        Yes |
| operator |  Yes |  Yes |   Yes | Yes |         No |
| viewer   |  Yes |   No |    No |  No |         No |

---

# 7. 数据模型

MVP 使用以下核心实体。

## 7.1 User

```text
User
----
id
email
display_name
created_at
```

---

## 7.2 Cluster

```text
Cluster
-------
id
name
owner_id
created_at
```

---

## 7.3 ClusterMember

```text
ClusterMember
-------------
cluster_id
user_id
role
created_at
```

唯一约束：

```text
(cluster_id, user_id)
```

---

## 7.4 Node

```text
Node
----
id
cluster_id
name
machine_id
hostname
os_version
agent_version
shell_type
public_key
status
last_seen_at
created_at
```

`machine_id` 用于识别 Windows 节点。

`shell_type` 记录 Agent 探测到的实际 shell：

```text
pwsh                Windows + PowerShell 7
windows-powershell  Windows 内置 5.1
bash / zsh          预留给 Linux Agent（见 Linux 演进节）
```

Node 状态：

```text
online
offline
disabled
```

在线状态主要由 Agent Connection 决定。

---

## 7.5 EnrollmentToken

```text
EnrollmentToken
---------------
id
cluster_id
token_hash
expires_at
max_uses
used_count
created_by
created_at
```

默认：

```text
TTL: 30 minutes
max_uses: 1
```

Token 格式：

```text
xnc_enroll_ + 32 bytes CSPRNG 的 base64url（43 字符）
```

仅在创建响应中返回一次明文。

数据库不得保存 Enrollment Token 明文，只保存 SHA-256 hash。

---

## 7.6 AuditLog

```text
AuditLog
--------
id
user_id
cluster_id
node_id
action
session_id
metadata
created_at
```

典型 action：

```text
node.enroll
node.disable

exec.start
exec.finish

shell.open
shell.close

rdp.open
rdp.close

file.upload
file.download

screen.open
screen.close
```

保留策略：

```text
默认 180 天，可配置
后台定期清理
```

---

# 8. 节点注册

首次注册通过 Enrollment Token 完成。

用户：

```text
POST /api/clusters/{clusterId}/enrollment-tokens
```

服务返回：

```text
enrollment token
```

用户在 Windows 节点运行：

```powershell
xnc-agent.exe install `
    --server https://control.example.com `
    --token <token>
```

Agent 执行：

```text
1. 生成 Device Key Pair
2. 收集基础机器信息
3. POST /api/agent/enroll
4. 提交 Enrollment Token
5. 提交 Device Public Key
6. Server 创建 Node
7. Server 返回 Node Identity
8. Agent 安全保存长期 Credential
9. Enrollment Token 作废
10. Agent 建立长期连接
```

注册请求示例：

```json
{
  "token": "<enrollment-token>",
  "hostname": "WEB-01",
  "machineId": "...",
  "osVersion": "Windows Server",
  "agentVersion": "1.0.0",
  "publicKey": "..."
}
```

---

# 9. Device Identity

Enrollment Token 只允许用于注册。

不得作为 Agent 长期认证凭证。

注册完成后 Agent 使用设备身份认证。

MVP 采用：

```text
Public/Private Key Challenge
```

不选 mTLS 的原因：

* 自建 CA 的签发、轮换、吊销对 1 人团队成本过高。
* TLS 经反向代理终止时，client cert 透传配置复杂。
* Key Challenge 在应用层完成，与部署拓扑无关。

密钥算法：

```text
Ed25519
```

实现：Go 标准库 crypto/ed25519（Server 与 Agent 同语言，均为原生支持）。

认证流程：

```text
1. Agent 建立 WebSocket 连接
2. Server 下发 CHALLENGE（随机 nonce，60 秒有效）
3. Agent 使用 Device Private Key 签名 nonce
4. Server 使用注册时的 public_key 验签
5. 验证通过后进入 HELLO 流程
```

mTLS 保留为商业化阶段的可选升级，不影响现有协议。

私钥必须：

* 永不上传中心服务器。
* 存储于 Local Machine Scope。
* 使用 Windows Certificate Store 或 DPAPI 保护。

---

# 10. Agent Control Connection

Agent 启动后主动连接：

```text
wss://control.example.com/api/agent/connect
```

连接要求：

```text
TLS
Device Authentication
```

Agent Connection 为长期连接。

Control Server 维护：

```text
NodeId -> AgentConnection
```

逻辑模型：

```text
ConcurrentDictionary<NodeId, AgentConnection>
```

Agent 与 Server 定期交换 Heartbeat。

例如：

```text
30 seconds
```

Server 超过一定时间未收到 Heartbeat：

```text
90 seconds
```

则 Node 标记为 Offline。

具体数值允许配置。

---

# 11. Control Protocol

系统只有**两种连接**：常驻的**控制连接**与按需短连的**会话连接**。职责严格分离——控制连接只承载小 JSON 消息（认证、心跳、会话管理），一切数据流（含 exec 输出）都走会话连接。

由此整类消除 v1 的正确性隐患：exec / shell 输出与心跳共享控制连接发送队列，10 MB 输出可能顶住心跳导致节点被误判离线；v2 控制连接只承载小 JSON，该问题不可能发生。会话限额也天然按连接执行。

## 11.1 控制连接（agent ↔ server，常驻、每 agent 一条）

```text
wss://server/api/agent/connect
```

认证流程不变（第 9 / 13 节）：WS 建立后 server 下发 CHALLENGE（nonce，60s 有效，单次）→ agent 用设备私钥签名 → server 验签 → agent 发 HELLO → server 回 HELLO_ACK → 进入 Ready。

之后控制连接上**只允许 JSON 文本帧**。收到二进制帧视为协议错误，立即断开。

消息 envelope：

```json
{"type": "...", "payload": {}}
```

无请求关联字段——控制面是通知性质；会话数据与终态全部走会话连接，client 经 REST 响应中的 sessionId 关联。

## 11.2 会话连接（按需短连，一条会话一条连接，两侧对称拨号）

```text
client 侧：POST /api/nodes/{id}/{kind}
          → {sessionId, token, expiresAt, websocketUrl}
          → 连接 websocketUrl

agent  侧：server 经控制连接下发 SESSION_OPEN {sessionId, kind, params, agentToken, wsUrl}
          → agent 拨 wss://server/api/agent/session?token={agentToken}

server：两侧都连上后双向粘合（byte stream forwarding）
```

双侧 token 均为：随机生成、60 秒有效、单会话、单用途、使用后失效（语义同第 24 节会话 Token；v1 仅 Tunnel 使用，v2 泛化到所有 kind）。token 经 URL query 传递（CLI/Agent 侧可用 `Authorization: Bearer` 头；浏览器 WebSocket 无法设头，统一用 query，风险由单次 + 60s + TLS 约束）。

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

## 11.3 会话生命周期（所有 kind 一致）

```text
Created  POST 返回，sessionId/token 生成
Opening  等两侧拨号（TTL 60s，超时清理）
Open     粘合开始
Closed   任一侧断开 / 会话超时 / 节点离线 / 主动关闭
```

关闭时 server **必经控制连接**下发 `SESSION_CLOSE {sessionId, reason}`，agent 据此执行统一清理：杀进程树、删临时文件、退 helper、关 TCP。这是 v2 新增的统一清理路径——v1 中每种会话各自处理清理是最易遗漏之处。

超时责任划分：

* exec：`params.timeoutSec` 由 **agent** 计时并 kill 进程树（单一计时器，server 不重复计时）。
* shell：idle 30min / max 8h 由 **server** 计时（server 拥有两侧），到点关双侧。
* Opening TTL 60s 由 server 计时。

## 11.4 统一帧规则（一条会话 WS 内部）

```text
text frame   = 会话级控制 JSON {type, payload}
binary frame = 不透明流字节，语义由 kind 决定
```

v1 的自定义二进制帧头（1 字节类型 + 16 字节会话 ID）删除——会话连接本身就是会话，无需复用标识。

控制面消息共 10 个（v1 约 24 个），总表见第 12 节；各 kind 的会话词汇见第 14 节（exec）、第 15-20 节（shell）、第 23-26 节（tunnel）、第 43 / 58 节（file）、第 64 节（screen）。

---

# 12. Agent 消息类型

控制面消息**全部 10 个**（v1 约 24 个），全部为控制连接上的 JSON 文本帧：

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

说明：

* `kind ∈ {exec, shell, file, screen, tunnel}`。
* `SESSION_OPEN` / `SESSION_REFUSED` / `SESSION_CLOSE` 为会话管理消息，**Phase 2 起随第一个会话 kind（exec）启用**；Phase 1 只有前 5 个消息 + ERROR。
* 会话面 text 帧词汇按 kind 定义：exec 1 个 / shell 3 个（含通用 ERROR）/ file 3 个 / screen 2 个 / tunnel 0 个，合计 9 个，且每个只在所属通道出现。

---

# 13. HELLO

Agent 建立连接后发送：

```json
{
  "type": "HELLO",
  "payload": {
    "nodeId": "...",
    "hostname": "WEB-01",
    "agentVersion": "1.0.0",
    "shellType": "pwsh"
  }
}
```

`shellType` 为 Agent 启动时探测结果（pwsh / windows-powershell），Server 据此更新 nodes.shell_type。

Server 验证 Node Identity 后：

```json
{
  "type": "HELLO_ACK"
}
```

之后 Agent Connection 才进入 Ready 状态。

---

# 14. PowerShell Exec

Exec 用于一次性命令执行。

例如：

```text
xnc exec web-01 "Get-Service"
```

客户端：

```text
Client
   ↓ POST /api/nodes/{id}/exec
Server（统一会话管理器）
   ↓ 控制连接下发 SESSION_OPEN（kind=exec）
Agent 拨会话 WS，双侧粘合
   ↓
PowerShell 子进程
```

会话词汇（exec 会话 WS 内，text = 控制，binary = 数据）：

```text
SESSION_OPEN params: {command? | script?, timeoutSec=300, cwd?}
binary frame: [1 字节流标识][字节块]     0x01=stdout  0x02=stderr
text frame:   EXEC_RESULT {exitCode|null, timedOut, durationMs}
```

规则：

* script ≤ 256 KB，agent 落地 `%TEMP%\xnc-<sessionId>.ps1` 执行后必删（含失败路径）。
* 超时：agent 计时到点 kill 进程树，发 `EXEC_RESULT {exitCode: null, timedOut: true}` 后关会话。
* 取消：client 断开会话 WS → server 发 SESSION_CLOSE → agent kill 进程树（无需回传结果，连接已断）。
* 1 字节流前缀保住 CLI 契约：`--json` 的 data 含独立的 stdout / stderr 字段。

已论证接受的代价：每次 exec 多一次 agent 侧会话拨号（+1 RTT，几十 ms 量级），远小于 pwsh 子进程冷启动（100-500ms），属噪音级。

Exec 长期保持进程外执行模型：

```text
pwsh.exe -NoLogo -NonInteractive
```

（或降级 powershell.exe）

不引入进程内 PowerShell 宿主（Runspace），与轻量 Agent 定位一致；一次性命令的进程启动成本可接受。

---

# 15. Interactive PowerShell

交互式 Shell 使用：

```text
ConPTY
+
pwsh.exe
```

Agent 不实现 PowerShell Protocol。

架构：

```text
Client Terminal
      │
      ▼
Control Server
      │
      ▼
Agent
      │
      ▼
ConPTY
      │
      ▼
pwsh.exe
```

Shell 会话建立：

```text
POST /api/nodes/{id}/shell
→ 202 {sessionId, token, expiresAt, websocketUrl}

SESSION_OPEN params: {cols, rows, shell?}
```

`shell` 字段可省略，缺省由 Agent 按探测结果决定：

```text
已安装 PowerShell 7 → pwsh.exe
未安装 → powershell.exe
```

Agent：

```text
Create ConPTY
Create pwsh.exe
Attach pwsh.exe to ConPTY
Start Process
```

---

# 16. Shell Input

客户端输入经 shell 会话 WebSocket 的 **binary frame** 传输：

```text
原始 VT/ANSI 字节，无帧头
```

Server 只负责转发。

Agent 将数据写入 ConPTY Input Pipe。

---

# 17. Shell Output

ConPTY Output 经 shell 会话 WebSocket 的 **binary frame** 返回客户端：原始 VT 字节，双向不区分方向（连接方向即语义），不经过 base64。

输出应该尽可能保持原始 VT/ANSI 数据。

不得由 Server 解析 PowerShell Prompt。

Server 将 Shell 看作：

```text
bidirectional byte stream
```

---

# 18. Terminal Resize

Terminal 尺寸变化，经 shell 会话 WS 的 text frame（会话级控制 JSON）：

```json
{
  "type": "SHELL_RESIZE",
  "payload": {
    "cols": 160,
    "rows": 40
  }
}
```

Agent 调用 ConPTY resize API。

---

# 19. Ctrl+C

客户端 Ctrl+C 必须能够发送给 Shell。

尽量通过 ConPTY 终端输入实现，而不是：

```text
Kill pwsh.exe
```

用户终止命令后，PowerShell Session 应继续存在。

---

# 20. Shell 生命周期

一个 Shell Session：

```text
Created
Opening
Open
Closing
Closed
```

以下情况关闭：

* 用户主动退出。
* `pwsh.exe` 退出。
* Client Connection 关闭。
* Node 离线。
* Session Timeout。
* 管理员强制关闭。

MVP 默认一个 User 可以同时建立多个 Shell Session。

Session Timeout 默认值：

```text
Idle timeout:    30 minutes（无输入输出）
Max lifetime:    8 hours（硬上限）
```

均可配置。Idle 指无输入输出的持续时间，活跃会话不会因 Idle 被关闭。

---

# 21. Remote Desktop

Remote Desktop 不自行实现协议。

使用：

```text
Windows RDP
```

节点上的 RDP Server：

```text
127.0.0.1:3389
```

Agent 只负责 TCP Forward。

公网禁止直接暴露：

```text
3389
```

注意：RDP 连接会改变会话状态（接管 console 导致本地锁屏，或新建独立会话）。无扰动观察桌面用桌面预览（第 64 节）。

---

# 22. RDP Client Flow

用户执行：

```text
xnc rdp web-01
```

CLI：

```text
1. 请求 Server 创建 RDP Session
2. 本地绑定随机 Port
3. 创建 Tunnel
4. 启动 mstsc.exe
```

例如：

```text
localhost:53482
```

然后：

```text
mstsc.exe /v:127.0.0.1:53482
```

网络路径：

```text
mstsc.exe
   │
   ▼
127.0.0.1:53482
   │
   ▼
xnc.exe
   │
 HTTPS/WSS
   ▼
Control Server
   │
   ▼
Agent
   │
   ▼
127.0.0.1:3389
```

---

# 23. Tunnel 会话

RDP 等高流量数据不得与 Agent 长期控制连接共用同一个发送队列——v2 由架构直接保证：控制连接只承载小 JSON，一切数据流（含 tunnel）走独立的**会话连接**，一条会话一条连接。

tunnel 是统一会话模型的一种 kind，建立流程与所有 kind 一致（见第 11.2 节）：

```text
POST /api/nodes/{id}/tunnel
请求体 { "target": "rdp" }
→ 202 {sessionId, token, expiresAt, websocketUrl}
```

Server 经控制连接下发 `SESSION_OPEN（kind=tunnel, params={target}）`，Agent 拨会话 WS（`wss://server/api/agent/session?token={agentToken}`），Client 凭返回的 websocketUrl + token 连接另一侧，Server 双向粘合。

---

# 24. 会话 Token（Tunnel Token 泛化）

v1 的 Tunnel Token 语义保留，并**泛化为所有 kind 的会话 token**（client 侧与 agent 侧对称各一枚，见第 11.2 节）。

会话 Token 必须：

* 随机生成。
* 短期有效。
* 单 Session。
* 单用途。
* 使用后失效。

建议有效时间：

```text
60 seconds
```

其作用只是完成会话连接建立，不作为长期认证。

token 经 URL query 传递：CLI/Agent 侧可用 `Authorization: Bearer` 头；浏览器 WebSocket 无法设置头，统一用 query，风险由单次 + 60s + TLS 约束。

---

# 25. Tunnel 数据

Tunnel 会话为**纯 binary 通道，零 text 帧**：建立即转发。

```text
Client WS
        │
        ▼
Server 双向粘合（统一会话管理器）
        │
        ▼
Agent WS
        │
        ▼
TcpClient
        │
        ▼
127.0.0.1:3389
```

Server 只做：

```text
byte stream forwarding
```

不得解析 RDP 数据。

Agent 连不上目标（本机 3389 未监听等）：在 binary 首帧前发 text `ERROR {code: RDP_NOT_AVAILABLE}` 后关闭会话。

---

# 26. Tunnel 限制

Client 只能传 **target 枚举**，永远不传 host/port：

```text
SESSION_OPEN params: {target}        枚举："rdp"
```

Server 端解析白名单：

```text
"rdp" → {host: "127.0.0.1", port: 3389}
```

解析结果随 SESSION_OPEN 下发，Agent 侧同时校验（只允许节点本机回环地址 + 白名单端口）。

不得允许 Client 自行发送：

```text
targetHost
targetPort
```

从而避免 Agent 成为任意网络代理。

Linux Agent 未来扩展 `target: "ssh"` → `{host: "127.0.0.1", port: 22}`（见第 60 节）。后续如果支持通用 TCP Tunnel，需要单独权限模型。

---

# 27. REST API

## Authentication

```text
POST /api/auth/login
```

---

## Cluster

```text
GET    /api/clusters
POST   /api/clusters
GET    /api/clusters/{id}
DELETE /api/clusters/{id}
```

---

## Membership

```text
GET    /api/clusters/{id}/members
POST   /api/clusters/{id}/members
DELETE /api/clusters/{id}/members/{userId}
```

---

## Enrollment

```text
POST /api/clusters/{id}/enrollment-tokens
```

---

## Nodes

```text
GET  /api/nodes
GET  /api/nodes/{id}
POST /api/nodes/{id}/disable
POST /api/nodes/{id}/enable
```

---

## 会话端点（exec / shell / files / screen / tunnel）

六个会话创建端点返回结构完全一致：

```text
POST /api/nodes/{id}/exec           {command|script, timeoutSec?, cwd?}
POST /api/nodes/{id}/shell          {cols?, rows?, shell?}
POST /api/nodes/{id}/files/upload   {path, size, sha256}
POST /api/nodes/{id}/files/download {path}
POST /api/nodes/{id}/screen         {fps?, quality?, maxWidth?}
POST /api/nodes/{id}/tunnel         {target}
```

统一返回：

```json
{
  "sessionId": "...",
  "token": "...",
  "expiresAt": "...",
  "websocketUrl": "..."
}
```

即 `202 {sessionId, token, expiresAt, websocketUrl}`。client 凭返回值连接 websocketUrl 建立会话；各 kind 的会话词汇见第 14、15-20、23-26、43 / 58、64 节。

说明：

* v1 的独立 RDP 端点（`POST /api/nodes/{id}/rdp`）并入 `POST /api/nodes/{id}/tunnel {target: "rdp"}`。
* v1 exec 响应中的请求关联字段统一为 `sessionId`。
* 其余 REST（auth / clusters / members / enrollment-tokens / nodes / audit）全部不变。

---

# 28. CLI

CLI 命令结构：

```text
xnc login

xnc cluster list
xnc cluster create

xnc node list

xnc exec <node> <command>

xnc shell <node>

xnc rdp <node>
```

例如：

```powershell
xnc node list
```

输出：

```text
NAME       CLUSTER       STATUS
web-01     production    online
web-02     production    online
sql-01     production    offline
```

Shell：

```powershell
xnc shell web-01
```

体验：

```text
Connecting to web-01...

PowerShell
PS C:\Windows\System32> hostname
WEB-01
```

完整命令结构、全局 flag、JSON envelope 与退出码契约见第 57 节。

---

# 29. Node Lookup

CLI 支持 Node：

```text
node id
```

或：

```text
node name
```

当名称有歧义时返回错误：

```text
Multiple nodes named "web-01".
Specify --cluster or node ID.
```

允许：

```text
xnc shell production/web-01
```

---

# 30. Agent 内部结构

Agent 为单一 Go 模块，按包划分职责（协议定义 import 自仓库根 `proto/` 共享包，与 server / cli / mockagent 共用唯一定义点）：

```text
agent/
├── cmd/service.go           宿主：golang.org/x/sys/windows/svc（服务名 XNCAgent，Automatic）
├── cmd/xnc-agent/main.go    install / upgrade 子命令
├── internal/identity/       Ed25519 设备身份；DPAPI（CryptProtectData）保护私钥
├── internal/enroll/         首次注册（流程不变，见第 8 节）
├── internal/connection/     控制连接：挑战认证、心跳、指数退避重连（1/2/5/10/30s，上限 30s）
├── internal/session/        统一会话管理器：SESSION_OPEN 分发 → 拨会话 WS → 统一清理
│   ├── exec/                子进程 + 超时 kill 进程树
│   ├── shell/               ConPTY + pwsh（shell_windows.go；未来 shell_unix.go build tag）
│   ├── file/                upload / download、sha256 校验、临时文件清理
│   ├── screen/              Phase 6：helper 拉起 + named pipe
│   └── tunnel/              TCP 转发 + 白名单校验
```

逐包职责：

```text
cmd/service.go           服务宿主：svc 运行循环、启动/停止控制
cmd/xnc-agent/main.go    install / upgrade 子命令（cobra）
internal/identity/       设备密钥对生成、DPAPI 私钥保护、签名
internal/enroll/         Enrollment Token 注册、Node Identity 安全保存
internal/connection/     控制连接生命周期、心跳、重连、入站消息分发
internal/session/        会话生命周期（见第 11.3 节）、SESSION_OPEN 分发到各 kind、统一清理
internal/session/exec/   exec 会话引擎（见第 34 节）
internal/session/shell/  shell 会话引擎（见第 33 节）
internal/session/file/   file 会话引擎（见第 43 / 58 节）
internal/session/screen/ screen 会话引擎（见第 64 节）
internal/session/tunnel/ tunnel 会话引擎（见第 35 节）
```

内部状态：

```text
map[SessionID]Session（统一会话管理器持有，替代 v1 各 Manager 分持的会话表）
```

---

# 31. 服务宿主与入口（cmd）

职责：

```text
cmd/service.go         Windows 服务宿主（svc Run 循环、启动/停止控制）
cmd/xnc-agent/main.go  install / upgrade 子命令
Initialize Agent
Load Identity
Enroll if required
Start Connection
Handle shutdown
```

服务名 `XNCAgent`、启动类型 `Automatic`、异常由 SCM 自动恢复（见第 5.2 节）。

---

# 32. internal/connection — 控制连接

职责：

```text
Connect to Server
Authenticate Device（CHALLENGE / CHALLENGE_RESPONSE）
Send HELLO
Maintain Heartbeat
Reconnect
Dispatch inbound messages（SESSION_OPEN → internal/session）
```

重连策略：

```text
exponential backoff
```

例如：

```text
1s
2s
5s
10s
30s
```

最大保持在：

```text
30s
```

左右即可。

---

# 33. internal/session/shell — Shell 会话

职责：

```text
Create ConPTY
Launch pwsh.exe / powershell.exe
Write input（会话 WS binary VT 字节 → ConPTY Input Pipe）
Read output（ConPTY → 会话 WS binary）
Resize terminal（SHELL_RESIZE）
Close session（SESSION_CLOSE → 清理进程树）
```

平台文件：`shell_windows.go`；未来 Linux 增量 `shell_unix.go` build tag。

---

# 34. internal/session/exec — Exec 会话

职责：

```text
Execute PowerShell
Stream stdout/stderr（1 字节流前缀 binary 帧）
Return result（EXEC_RESULT）
Support cancellation（超时 / 会话断开 → kill 进程树）
script 落地临时 .ps1 并保证清理
```

直接启动子进程：

```text
pwsh.exe / powershell.exe
```

长期保持进程外执行模型，不引入 in-process PowerShell 宿主。

---

# 35. internal/session/tunnel — Tunnel 会话

职责：

```text
Receive Tunnel Request（SESSION_OPEN kind=tunnel）
Validate allowed destination（白名单：本机回环 + 允许端口）
Open TcpClient
Dial 会话 WS 后纯 binary 双向转发
Close resources（SESSION_CLOSE → 关 TCP）
```

---

# 36. Agent Service Identity

Agent Service 默认运行：

```text
LocalSystem
```

原因：

* 能进行系统级管理。
* 能访问本机 RDP。
* 能执行管理员 PowerShell。
* 不依赖交互式用户登录。

因此系统必须默认假设：

```text
Remote Shell Access == Administrator Access
```

不得将 Shell 视为低权限功能。

---

# 37. Security Requirements

必须实现：

## Transport

所有公网通信：

```text
TLS 1.2+
```

推荐 TLS 1.3。

禁止：

```text
HTTP plaintext
WS plaintext
```

---

## Agent Authentication

必须使用长期 Device Identity。

禁止：

```text
NodeId only authentication
Enrollment token reuse
Static shared cluster password
```

---

## User Authentication

推荐：

```text
OIDC
```

或者 MVP：

```text
email/password + JWT
```

密码必须使用成熟 Password Hasher。

---

# 38. Authorization

每次以下操作都必须检查：

```text
Cluster Membership
Role
Node.cluster_id
```

操作包括：

```text
exec
shell
rdp
node disable
token creation
```

绝不允许根据：

```text
nodeId
```

单独授权。

---

# 39. Session Authorization

Server 创建 Session 后生成：

```text
SessionId
SessionToken
```

SessionToken：

* 与用户绑定。
* 与 Node 绑定。
* 与 Session Type 绑定。
* 短期有效。
* 不允许访问其他 Session。

---

# 40. Logging

日志不得记录：

```text
用户密码
Enrollment Token 明文
Device Private Key
JWT
Session Token
完整 RDP Payload
```

Shell Command 是否完整记录由部署配置决定。

默认 MVP 只记录：

```text
user
node
session
start time
end time
exit status
```

避免无意记录用户输入的敏感信息。

---

# 41. Server 内存状态

Central Server 维护：

```text
map[NodeID]AgentConn（控制连接）
map[SessionID]Session（统一会话管理器，覆盖全部 kind）
```

MVP 不要求这些状态持久化。

Server 重启后：

```text
Agent reconnect
Sessions terminated（所有 kind 一致）
```

这是允许的。

---

# 42. 单实例约束

MVP Central Server 只支持：

```text
1 active instance
```

因此可以直接将：

```text
NodeId -> WebSocket
```

保存在内存。

无需 Redis。

未来需要横向扩展时再引入节点连接路由。

---

# 43. 文件传输

MVP 实现最小文件传输（数据流见第 58 节）：

```text
upload / download
sha256 校验
单文件默认 ≤ 256 MB
独立 File WebSocket，不占用 Control Connection
```

不实现（仍为非目标）：

```text
目录浏览、权限管理、断点续传、批量同步
```

不修改 Agent Control Protocol 基本模型。

---

# 44. Node Group Exec

MVP 后期可以实现：

```text
xnc exec --cluster production "Get-Uptime"
```

Server：

```text
query online nodes
dispatch same request
collect results
```

不需要建立 Job Scheduler。

结果：

```text
web-01    success
web-02    success
sql-01    offline
```

---

# 45. Agent 更新

MVP 不实现自动更新。

第一版通过人工升级：

```text
xnc-agent.exe upgrade
```

未来可以加入：

```text
signed package
download
signature verification
service restart
rollback
```

但不作为当前架构依赖。

---

# 46. 部署

部署产物为 `deploy/` 下的一套 Docker Compose 栈，单台 Ubuntu 云服务器 `docker compose up -d` 即完成部署：

```text
deploy/
├── docker-compose.yml     caddy + xnc-server + postgres 三容器
├── Caddyfile              :443 TLS termination，Let's Encrypt 自动签发
└── .env.example           域名 / PG 凭据 / Bootstrap Admin 环境变量

Internet → :443 Caddy(容器) → xnc-server(容器) → PostgreSQL(容器)
```

推荐 MVP 部署形态：

```text
单台云服务器（Ubuntu 22.04+，2C4G 起步）
域名：control.example.com
```

要求：

* xnc-server 镜像：多阶段构建，distroless/static 基底（Go 静态二进制），CI 构建推送。
* Caddy 对 WSS 的透传：WebSocket Upgrade 自动处理；tunnel / screen 等流式路径配置 `flush_interval -1` 禁用响应缓冲，保证转发低延迟。
* 长连接：代理层不得设低于心跳判定窗口的 idle 超时（在线判定 90s，代理 idle timeout 需大于 90s 或禁用）。
* PostgreSQL 数据卷持久化；全部凭据经 `.env` 注入，不入库。
* 单实例约束不变：一套 compose 栈即单实例（见第 42 节）。
* E2E 测试的 docker-compose 与本生产栈同构（仅追加 mockagent / cli 服务），开发与生产环境一致性由同一配方保证。

---

# 47. Firewall

Windows Node 仅要求：

```text
Outbound TCP 443
```

不要求开放：

```text
5985
5986
3389
22
```

公网侧只开放：

```text
443
```

这是系统的重要部署优势。

---

# 48. 性能目标

MVP 目标：

单 Central 实例能够稳定管理至少：

```text
1,000 connected agents
```

日常 Agent Heartbeat 不产生显著 CPU 压力。

交互 Shell 延迟主要由网络 RTT 决定。

Server 对 Shell Output 不进行重解析。

Tunnel 使用 Streaming，不进行整包 Buffer。

对于 RDP：

```text
Client -> Server -> Agent
```

应直接进行流式转发。

---

# 49. Backpressure

WebSocket/Tunnel 必须提供有限 Buffer。

不得无限累积：

```text
Shell Output
RDP Data
```

如果 Client 长时间无法读取，应：

```text
apply backpressure
```

或关闭 Session。

目标是避免单 Session 造成 Server OOM。

---

# 50. Session Limits

建议默认限制：

```text
Shell per Node:      10
Tunnel per Node:     10
File per Node:       4
Screen per Node:     2
Exec concurrent:     10
```

均可配置。

注：会话限额 Phase 2 起随会话实现生效（Phase 1 只有控制连接，无会话）。

限制目的主要是防止异常客户端耗尽资源，而不是构建复杂配额系统。

---

# 51. Error Model

REST API 统一返回：

```json
{
  "error": {
    "code": "NODE_OFFLINE",
    "message": "Node is offline."
  }
}
```

主要错误码：

```text
UNAUTHORIZED
FORBIDDEN

CLUSTER_NOT_FOUND
NODE_NOT_FOUND
NODE_OFFLINE
NODE_DISABLED

ENROLLMENT_TOKEN_INVALID
ENROLLMENT_TOKEN_EXPIRED
NODE_ALREADY_ENROLLED

SESSION_NOT_FOUND
SESSION_EXPIRED
KIND_UNSUPPORTED

SHELL_START_FAILED

RDP_NOT_AVAILABLE

FILE_NOT_FOUND
FILE_TOO_LARGE
HASH_MISMATCH

SCREEN_NOT_AVAILABLE
NO_INTERACTIVE_SESSION

AGENT_VERSION_UNSUPPORTED
```

v2 变更：新增 `NODE_ALREADY_ENROLLED`（machine_id 已注册，注册流程返回）、`KIND_UNSUPPORTED`（agent 不认识 SESSION_OPEN 的 kind，版本协商兜底）；删除 `TUNNEL_START_FAILED`（并入会话内 `ERROR {code: RDP_NOT_AVAILABLE}`）。

---

# 52. Web UI 页面（Phase 7 交付）

Web UI 整体后置 Phase 7：Phase 1-6 全部经 CLI 交付与验收；shell（Phase 3）与 screen（Phase 6）协议就绪后，Web UI 作为纯消费方一次性交付，无返工风险。

页面清单（全部 Phase 7）：

```text
Login

Clusters
Cluster Details

Nodes
Node Details

Terminal（消费 shell 会话）
预览面板（消费 screen 会话）
```

Node 页面：

```text
WEB-01

Status: Online
Cluster: production
OS: Windows Server
Agent: 1.0.0
Last Seen: now

[Terminal]
[Remote Desktop]
[Screen Preview]（消费 Phase 6 screen 能力）
[Info]
```

第一版 Remote Desktop 按钮可以直接给出 CLI 指令：

```text
xnc rdp web-01
```

后续再增加 Desktop Client integration。

---

# 53. 开发顺序

推荐按以下顺序实现。各 Phase 所需测试设备见第 59 节测试设备矩阵，凭据读自 `config.env`。

只重排不砍：v1 的全部功能能力保留，Web UI 独立成相（Phase 7），Linux 远期（Phase 8）。

```text
Phase 1  连接面      Bootstrap Admin、JWT login、enrollment、设备身份、控制连接、心跳、
                     node list（CLI）、xnc login/whoami/status/version、compose 部署
Phase 2  exec 会话   exec + run、超时取消、退出码契约、（CLI golden 测试随之建立）
Phase 3  shell 会话  ConPTY、resize、Ctrl+C、xnc shell（CLI 交互终端；Web Terminal 后置）
Phase 4  数据通道    file 会话（upload/download）、tunnel 会话、RDP + mstsc
Phase 5  多用户      Cluster、成员角色（owner/operator/viewer）、审计查询
Phase 6  桌面预览    screen 会话 + helper（三态验收见本节 Phase 6）
Phase 7  Web UI      React 整体交付：登录、节点列表、Terminal、预览面板
Phase 8  Linux       远期不变（第 60 节）
```

## Phase 1 — 连接面

交付：

```text
Bootstrap Admin 账号（环境变量首次初始化）
JWT Login
Enrollment（Enrollment Token 注册流程）
设备身份（Ed25519 + DPAPI）
控制连接（纯 JSON 控制面：CHALLENGE / HELLO / HEARTBEAT）
心跳与 online / offline 判定
xnc node list（CLI）
xnc login / whoami / status / version
deploy/ Docker Compose 栈部署（caddy + xnc-server + postgres，见第 46 节）
```

说明：完整多用户与 Cluster 权限在 Phase 5 实现，Phase 1 只需单管理员账号即可验收。

验收标准：

```text
compose 栈部署后，节点注册即出现并显示 online
Agent 停止后变 offline
```

---

## Phase 2 — exec 会话

交付：

```text
POST /nodes/{id}/exec（command 与 script 两种参数）
exec 会话词汇（binary 1 字节流前缀 + text EXEC_RESULT）
超时取消与进程树清理（agent 侧单一计时器）
xnc exec / xnc run（脚本即传即执行即清理）
退出码契约（CLI golden 测试随之建立）
```

验收：

```text
xnc exec web-01 hostname
```

能够返回：

```text
WEB-01
```

并且：

```text
xnc run web-01 --file test.ps1
```

返回脚本 exitCode，Agent 端临时文件已删除。

---

## Phase 3 — shell 会话

交付：

```text
ConPTY（Go 封装，二选一选型落地）
pwsh.exe / powershell.exe 自动降级
shell 会话 WS（binary 原始 VT 字节，无帧头）
Terminal Resize（SHELL_RESIZE）
Ctrl+C（经终端输入传递）
xnc shell <node>        CLI 交互终端
```

Web Terminal 后置 Phase 7，与 CLI 共用同一条 shell 会话协议。

验收：

```text
xnc shell web-01
```

获得真正的交互式 PowerShell。

必须支持：

```powershell
cd C:\Windows

$x = 123

echo $x

Get-Process
```

Session State 必须保持。

---

## Phase 4 — 数据通道（file / tunnel / RDP）

交付：

```text
file 会话（upload / download / sha256）
tunnel 会话（target 枚举白名单 + 纯 binary 转发）
RDP：本地随机端口转发 + mstsc launch
```

验收：

```text
xnc rdp web-01
```

可以打开 Windows Remote Desktop，同时节点公网无需开放 3389。

---

## Phase 5 — 多用户

增加：

```text
Cluster
Membership
Owner
Operator
Viewer
Audit（审计查询，xnc audit list）
```

完成基本多用户能力。

---

## Phase 6 — 桌面预览

交付：

```text
screen 会话 + session helper（用户会话捕获）
会话 WS 低频 JPEG 帧
xnc screen --snapshot
（Web 预览面板后置 Phase 7）
```

验收：

```text
打开预览，1 fps 看到当前桌面
未登录 / 锁屏返回状态而非黑屏
关闭预览后节点无捕获进程残留
预览期间 RDP 会话状态不受影响
```

---

## Phase 7 — Web UI

交付：

```text
React 整体交付：登录、节点列表、Terminal、预览面板（页面清单见第 52 节）
```

依赖说明：Web Terminal / 预览面板依赖的 shell / screen 协议在 Phase 3 / 6 已就绪，Phase 7 纯消费方，无返工风险。

---

## Phase 8 — Linux（远期）

不变，见第 60 节。

---

## Phase 验收映射

各 Phase 验收 = v1 Scenario A-H 映射不变：

```text
Phase 1    Scenario A / B / G
Phase 2    Scenario C / H
Phase 3    Scenario D
Phase 4    Scenario E / H
Phase 5    Scenario F
```

---

# 54. MVP 验收场景

## Scenario A — 注册

Given：

```text
用户拥有 production Cluster
```

When：

```text
用户创建 Enrollment Token
在 Windows Server 安装 Agent
```

Then：

```text
节点自动出现在 production
状态为 online
```

---

## Scenario B — NAT

Given：

```text
节点位于 NAT 后
没有公网 IP
```

When：

```text
节点允许 outbound TCP 443
```

Then：

```text
Shell 和 RDP 仍然可以工作
```

---

## Scenario C — Exec

When：

```text
xnc exec web-01 "Get-Service WinRM"
```

Then：

```text
Client 能收到 PowerShell 输出和 Exit Result
```

---

## Scenario D — Interactive Shell

When：

```text
xnc shell web-01
```

Then：

```text
用户获得交互式 PowerShell
```

并且：

```powershell
$x = 42
```

之后：

```powershell
$x
```

返回：

```text
42
```

---

## Scenario E — RDP

Given：

```text
Node RDP 已启用
Node 3389 未开放公网
```

When：

```text
xnc rdp web-01
```

Then：

```text
mstsc 成功通过中心 Tunnel 连接目标节点
```

---

## Scenario F — Authorization

Given：

```text
viewer 用户
```

When：

```text
调用 shell / exec / rdp
```

Then：

```text
HTTP 403
```

---

## Scenario G — Node Disconnect

Given：

```text
用户正在 Shell
```

When：

```text
Agent 网络断开
```

Then：

```text
Shell Session 自动关闭
Node 状态变为 offline
Agent 网络恢复后自动 reconnect
Node 重新变为 online
```

---

## Scenario H — 文件与脚本

Given：

```text
节点 online
本地存在 fix.ps1
```

When：

```text
xnc upload web-01 fix.ps1 C:\Temp\fix.ps1
xnc run web-01 --file fix.ps1
xnc download web-01 C:\Temp\out.log ./
```

Then：

```text
sha256 校验通过
脚本 exitCode 正确返回
Agent 端临时文件已清理
```

---

# 55. 最终 MVP 架构

最终系统应尽量保持为：

```text
                        ┌───────────────────────┐
                        │    Control Server     │
                        │                       │
     Web / xnc ────────▶│ REST API              │
                        │ Auth                   │
                        │ Cluster / Node         │
                        │ 统一会话管理器         │
                        │                       │
                        │ PostgreSQL             │
                        └───────────┬───────────┘
                                    │
                                  :443
                                    │
                             Agent outbound
                                    │
                  ┌─────────────────▼──────────────┐
                  │        Windows Agent           │
                  │        Windows Service         │
                  │                                │
                  │ connection（控制连接）          │
                  │ session（统一会话引擎）         │
                  │  exec / shell / file /         │
                  │  screen / tunnel               │
                  └──────────┬───────────┬─────────┘
                             │           │
                           ConPTY        TCP
                             │           │
                         pwsh.exe   localhost:3389
```

核心实现原则保持不变：

```text
Remote Shell
=
ConPTY over reverse authenticated session connection
```

```text
Remote Desktop
=
RDP over reverse authenticated TCP tunnel（统一会话的一种 kind）
```

```text
Node Connectivity
=
outbound HTTPS/WSS only
```

第一版本质上只需要可靠实现：

```text
认证
节点连接
消息路由（现为：控制面路由 + 会话粘合）
ConPTY
TCP Tunnel
Cluster ACL
```

这六个能力。

除这些能力之外的基础设施都应在出现明确需求后再增加。

---

# 56. Agent-First 设计原则

XNC 的首要操作者是 AI Agent（ZCode / Claude Code 等），人类通过 Web UI 交互为辅。

对 CLI 的硬性约束：

* 一切命令支持 `--json`，输出统一 envelope（见第 57 节）。
* 无交互提示：默认非交互，破坏性命令用 `--yes` 显式确认。
* 认证走环境变量 `XNC_SERVER` / `XNC_TOKEN`，`xnc login` 只需执行一次。
* 退出码契约确定：远端命令退出码透传，240+ 表示命令未执行。
* 长任务：CLI 默认阻塞 + 流式输出，Agent 将其放后台执行并轮询即可，不引入服务端 Job 模型。
* 交互式 Shell 需要真 TTY，Agent 场景一律用 exec / run 代替。

Skills 作为发布物同步交付（随 CLI 演进更新）：

```text
skills/xnc/SKILL.md              Agent 操作手册：工作流 + 示例 + 安全注意
skills/xnc/references/cli.md     完整命令参考：命令树 / flag / 退出码 / 错误码
```

---

# 57. CLI 命令结构

命令结构：动词-资源-动作，全局 flag 统一。

## 命令树

```text
xnc login [--server URL]                    # 凭证写入配置文件
xnc whoami

xnc cluster list|show|create|delete
xnc cluster member list|add|remove

xnc token create <cluster> [--ttl 30m] [--max-uses 1]
xnc token revoke <id>

xnc node list [--cluster c] [--status online]
xnc node show <node>
xnc node disable|enable <node>

xnc exec <node> [--timeout 300] [--cwd PATH] -- <command...>
xnc run <node> (--file x.ps1 | -) [--timeout 300]     # - 表示 stdin
xnc shell <node> [--cols N] [--rows N]
xnc rdp <node> [--local-port N]

xnc upload <node> <local> <remote>
xnc download <node> <remote> <local>
xnc screen <node> [--snapshot out.jpg | --open] [--fps 1]

xnc audit list [--node n] [--user u] [--action a] [--since 7d]

xnc version
xnc status                                  # Server 连通性
```

## Node 选择器

```text
web-01                 唯一名
production/web-01      cluster + name
<node-id>              UUID
--cluster production   组合 flag
```

名称歧义时报错并列出候选。

## 全局 flag 与环境变量

```text
--server        默认 $XNC_SERVER
--token         默认 $XNC_TOKEN，其次配置文件
--output        json | table（默认 table）
--timeout       请求超时（默认 30s；exec/run 默认 300s）
--yes           破坏性命令确认
```

## JSON envelope

```json
{"ok": true, "data": {}, "error": null}
{"ok": false, "data": null, "error": {"code": "NODE_OFFLINE", "message": "..."}}
```

exec / run 的 data：

```json
{
  "node": "web-01",
  "exitCode": 0,
  "stdout": "...",
  "stderr": "...",
  "durationMs": 1234
}
```

## 退出码契约

```text
0          成功
2          用法错误
240        认证失败
241        权限不足
242        节点离线
243        超时
244        资源不存在
245        网络错误
246        会话/配额超限
250        内部错误
```

透传优先：远端命令已执行则原样返回其 exitCode；240+ 仅在命令未能执行时出现。

---

# 58. 场景数据流：命令 / 文本 / 脚本 / 文件

## 一次性命令（exec）

```text
CLI → POST /exec → 会话 WS（双侧粘合）→ Agent → pwsh 子进程
```

通道描述：exec 会话 WS（text=EXEC_RESULT 终态，binary=1 字节流前缀+字节块）。

回调语义：

```text
binary 前缀块   流式增量输出（0x01=stdout / 0x02=stderr）
EXEC_RESULT     终态：exitCode（超时/取消为 null）+ timedOut + durationMs（会话 WS text 帧）
取消            客户端断开 / 超时 → Server 下发 SESSION_CLOSE → Agent kill 进程树
CLI 行为       阻塞至 EXEC_RESULT 或超时；输出实时打印（两流交错展示）
CLI 退出码     已执行 → 透传；未执行 → 240+
```

超时取消：

```text
agent 侧 timeoutSec 计时到点 → kill 进程树 → EXEC_RESULT {exitCode: null, timedOut: true} → 关会话
```

## 脚本（run）

复用 exec 会话，SESSION_OPEN params 扩展 script 字段：

```json
{
  "script": "<UTF-8 脚本内容>",
  "timeoutSec": 300
}
```

Agent 行为：

```text
1. 写入 %TEMP%\xnc-<sessionId>.ps1
2. pwsh -NoLogo -NonInteractive -File 执行
3. 流式 binary 输出，最终 EXEC_RESULT（exitCode）
4. finally 删除临时文件（失败路径同样清理）
```

限制：script ≤ 256 KB；更大脚本先 upload，再 exec 文件路径。

## 文本（交互 Shell 流）

向既有 Shell 会话注入文本属于交互终端能力（第 15-19 节）：shell 会话 WS 的 binary frame 双向原始 VT 字节流，Server 纯转发。

Agent 驱动终端的原则：优先 exec / run 组合，仅在确需会话状态延续时用 shell。

## 文件（upload / download）

会话建立走统一会话模式（kind=file，见第 11.2 节）：

```text
POST /api/nodes/{id}/files/upload    {path, size, sha256}
POST /api/nodes/{id}/files/download  {path}
→ 202 {sessionId, token, expiresAt, websocketUrl}
```

Server 经控制连接下发 SESSION_OPEN（kind=file），Agent 拨会话 WS；Client 凭 token 连接另一侧，Server 双向粘合。

会话 WS 消息（WS message 本身即分帧）：

```text
text    FILE_BEGIN  {direction, path, size, sha256}
binary  原始字节块（默认 64 KB/条）
text    FILE_RESULT {bytes, sha256, ok}
text    FILE_ERROR  {code}
```

校验与限制：

```text
双方核对 sha256，不匹配 → 删除半成品 + HASH_MISMATCH
单文件 ≤ 256 MB
File Session ≤ 4 per Node
绝对路径，不做路径白名单（operator+ 权限 + 审计兜底）
```

## 回调总则

```text
一切操作 = 流式增量（会话 WS binary 数据帧）+ 终态结果（会话 WS text RESULT），sessionId 关联
MVP 无 webhook：CLI 进程即任务载体，Agent 后台执行 + 轮询退出码
```

---

# 59. 测试方法

## 分层与工具

```text
Server             Go unit + testcontainers-go(PostgreSQL) + httptest
Agent              go test 单元 + build tag `windows` 集成（ConPTY / 服务 / DPAPI，真机 NODE_MAIN）
协议 conformance   同一套用例跑两遍：mockagent（内存传输）+ agent 真实会话引擎
E2E                docker-compose：server + PostgreSQL + mockagent×N + cli（与 deploy/ 生产栈同构，仅追加 mockagent / cli 服务）
负载               mockagent × 1000 并发连接（不占真机）
CLI                golden 测试：--json envelope 快照、退出码表逐条、选择器歧义
```

MockAgent 不再是第三份协议实现——就是 agent 核心包 + 内存虚拟传输；协议定义 import 自仓库根 `proto/` 共享包。

## 测试设备矩阵

开发/测试依赖以下设备。凭据统一存放仓库根目录 `config.env`（dotenv 格式、按设备前缀分节、已被 .gitignore 排除，当前已含 SRV 与 NODE_MAIN 真实凭据），开发脚本与 AI Agent 从中读取；未启用的设备栏位留空，脚本跳过对应测试。

| 设备 | 系统 | 用途 | 阶段 |
| ---- | ---- | ---- | ---- |
| NODE_MAIN（开发机本体） | Windows 11 + pwsh 7，LABS-XIAOXIN，常驻交互登录 | 主 Windows 测试节点：新版功能、RDP、桌面预览 helper 三态（capturing / locked / no_session）；兼日常开发 | 全程 |
| SRV | Ubuntu，阿里云国际区域，域名 control.xnc.app | xnc-server / Caddy / PostgreSQL 真机部署验证；NAT 场景对端 | Phase 1 起 |
| NODE2019（预留） | Windows Server 2019，不装 pwsh | 最低版本线：1809 ConPTY、powershell.exe 5.1 降级；Hyper-V / 云 VM 后补 | Phase 2-4（后补） |
| NODELINUX（预留，可选） | Ubuntu x64 | 跨平台守门：编译目标、PTY 抽象冒烟 | Phase 8 预研 |

要求与技巧：

* Windows 节点启用 OpenSSH Server（自动化部署测试构建、收集日志），RDP 保持开启。
* NODE_MAIN 开发期经 WinRM PowerShell Remoting 自动化配置（部署测试构建、收集日志），仅为开发便利，与 XNC 协议无关。
* 节点从办公/家庭网络连公网 SRV，天然覆盖 Scenario B（NAT），无需专门构造。
* Scenario G（断连重连）：节点经手机热点联网即可模拟弱网。
* NODE_MAIN 被 RDP 接管会锁定 console 会话——正好用于桌面预览"无扰动"对比验证（第 64 节）。
* SRV 为阿里云国际区域（control.xnc.app → 47.243.209.52）：免 ICP 备案，.app 域名 + Let's Encrypt 直接可用。
* 负载测试（1000 agents）用 MockAgent 跑在开发机/SRV，不占真机。

## 功能测试矩阵

| 功能 | 测试方法 | 关键用例 |
| ---- | ---- | ---- |
| 登录 / JWT | unit + API 集成 | 错误密码 401、过期 token、bcrypt 成本 |
| Enrollment | API 集成 | 过期 token、重放、max_uses 耗尽、库中无明文 |
| 设备认证 | unit + 集成 | Ed25519 验签、nonce 60s 过期、nonce 单次、错误密钥拒绝 |
| 心跳 / 在线 | MockAgent | 90s 无心跳 → offline、重连 → online、退避间隔采样 |
| exec | MockAgent + 真机 | 退出码透传、超时 kill 进程树、10 MB 输出背压、cwd |
| run 脚本 | 真机 | exitCode、失败路径临时文件清理、256 KB 上限 |
| Shell / ConPTY | Windows 集成 | 回显、resize、Ctrl+C 后会话存活、$x=42 状态保持、断连后 pwsh 进程无残留 |
| 文件传输 | 集成 + 真机 | sha256 校验、故意损坏 → HASH_MISMATCH、256 MB 上限、下载不存在 → FILE_NOT_FOUND |
| Tunnel / RDP | MockAgent + 手工 | 会话 token 单次有效、target 枚举外目标拒绝、mstsc 实连（手工） |
| RBAC | 表驱动 API 测试 | viewer 全 403、operator 可 exec 不可管 Cluster、owner 全通 |
| 审计 | 集成 | 每动作有记录；日志 grep 扫描无 token / 密码 / 私钥 |
| CLI | golden 测试 | --json envelope 快照、退出码表逐条、选择器歧义报错 |
| Web UI | 手工冒烟 | 登录、节点列表、Terminal 输入输出 |
| 负载 | 压测 | 1000 agent 心跳 CPU/内存基线、100 并发 exec |

## Phase 验收测试

```text
Phase 1    Scenario A / B / G（MockAgent 重连矩阵自动化）
Phase 2    Scenario C + 退出码/超时矩阵
Phase 3    Scenario D + ConPTY 集成套件
Phase 4    Scenario E / H + Tunnel 白名单拒绝
Phase 5    Scenario F + 审计完整性扫描
```

## 发布前手工清单

```text
mstsc 经隧道实连
真实 NAT 节点注册与操作
Windows Server 2019 无 pwsh → powershell.exe 降级
拔网线 30s 重连恢复
日志脱敏抽查
```

---

# 60. Linux 支持演进（Phase 8，远期）

定位：MVP 只交付 Windows Agent，但架构从第一天就跨平台。

现在锁定的设计：

* Agent 代码分层：协议层 / 会话层平台无关，平台层（PTY、服务宿主、凭据存储）按 OS 源文件 + build tag 实现。
* PTY 平台层：Windows = ConPTY（`shell_windows.go`），Linux = openpty（未来 `shell_unix.go` build tag）。
* 协议消息不包含 Windows 专有字段。
* shell_type 预留 bash / zsh。

Linux Agent 未来的增量工作：

```text
平台层实现：服务单元、Unix 凭据存储（0600 文件或 keyring）
Exec：sh -c <command>
Tunnel 白名单：target "ssh" → 127.0.0.1:22，按平台下发
编译目标：linux/amd64 / linux/arm64（Go 交叉编译）
```

Linux 无 RDP 概念，Remote Desktop 仅作为 Windows 节点能力。

---

# 61. 已确认决策记录（2026-08）

| 决策点 | 结论 | 理由 |
| ---- | ---- | ---- |
| 产品定位 | 内部工具起步（≤10 用户），未来商业化 | 多用户保留，MVP 单租户 |
| 项目命名 | XNC；CLI 为 xnc，Server 为 xnc-server，Agent 为 xnc-agent | 短名全局统一 |
| Server 技术栈 | Go 1.26+（chi + pgx + sqlc） | 单静态二进制容器化部署，长连接网关原生并发，1 人迭代最快 |
| Agent 技术栈 | v1 曾选 Rust；v2 改为 Go（见文末 v2 变更） | 全栈单语言、协议单一定义，1 人团队可维护 |
| CLI 技术栈 | Go，与 Server 同语言 | CLI 是 Server API 客户端，交叉编译单二进制 |
| 设备认证 | Key Challenge + Ed25519 | 1 人可维护，不受 TLS 终止位置影响 |
| 数据库 | 统一 PostgreSQL（含测试容器），无 SQLite 双轨 | 商业化方向避免中途迁移 |
| Web 前端 | React + TypeScript + xterm.js | 生态成熟，AI 协作效率高 |
| 客户端节奏 | v1 曾 CLI 与 Web 并行；v2 改为 CLI 先行、Web UI 后置 Phase 7（见文末 v2 变更） | 共用同一套 API 与 WS 协议，Web 为纯消费方 |
| 节点 shell | pwsh.exe 优先，自动降级 powershell.exe | 兼容未装 PowerShell 7 的节点 |
| Exec 模型 | 长期进程外执行，不引入 Runspace | 与轻量 Agent 定位一致 |
| Linux 演进 | Agent 第一天跨平台架构，MVP 只发 Windows 版 | 平台层按 OS 源文件 + build tag 隔离 |
| 部署形态 | v1 曾为裸机二进制直装 + 同机 PG；v2 改为 Docker Compose（见文末 v2 变更） | 单台 Ubuntu 运维最简 |
| Agent-First | CLI 机器可读（--json + 退出码契约），Skills 随发布同步交付 | 主要操作者是 AI Agent |
| 文件传输 | 最小 upload/download 进入 MVP，独立 File WebSocket | 脚本与文件场景的前置能力 |
| 痛点基线 | 无统一方案；合规禁穿透 / VPN / 开端口 | XNC 填补"合规 + 命令行"空档 |
| 合规边界 | 仅出站长连接；中转 Shell/RDP 属允许范围，Server 可在公网 | 反向连接架构不变 |
| 桌面预览 | 只读低频 JPEG（默认 1 fps），helper 进用户会话捕获 | RDP 会扰动会话状态，预览提供无扰动观察 |
| 测试设备 | NODE_MAIN（开发机本体）+ SRV(Ubuntu 公网)；NODE2019 / NODELINUX 预留；凭据入 config.env | 真机覆盖版本线与 NAT 场景，负载用 MockAgent |
| 协议模型（v2，2026-08） | 统一会话模型：控制连接纯 JSON；一切数据流（含 exec）走同一会话模式 | v1 五种通道三种帧规则并存、双实现漂移，不可维护 |
| Agent 语言（v2，2026-08） | 换 Go，全栈单语言，proto/ 单一定义 | Go/Rust 双协议实现 + MockAgent 第三份镜像，1 人团队不可持续 |
| Web UI（v2，2026-08） | 整体后置 Phase 7，CLI 先行 | 定位 Agent-First，第一版不应并行交付全套 React |
| 部署形态（v2，2026-08） | Docker Compose：caddy + xnc-server + postgres 三容器 | compose 一键部署；E2E compose 同构保证环境一致 |

---

# 62. 假设与待验证

> ⚠️ 假设：Go 侧 ConPTY 封装（x/sys/windows CreatePseudoConsole 或 UserExistsError/conpty）满足生产稳定性，Phase 3 验证。推翻影响：第 5.2、15 节。

> ⚠️ 假设：coder/websocket 双向粘合在 RDP 流量（数 MB/s）下吞吐与延迟可接受，Phase 4 用 mstsc 实连验证。推翻影响：第 11、48 节。

> ⚠️ 假设：目标节点均为 Windows 10 1809+ / Server 2019+。若存在更老节点，ConPTY 需另行降级方案。推翻影响：第 5.2、15 节。

> ⚠️ 假设：商业化前保持单租户。Cluster 模型天然支持团队级隔离，未来多租户预计只需增加 tenant 维度。推翻影响：第 6、7 节。

> ⚠️ 假设：节点 RDP 已启用且 Windows 许可允许远程管理会话。推翻影响：第 21 节。

> ⚠️ 假设：1000 agents 心跳（30s 间隔约 33 msg/s）在单实例 xnc-server（Go）承载范围内，Phase 1 完成后压测验证。推翻影响：第 42、48 节。

---

# 63. 闭环自查表

| P0 能力（对应目标） | 架构组件 | 里程碑 | 验收 |
| ---- | ---- | ---- | ---- |
| 注册与上线（目标 4-8） | internal/enroll、EnrollmentToken、internal/connection | Phase 1 | Scenario A |
| NAT 穿透（核心原则） | 反向 WSS 连接 | Phase 1 | Scenario B |
| 一次性 Exec（目标 9） | internal/session/exec、SESSION_OPEN + EXEC_RESULT 会话词汇 | Phase 2 | Scenario C |
| 交互式 Shell（目标 10） | internal/session/shell、ConPTY、会话 WS binary VT 流 | Phase 3 | Scenario D |
| Remote Desktop（目标 11） | internal/session/tunnel、统一会话管理器、mstsc | Phase 4 | Scenario E |
| 权限检查（目标 12） | ClusterMember、SessionToken | Phase 5 | Scenario F |
| 审计日志（目标 13） | AuditLog | Phase 5 | 随 Scenario F 验证 |
| 断线恢复（目标 7） | Heartbeat、指数退避重连 | Phase 1 | Scenario G |
| 脚本执行（目标 15） | internal/session/exec（script 参数）、临时文件生命周期 | Phase 2 | Scenario H |
| 文件传输（目标 14） | internal/session/file、会话 WS、sha256 校验 | Phase 4 | Scenario H |
| Agent 调用体验（目标 16） | xnc CLI（--json、退出码）+ skills/xnc | Phase 1-6 随功能交付 | CLI golden 测试 |
| 桌面预览（目标 17） | internal/session/screen、session helper、会话 WS | Phase 6 | Phase 6 验收场景（第 53 节） |

每条 P0 目标均有架构组件、里程碑与验收场景对应，无悬空项。

---

# 64. 桌面预览（Screen Preview，Phase 6）

## 动机

RDP 连接本身会改变会话状态：

```text
接管 console 会话 → 本地屏幕被锁定
或新建独立会话   → 看到的不是物理桌面
```

预览解决"先无扰动看一眼"：是否有弹窗卡住、安装器是否在等输入、谁登录着——判断清楚再决定是否 RDP。

## 边界（不演变为视频流）

```text
只读：无键鼠注入，控制仍走 RDP Tunnel
低频：默认 1 fps，上限 5 fps
压缩：JPEG 质量 ~60，宽度 ≤ 1280，单帧 ≤ 300 KB
按需：预览会话存在才捕获，关闭即停止
```

## 架构（Session 0 问题与 helper）

Agent 是 Session 0 服务，无法直接访问用户桌面（Session 1+）：

```text
screen 会话引擎 internal/session/screen (service, session 0)
   │ WTSQueryUserToken + CreateProcessAsUser
   ▼
xnc-screen-helper.exe (user console session)
   │ GDI BitBlt → 缩放 → JPEG
   ▼ named pipe
screen 会话引擎
   ▼ 会话 WS（JPEG binary frame）
Server（纯转发，不解析不落盘）
   ▼
Web 预览面板 / xnc screen --snapshot
```

* MVP 用 GDI：Win10 1809+ 全兼容，1 fps 下单帧 capture+encode < 100ms，开销可忽略。
* Windows.Graphics.Capture 为后续优化（1903+，GPU 加速），对外接口不变。
* 捕获活动 console 会话主显示器，多显示器以 monitor 参数预留。

## 状态而非硬抓

```text
no_session    无交互会话（未登录）
locked        锁屏 / 无输入桌面
capturing     正常出帧
```

## 协议

screen 会话（统一会话模型的一种 kind，Phase 6 交付、协议现在锁定；建立流程见第 11.2 节）：

```text
SESSION_OPEN params: {fps=1, quality=60, maxWidth=1280}
text frame:   SCREEN_BEGIN {width, height, state}
text frame:   SCREEN_STATE {state}            capturing / locked / no_session
binary frame: 单帧完整 JPEG（WS message 即一帧）
```

REST：

```text
POST /api/nodes/{id}/screen    {fps?, quality?, maxWidth?}
→ 202 {sessionId, token, expiresAt, websocketUrl}
```

## 权限与隐私

```text
operator+（viewer 不可见，桌面可能含敏感信息）
audit: screen.open / screen.close
帧数据不落盘、不进日志
```

## CLI（Agent-First）

```text
xnc screen <node> --snapshot out.jpg    # 单帧快照，--json 返回元数据
xnc screen <node> --open                # 浏览器打开实时预览
```

## 测试

```text
helper 拉起：有 / 无用户会话两种路径
帧预算：<100ms / 帧、≤300KB
预览 WS 关闭 → helper 进程退出无残留
viewer 403 权限矩阵
审计记录完整
Node 离线 → 预览会话关闭
```
