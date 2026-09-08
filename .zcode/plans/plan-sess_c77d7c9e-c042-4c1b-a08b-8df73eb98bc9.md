# 桌面端架构重构：MVP-RTV 全链路迁移计划

## 0. 已拍板决策（用户确认）

| 决策点 | 结论 |
|---|---|
| 策略 | 大爆炸式：`.worktrees/desktop-rtv` + feature 分支单次重写，一次合并，一次发布 |
| 范围 | 端到端：Rust host + server 三腿中继 + web 渲染端，旧桌面栈全量删除 |
| 证书 | 真实 CA：xnc-server 内嵌 ACME DNS-01（lego + Aliyun DNS）签发 xnc.app 证书，浏览器标准 Web PKI 校验；**删除 serverCertificateHashes/cert.json 整层**（host 腿 UDP4433 同用该证书，rustls 走系统根校验替换 AcceptAnyServer） |
| host 进程模型 | 沿用 xnc-core spawn：白名单 `xnc-desktop.exe` → `xnc-host.exe`，endpoint+token 经 stdin 匿名管道下发（守"argv 零密钥"契约），rt pipe 协议全删 |
| 代码忠实度 | MVP 代码忠实搬用：`rs.rs`/`rs.js`/framing/worker/pacer 语义逐字节不动，黄金向量 fixture 原样迁移；改动只允许出现在下文枚举的集成缝 |
| 功能对齐 | 多显示器、SAS、TEXT/LOCK、键盘、JPEG 快照**均不在本次范围**，记为后续 PATCH（见 §7 回归清单） |

## 1. 目标架构（迁移后）

```
浏览器 ── WT(UDP443, H3, xnc.app LE 证书, /wt) 主路 ──┐
      └── WS 兜底(wss://xnc.app/ws, 经 caddy TCP443 → xnc-server:8080) ──┤
                                                                        xnc-server(Go)
                                              Hub[nodeId] ← 字节扇出/控制最小路由/lease 门控
                                                   ▲ UDP4433 raw QUIC (ALPN xnc-host/1)
xnc-host.exe (Rust, core spawn 于用户会话, SYSTEM) ──┘ datagram=媒体+FEC / stream=控制
  scrap DXGI→GDI · hwcodec QSV→NVENC→AMF→x264 · Cauchy RS FEC · 令牌桶 · host 侧 QoS
agent(瘦身): SESSION_OPEN → coreclient.StartCapture(endpoint+token 经 stdin) ；SESSION_CLOSE 引用计数 → StopCapture
xnc-core(保留): pipe RPC/spawn/token/WtsMonitor/DesktopSupervisor 崩溃退避
coturn: 退役（desktop 是其唯一用户，合并前审计确认后从 compose 移除）
```

**删除**（约 4.5 万行）：`native/desktop/` 全部（含 10.6k 行 selftest）、`agent/desktop/` Pion 栈（transport/viewer_sender/qos_controller/input/keyframe_coordinator/frames/source ~5.5k 行）、`agent/desktoppipe/`、server 的 `turnpool.go`+`desktop_media_select.go`+desktop pump 复用、web 旧 DesktopLive WebRTC 路径 + `ScreenPreview.tsx` 死代码 + `keymap.ts`、`tools/e2eviewer/`（被 loadclient 取代）。
**保留**：`native/core`（裁剪）、`native/common`（XNIP，agent↔core 仍在用）、`agent/coreclient`（裁剪 rt-pipe 部分）、lease/capability 语义（从 agent 侧强制改为 server 中继侧强制）、`tools/nalcheck`（Annex-B 分析不受影响）、`tools/desktopreport`（瘦身适配新遥测源，或随合并退役并留终版报告）。

## 2. 新代码落位

| 来源 | 去处 | 改动缝（仅此清单） |
|---|---|---|
| `mvp/host` | 顶层 `host/`（crate `xnc-host`） | ① scrap 依赖 vendor 为 `third_party/scrap`（rustdesk fork 裁剪，修复断链）；② main.rs 配置改 stdin JSON（endpoint/token/fps/bitrate/fec/display/logFile），argv 只留非密钥；③ transport.rs hello 加 `nodeId+token`，TLS 换系统根校验；④ 日志落 `ProgramData\XNC\logs`；⑤ 其余模块逐字节搬用 |
| `mvp/server` | `server/internal/rtv/`（relay+legquic+legwt+legws） | ① Hub 键 = nodeId，host 注册必须持有效 HostToken（SESSION_OPEN 经 agent 下发，消灭任意顶替）；② viewer 腿挂会话 JWT 鉴权 + RBAC/lease 门控（无 lease 者控制面 input 消息丢弃）；③ legwt/legquic 用 ACME 证书；legws 挂现有 mux 走 caddy；④ `/statsz` 收管理权限；⑤ viewer 迁移/hello 重试/hostOffline 语义**原样保留**（踩坑结晶） |
| `mvp/server/web/js` | `web/src/lib/rtv/`（proto/rs/transport/worker/input） | Worker 改 Vite `new URL(...,import.meta.url)` 形态；rs/proto 逐字节；transport 层框架无关化；渲染层重写为 React 组件持 canvas ref，Pacer 队列≤3/延迟一帧释放/空转重绘语义不变 |
| `mvp/server/cmd/loadclient` | `tools/rtvload/` | 加 token 鉴权头；作为新 e2e 验收门 |
| `mvp/proto/fixture-rs.json` + `test/` | `host/../proto-fixtures/` + `tools/rtvload/` 测试 | 原样迁移，CI 双端对拍 |

**proto 改动**：`DesktopParams` 重写为 `{StreamEndpoint, HostToken, WTSSession, LeaseID, Capabilities}`（Turn/IceTransportPolicy/MediaProtocol 删除）；`POST /api/nodes/{id}/desktop` 响应改 `{sessionId, token, wtUrl, wsUrl}`；server 会话管理器：desktop kind 不再走 pump，首个 viewer 触发 SESSION_OPEN 拉起 host、无 viewer 经 janitor 收线 StopCapture。

## 3. 任务分解（worktree 内按序执行，每任务独立提交 + 验证）

- **T0 规格与脚手架**：按仓库惯例先写 `docs/superpowers/specs/2026-09-08-desktop-rtv-rewrite-design.md`（以 mvp/docs/proto.md 为基础扩 XNC 鉴权/会话语义）+ 建 worktree/分支。
- **T1 host 落位**：vendor scrap、Cargo 就位、`cargo test` 7/7 绿（RS 黄金向量不变）。
- **T2 host 集成缝**：stdin 配置/token hello/系统根 TLS/日志路径/nodeId；本机冒烟对 dev server 出流。
- **T3 core 适配**：白名单换名、StartCapture 请求带 stdin 配置 blob、响应去 pipe 字段、DesktopSupervisor/WtsMonitor 保留、rt-pipe 残留清理；`native/core` 自测绿。
- **T4 agent 瘦身 + proto**：新 thin handler（intent 引用计数模式保留）、删除 agent/desktop+desktoppipe 旧实现与其 6.7k 行测试、`go build ./...` + `go test` 绿（agent/session 的 linux 遗留失败**不顺手修**）。
- **T5 server 中继**：rtv 包移植 + Hub/nodeId/HostToken 鉴权 + 会话管理器接线 + lease/capability 服务端门控 + statsz 收权；server 测试（真 PG）绿。
- **T6 证书与部署面**：xnc-server 内嵌 ACME DNS-01（lego+alidns，凭据进 `deploy/.env` 新键）+ 证书卷 + 热加载；compose 加 `443:443/udp`、`4433:4433/udp`，Caddyfile 全局 `protocols h1 h2` 让出 UDP443，coturn 移除（先审计无其他用户），PORTS.md/AGENTS/spec 更新。
- **T7 web 端**：DesktopLive 重写（React 壳 + rtv lib + HUD + lease UI），删旧路径与死代码；`make web test`/oxlint/vitest 绿。
- **T8 删除旧栈**：native/desktop、turnpool、media_select、e2eviewer 等全量删除，全模块构建绿。
- **T9 构建/CI/安装器**：build.ps1 加 cargo 步骤（五 exe 数不变，xnc-desktop→xnc-host，签名链不变）；release.yml/ci.yml 加 Rust 工具链 + vcpkg 二进制缓存 + `tools/rtvload` 编译；node RS 测试入 CI。
- **T10 验收**（发布前置门，全绿才进 T11）：① `cargo test` 7/7 + node RS 对拍双绿；② dev server + 本机 host：rtvload 30s 报告（丢帧/FEC 恢复/RTT/吞吐）+ dump 经 nalcheck + ffmpeg 解码零错误；③ LABS-XIAOXIN 真机：交互会话采集、QSV 硬编、浏览器 WT 主路 + `?transport=ws` 兜底、host 崩溃退避恢复、server 重启 viewer 无感续流；④ LABS-TB16G7（用户机）实测通过。
- **T11 发布切换**：server 先行（build-server 手动触发，Watchtower ~5-10 分钟换版，新腿上线）→ 触发 release（安装器）→ 节点 WS 推送升级（秒级）→ 桌面恢复。窗口期桌面不可用（分钟级）。**回滚 playbook**：server 镜像回钉 `:v<旧>` + 节点重装上一版安装器（0.9.3 已演练过的路径）。

## 4. 验收门汇总

cargo 7/7（fixture 不变）· node RS 对拍 · 各 Go 模块 test · rtvload 指标（e2e p50 目标 ≤50ms、FEC 恢复生效、ffmpeg 零解码错误）· 真机 WT/WS 双路 + 三类故障恢复（host 崩溃/server 重启/断网重连）· TB16G7 用户实测。

## 5. 主要风险与对策

- **vcpkg/ffmpeg 构建时长**（CI 30-60min）→ GitHub Actions 二进制缓存；release 文档写明环境（VCPKG_ROOT/LIBCLANG_PATH 已在实验机就绪）。
- **UDP 4433/443 被严格网络封锁**：host 腿无 TURN 兜底（QUIC 必须 UDP）→ 已知约束记入 spec；浏览器侧 WS/TCP443 兜底已内建。
- **证书轮换断流**（LE 90d 续期换证书时 QUIC 连接断）→ 热加载 + host/viewer 自动重连语义已内建（MVP 掉线恢复三件套）。
- **0.9.3 式现场失败重演** → T10 真机四层验收 + TB16G7 用户机先行 + T11 回滚 playbook。
- **scrap vendor 维护成本** → 裁剪至 Windows DXGI/GDI+h wcodec 最小面，锁 rev。

## 6. 已知功能回归（用户已确认接受，后续 PATCH 序列）

1. **键盘输入**（含 TEXT/LOCK）——回归影响最直接，建议第一个 PATCH；
2. 多显示器枚举/切换；
3. SAS（Ctrl+Alt+Del）；
4. kind=screen JPEG 快照（NodeDetail 预览/CLI 受影响，T8 前审计调用面并以明确错误码兜底）；
5. QoS 采用 MVP 单一最新反馈语义，多 viewer 异质网络差异化 FEC/码率后置。

## 7. 仓库规约遵循

英文 conventional commits、代码注释中文、逻辑单元独立提交；spec.md/AGENTS.md/docs/ci-release-and-deploy.md 随合并同步改写（桌面章节：TURN→QUIC 中继、XNCCore 拉起 xnc-host、mvp/ 目录迁移完成后从工作区移除，desktop-reserch 研究区不动）。