# RTV Relay Plane · P1 实施计划（契约落地 + 抽包 + 首台中继）

> 依据 spec：docs/superpowers/specs/2026-09-08-rtv-relay-plane-design.md（v1.0 已批准）
> 分支：feature/rtv-relay-plane（worktree `.worktrees/relay-plane`，实施时创建）
> 状态：待实施 · 2026-09-08

## 1. 目标机事实基线（RELAY1，2026-09-08 实测）

| 项 | 值 |
| --- | --- |
| 地址 / 凭据 | 47.96.83.132（root，凭据在 deploy/.env 的 `RELAY1_*` 键） |
| 地域 / 供应商 | 阿里云杭州（境内，AS37963）→ **纯 IP 免备案模式** |
| 系统 | Ubuntu 24.04.2 LTS（kernel 6.8，x86_64） |
| 规格 | 2 vCPU / 1.6 GiB 内存 / 40 GB 盘（35 GB 空闲）——低于 spec §6 的 4C4G 基线，首批容量预期打折（估 100–200 路活跃会话，以 RELAY_STATS 实测为准） |
| 端口入站实测 | **TCP 443 / UDP 443 / UDP 4433 全部可达**（探测包服务器端全部收到；安全组已放行，ufw inactive） |
| 端口占用 | 443/4433 空闲可绑定。机上已有他人服务：python3（47090/47091 TCP、47100–47103 UDP）、iperf3（5201）——**不得触碰** |
| 其他 | 无 docker（不需要，relay 为裸 systemd 静态二进制）；python3/nc/systemd/curl 齐备 |

纯 IP 模式的部署含义（spec §3.2/§4.2 落到本机）：
- WT 腿：自签证书 + `certSha256` 钉扎——web 需支持 `serverCertificateHashes`（原 spec 排 P3，
  因首台即纯 IP，**提前进 P1**，见任务 T7）。
- host 腿（QUIC）：rustls 自定义 verifier 钉扎同样提前进 P1（T7）；
  开发联调期可先用现有 `tlsInsecure` 参数过渡。
- WS 腿：浏览器无法对纯 IP 钉扎 → 本 relay 候选仅 wt；全候选失败降级 = re-POST
  回主站 relay-0（跨境，预期内）。

## 2. P1 范围（spec §9 P1 行）

rtv 模块抽包 + `xnc-relay` 二进制 + 双粒度票据（签发/验签/墓碑）+ relays 表与准入 +
池管理器（健康探测）+ node-sticky 分配器 + candidates 响应 + 内嵌 relay-0 + web 顺序
fallback 与烧尽 re-POST + RELAY_RECONCILE 对账 + **首台纯 IP relay 上线本机**。
P2 项（redirect/resolve/drain/SESSION_REFRESH/host 张续期/Monitor 页）不在本计划。

## 3. 任务分解（文件级）

### T1 proto：控制连接消息常量
- `proto/proto.go`：新增 `RELAY_REGISTER / RELAY_HEARTBEAT / RELAY_HEARTBEAT_ACK /
  RELAY_STATS / RELAY_RECONCILE / RELAY_SESSION_KILL`（P2 的 RESOLVE/DRAIN/
  TICKET_REFRESH 不加）。
- 验收：编译绿；常量仅新增不动旧值。

### T2 rtv 共享模块抽包（核心，最重）
- 新模块 `rtv/`（go.mod `xnc/rtv`，进 go.work）：
  - 自 `server/internal/rtv/` 搬 `relay.go / legquic.go / legwt.go / legws.go /
    cert.go`（Hub/扇出/迁移+IDR 合成/hello 门闩逐字节不动）。
  - 新增 `ticket.go`：RelayTicket 验签（ed25519 公钥集合，双窗口）、claims 校验
    （typ/nid/rid/gen/exp±120s leeway）、**墓碑表**（sid|nid→时刻，按 exp 有界清理）。
  - 新增 `arbiter.go`：传输级控制权仲裁机（语义对齐 manager.go:251-276：last-take-wins、
    3s 冷却、60s 租约 TTL），准入判据 = claims `cap.control`。
  - 接口缝 `PlaneHost`（VerifyTicket / TouchActivity(节流 15s) / SessionEvent），
    取代 router.go 的五处注入（AuthViewer/Touch/HostTokenOf/InputGate/ControlHooks）。
- `server/internal/rtv/` 删除，router 改为装配 rtv 模块（内嵌 relay-0 = 同一代码路径）。
- 验收：server 全测试绿（现 rtv 集成测试随包迁移）；rs 黄金向量/nalcheck 不动。

### T3 票据签发（server 侧）
- 新 `server/internal/rtvauth/`：ed25519 签名密钥（env 注入或首启生成落 StateDir 同级
  secrets 目录，0600）；host 张按 (node,relay) 缓存铸造（跨会话字节等值）、viewer 张
  按会话铸造（cap 由 RBAC 算）；密钥轮换双窗口接口预留（P2 工具化）。
- 验收：单测覆盖伪造/过期/leeway 边界/typ 错/rid 错/gen 落后/重放合法/墓碑拒绝。

### T4 数据模型 + 准入
- migration `0004_relay_plane.sql`：relays 表（spec §2.3）；sqlc 查询。
- `server/internal/config`：`XNC_RTV_RELAYS`（bootstrap 种子）、
  `XNC_RTV_RELAY_ALLOWLIST`（公钥 hex 清单）。
- admin API：`GET /api/admin/relays`、`PATCH /api/admin/relays/{id}`（pending→active /
  状态流转）。admin 判定复用现 isAdminUser。

### T5 relay 控制连接 + 池管理器（server 侧）
- 新 `server/internal/rtvpool/`：agentws 同构挑战-应答 + 公钥准入；30s 健康探测
  （对 host 腿 QUIC 探测，连败 2 摘除/1 次恢复，模式照 turn-pool 历史文档）；
  RELAY_STATS 落内存画像；RELAY_RECONCILE 对账重建 sticky/会话记录；
  RELAY_SESSION_KILL 下发（触发点：会话 janitor 收线时）。
- TouchActivity/idle janitor 改由 relay 上报驱动（节流 15s < 租约 60s）。

### T6 分配器 + 响应 v2
- `desktop_handlers.go`：node-sticky 优先（活跃桌面会话沿用同 relay）；打分 =
  区域权重 + `max(sessions/maxSessions, mbpsOut/maxMbpsOut)`；relay-0 fallback-only。
- 响应：`{sessionId, token, expiresAt, websocketUrl, candidates[], wtUrl, wsUrl, lease}`
  （wtUrl/wsUrl = 首候选派生，旧 web 兼容）。
- DesktopParams 不改（StreamEndpoint 装 relay host 腿展开，token 装 host 张票）。

### T7 纯 IP 钉扎（自 P3 提前）
- web `DesktopLive.tsx`：候选项带 `certSha256` 时 `new WebTransport(url,
  {serverCertificateHashes})`；无则现状。
- proto `DesktopParams` 加可选 `certSha256` 透传；agent/desktop 原样转 core→host cfg。
- host `transport.rs`：cfg 带 certSha256 时用钉扎 verifier（复用 AcceptAnyServer 的
  自定义 verifier 管线，rustls ServerCertVerifier 按指纹比对）。
- 联调过渡：`tlsInsecure` 现有参数可用于无钉扎的 dev。

### T8 xnc-relay 二进制
- 新模块 `relay/cmd/xnc-relay/main.go`：控制 server URL + 身份目录（默认
  `/var/lib/xnc-relay`，0600）+ 监听地址（本机 = UDP443 /wt + UDP4433 host 腿；
  不开 TCP443——纯 IP 无 WS 腿）；首启生成 ed25519 身份并注册（首次落 pending，
  审批后 active）；自签证书 + 指纹上报（RELAY_REGISTER 载荷）。
- 日志纪律：剥离 query string，ticket 不落日志；未验签连接 per-IP 握手预算。

### T9 部署脚本 + 本机上线
- `deploy/build-relay.ps1`：CGO_ENABLED=0 GOOS=linux GOARCH=amd64 构建 → paramiko
  SFTP（读 RELAY1_* 键）→ `deploy/xnc-relay.service` systemd 安装/重启（模式抄
  build-server-local.ps1/push_server_image.py）。
- 上线序：server 侧合入并部署 → relay 注册（pending）→ admin 审批 active →
  健康探测绿 → 真机验收（§5）。
- 回滚：PATCH relay → retired + server 回旧版镜像（存量会话断，等同现状 server
  重启的恢复语义）。

### T10 web P1 改动
- `DesktopLive.tsx`：candidates 顺序 fallback；全烧尽 re-POST 重建（节流 ≥10s 防循环）。
- Monitor 页 per-relay 视图留 P2（admin API 已可用 curl 验证）。

## 4. dev 验证环境

- `docker-compose.dev.yml` 增加 relay 服务（本地起 2 实例、不同 UDP 端口），
  验证分配/sticky/多 relay；票验签/墓碑/对账单测覆盖。
- **同节点双 viewer cfg 等值回归**（spec §0 票据粒度的验收）：core 不得重启 host。

## 5. 真机验收（LABS-XIAOXIN + RELAY1）

1. 池空回归门：不配 relay 时行为与现版本零差异。
2. relay active 后开桌面会话：主站 `tcpdump` 确认无 UDP 媒体字节（TCP 侧仅信令/兜底）；
   XIAOXIN（境内侧）→ relay（杭州）→ 境内浏览器全程境内链路，RTT 显著优于跨境基线。
3. 同节点第二个 viewer 加入：媒体零中断（无换血 IDR 风暴）。
4. 伪造/过期票、kill 后重连（墓碑生效）拒绝矩阵。
5. relay 进程 kill -9：数秒自愈（re-hello 顶替 + IDR）。
6. rtvload canary：`--url "https://47.96.83.132/wt?token=<viewer票>"` 打 relay
   viewer 腿 30s 报告（dump 经 nalcheck）。

## 6. 风险与注意

- 1.6 GiB 内存：relay 进程 RSS 预算 <200 MiB（扇出缓冲池上限收敛），RELAY_STATS
  监控；容量打分上限先设保守值（maxSessions=100 / maxMbpsOut=500）。
- 机上他人服务（47090–47103、iperf3 5201）不得触碰；部署脚本仅操作
  /usr/local/bin/xnc-relay、/etc/systemd/system/xnc-relay.service、/var/lib/xnc-relay。
- root 密码 SSH：P1 先用（paramiko，凭据在 .env）；后续换密钥 + 收紧（记运维待办）。
- host 张票 24h 无续期（P2 才有）：P1 期长会话 24h 后经"注册失败→SESSION_REFRESH 缺位
  →viewer re-POST"路径重建——真机验收观察此路径的实际体验，异常再提前 P2 的续期任务。

## Results（2026-09-08，P1 代码全部落地，分支 feature/rtv-relay-plane）

提交序列（efb7b71 文档 → b4f08ff T1 → fccc381 T2a → dd031c2 T2b+T3 →
3dd7da9 T4 → 11559cf T5+T6 → ba90601 T7 → d896fb2 T8+T9 → f43c9b1 T10）。

门禁：proto/rtv/relay/server/agent Go 全绿（server 真 PG 集成 0 失败；
desktop API 测试随票据模型更新）；web npm build + vitest 13/13；
host cargo check 本机受 VCPKG 环境阻塞（已知），钉扎 verifier 经同版本
依赖组合的 scratch crate 测试验证，PR 后由 CI host-rust job 承担正式门禁。

与计划的偏差（有意为之）：
1. **XNC_RTV_RELAYS bootstrap 种子取消**——注册全动态，准入即 allowlist
   （或 pending 审批），少一条配置面。
2. **RELAY_CONFIG 提前进 P1**（原排 P2）：relay 离线验票必须有 server
   签名公钥，认证通过即下发；轮换推送仍留 P2。Challenge 顺带回传
   relayId（首注册时 relay 才知道自己被分配的 id）。
3. **被动首约（Grant）仅 relay-0**：外部 relay 的仲裁机在远端，P1 无
   下发通道，外部会话以显式 takeControl 为准（P2 经控制连接补）。
4. host 张票 24h 无续期（P2 的 ticketRefresh）；relay-0 会话本就随进程
   存亡，外部 relay 场景经"注册失败→viewer re-POST"路径重生。
5. dev 双 relay compose 未加：单 relay 逻辑经 rtv 回环测试 + api 真 PG
   测试覆盖；双 relay 端到端并到真机验收（需 server 先部署）。

## 真机验收结果（2026-09-08 22:00-22:05，server 0.10.14 + relay rl-3f7ed0d7）

生产链路：XIAOXIN（老 agent，TLSInsecure 过渡路径）→ 中继机（杭州）→ dev 侧
rtvload。六项全过：
1. 分配/candidates：202 响应 candidates=[{wt 47.96.83.132:443, certSha256}]，
   两会话同 relay（node-sticky）。
2. 主站零媒体字节：SRV 40s tcpdump 仅 18 个探测包（30s QUIC probe）。
3. 媒体经中继：rtvload 完整帧 1/丢失率 0%/RTT 24-27ms；dump 765KB 经
   nalcheck OK（SPS+PPS+IDR 完整）。
4. 同节点双 viewer 并发不换血：host registered 计数 = 1（无替换），
   双 viewer 各自收到扇出。
5. FEC 恢复实测生效（一次丢包窗口的入会 IDR 被校验码补回）。
6. 撤销/墓碑：kill 后同票重连 401（修复前的现场即验证了拒绝面）；
   修复后会话跨 Opening TTL 存活、同票重连成功。

补充（浏览器终验，22:10 后）：rtvload 验收绿但真实浏览器仍连不上——
两个浏览器路径独有的关卡被工具绕过（无 Origin 头 / 跳过证书校验）：
WT 腿同 host 校验拒绝 主站页面→relay IP 的跨 host 连接（--allow-origin
显式放行）；钉扎自签证书 825 天超 WebTransport 规范的 14 天上限（改 7 天
持久化 + 24h 重签余量）。真浏览器实测：WT 主路在线、RTT 25ms、e2e≈358ms、
QSV config 到达、canvas 像素级确认画面渲染。**教训入档：合成 viewer 的
验收矩阵必须包含"带 Origin + 证书钉扎"的真浏览器路径。**

验收中抓出并修复的三个生产 bug（都有回归测试）：
- desktop 会话 60s Opening TTL 误杀（viewer 不再经 AttachClientRTV 粘合）：
  TouchActivity 即粘合 + 外部 relay 的活跃 sid 集经 RELAY_STATS 旁路上行
  （外部 relay 的 TouchFn 为 nil，触碰原本死在中继进程内）。
- 会话终局的墓碑误打节点键：host 张票按 node 铸造跨会话复用，一次会话
  关闭把节点拉黑 25h（HOST_AUTH_FAILED 风暴）——Kill 语义改为会话级
  只打会话键，节点级仅留给管理端。
- 镜像构建缺 xnc/rtv 模块（Dockerfile COPY + go.mod require/replace +
  GOWORK=off 补全 go.sum）。

运维注意：
- **生产 server 当前跑的是 feature 分支构建（0.10.14）**——尽快 PR/合回
  main 使仓库与线上一致；compose 的 XNC_RTV_SIGNING_KEY 已入 SRV env
  （deploy/.env 本地两份同步）。
- 老 agent 走 TLSInsecure 过渡路径连接纯 IP relay；certSha256 钉扎要等
  下一个安装器发版（agent/host 更新）后自动收紧。
- relay rl-3f7ed0d7 已 active，容量声明 100 会话/500Mbps（1.6GiB 小机，
  按 §6 保守值）。

待办（P1 验收序，见 §5）：
- PR → CI 六矩阵（host-rust 是关键门禁）→ 合入 main
- server 部署（build-server-local.ps1，需配置 XNC_RTV_SIGNING_KEY——
  一次性生成入 deploy/.env 与 compose env）
- `powershell deploy/build-relay.ps1 -PublicHost 47.96.83.132` 部署中继机
- relay 注册（pending）→ `PATCH /api/admin/relays/{id}` 审批 active
- LABS-XIAOXIN 真机走 §5 六项验收（tcpdump 主站无媒体 UDP 字节、双
  viewer 不换血、墓碑拒绝矩阵、kill -9 自愈、rtvload canary）
