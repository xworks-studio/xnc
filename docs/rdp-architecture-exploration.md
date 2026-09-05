# 远程桌面技术架构探索（实验记录）

> 目标：评估并实验验证 XNC 远程桌面（`xnc rdp`）技术架构的改造方向。
> 本文是**活文档**：实验一边做一边把关键发现写进来。
> 实验工具：`xnc` CLI 本身（`bin/xnc.exe`，用法见 `xnc --help`）+ 少量自制探针程序。

## 0. 基线：当前架构（v2 现状）

### 0.1 RDP 路径（`xnc rdp`）

```
mstsc ──TCP── 127.0.0.1:随机端口（CLI 本地监听）
  │
CLI ──WS(binary)── Server（会话中继，kind=tunnel）──WS── Agent
                                                    │
                                              TCP dial 127.0.0.1:3389
                                                    │
                                              Windows 原生 RDP 服务（TermService）
```

- CLI `POST /api/nodes/{id}/tunnel {target:"rdp"}` → server 白名单解析 `rdp → 127.0.0.1:3389` → agent 收 `SESSION_OPEN(kind=tunnel)` 拨会话 WS → server 双向粘合（`cli/cmd_rdp.go`、`server/internal/api/tunnel_handlers.go`、`agent/session/tunnel.go`）。
- 单连接 pump（`ln.Accept()` 一次），mstsc 重连不支持（代码注释标注为 Phase 5 细化）。
- 协议完全透传：X.224 协商、CredSSP/NLA、TLS、RDP 图形管线全部原样走 WS binary。
- spec 立场（spec.md §21/§83）：**优先复用 Windows 原生 RDP，agent 只做反向 TCP tunnel，不实现桌面协议**。

### 0.2 屏幕路径（`xnc screen`，已具备的"另一条腿"）

```
浏览器 WebCodecs ←─WS(binary H.264 NALU)─ Server 中继 ←─WS─ Agent ScreenStreamManager
                                                                    │ named pipe 帧协议
                                                            xnc-screen-helper.exe
                                                    （CreateProcessAsUserW 进用户会话）
                                                    WGC / DXGI DDA 采集 → H.264
```

- 单 helper 进程多观众共享管线（`ScreenStreamManager` 单例）；I/P 帧广播、慢观众丢帧等下个 I 帧（`agent/session/screen.go`）。
- helper 经 `WTSQueryUserToken` + `CreateProcessAsUserW(winsta0\default)` 桥接进用户会话（`agent/session/screen_windows.go`）——这是 Session 0 服务操作用户桌面的既有通路。
- DDA 路径自带光标形状合成（`agent/screen-helper/cursor_windows.go`），WGC 由系统合成。
- **只有下行视频，无输入通道、无音频、无剪贴板。**

### 0.3 现状痛点（改造动机，待实验证实/量化）

| # | 痛点 | 影响 |
|---|------|------|
| P1 | 依赖节点原生 RDP：需开 3389、系统设置允许远程、防火墙放行、NLA/凭据 | 默认装机不开 RDP 的机器直接不可用；需 admin 预配置 |
| P2 | mstsc 需要目标机 Windows 账号密码 | 运维方要有节点凭据，与 XNC token 体系割裂 |
| P3 | RDP 接管 console 会话 → 本地锁屏/会话切换 | spec §21 明示的扰动；旁观者立刻察觉 |
| P4 | tunnel 单 accept、无重连；TCP-over-WS 队头阻塞 | 断线即整段退出；弱网体验存疑 |
| P5 | 双腿割裂：screen 能无扰动看，但不能操作；rdp 能操作但高门槛 | 没有"中间形态" |

## 1. 实验环境

- 控制面：`https://xnc.app`（v0.1.0），CLI `bin/xnc.exe`。
- 节点（`xnc node list`）：LABS-TB16G7 / LABS-XIAOXIN / LABS-YOGAP7G11 在线；LABS-DEV 为本机（disabled）。
- 所有实验经 `xnc exec`（auto shell 探测，支持 `--shell powershell/pwsh/cmd/bash`）、`xnc put/get`、`xnc screen --snap` 完成。

## 2. 实验记录

（以下每节随实验进展填写）

### E0. 节点 RDP 就绪状态调查 ✅ 2026-08-22

问题：P1 是否真实——现网节点到底有多少机器"开箱即可 rdp"？

方法：`xnc exec <node> --shell powershell`，读注册表 `fDenyTSConnections`、`UserAuthentication`（NLA）、`TermService` 状态、`netstat :3389`。

结果：

| 节点 | fDenyTSConnections | NLA | TermService | :3389 监听 | `xnc rdp` 可用 |
|---|---|---|---|---|---|
| LABS-TB16G7 | 0（允许） | 1 | Running | ✅ | ✅ |
| LABS-XIAOXIN | 1（**禁用**） | 1 | **Stopped** | ❌ | ❌（agent dial 3389 失败 → ERROR{RDP_NOT_AVAILABLE}） |
| LABS-YOGAP7G11 | 1（**禁用**） | — | **Stopped** | ❌ | ❌ 同上 |

**发现：**

1. **P1 坐实：3 台在线节点仅 1/3 开箱可用**。两台禁用机器上 `xnc rdp` 必然失败，且失败发生在数据面（agent 拨 3389 超时）而非配置面——用户要到最后一刻才发现。
2. 恢复路径存在但重（需 admin exec）：`Set-ItemProperty fDenyTSConnections=0` + 启动 TermService + 防火墙规则——恰恰是 XNC 自己的 exec 能自动化的事，说明"agent 自动开通 RDP"是一个低成本演进方向（属候选架构 A）。
3. 附带观察：YOGAP7G11 实验期间短暂掉线又恢复（last seen 从 2m 到 just now），弱网韧性对任何架构都是硬指标。

### E1. tunnel 数据面探测 ✅ 2026-08-22

问题：不开 mstsc UI，直接对 tunnel 通道做 X.224 协商探针，验证数据面连通性并测量时延基线。

方法：自制探针 `experiments/rdp-probe`（独立 Go module，`GOWORK=off go build`）：解析节点 → `POST /tunnel` → 拨会话 WS → 发 19 字节 X.224 Connection Request（含 RDP_NEG_REQ）→ 读首个响应帧并计时。凭据复用 `~/.xnc/config.json`。

结果（TB16G7，RDP 已开启，4 次）：

```
[2] POST /tunnel   ~40 ms（202 + sessionId/token/websocketUrl，60s 单次有效）
[3] ws dial        164–190 ms
[4] x224 roundtrip 135–353 ms  BIN TPKT/X.224 CC selectedProto=0x08 len=19
```

XIAOXIN / YOGAP7G11（RDP 禁用）：

```
[4] x224 roundtrip ~91 / ~330 ms  TEXT ERROR RDP_NOT_AVAILABLE "tunnel target unreachable"
```

**发现：**

1. **数据面全链路验证通过**：CLI 侧 WS → server 中继 → agent → 127.0.0.1:3389 → TermService 的 X.224 Connection Confirm 原样返回。tunnel 对 RDP 协议字节完全透明（含协商头 0x08 选项位）。
2. **失败是"快速失败"而非超时**：TermService 停止时 agent 拨 3389 立即 ECONNREFUSED → ERROR{RDP_NOT_AVAILABLE} 在 ~100-330ms 内返回（`agent/session/tunnel.go` 的 10s 超时只是兜底）。失败模式干净，但**发生时机太晚**：POST /tunnel 和 WS 拨号都成功，用户端 mstsc 已经弹出后才报错。
3. **时延基线**：公网控制面（xnc.app）下，建链全程（REST+WS）约 400ms，X.224 首往返 135–353ms。这是所有"经 server 中继"方案共享的底价；RDP 图形会话建立后续是多轮往返，首次出画面预估 2–5s（未量化，属后续实验）。
4. 探针代码证明：**tunnel 通道可以被任意程序消费**（不限于 mstsc）——为"服务端/浏览器侧自研 RDP 客户端"（候选 D）保留了接口可行性。

### E2. screen 管线基线测量（进行中）

问题：现有视频腿的时延/帧率/码率基线，作为“浏览器交互桌面”方案的底座评估。

已确认的协议事实（代码 + `xnc screen --help`）：

- 流式模式默认 `15 fps / quality 60 / maxWidth 1920`，服务端允许范围为 1–30 fps、quality 1–100、maxWidth 1–1920。
- 数据面是 `SCREEN_BEGIN/SCREEN_STATE` 文本状态帧 + 带 1 字节子头的二进制帧：`0x01=H.264 key`、`0x02=H.264 delta`、`0x03=JPEG snapshot`。
- helper 是单实例共享管线：首个订阅者决定捕获参数；新订阅者会收到缓存的 SPS/PPS + 最新关键帧；慢订阅者队列满时丢帧，不反压采集管线。
- 浏览器消费端使用 WebCodecs 且配置 `optimizeForLatency: true`，但当前协议不携带采集时间戳/帧序号，因此无法从接收端单独精确分解“采集→编码→网络→解码→显示”的各段延迟。

测量方法：使用仓库自带 `tools/screendiag` 通过 XNC 控制面创建 screen 会话，连续采样每台在线节点 8 秒，记录状态、编码分辨率、总帧数、关键帧数、总字节数、首帧等待时间与接收间隔。先测静态/自然桌面基线；如果帧率明显低于目标，再用 `xnc exec` 制造可控屏幕运动做第二组对照。

验收指标（本轮用于方向判断，不是最终 SLA）：

- 首个可解码关键帧 ≤ 2 秒；
- 有持续画面变化时有效帧率接近请求值（15 fps）；
- 1920 宽、quality 60 的平均码率在公网中继可接受范围内；
- 节点间失败模式可解释，且不会因单个慢观众拖垮共享采集。

阶段结果（LABS-TB16G7，XNC CLI / agent 0.3.0）：

| 场景 | 采样 | 首帧 | 帧数 / 有效 fps | 关键帧 | 平均码率 | 观察 |
|---|---:|---:|---:|---:|---:|---|
| 静态/自然桌面 | 3 台 × 8s | 未加时标 | 2–3 帧 | 1–3 | 内容相关 | DDA/MFT 是变化驱动，不会为了凑请求 fps 重复发送静态画面 |
| 小范围光标运动 | TB16G7 × 12s | 未加时标 | 45 帧 | 23 | 未统计 | SYSTEM 令牌切到 session 1 后可移动 console 光标；光标变化多数编码成极小 P 帧 |
| 全屏高变化 HTML 压力动画 | TB16G7 × 20s | **0.536s** | 82 帧 / **5.02 fps** | **35 / 82** | **2.865 Mbps** | 短时稳定段约 75–90ms/帧（11–13 fps），但有 0.6s 与 5.42s 停顿；全屏场景切换造成关键帧风暴 |
| 局部普通 UI HTML 动画 | TB16G7 × 20s | **0.428s** | 81 帧 / **4.86 fps** | **35 / 81** | **2.955 Mbps** | 固定背景，仅移动 360×190 卡片、小光标和计数文本；仍复现关键帧风暴与 3.52s / 1.95s 停顿，排除“仅因全屏变化过大” |

编码事实与初步解释：

1. 输出为 H.264 Main Profile、Level 5.0，实际编码分辨率 1920×1200；首帧目标通过。
2. 采集并非恒定 15 fps：无变化时自然降帧是正确优化；有变化时大部分稳定间隔仍未达到 15 fps，压力场景平均约 5 fps。
3. CODECAPI_AVEncVideoForceKeyFrame 按微软定义只作用于下一输入帧并自动复位；但当前调用方在“尚未看到关键帧输出”期间会对每个输入重复设置它。MFT 的输入/输出缓冲使多个输入都被标记为 IDR，这与后续定位到的关键帧风暴根因一致。
4. 当前 MFT 输入媒体类型写死 30 fps，但 screen 请求默认 15 fps，PTS 又使用真实墙钟；这不是已证实根因，却是需要隔离验证的时基/码控不一致。
5. 当前 WS 协议没有 `frameSeq / capturedAt / encodedAt`，只能测客户端到帧的接收节奏，无法精确定位 5.42s 停顿发生在 DDA、MFT、agent 队列、server pump 还是公网链路。
6. **关键帧风暴根因已定位到 helper 的请求状态机。** `sendEncoded` 以“是否已经收到 key 输出”决定 `forceKey`；MFT 在启动/缓冲期接受多个输入但暂不输出，于是每个输入都再次设置 `CODECAPI_AVEncVideoForceKeyFrame=1`。微软定义该属性只强制紧接着的一个 `ProcessInput`，因此这些输入稍后会逐个输出为 IDR。节点 helper 日志在 10 秒内直接记录 `sent=65 key=34 delta=31`，证明风暴在进入 named pipe 之前已经形成，不是 server/WS 重复。
7. 普通 UI 动画的 5 秒资源样本：`xnc-screen-helper` 约 **21.2% 单核、732.6 MB working set**；Edge 动画约 9.4% 单核、429.4 MB；agent 本体接近 0、31.1 MB。733 MB 对常驻远程控制 helper 偏高，需要在修复关键帧风暴后复测，以区分 MFT 输出缓冲与固定分配成本。

**阶段架构结论：** `screen` 可作为交互桌面的视频底座，但应先完成一个编码修复 spike：关键帧请求按 `ProcessInput` 一次性消费、显式低延迟模式、编码 fps 与请求对齐，然后用同一 HTML 动画复测。协议同时补齐可观测性（帧序号、单调时间戳、丢帧计数）；在此之前直接加入输入通道会让交互体验问题难以归因。

### E3. 输入注入可行性（进行中）

问题：agent（SYSTEM, Session 0）能否借用 screen helper 的会话桥接模式，把鼠标/键盘事件注入用户会话（SendInput）？这是"免 RDP 浏览器交互桌面"的核心可行性。

早期证据：实验工具用 SYSTEM 主令牌复制 + `TokenSessionId=1` + `CreateProcessAsUserW(winsta0\\default)` 启动 PowerShell，`SetCursorPos` 小范围运动成功，进程退出码 0；同时 screen 流产生了对应的小 P 帧。这证明 session 0 之外的 console 桌面输入定位是可行的，但还没有验证完整 `SendInput` 键盘/鼠标序列、坐标映射、UAC 安全桌面和锁屏边界。

### E4. RustDesk 架构与数据管线研究 ✅ 2026-08-22

研究对象：C:\Users\LABS\Documents\GitHub\rustdesk-master。技术取舍优先级由 owner 明确为 RustDesk > XNC，但 XNC 不能接受 RustDesk 主仓库的 AGPLv3 衍生约束，因此本轮只提取架构经验，后续采用 clean-room 独立实现。

代码级发现：

1. **安装版的 Session 0 进程主要是监督器。** Windows Service 监测活动 console/RDP 会话，并在目标会话启动高权限 --server；会话变化时重新启动。真正接触桌面的 Host 位于正确的交互会话，而不是让 Session 0 直接捕获。
2. **便携版缩小 SYSTEM 边界。** portable-service 只承担主屏采集、光标和输入；原始像素通过共享内存回主进程，低频输入通过一次性 token 认证的 IPC 进入 SYSTEM helper。
3. **每显示器只有一个 VideoService/capturer/encoder。** 多连接订阅共享编码结果；无人订阅时服务休眠。
4. **新订阅者按快照边界加入。** RustDesk 把新订阅者暂存在 new_subscribes，在可解码边界切换或重建服务，而不是简单把旧 IDR 与当前 P 帧拼接。
5. **视频和控制使用独立发送通道。** VideoFrame/SwitchDisplay 走视频队列，其他控制消息走普通队列，防止视频拥塞拖住控制。
6. **存在真实反馈闭环。** 每帧记录目标连接，等待连接取走或客户端回 VideoReceived；VideoQoS 综合 RTT、发送延迟、解码 FPS 和客户端队列长度动态调整 FPS/质量。
7. **客户端有界队列和独立解码线程。** 队列增长会降低自适应 FPS，溢出会请求刷新或关键帧，不让延迟无限累积。
8. **输入从网络循环隔离。** 连接先检查 keyboard 权限，再投递专用输入线程；Windows 最终使用 SendInput、virtual desktop 坐标、扫描码/Unicode、modifier/lock 状态和 dwExtraInfo 标记。
9. **采集/编码有降级梯子。** DXGI 失败后可转 GDI；编码器按协商选择 H.264/H.265/VP8/VP9/AV1 和硬件/软件路径。对于有启动延迟的编码器，会继续 drain，而不是把无输出误判成需要反复 force-key。
10. **直连与 relay 只改变连接建立。** 建链完成后使用相同的高层消息和视频服务。本项目仍保留中心 relay/仅出站 443，不在第一阶段引入 P2P。

对 XNC 的直接结论：

- 当前 Session 0 Agent → 普通用户 token helper 只能覆盖普通桌面，无法自然扩展成可靠的 UAC/安全桌面控制。
- 当前 helper 同时承担采集、编码、pipe 服务且缺少 QoS；Server 又是完全通用 pump，结构上无法获得 RustDesk 的反馈式视频服务。
- 目标架构应是 Session 0 Go Agent 监督器 + 活动会话 SYSTEM Rust Desktop Host。Host 直接建立桌面会话 WSS，高码率视频不经过本地 Agent IPC。
- Browser 继续使用 WebCodecs/Canvas，并补充 frameSeq、单调时间戳、ACK、decode FPS、decodeQueueSize、页面可见性和输入通道。
- 多观众共享单编码器，但只允许一个 control lease；慢观众丢 delta 后必须等待新 IDR，不能只靠缓存旧关键帧追当前 P 帧。

正式设计已固化到 docs/superpowers/specs/2026-08-22-rustdesk-inspired-browser-desktop-design.md。

## 3. 候选架构（随实验更新结论）

> 2026-08-22 方向更新：E4 的 RustDesk 研究和 owner 分节确认已取代本节早期“继续扩展现有 helper”的推荐。最终目标为 clean-room Rust Desktop Host；下列 A–D 保留为决策演变记录。

### A. 原生 RDP tunnel 强化

- 做法：连接前预检 RDP 就绪状态，可选一键启用 TermService/防火墙；tunnel 支持重连与多 accept。
- 优点：改动最小，音频、剪贴板、键鼠、分辨率自适应由 Windows 提供。
- 硬伤：仍需要 Windows 凭据，仍可能锁定/切换 console，且 2/3 实验节点默认不可用。

### B. 浏览器交互桌面（screen + 输入）

- 做法：沿用 DDA/WGC → H.264 视频腿，在同一个 screen 会话增加浏览器→agent 的输入控制帧，由用户会话 helper 调 `SendInput`。
- 优点：免 3389、免节点账号密码、直接操作当前 console，不打断本地用户会话。
- 成本：需要自行解决编码低延迟、输入坐标、多显示器、键盘布局、剪贴板、UAC/安全桌面与控制权仲裁。

### C. 混合架构（早期推荐，已被 E4 取代）

- 默认路径：浏览器 `screen + input`，服务于“观察并轻量操作当前桌面”。
- 兼容路径：保留 `xnc rdp`，用于需要原生 RDP 能力、隔离会话、音频/设备重定向或复杂管理员操作的场景。
- 共享控制面：两条路径都复用 XNC 节点身份、RBAC、审计和独立 session WS，不再把 Windows 凭据当作 XNC 的主认证体系。
- 演进顺序：先修视频底座 → 再做输入最小闭环 → 最后评估剪贴板/多显示器；不在第一版复制完整 RDP 功能集。

### D. 服务端/浏览器 RDP 网关

- 做法：控制面或浏览器实现 RDP client，继续消费现有透明 tunnel。
- 优点：理论上可获得 RDP 完整能力并摆脱 mstsc UI。
- 硬伤：仍依赖目标机开启 RDP 和 Windows 凭据；引入 CredSSP/NLA、TLS、图形解码等巨大协议面，无法解决 P1/P2/P3，当前不建议。

### 早期推荐的数据流边界（已被 E4 取代）

```text
Browser canvas
  ├─ video: WS binary {frameSeq, capturedMono, kind, H.264 AU}
  └─ input: WS text/binary {controlLease, seq, kind, coords/key}
                 │
              Server relay（只鉴权、限流、审计，不解析视频）
                 │
              Agent screen session
                 │ named pipe（双向）
        user-session screen/input helper
          ├─ DDA/WGC → H.264
          └─ validated input → SendInput
```

输入不是“任何 viewer 都能写”：建议由 operator 显式申请短期 control lease，单节点同一时刻只允许一个控制者；viewer 继续只读。agent 对输入做序号去重、频率限制、坐标夹紧和按键释放兜底，server 记录 lease 获取/释放与高层输入类别，不记录具体文本内容。
