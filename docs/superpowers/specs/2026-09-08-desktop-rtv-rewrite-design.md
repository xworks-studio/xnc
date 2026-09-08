# XNC 桌面端架构重构（RTV 链路）系统设计

> 版本 v1.0 · 2026-09-08 · 状态：已批准（大爆炸式单次重写）
> 一句话定位：以已验证的 MVP-RTV 三端实现为蓝本，将 XNC 桌面链路整体替换为
> "Rust 采集端 + QUIC 不可靠 datagram + RS FEC + 服务器字节中继 + WebCodecs 硬解"
> 的自有协议栈，同时退役 WebRTC/SRTP/TURN、C++ 采集栈与 agent 侧 Pion 媒体面。

## 0. 决策记录（用户已拍板，实施不得偏离）

| 决策点 | 结论 |
| --- | --- |
| 推进策略 | 大爆炸式：feature/desktop-rtv-rewrite 分支单次重写，一次合并，一次发布 |
| 范围 | 端到端：host（Rust）、server 三腿中继（Go）、web 渲染端（React/Vite） |
| 证书 | 真实 CA：xnc-server 内嵌 ACME DNS-01（lego + Aliyun DNS）签发 xnc.app 证书；浏览器标准 Web PKI 校验，删除 serverCertificateHashes/cert.json 整层；host 腿同用该证书（rustls 系统根校验） |
| host 进程模型 | 沿用 xnc-core spawn：白名单 `xnc-desktop.exe` → `xnc-host.exe`；endpoint+token 经 stdin 匿名管道下发（argv 零密钥契约） |
| 代码忠实度 | MVP 代码忠实搬用：rs.rs/rs.js、framing、worker、pacer 语义逐字节不动，RS 黄金向量 fixture 原样迁移；改动只允许出现在 §4 枚举的集成缝 |
| 功能对齐 | 键盘（含 TEXT/LOCK）、多显示器枚举/切换、SAS、JPEG 快照均不在本次范围，记为后续 PATCH（§8） |

## 1. 背景与动机

### 1.1 现状

现行链路：`DXGI → xnc-desktop(C++) → named pipe → agent(Go/Pion) → SRTP/TURN → 浏览器 WebRTC`。
0.9.3 QoS 修复周期现场失败后整体回滚（commit 132e875），三处确诊缺陷重新在场：
reset-grace 闩死、PauseSpectator 无恢复通道、带宽估计退回浏览器 goodput 自指源。
结构债：M0/V2 双管线并存、WAIT_IDR 语义三份拷贝、二进制协议三语言手写镜像、
selftest 单文件 10.6k 行、DesktopLive.tsx 巨石组件零测试。

### 1.2 研究结论（desktop-reserch/，《桌面采集与串流技术研究报告》）

- Moonlight 栈（单向 UDP 不重传 + FEC + 客户端检测/服务端执行 IDR）与 RustDesk
  双闭环 ABR 是成熟范式；QUIC datagram 是浏览器内唯一"不可靠、无队头阻塞"的包
  语义，等价裸 UDP，可原样移植 FEC+IDR/RFI 恢复机制。
- 服务器无 GPU ⇒ 只能字节中继（不解码不转码），与"Server 中转 + 多观察者扇出"
  需求一致；实测中转一跳 RTT p50 ≈14ms，总延迟预算 40–90ms。
- WebRTC 的自适应 jitter buffer 引入 +20–100ms；WebCodecs `optimizeForLatency`
  硬解无此负担。
- MVP-RTV 已在公网全链路验证（e2e p50 14ms、ffmpeg 解码零错误、WT 主路 + WS
  兜底、host 掉线零操作自动恢复）。

### 1.3 成功标准

- `cargo test` 7/7（RS 黄金向量不变）+ node RS 对拍双绿。
- rtvload 30s 报告：e2e p50 ≤ 50ms（同机房 dev 环境）、FEC 恢复生效、
  dump.h264 经 ffmpeg 解码零错误、nalcheck 不变量通过。
- 真机（LABS-XIAOXIN）：交互会话采集、QSV 硬编、WT 主路 + `?transport=ws`
  兜底、host 崩溃退避自愈、server 重启 viewer 无感续流。
- LABS-TB16G7（用户机）实测通过后方可发布。
- 全模块（proto/server/agent/cli/shellhost + web + cargo）构建与测试绿。

### 1.4 已确认约束

- Windows 10/11 节点；Chrome/Edge 浏览器。
- 端口：TCP 443（caddy→xnc-server，WS 兜底腿）、UDP 443（WT 主路腿）、
  UDP 4433（host QUIC 腿）。host 腿无 TURN 兜底（QUIC 必须 UDP），记为已知约束。
- 鉴权不可回退：host 注册必须持 HostToken；viewer 连接必须持会话 token；
  input 消息必须过 lease + capability 门控。
- 存量在线节点（当前 0.7.4）经安装器自更新跟进；server 与安装器发布顺序见 §7。

## 2. 目标架构

```
浏览器 ── WT(UDP443, H3+WebTransport, xnc.app LE 证书, /wt) 主路 ──┐
      └── WS 兜底(wss://xnc.app/ws, 经 caddy TCP443 → xnc-server:8080) ──┤
                                                                        xnc-server (Go)
                                              Hub[nodeId]：字节扇出 / 控制最小路由 /
                                              lease+capability 门控 / statsz(管理权限)
                                                   ▲ UDP4433 raw QUIC (ALPN xnc-host/1)
xnc-host.exe (Rust, xnc-core spawn 于用户会话, SYSTEM) ──┘
  datagram=媒体+FEC（不可靠） / bidi stream=控制 JSON（可靠）
  scrap DXGI→GDI 回退 · hwcodec QSV→NVENC→AMF→libx264 · Cauchy RS FEC
  · 令牌桶整流 · host 侧 QoS（viewer feedback 驱动）

agent（瘦身）：SESSION_OPEN → coreclient.StartCapture(endpoint+token 经 stdin)
             SESSION_CLOSE → 引用计数归零 → StopCapture
xnc-core（保留）：pipe RPC / spawn / token / WtsMonitor / DesktopSupervisor 崩溃退避
coturn：退役（desktop 是其唯一用户，合并前审计确认）
```

### 2.1 端到端时序

1. 浏览器 `POST /api/nodes/{id}/desktop` → server 会话管理器创建会话
   （token、lease 仲裁、capabilities 计算）→ 若该节点 host 未运行，经控制连接
   下发 SESSION_OPEN（DesktopParams{StreamEndpoint, HostToken, WTSSession,
   LeaseID, Capabilities}）→ agent → coreclient.StartCapture → core 以
   SessionSystemToken spawn xnc-host.exe（stdin 下发 endpoint+token+运行参数）。
2. xnc-host 直连 server UDP4433，控制流首条 `hello{role:"host", nodeId, token}`
   注册；server 校验 HostToken 合法性与节点绑定后入 Hub[nodeId]。
3. 浏览器持 token 连 `/wt`（主路）或 `/ws`（兜底），`hello{role:"viewer"}` →
   server 校验 token → 绑定到会话对应节点的 HostSession → 补发缓存 config +
   合成 frameLoss（新 viewer 触发 IDR）。
4. 媒体下行：host 逐帧 Annex-B → 34B 包头分片 + RS FEC → 令牌桶 → datagram →
   server 字节扇出 → viewer（WT datagram / WS 二进制帧）。
5. viewer worker：组帧 + FEC 恢复（五重校验）→ WebCodecs 硬解 → VideoFrame
   转移主线程 → rAF Pacer（队列≤3）→ desynchronized canvas。
6. 控制上行：feedback/fecStats/frameLoss/heartbeat → server 最小路由 → host
   QoS；input（鼠标）→ server lease/capability 门控 → host SendInput。
7. 收线：viewer 断开（hostOffline 时 hello 循环自动重试）；全部 viewer 离场 →
   janitor 收 desktop 会话 → SESSION_CLOSE → agent 引用计数 → StopCapture →
   core 终止 xnc-host.exe。

### 2.2 协议

线协议 = `mvp/docs/proto.md` v1 原样（34B 包头、FEC 运算域、丢帧判定、控制
消息词汇、QoS 分档），本 spec 不重复定义；差异仅限：

| 项 | MVP | XNC 迁移后 |
| --- | --- | --- |
| host hello | `{role:"host", sessionId:"default"}` | `{role:"host", nodeId, token}`（token 必须有效且绑定该节点，否则注册拒绝） |
| viewer hello | `{role:"viewer", sessionId}` | `{role:"viewer", token}`（连接级：WT/WS 握手携带会话 token，server 完成会话→节点映射后再进入 hello 流程） |
| input 门控 | 无（本地 UI 开关） | server 侧强制：无 lease 或缺 input.mouse capability 的 viewer 的 input 消息直接丢弃 |
| hostOffline/viewer 迁移/hello 重试门闩 | 已实现 | 语义原样保留（MVP 踩坑结晶） |

## 3. 组件设计

### 3.1 `host/`（Rust crate `xnc-host`，二进制 xnc-host.exe）

模块自 `mvp/host/src` 忠实迁移：capture/encoder/framing/rs/transport/qos/
stats/shared/input/main。集成缝（仅此清单，见 §4）：

- `Cargo.toml`：scrap 指向 `third_party/scrap`（vendored rustdesk fork，锁 rev）；
  crate/binary 更名 xnc-host。
- `main.rs`：配置源从 CLI 默认值改为 stdin JSON（`{endpoint, token, nodeId, fps,
  bitrateKbps, fec, display, sendKbps, logFile}`）；argv 只允许非密钥覆盖项。
- `transport.rs`：hello 携带 nodeId+token；TLS 校验从 AcceptAnyServer 换系统根
  （server 证书为 CA 签发）；ALPN 不变。
- 日志：`--log-file` 落 `C:\ProgramData\XNC\logs\xnc-host.log`（core spawn 传参）。

### 3.2 `server/internal/rtv/`（Go，自 mvp/server 移植）

- `relay.go`：Hub 键改为 nodeId；HostToken 签发/校验（会话创建时生成，经
  SESSION_OPEN→agent→core→host stdin 下发；host 重连凭同 token 再注册=合法
  顶替）；viewer 迁移、hello 重试门闩、hostOffline 广播、config 缓存、frameLoss
  合成原样保留。
- `legquic.go`（UDP4433）/`legwt.go`（UDP443, /wt, handler 会话期阻塞）/
  `legws.go`（/ws，挂现有 HTTP mux 经 caddy）：三腿共用控制帧编解码。
- 鉴权与门控：viewer 腿在握手期校验会话 token（query param，与现有 session WS
  一致）；input 消息按会话 lease + capabilities 过滤后才 forwardToHost；
  `/statsz` 收管理权限；`/cert.json` 删除。
- 会话接线：desktop kind 不再走 pump；首个 viewer 触发 SESSION_OPEN，无 viewer
  经 janitor 收线（复用现有 idle sweep + lease TTL 语义）。

### 3.3 `native/core/`（C++，裁剪保留）

- 白名单 `spawn.h`：`xnc-desktop.exe` → `xnc-host.exe`。
- StartCapture RPC：请求扩展携带 stdin 配置 blob（endpoint/token/参数 JSON）；
  响应去掉 pipe/secret 字段（rt pipe 不复存在）。
- DesktopSupervisor（崩溃退避 1s→60s + 5 次锁定降级）、WtsMonitor 会话切换
  监视、watchdog 全部保留。
- 删除：rt-pipe 相关回调/等待逻辑、快照 spawn 路径（--jpeg-single 随 C++ 栈
  退役，agent 侧以明确错误码兜底）。

### 3.4 `agent/`（Go，瘦身）

- `agent/desktop` 重写为 thin handler：SESSION_OPEN → StartCapture（参数透传）；
  SESSION_CLOSE → intent 引用计数归零 → StopCapture。intent.go 的源监督模式
  保留（崩溃退避重拉由 core DesktopSupervisor 承担，agent 不再自己监督）。
- 删除：transport/viewer_sender/qos_controller/input/keyframe_coordinator/frames/
  source/frame_meta（Pion 栈）、`agent/desktoppipe/` 全部。
- `agent/coreclient`：StartCapture 请求加 stdin blob；删 rt-pipe 辅助。

### 3.5 `web/`（React/Vite）

- `web/src/lib/rtv/`：proto/rs（逐字节）、worker（Vite `new URL(...,
  import.meta.url)` 形态）、transport（框架无关，WT 主路 + WS 兜底 + 证书直连
  Web PKI）、input（鼠标采集与 letterbox 映射）。
- `DesktopLive.tsx` 重写：React 壳（会话创建/lease UI/HUD/framediag 等价物）+
  canvas ref 持有 Pacer 渲染层；队列≤3、迟到丢帧不追、延迟一帧释放、空转重绘
  语义不变。
- 删除：旧 WebRTC 路径、`ScreenPreview.tsx`（死代码）、`keymap.ts`（键盘后置）。

### 3.6 proto

`DesktopParams` 重写：`{StreamEndpoint, HostToken, WTSSession, LeaseID,
Capabilities}`；删除 `DesktopTurnConfig`、`DesktopIceAll`、MediaProtocol 词汇与
canary 机制。REST `POST /desktop` 响应：`{sessionId, token, wtUrl, wsUrl}`。

## 4. 允许的改动缝（忠实搬用契约的边界）

1. `host/Cargo.toml` 依赖路径与 crate 命名；`.cargo/config.toml` 不变。
2. `main.rs` 配置装载（stdin JSON + argv 非密钥覆盖）。
3. `transport.rs` hello 字段与 TLS verifier。
4. 日志文件路径参数化。
5. server 侧 Hub 键/鉴权/门控/证书来源/observability 收权。
6. web 侧模块导入路径与 Worker 构建形态、React 渲染壳。
7. 其余（rs/framing/worker 逻辑/pacer/QoS 决策/恢复语义）逐字节搬用；
   `proto/fixture-rs.json` 不动。

## 5. 证书与部署

- xnc-server 内嵌 ACME DNS-01（go-acme/lego + alidns provider）：签发 xnc.app
  证书 → 卷持久化 → 到期前 30d 自动续期 → WT/QUIC 监听热加载；凭据新增
  `deploy/.env` 键（ACME_/ALIDNS_ 前缀），严禁入库。
- compose：xnc-server 增加 `ports: ["443:443/udp", "4433:4433/udp"]`；
  Caddyfile 全局 `servers { protocols h1 h2 }` 让出 UDP443；coturn 移除（先
  审计无其他用户：grep TURN/tunnel 用途）；`deploy/PORTS.md` 扩写。
- 阿里云安全组放行 UDP 443/4433。
- caddy TCP443 链路与其自身证书机制不变。

## 6. 构建与 CI

- `installer/build.ps1`：新增 cargo 构建步骤（xnc-host.exe 替换 xnc-desktop.exe，
  五 exe 数不变）；签名链不变（codesign 指纹/装卸信任复用）。
- `ci.yml`：新增 Rust job（rust-toolchain.toml 锁版本 + vcpkg 二进制缓存 +
  cargo test/build）；node RS 对拍（`tools/rtvload` 测试）入 CI。
- `release.yml`：windows runner 加 Rust 工具链 + VCPKG_ROOT/LIBCLANG_PATH 环境
  （二进制缓存预热）；`tools/rtvload` 随矩阵编译。
- `tools/`：新增 `rtvload/`（自 mvp loadclient，加 token 鉴权）；删除
  `e2eviewer/`；`nalcheck` 保留；`desktopreport` 瘦身适配（frameStats 为新遥测
  源）或退役并留终版报告。

## 7. 发布与回滚

1. 合并 feature/desktop-rtv-rewrite → main。
2. server 先行：手动触发 build-server（版本号 X.Y.Z）→ Watchtower ~5–10 分钟
   换版，三腿上线。
3. 触发 release（安装器同版本或 +PATCH）→ 节点 WS 推送升级（秒级中断）→ 桌面
   恢复。窗口期（server 更新后、节点升级前）桌面不可用，分钟级。
4. 回滚 playbook：server 镜像回钉 `:v<旧>` + 节点重装上一版安装器（0.9.3 已
   演练过的路径）。

## 8. 已知功能回归（用户确认接受，后续 PATCH 序列）

1. 键盘输入（含 TEXT/LOCK）——回归影响最直接，建议第一个 PATCH。
2. 多显示器枚举/切换（现 0x0128 能力随 C++ 栈退役）。
3. SAS（Ctrl+Alt+Del；agent→core 0x0110 路径删除）。
4. kind=screen JPEG 快照（core 0x0111 spawn --jpeg-single 路径删除；agent 以
   SCREEN_SNAPSHOT_UNSUPPORTED 明确错误兜底，T8 前审计调用面）。
5. QoS 为 MVP 单一最新反馈语义；多 viewer 异质网络差异化 FEC/码率后置。

## 9. 测试与验收资产

- RS 黄金向量：`proto-fixtures/fixture-rs.json` + Rust `golden_fixture` + node
  `rs.test.js`（双端对拍，CI 门禁）。
- `tools/rtvload`：合成 WT viewer（鉴权版），30s 报告 + Annex-B 落盘。
- nalcheck：IDR ratio / SPS+PPS 前置不变量。
- 真机矩阵：LABS-XIAOXIN（干净实验台）→ LABS-TB16G7（用户机）。
