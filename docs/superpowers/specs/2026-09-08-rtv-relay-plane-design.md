# XNC RTV 媒体面中继化（Relay Plane）系统设计

> 版本 v1.0 · 2026-09-08 · 状态：已批准（owner 拍板 2026-09-08；v0.2 内部评审修正后定稿）
> 一句话定位：把 RTV 媒体面从 xnc-server 单机字节中继升级为"可水平扩展的中继池子系统"，
> 信令面留在主站不动；六个契约（信任、寻址、状态、传输、版本、密码学边界）一次定死，
> 实现按 Phase 渐进。本文是 2026-09-08-desktop-rtv-rewrite-design.md 的后续：
> 线协议（34B MVP1 头 / RS FEC / 控制词汇）与三腿语义**不变**，本 spec 只重新回答
> "谁提供三腿、会话如何找到它、它被信任到什么程度"。
> v0.1 → v0.2：吸收内部评审（事实核对 + 设计评审）结论，修正四个契约级缺陷
> （relay 准入、撤销墓碑、票据粒度、候选收敛，见 §12 修订记录）。

## 0. 决策记录（owner 已拍板，实施不得偏离）

| 决策点 | 结论 | 拒绝的替代方案 |
| --- | --- | --- |
| 演进策略 | 契约一次定死（本文 §3 全部），实现分四阶段（§9）；每阶段独立可发布、可回滚 | 一步到位全量实现（风险集中）；先上静态分配以后再改（寻址契约会破坏性变更） |
| 信任模型 | **能力令牌**：server 用 ed25519 签发 RelayTicket（claims 含 capability），relay 持 server 公钥离线验签。**server 决定 who/what，relay 只决定 when** | 仲裁逻辑复制进 relay（RBAC 演进会双改）；relay 每连接回源鉴权（控制面重新成为逐包依赖） |
| relay 准入 | relay 注册需通过**公钥准入**：`XNC_RTV_RELAY_ALLOWLIST`（deploy/.env，ed25519 公钥 hex 清单）或动态注册落 `pending` 状态、管理端审批激活；仅 active 参与分配。挑战-应答只证明持钥，**准入由清单/审批决定** | 裸挑战-应答即入池（任何人可注册 relay 投毒池，v1 明文媒体+input 门控下等效 RCE） |
| 撤销语义 | **relay 本地墓碑 + 控制连接重连对账**：sessionKill 时 relay 记本地墓碑（sid→时刻，按票 exp 有界），hello 期拒命；控制连接（重）建立时双向对账活跃 sid 集。**无全局分布式吊销列表** | 单次 sessionKill 断线（可重放票 + 客户端重连循环数秒内复活会话）；全局 CRL（最易烂的分布式状态） |
| 票据粒度 | **host 张按 (node, relay) 铸造**（不带 sid，同 relay 跨会话字节等值——保持 core cfg 等值复用与 Hub[nodeId] 共享语义）；**viewer 张按会话铸造**（sid + cap）。分配**node-sticky**：节点已有活跃桌面会话则沿用其 relay。host 张续期走 host 控制流带外刷新，**绝不经 agent→core cfg**（防换血） | 每会话铸一对票（第二个 viewer 开会话即 cfg 不等值 → core 杀 host 换血，全体媒体中断，pipe_server.cpp:952-968 证据） |
| 寻址 | **候选 = 同一 relay 的传输变体**（域名模式 wt+ws；纯 IP 模式仅 wt），client 顺序尝试（P2 升级竞速）；**跨 relay 移动只经 server 编排**（drain 的 redirect 再铸双票 / SESSION_REFRESH 换血），绝无 client 盲试跨 relay 候选 | 候选含其他 relay/主站兜底（host 单宿主：兜底落点上没有该节点 host 流 → hostOffline 死循环；rid 绑定下还可能 redirect 环） |
| 状态归属 | 会话仍为 server 内存态（**不落 PG**，与现状一致）；relay 分配表为 server 内存态，**经控制连接对账重建**。PG 只持久化 relays 表。媒体流动独立于 server 存活 | 会话/分配落 PG（引入一个未承接的大工作项；对账机制以更小代价覆盖同等语义） |
| 传输与端口 | 端点=多传输多端口描述符（transport/host/port/alpn/path/certSha256）；Phase 1 host 腿保持 raw QUIC UDP4433 **零改动**；终极形态收敛 UDP443（/wt + /host）+ TCP443（WSS），host 获得 WSS 兜底（消掉现"host 无兜底"硬约束） | 现在就统一端口（host 换 wtransport crate，大改且非必要） |
| 版本化 | relay 支持 N 与 N-1；控制词汇 JSON unknown-field 容忍（已满足）；MVP1 头 reserved u16 语义**现在定死**（§3.6）；包头布局经 config 消息协商 | 无版本协商的头部直读（framing 变体必破坏 relay） |
| 密码学边界 | v1 不实现但**占位定死**：媒体 E2E 逐 shard 流加密（与 RS 线性删除码兼容）、input 事件端到端签名（viewer 密钥）。relay 第一方部署期与 server 同信任级 | 现在实现（第一方场景无收益）；不占位（reserved 位将来被挪用=破坏性变更） |
| relay 证书 | 域名模式由 **server 代签发**：server 侧复用现有 ACME DNS-01（lego+alidns）签 relay 域证书，经控制连接下发，relay 热加载；**Aliyun DNS API 凭据不铺到 relay 主机** | 每台 relay 进程内 ACME（DNS API key 爆炸半径 = 接管 xnc.app 整域解析，远大于单台 relay 被攻陷） |
| 内嵌中继 | server 进程内嵌 relay 组件 = 池中 relay-0（标 fallback-only）。**单一代码路径，无双模式** | server/relay 两套实现（内嵌路径必烂） |
| 发布节奏解耦 | 媒体面协议演进以 relay/ server 快节奏为准，agent/host 慢节奏（安装器周级）只依赖 N-1 兼容 | 每次协议改动都要求 host 同步发版 |

## 1. 背景与动机

### 1.1 现状（2026-09-08 RTV 重构后）

- 媒体字节全部泵过 SRV 单机：`server/internal/rtv/relay.go` 的 Hub 按 nodeId 扇出，
  server 出带宽 = Σ(会话 × 码率 × 观察者)。码率口径（运营估计，代码锚点）：host
  默认码率 15000 kbps（`host/src/main.rs:55`）、QoS 下限 2000 kbps（`qos.rs`）、
  静止桌面因 `would_block_if_equal` 跳帧近零（`capture.rs`）——桌面流平均显著低于
  峰值，**P1 上线后以 RELAY_STATS 实测替代本估计**。量级判断：几十路并发活跃
  会话即可顶住国际区固定带宽机型，且境内用户吃跨境链路质量（2026-08-24 TURN
  池文档的原始动机）。
- CPU 是第二瓶颈但更远：fanout 逐观察者拷贝 + quic-go 用户态逐包发送，
  千兆量级前用 GSO + buffer pool 可压住。
- 控制面（信令/REST/WS 握手）是 KB 级小包，单实例长期够用（mockagent 工具
  链支持 N=1000 负载 smoke）。**优先级：先卸媒体，不拆控制面。**

### 1.2 已有资产（本设计直接兑现的）

- **哑字节泵属性**：server 不解码不转码，只读包头 captureUnixUs 偏移做延迟
  EMA（`relay.go:86-123`，长度守卫 ≥32B）——任何 relay 都是同构字节泵。
- **端点选择权已收敛在 server**：`DesktopParams.StreamEndpoint`（host 侧）、
  响应里的 `wtUrl/wsUrl`（viewer 侧）全部由 desktopStart 决定
  （`desktop_handlers.go:58-84`，客户端白名单只有 wtsSession）——**接中继池
  对 host 透明**（endpoint 来自 stdin cfg）、对 agent 透明（cfg 透传）。
- **TURN 池运维模式**（`docs/2026-08-24-turn-pool-architecture.md`；注意
  coturn 与其 server 侧代码已随 RTV 重构退役，**模式只能抄该历史文档**）：
  30s 健康探测、连败 2 次摘除、1 次恢复、round-robin、空池回落、加机器即扩容。
- **迁移/重连语义已内建**：host 重连 re-hello 顶替 + viewer 迁移 + 合成
  frameLoss 请求 IDR（`relay.go:321-351`）；viewer hello 2s 周期重发
  （`DesktopLive.tsx:299-303`）；host 崩溃退避重连（`transport.rs` 2s→10s）。

### 1.3 非目标（同样一次写清，防过度设计）

- 不做 relay 间树状扇出（单观察者主导的产品用不上）。
- 不做浏览器 P2P/打洞（WebRTC 退役是有理由的，不请回来）。
- 不强制 E2E 媒体加密（第一方 relay 与 server 同信任级；仅预留，§3.7）。
- 不做多租户 relay（relay 只服务我方 server）。
- 不做逐包动态路由（分配发生在会话建立/编排时，数据面无调度）。
- 不拆控制面多实例（维持单实例纪律；共享状态外置的纪律保持，远期可拆）。

### 1.4 已确认约束与契约对齐

- **AGENTS.md 硬性契约对齐**：argv 零密钥（relay 的身份/密钥走文件 0600，
  host 的 mediaKey/ticket 走 stdin 匿名管道）；凭据不落日志——**relay 自管
  TLS，不经 caddy，其访问/错误日志是新增回显面，必须剥离 query string、
  ticket 不落任何日志**；ticket 在 URL query 与现行为一致（session client
  token 同样在 query），caddy 侧暴露面不变更差。
- 现网节点经安装器慢节奏升级（周级）；relay/server 天级可发——兼容矩阵见 §8。
- owner 既定需求：境内媒体中继走**纯 IP 池免备案**（TURN 池文档 §背景）。
- 已知硬约束：UDP4433 在严格网络封锁时 host 无兜底（PORTS.md）——本设计
  Phase 3 消解。
- 本机禁止运行 server 栈（spec 红线）对 relay 同样适用：relay 只活在目标
  区域的独立主机，dev 经 docker-compose.dev.yml 起多实例。

## 2. 目标架构

```
浏览器 ── REST/信令(小包,不变) ──▶ xnc.app 主站(单实例)
   │        POST /desktop → 分配器(node-sticky 打分) → 响应候选 + viewer RelayTicket
   │                                   │ 控制连接(WS, ed25519 挑战-应答 + 公钥准入,
   │                                   │            镜像 agentws 模式)
   │                                   │     register/heartbeat/statsReport/resolve/
   │                                   │     sessionKill/reconcile/drain/configUpdate
   │── WT 候选(UDP443, /wt) ──▶ ┌─────────────┐
   │── WS 候选(TCP443, /ws) ──▶ │ 区域 relay N │◀── raw QUIC(UDP4433, xnc-host/1)
   │   (同一 relay 的传输变体)   │ 字节泵+扇出+ │    host 张票按 (node,relay) 铸造,
   │                               │ 传输级仲裁 + │    同 relay 跨会话字节等值
   │                               │ ticket 验签 │
   │                               └─────────────┘
   └── 主站内嵌 relay-0(fallback-only, 池空时=现行为零差异)

xnc-host（零改动 Phase 1）: token 即 RelayTicket（不透明字符串透传）,
  hello{role:"host", nodeId, token} → relay 验签（取代 server 内存 minted 表）
  跨 relay 移动只经 server 编排（redirect 再铸 / SESSION_REFRESH）,无 client 盲试
```

### 2.1 端到端时序（Phase 1）

1. 浏览器 `POST /api/nodes/{id}/desktop`（JWT + RBAC operator，不变）→
   session manager 创建会话 → 分配器**先查 node-sticky**（该节点已有活跃桌面
   会话则沿用其 relay，保证 host 张票字节等值、core 不换血）→ 无则打分选新
   relay → **签票**：复用/铸造 host 张（(node,relay) 粒度）+ 铸造 viewer 张
   （会话粒度，claims 见 §3.1）→ 响应 v2（§3.2）。
2. SESSION_OPEN{params}（DesktopParams.StreamEndpoint = 选中 relay 的 host 腿
   描述符展开）→ agent → coreclient.StartCapture → core spawn xnc-host
   （stdin 下发 cfg；**token 字段对 host 仍是不透明字符串，零改动**）。
   同节点后续会话：StartCapture 的 cfg 与在跑 host 等值 → core 复用不换血
   （`pipe_server.cpp:952-968` 的等值判据原样成立）。
3. xnc-host 连 relay host 腿（raw QUIC, ALPN `xnc-host/1`），控制流首条
   `hello{role:"host", nodeId, token}` → relay 离线验签（ed25519 公钥）+
   claims 校验（typ=host、nid、rid=本机、gen、exp±leeway、查墓碑）→ 注册入
   Hub[nodeId]。校验失败拒绝注册（防顶替，语义同今 validateHost）。
4. 浏览器按候选序**顺序尝试**（P1；P2 升级为竞速）连 viewer 腿，鉴权 token
   走 URL `?token=`（与现行为一致）；绑定触发 `hello{role:"viewer"}`（**hello
   本身不携带 token**，字段为 transport/codecs/maxBitrateKbps/fps，现状）→
   relay 验 viewer 张 → 绑定 HostSession → 补发 config 缓存 + 合成 frameLoss
   （新 viewer 触发 IDR，语义不变）。
5. 媒体下行/上行、feedback/frameLoss/heartbeat/input 的最小路由、QoS——
   **全部原样**（relay.go 逻辑搬家，只是换了宿主进程）。
6. 控制权：`takeControl/releaseControl` 在 relay 终结——传输级仲裁机
   （§3.5）按 `cap.control` 门控，last-take-wins + 3s 冷却 + 60s 租约 TTL，
   结果广播 controlState 并异步上报 server 入审计。
7. 收线：全部 viewer 离场 → relay 上报 sessionIdle → server janitor 收会话 →
   SESSION_CLOSE → agent 引用计数 → StopCapture（不变）。

### 2.2 状态归属

| 状态 | 归属 | 说明 |
| --- | --- | --- |
| 会话生命周期、viewer 票签发、审计、relay 分配表 | server **内存**（与现状一致，不新增 PG 依赖） | server 重启后旧会话自然终结；经对账与 relay 收敛（§3.4） |
| relays 注册表（身份/容量/状态） | server + **PG** | 唯一新增持久化；重启不丢准入与容量画像 |
| host/viewer 连接、租约表、viewer 计数、QoS 透传、stats、**墓碑** | relay 内存（墓碑按票 exp 有界） | relay 崩溃由重连语义重建；墓碑随有界清理 |
| ticket 验签能力 | relay 持 server 公钥 | **离线**，不依赖 server 存活 |
| 媒体流动 | relay 独立 | server 重启期间不断流（新增韧性红利） |

### 2.3 数据模型（migration `0004_relay_plane.sql`）

- 新表 `relays`：`id`(text, `rl-<8hex>`, server 分配) · `pubkey`(ed25519 hex,
  唯一) · `region` · `endpoints`(jsonb, 描述符数组) · `capacity`(jsonb:
  maxSessions/maxMbpsOut) · `status`(**pending**/active/draining/retired)
  · `version` · `last_seen_at`。
- 准入：`XNC_RTV_RELAY_ALLOWLIST`（deploy/.env，公钥 hex 逗号清单）命中 →
  注册即 active；未命中 → 落 `pending`，管理端 `PATCH /api/admin/relays/{id}`
  审批激活（admin = cluster owner，与 releases 同判定）。仅 active 参与分配。
- `XNC_RTV_RELAYS`（bootstrap 种子，endpoint+公钥指纹）仅用于首台引导，
  注册入库后即过渡为动态管理。
- **会话不落库**；分配表为 server 内存 map（nodeId→relayId），崩溃后经
  对账重建（§3.4 RELAY_RECONCILE：relay 上报在服 sid/nid 集，server 重建
  最小会话记录与 sticky 表）。

## 3. 协议契约（一次定死的部分）

### 3.1 RelayTicket（准入票据）

- 形态：`payloadB64url..sigB64url`（server ed25519 对 canonical JSON 签名，
  自包含双段串，不引 JWS 依赖）。作为**不透明字符串**流通：装进现有
  HostToken 字段（host 侧零改动）与 viewer 连接 `?token=`（web 零语义变化）。
- 两类票，粒度不同（v0.2 修正，理由见 §0 票据粒度行）：

```json
// host 张：按 (node, relay) 铸造，不带 sid —— 同 relay 跨会话字节等值
{ "v": 1, "typ": "host", "nid": "<nodeId>", "rid": "<relayId>",
  "gen": 1, "iat": 0, "exp": 0 }

// viewer 张：按会话铸造
{ "v": 1, "typ": "viewer", "sid": "<sessionId>", "nid": "<nodeId>",
  "rid": "<relayId>", "gen": 1,
  "cap": { "control": true, "input": true },
  "iat": 0, "exp": 0 }
```

- 语义：
  - **可重放**：host 张必须（core DesktopSupervisor 崩溃重启原样重放 cfg，
    重连凭同票再注册 = 合法顶替——完整继承现 HostToken 的 per-node 稳定 +
    可重放语义）；viewer 张的重附语义同现状（`manager.go:442-469`）。
  - **绑定 rid + gen**：只在此 relay 此代有效。`gen` 为该 (sid 或 node×relay)
    的票据世代，迁移/换血再铸时 +1；relay 对同键**只认最新 gen**（并放行
    旧 gen 一个短并存窗口 = redirect 传播期，窗口内旧 gen 只触发 redirect
    不给服务）。跨 relay 连接 → redirect（§3.3）。
  - **exp**：host 张 24h、viewer 张与会话生命周期对齐（上限 24h）；撤销靠
    墓碑+对账（§3.4），不靠短 exp（ticket 在 URL query 有日志暴露面，
    24h 与现 session token 的会话期语义一致，不劣化）。
  - **host 张续期**（P2）：server 在 exp 前 2h 再铸，经控制连接下发 relay →
    relay 在 **host 控制流**上推 `ticket{token}` 刷新消息，host 更新内存值。
    **绝不经 agent→core 路径**（cfg 变更会触发 core 换血，破坏共享）。
    P1 期无续期：host 张 24h 过期后注册失败 → host 退出 → 走 §5 #5 的
    SESSION_REFRESH 路径重生（可接受的初期限制）。
  - `cap` 由 desktopStart 时的 RBAC 计算填入（operator=control+input；
    viewer 角色=皆 false）。**策略演进只改 claims，relay 不发版。**
  - 预留字段（v1 不填，语义已注册）：`vk`（viewer 输入签名公钥，§3.7）。
- 时钟：relay 以本地时钟验 exp，**±120s leeway**；心跳携带 relay 时钟，
    漂移 >5m 告警并停止分配（§7）。

### 3.2 端点描述符与候选列表

```json
{ "transport": "quic" | "wt" | "ws",
  "host": "1.2.3.4" | "relay-cn-1.xnc.app",
  "port": 443, "alpn": "xnc-host/1", "path": "/wt",
  "certSha256": "<纯IP模式自签钉扎, 域名模式省略>" }
```

- `POST /desktop` 响应 v2：`{sessionId, token, expiresAt, websocketUrl,
  candidates[], wtUrl, wsUrl, lease}`（前四项与 lease 沿现状；wtUrl/wsUrl
  =首候选派生，旧 web 零改动可用）。
- **候选 = 同一 relay 的传输变体**（v0.2 收敛）：域名模式 = 该 relay 的
  wt + ws；纯 IP 模式 = 仅 wt。**不含其他 relay、不含主站**——host 单宿主，
  viewer 落到没有该节点 host 流的落点上只会 hostOffline（§0 寻址行）。
  全候选失败的降级 = viewer 重新 POST /desktop，由 server 重新分配（池内
  其他 relay 或 relay-0，跨境降级）。
- 一个 relay 可在描述符集合里暴露多端口/多传输（14433 式过渡 = 加一条
  同 relay 候选，切完删——**安全组过渡永久不再是事**）。

### 3.3 redirect 语义（自愈寻址，仅服务端编排触发）

- relay 在任意腿上收到**验签合法但不归自己管**的连接（rid≠本机，或本机
  draining，或 gen 落后）时：经控制连接 `resolve{sid|nid, gen}` 问 server →
  server 回三态之一：
  - `accept`：你就是归属（relay 本地状态过期，如对账竞态）→ 正常服务；
  - `redirect`：`{candidates, tickets}`（**host 张与 viewer 张同批再铸**，
    gen+1；viewer 新票随 redirect 载荷交付）；
  - `closed`：会话/节点票已废止 → 拒绝（记墓碑）。
  控制连接不可达时：hello 挂起至多 5s 后回 `error{code:SERVER_UNREACHABLE}`，
  client 走正常重试/最终 re-POST。
- relay 向来者回 `redirect{reason: not-owner|draining|moved, candidates,
  tickets?, visited}`；**防环**：redirect 携带已访问 rid 集合（首跳为空），
  relay 收到 visited 含本机或超 3 跳的连接 → 拒绝（client 回退正常退避）；
  host 跟随 redirect **前两次免退避立即重连**，之后恢复常规退避。
- drain 编排（P2）：server 向 relay 发 `drain{successor}` → relay 停接新
  会话、对现有会话逐一下发 redirect（successor 的 candidates + 再铸双票，
  gen+1）→ server 更新 sticky 表 → 会话零感知迁移。draining 状态**最短驻留
  10 分钟**（保持 redirect 应答），升级 runbook 要求逐台串行（§7）。
- host 长期连不上（relay 永久下线、重连烧尽）：server 侧孤儿检测（relay
  retired 且 T 内无 re-register）→ 经控制连接向 agent 推
  **`SESSION_REFRESH{sessionId, params}`**（proto 新消息常量）→ agent 对
  core 重新 StartCapture（cfg 变更 = core DecideCaptureReuse 换血语义，
  现有行为）→ host 以新 cfg（新 relay 的 host 张票）起连 → viewer 候选
  重试/redirect 命中。

### 3.4 relay ↔ server 控制连接

持久 WS（wss 到主站，经 caddy TCP443，复用现有通道），ed25519 挑战-应答
（镜像 `agentws.go` 的 CHALLENGE/CHALLENGE_RESPONSE 流程）+ **公钥准入**
（§2.3）。消息走 proto.Message envelope 新 type 段 `RELAY_*`。词汇：

| 消息 | 方向 | 载荷要点 |
| --- | --- | --- |
| `RELAY_REGISTER` | relay→srv | pubkey（准入判定见 §2.3）；endpoints、capacity、region、version |
| `RELAY_HEARTBEAT/_ACK` | 双向 | 30s 间隔 / 90s 超时（与 agent 同参）；载荷含 relay 时钟（漂移观测） |
| `RELAY_STATS` | relay→srv | 10s 一报：sessions/viewers/mbpsIn/mbpsOut/rttP50——分配打分（用 **mbpsOut**，扇出型瓶颈在 egress）与 Monitor 页 |
| `RELAY_RESOLVE/_RESULT` | 双向 | §3.3 三态（accept/redirect/closed） |
| `RELAY_SESSION_KILL` | srv→relay | `{sid|nid, reason}`：断开该键全部连接 + **记墓碑**（有界，按票 exp 过期清理） |
| `RELAY_RECONCILE` | 双向 | （重）建立连接时：relay 上报在服 {sid,nid,gen} 集 → server 重建最小会话记录 + sticky 表；server 下发仍在世的 sid 集 → relay 把差集记墓碑（server 重启后的孤儿收敛） |
| `RELAY_TICKET_REFRESH` | srv→relay | host 张再铸（exp 前 2h），relay 转推 host 控制流（§3.1） |
| `RELAY_DRAIN` | srv→relay | `{successor?}`（编排见 §3.3） |
| `RELAY_CONFIG` | srv→relay | server 签名公钥集合 + relay 证书/私钥（域名模式 server 代签发，§4.2） |

**签名密钥轮换时序**（一次定死）：① server 生成新钥，RELAY_CONFIG 增发新
公钥（集合式）；② 等全部 active relay ACK（离线 relay 重连时**先同步配置
再恢复服务**）；③ server 切换签发；④ 旧公钥保留至全部存量票自然过期
（max exp + leeway，约 25h）后移除。relay-0 内嵌直连不经此通道（同进程）。

### 3.5 控制权仲裁（传输级，无业务）

- 仲裁机与现 `manager.go` 语义逐条对齐：last-take-wins、3s 防互抢冷却
  （`manager.go:251-276`）、60s 租约 TTL（`manager.go:138`，连接存活续约）、
  controlState 广播。
- 差异仅一处：准入判据从"session manager 的租约表 + capabilities 计算"变为
  **ticket claims `cap.control`**——策略留在 server。
- 抢占/让位/冷却的 web 交互语义（controlResult 三态）不变。

### 3.6 MVP1 包头 reserved 位与 config 扩展（版本化锚点）

- 34B 头 reserved u16 语义**现在注册**（framing.rs:99 现置零）：bit0 =
  crypto（0=明文，§3.7）；bits1-3 = channelId（3 bit 上限 = 主桌面流 +
  音频 + 至多 6 路显示器，当前够用；**relay 不校验不解析此域**，将来语义
  再分配不构成对本契约的破坏）；bits4-15 必须为零（发送端置零）。
- host `config` 消息扩展字段（unknown-field 容忍，旧端忽略）：
  `headerLayout: 1`、`crypto: null | {suite, keyId}`、`channels: [...]`。
- relay 读包头只为 captureUnixUs EMA（现状）——**包头布局经会话 config 的
  headerLayout 协商**，framing 未来变体不破坏 relay。
- RS 运算域规则不变（数据 shard 对完整包字节运算，framing.rs:177-199）；
  crypto 启用时对密文 shard 运算（§3.7 已论证兼容性）。

### 3.7 E2E 密码学预留（设计完成，实现后置 Phase 4）

- **媒体加密（与删除码兼容）**：server 生成 256-bit `mediaKey`，经两条
  relay 不可见通道交付——host 走 stdin cfg（argv 零密钥契约），viewer 走
  POST /desktop 响应体（TLS，不进 URL/日志/ticket）。逐 shard 派生密钥流
  `HKDF-SHA256(mediaKey, "shard", frameIndex|blockIdx|shardIndex|channelId)`
  后 XOR——流密码与 RS 的线性重组可交换（先恢复后解密等价先解密后恢复），
  恢复语义（k/n 分片齐即重组）不受影响。relay 降级为真·加密字节泵，
  泄露面只剩可用性。
- **输入签名**：viewer 会话建立时在浏览器生成 ed25519 密钥对（WebCrypto），
  公钥随请求上送 → server 签入 ticket claims `vk` → host 持 server 公钥
  验 ticket 得 vk，对每条 input 验签 + seq 单调窗口防重放。伪造输入=
  变相 RCE，relay 数量扩张后这是最值得堵的洞；v1 第一方部署不启用。
- 净效果：未来 relay 可以放到信任更低的主机（廉价 VPS/边缘）而不提升
  保密性与输入完整性的风险面——这是"一次设计"里最长的 lookahead。

## 4. 组件设计

### 4.1 `rtv/`（新共享 Go 模块 xnc/rtv，自 internal/rtv 抽出）

- 内容：Hub/relay（字节扇出、控制最小路由、迁移/IDR 合成、hello 重试门
  闩——逐字节搬用）、legquic/legwt/legws、ticket 验签（含墓碑）、传输级
  仲裁机。
- 对外的接口缝（取代现 router.go 的**五处**注入点：AuthViewer/Touch/
  HostTokenOf/InputGate/ControlHooks，`router.go:111-181`）：

```go
type PlaneHost interface {
    VerifyTicket(tok string) (Ticket, error) // 离线验签（内嵌 relay-0 直连本进程签发器）
    TouchActivity(sid string)                // 活跃上报（内嵌=janitor; 外部=控制连接上报, 节流间隔 15s < 租约 TTL 60s）
    SessionEvent(ev ...)                     // 审计/生命周期事件上行
}
```

### 4.2 `xnc-relay`（新二进制，`relay/cmd/xnc-relay`）

- 单静态二进制（distroless 或裸 systemd），身份/密钥文件 0600
  （`/var/lib/xnc-relay/identity.json`，无 argv 密钥）。
- 启动：载入/生成密钥 → 拨控制连接注册（准入判定在 server）→ 按注册的
  endpoints 监听。
- 证书：域名模式由 **server 代签发**（server 侧复用 acme.go 的 lego+alidns
  DNS-01，为 relay 域签发，RELAY_CONFIG 下发，relay 按 mtime 热加载）——
  Aliyun DNS API 凭据不出主站；纯 IP 模式自签 + certSha256 经候选下发，
  web 走 serverCertificateHashes 钉扎。
- **日志纪律**：剥离一切 query string；ticket/证书私钥不落日志；错误输出
  不含凭据。
- DoS 面：hello 验签发生在 TLS 之后——对未完成验签的连接做 per-IP 握手
  预算/限速（配置项），公网单用途 relay 的基线要求。
- 部署双模式一等公民：**域名模式**（UDP443 /wt + TCP443 WSS，全功能）与
  **纯 IP 模式**（UDP443 /wt 钉扎；降级 = re-POST 重建，§3.2）——对应
  owner 的免备案约束。

### 4.3 `server/`（改动面）

- `internal/api/desktop_handlers.go`：分配器（**node-sticky 优先**；打分 =
  区域命中权重 + 归一化负载 `max(sessions/maxSessions, mbpsOut/maxMbpsOut)`，
  relay-0 标 fallback-only 不参与打分除非池空/全不健康）→ 签票（host 张
  (node,relay) 复用/铸造 + viewer 张会话铸造）→ 响应 v2。
- 池管理器：30s 健康探测（对 host 腿 QUIC 探测，模式抄 TURN 池**历史文档**，
  现无代码可抄）、孤儿会话检测、drain 编排、签名密钥轮换工具、relay
  admin API（审批/状态）。
- 内嵌 relay-0：rtv 模块 in-process 装配（PlaneHost 直连实现）。
- janitor 的 desktop idle 语义改由 relay RELAY_STATS/会话事件驱动
  （TouchActivity 节流 15s，< 租约 TTL 60s）。

### 4.4 `agent/` 与 `host/`（Phase 1 零改动；Phase 2 各一处）

- Phase 1：**零改动**。DesktopParams.StreamEndpoint 换成 relay 的 host 腿
  endpoint，token 字段装 RelayTicket——两者对 agent/host 都是不透明透传；
  同节点多会话因 host 张票字节等值而自然复用 host（§3.1）。
- Phase 2 小改各一处：host `transport.rs` 认识 `redirect`（换 endpoint+
  票立即重连）与 `ticket`（控制流续期，§3.1）；agent 新增
  `SESSION_REFRESH` handler（对 core 重新 StartCapture）。`DesktopParams`
  Phase 3 加可选 `certSha256` 透传（纯 IP 模式 host 钉扎；rustls 自定义
  verifier 管线已有，AcceptAnyServer 的 debug 路径证明可插）。

### 4.5 `web/`

- Phase 1：读 candidates 做**顺序 fallback**（等价现 WT 失败转 WS 的推广）
  + **全候选烧尽后 re-POST 重建会话**（现状是 fatal 退出，`DesktopLive.tsx:
  187-195`——此为 P1 必改项，否则 relay 下线无恢复路径）；保留
  wtUrl/wsUrl 兼容路径。
- Phase 2：同 relay 双候选**竞速**（Happy Eyeballs）；redirect 跟随
  （含载荷里的新 viewer 票）。
- Monitor 页：per-relay 卡片（sessions/mbps/viewers/状态）+ relay 审批入口
  （pending 列表）。

### 4.6 构建与部署

- relay 不走 Windows 安装器：`deploy/build-relay.ps1`（交叉编译 + SFTP +
  systemd unit，模式抄 build-server-local.ps1/push_server_image.py，凭据读
  deploy/.env 新增 RELAY_* 键）；CI 加 go-linux 维度的 relay job（单测 +
  构建）。
- compose：主站不变（仅 bootstrap/allowlist env）；relay 独立主机 systemd。
- dev：docker-compose.dev.yml 起 2 个 relay 实例（不同 UDP 端口）验证
  分配/sticky/迁移/redirect 全链路。

## 5. 故障矩阵（设计完备性的验收清单；"期"= 恢复路径开始可用)

| # | 故障 | 检测方 | 恢复路径 | 用户观感 | 期 |
| --- | --- | --- | --- | --- | --- |
| 1 | viewer WT 握手失败 | client | 同 relay WS 候选 → 全烧尽 **re-POST 重建**（server 重分配） | 秒级 | P1 |
| 2 | viewer 媒体中断（host 正常） | client | hello 2s 周期重试（现有） | 短卡 | 现有 |
| 3 | host QUIC 断（网络抖动） | host | transport 重连（现有）→ re-hello 顶替 → viewer 迁移 + IDR（现有） | 短卡 + 一次 IDR | 现有 |
| 4 | relay 进程崩（同机回来） | 各端 | 全断重连 → relay 软状态由 hello 重建（墓碑丢失去可接受：对账收敛） | 数秒卡顿 | P1 |
| 5 | relay 永久下线 | server 心跳超时标 retired | 孤儿会话推 SESSION_REFRESH → agent 换血 → 新 relay → viewer 重试命中 | 10–30s 中断 | **P2**（P1 期 relay 不可摘除，摘除=存量会话死亡，运维红线） |
| 6 | 计划内 drain | server 编排 | redirect 逐会话迁移（双票再铸 gen+1） | 零感知（一次 IDR） | **P2** |
| 7 | **server 重启** | relay 控制连接断 | **媒体继续流动**；重连后 RELAY_RECONCILE 对账重建 sticky/会话记录 | 媒体无感，新会话短暂 5xx（relay-0 会话除外——随进程死） | P1 |
| 8 | PG 不可用 | server | 新会话/新注册失败；存量媒体继续 | 降级可用 | P1 |
| 9 | ticket 伪造/过期/错 rid/旧 gen | relay 验签 | hello 拒绝；错 rid/落后 gen → redirect | 表现为会话不可建 | P1 |
| 10 | 无 cap.control 者抢占 | relay 仲裁机 | controlResult{ok:false, reason:"capability"} | 明确拒绝 | P1 |
| 11 | server 签名密钥轮换 | configUpdate | §3.4 四步时序，双窗口并验 | 无感 | P2 |
| 12 | relay 满载 | RELAY_STATS | 分配器跳过；全池满 → relay-0 兜底 | 不影响存量 | P1 |
| 13 | **会话被 kill 后 client 重连** | relay 墓碑 | 同 sid/nid 的票在墓碑期内一律拒绝（防"断线即复活"） | 会话关闭成立 | P1 |
| 14 | redirect 环（互指/翻转） | redirect visited 集 | 超过 3 跳或 visited 含本机 → 拒绝 → client 正常退避/re-POST | 表现为候选失败 | P2 |
| 15 | 控制连接断开期间收到陌生归属 hello | relay | 挂起 ≤5s → SERVER_UNREACHABLE 拒绝 | 客户端正常重试 | P1 |
| 16 | relay 时钟漂移 >5m | 心跳时钟观测 | 告警 + 停止分配（exp ±120s leeway 内不误拒） | 不影响存量 | P1 |

## 6. 容量与经济性（规划基线）

- relay 单盒（4C4G、千兆口、境内 BGP 按流量）：千兆口有效出带宽 ~900 Mbps，
  **舒适区 200–400 路活跃会话 / 峰值 ≤800 Mbps**（Go 字节泵 + UDP GSO +
  sync.Pool 拷贝）；更高量级换 2.5G/万兆口或多盒，不追单机极限。
- 打分用 **mbpsOut**（扇出型瓶颈在 egress；mbpsIn 是 host 单份，仅观测）。
- 扩容 = 新机部署 xnc-relay → 准入（allowlist/审批）→ 入池（30s 健康探测
  内参与分配）；缩容 = drain（P2 前 = 摘除即断流的运维红线）。**容量规划
  以 RELAY_STATS 实测为准，先上观测再上容量。**

## 7. 观测与运维闭环

- 会话→relay 映射经对账常驻 server 内存 + Monitor 页 per-relay 视图。
- **canary 流程**：relay 注册后置 draining（不参与分配）→ `tools/rtvload`
  打该 relay 的 **/wt viewer 腿**（rtvload 现仅支持 WT 拨号，viewer 腿正好
  够用；打 host 腿属非必须的后续扩展）出 30s 报告 → 通过后翻 active。
  drain 反向。
- 审计事件清单（P2 随 Monitor 落地）：relay.register/approve/drain/retire、
  session.assign/migrate/kill、key.rotate、relay.clock_drift。
- 告警阈值基线（P2）：心跳丢失 >90s、容量水位 >80%、redirect 率突增、
  时钟漂移 >5m。
- **relay 升级 runbook**：逐台串行（drain → 升级 → canary → active → 下一台），
  draining 最短驻留 10 分钟；禁止全池同时缩容。
- relay 日志带 sid/rid 结构化字段（无 query/ticket）；问题定位路径：
  Monitor 页定位 relay → relay 日志按 sid 过滤 → rtvload 复现。

## 8. 版本与兼容矩阵

- 兼容规则：relay 同时支持媒体面协议 N 与 N-1（ALPN `xnc-host/1` 不变即
  同代；升 `/2` 时双栈）；server 是唯一发版门（升级顺序：先 relay 后 server
  后节点安装器，回滚反向）。
- 线协议改动分级：**不触 relay**（键盘/音频/多显示器——控制词汇与 framing
  channelId 预留承载；注意**多 viewer 差异化 FEC/码率**不在此列，它需要
  per-viewer 流 demux，属"触 relay"级）vs **触 relay**（包头布局/传输变化
  ——走 headerLayout 与 ALPN 版本协商）。
- 现网 web 与旧 host 在 Phase 1 全程可用（wtUrl/wsUrl 兼容字段 + token
  不透明透传）；Phase 2 的 redirect/SESSION_REFRESH 需 host 配套——server
  按 agent 自报版本门控下发（低于门槛时退化为故障 #5 的 re-POST 路径）。

## 9. 实施阶段（每阶段独立可发布、可回滚）

| Phase | 内容 | 交付判据 |
| --- | --- | --- |
| **P1 契约+抽包** | rtv 模块抽包 + xnc-relay 二进制 + 双粒度票据签发/验签/墓碑 + relays 表与准入 + 池管理器（健康探测）+ node-sticky 分配器 + candidates 响应 + 内嵌 relay-0 + web 顺序 fallback 与烧尽 re-POST + RELAY_RECONCILE 对账 | 池空=现行为零差异（回归门）；单外部 relay 会话媒体经 relay（主站 **UDP** 无媒体字节；TCP 侧仅 WS 兜底与信令）；伪造/过期/错 rid/旧 gen/墓碑票全拒；**同节点双 viewer 并发会话不换血**（core 复用，媒体零中断） |
| **P2 韧性+运维** | redirect + resolve 三态 + visited 防环 + drain 编排 + SESSION_REFRESH + host 张续期 + host redirect/ticket 跟随 + 签名密钥轮换工具 + Monitor 页 + canary + 审计/告警 | 故障矩阵 #5/#6/#11/#14 实测通过；rtvload canary 闭环；升级 runbook 演练 |
| **P3 传输统一+境内池** | host WT `/host`（或同端口 ALPN 分发）+ host WSS 兜底 + 纯 IP 模式（certSha256 钉扎端到端）+ 首批境内 relay 上线 | 纯 IP relay 服务真机；严格 UDP 封锁网络 host 仍可连（WSS）；安全组单端口化 |
| **P4 密码学（可选）** | 媒体 E2E 逐 shard 加密 + 输入签名（claims `vk`）+ 密钥轮换演练 | relay 主机上抓包不可解码；伪造 input 被拒；性能损耗 <5% |

## 10. 测试与验收资产

- ticket 单测矩阵：签名非法/exp 过期(含 leeway 边界)/typ 错/rid 错/gen 落后/
  nid 不匹配/host 张可重放（必须通过）/墓碑期内拒绝。
- relay 集成测试：双 relay dev 栈跑分配/node-sticky/redirect/drain/迁移/
  对账；故障矩阵逐行自动化（断连注入）；**同节点多 viewer cfg 等值复用**
  回归（core 不重启 host）。
- rtvload：relay /wt 腿 canary 模式（--url 带 viewer 票）。
- RS 黄金向量与 nalcheck 不变量**不动**（媒体面字节语义未变，这是本设计
  的回归底线）。
- 真机矩阵：LABS-XIAOXIN（实验台，双 relay dev 栈）→ 境内 relay 生产首台
  → LABS-TB16G7（用户机）。

## 11. 与既有文档的关系

- 本 spec 继承 `docs/2026-08-24-turn-pool-architecture.md` 的池管理模式
  （该文为 pre-RTV 历史存档，coturn 与其 server 侧代码已退役；本文吸收其
  "纯 IP 免备案/30s 探测/round-robin/空池回落"四条决策）。
- 本 spec 不修改 `2026-09-08-desktop-rtv-rewrite-design.md` 定义的线协议
  与三腿语义；若实施中发现必须改动（预期只有 §3.6 的扩展字段），在该文
  追加修订记录而非静默偏离。
- 实施计划（带文件级改动清单的 plan 文档）在本文评审定稿后另行起草。

## 12. 修订记录

- **v1.0（2026-09-08）**：owner 拍板定稿，§0 决策固化为实施不可偏离。
- **v0.2（2026-09-08，内部评审后）**：
  1. 【阻断】relay 注册增加公钥准入（allowlist/pending 审批）——挑战-应答
     不构成准入，原稿任何人可入池投毒。
  2. 【阻断】撤销语义补墓碑 + 对账——原稿单次 sessionKill 会被可重放票 +
     客户端重连循环击穿（"断线即复活"）。
  3. 【阻断】票据粒度改双轨：host 张按 (node,relay)（跨会话字节等值，保
     core cfg 等值复用与 Hub[nodeId] 共享）、viewer 张按会话；分配 node-
     sticky;host 张续期走控制流带外刷新。原稿 per-session 票据会让同节点
     第二个 viewer 触发 host 换血（pipe_server.cpp:952-968）。
  4. 【阻断】候选列表收敛为同 relay 传输变体——host 单宿主，跨 relay 盲试
     是死路;降级改 re-POST 重分配。
  5. 【应修】删除"sessions 落 PG"的凭空假设（现无 sessions 表），改内存 +
     对账；PG 仅新增 relays 表。
  6. 【应修】redirect 增加 visited 防环与免退避跳数上限;resolve 定三态与
     server 不可达行为;迁移时双票同批再铸并引入 gen 世代。
  7. 【应修】签名密钥轮换定四步时序；relay 时钟 ±120s leeway + 漂移停配；
     打分明确用 mbpsOut;relay 日志剥离 query/ticket;域名模式改 server
     代签发证书（DNS API 凭据不铺 relay）。
  8. 【事实修正】viewer hello 不携带 token（鉴权在 `?token=`，现状）；
     rtvload 仅支持 WT 拨号（canary 打 viewer 腿够用）;TURN 池模式抄历史
     文档而非已退役代码;带宽数字标注为运营估计并给代码锚点;交叉引用
     §3.6/§3.7 修正;故障矩阵补"期"列如实标注 P1 能力边界。
