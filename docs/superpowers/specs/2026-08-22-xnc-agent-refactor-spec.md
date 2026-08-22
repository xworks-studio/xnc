# XNC Agent 重构规格(细化稿 v1.1)

- 日期:2026-08-22
- 状态:**已核定** — 全部开放决策点(DD-01~DD-14)于 2026-08-22 由 owner 批准,一律采纳建议
- 修订:v1.3 — owner 决策:M0 不在旧 helper 上做任何修复或加层(详见 §20 M0),M0 收窄为新架构地基
- 修订:v1.2 — 二次自审修订(UAC 采集风险条目、无活动会话 shell 语义、file kind 降权影响、多观众 PeerConnection 澄清、探活节奏消歧、SendSAS 措辞、事件命名一致性、CLI 演进、EDR 灰度提示、M1 简化 lease;§21 转决策记录)
- 修订:v1.1 — 自审修订 10 项(fps 上限、光标形状通道、pipe 握手密钥、锁屏会话校验、息屏与 headless 冲突、screen 退役衔接、修饰键机制简化、共享编码器需求取值、shell pipe ACL、M1 验收标注);命名定稿 core 家族
- 上游输入:
  - 《Windows Remote Desktop & Remote Shell Technical Specification》(初稿,本重构的目标蓝图)
  - 《XNC 浏览器远程桌面架构设计》(2026-08-22,已批准,本稿部分覆盖,见 §1.4)
  - RustDesk 源码研究(E4,clean-room,仅提取架构经验)
  - XNC v2 现有代码(agent/、screen-helper、proto/、server/)
- 语言边界:Go(网络/控制面)+ C++(Windows 特权与 GPU 热路径)
- Clean-room 规则:继承 browser-desktop 设计 §19,全文适用

---

# 1. 定位与目标

## 1.1 一句话定位

把现有单进程 SYSTEM Go agent 重构为**权限隔离、故障隔离的多进程 Host**,在不破坏 XNC「仅出站 443、中心 server 中转、合规可过审」立场的前提下,达到商业级无人值守远控 + 远程 Shell 的可用性与性能。

## 1.2 重构必须达成的目标

1. 浏览器(Chrome/Edge)免装控制当前 Windows 交互桌面:普通桌面、UAC 安全桌面、锁屏、会话切换。
2. 键鼠输入低延迟、可靠:移动不阻塞,按键不丢失、不卡键。
3. 采集→编码全 GPU 路径(D3D11 纹理不出显存直至编码输出码流)。
4. 每显示器单采集单编码,多观众共享;慢观众不影响控制者。
5. 网络拥塞丢旧帧,显示延迟不累积。
6. exec/shell 获得正确的令牌语义:用户 shell 以登录用户运行,SYSTEM shell 独立授权。
7. 无人值守:用户注销 agent 仍在线;无显示器/合盖环境可用(可选 IDD)。
8. 每个子系统可独立重启,单点崩溃不拖垮全局。
9. 公网进程(agent)不持有 SYSTEM,SYSTEM 进程(xnc-core)不接触互联网协议。

## 1.3 V1 非目标

继承初稿 §2,并明确以下补充:

- 不做 P2P 直连打洞(WebRTC 仅 relay 模式,见 §10.1;ICE host/srflx 候选默认禁用)
- 不做音频重定向、剪贴板(V1.5)、USB/打印机、RDP 协议兼容
- 不自研 UDP/QUIC 视频协议;不做 SFU 多点转发(server 侧 TURN relay 已够 V1 规模)
- 不做 Linux/macOS Host
- 不做 4K120/HDR;分辨率上限 V1 为 2560×1440@60
- 单 Windows 会话:同一时刻只服务当前活动交互会话(console 或活动 RDP)
- 不追求与旧 screen 协议的 wire 兼容(desktop 是新 session kind)

## 1.4 与既有文档的关系

| 文档 | 关系 |
| --- | --- |
| 初稿(上游蓝图) | 本稿细化其全部章节;命名从 remote-* 改为 xnc-*;其余结构保持 |
| browser-desktop 设计(2026-08-22,已批准) | **部分覆盖**。继承:desktop session kind、generation、control lease、VIDEO_ACK/关键帧合并词汇、DisplayVideoService、QoS 阈值、验收门槛、clean-room 规则。变更(已于 2026-08-22 确认,见 §21):① Host 语言 Rust→**C++**;② 数据面 WSS relay → **WebRTC(relay-only)**;③ 单一 desktop-host → **xnc-core + xnc-desktop** 两进程拆分;④ 视频路径 agent IPC 中转(agent 做 RTP)→ 保留,因 Pion 在 agent。 |
| spec.md 主规格 | 控制协议(§11)、exec(§14)、自更新(§45)、CLI(§57)继续有效;§21-26(RDP tunnel)保留为兼容路径;§64(只读 screen)被本稿覆盖为「legacy 预览,M2 退役」。 |
| rdp-architecture-exploration.md | 实验记录保留;候选 D(服务端 RDP 客户端)维持不采用。 |

## 1.5 合规红线(不可协商)

1. 节点侧所有进程:仅出站长连接,无任何 TCP/UDP 监听端口。
2. WebRTC ICE 策略 V1 强制 `relay`,且 TURN over TLS/TCP 443(节点与浏览器两侧都只出站 443)。P2P 直连作为部署级策略开关保留,V1 默认关闭。
3. 凭据不出现在命令行、环境变量或普通用户可读文件。

---

# 2. 设计原则

1. **控制面与数据面分离**:Go 负责网络、WebRTC、信令、会话路由、遥测;C++ 负责 Windows 特权操作、Session/Token/Desktop、DXGI/D3D11/GDI、SendInput、硬件编码、ConPTY。
2. **实时图像不跨 CGO**:DXGI 纹理→编码→H.264 码流全在 C++(xnc-desktop)内完成;Go 只见压缩码流。禁止 GPU→CPU RGB→CGO→GPU 路径。
3. **权限最小化**:公网输入不得直接进入 SYSTEM 进程;agent 低权运行,xnc-core(SYSTEM 控制面)只暴露 capability 型 RPC,不暴露万能 SYSTEM API。
4. **一切可重启**:agent、xnc-core、xnc-desktop、xnc-shell 相互监督、独立重启;视频子系统内部任何变更(编码器/分辨率/桌面切换)走统一的 CaptureReset 重建流,不做运行中热修补。
5. **反馈闭环**:视频有 ACK/丢帧/关键帧请求/解码反馈;QoS 由真实网络与解码指标驱动,不做盲目定频。
6. **延迟不积累**:拥塞时丢旧帧编码最新帧;静止桌面视频自然降到 0 fps。
7. **状态机正交**:Windows Session、Desktop、Display、Capture Backend、Encoder 各自独立状态机,综合决策,不拿单一 HRESULT 判断全局。
8. **稳定错误码**:所有对客户端可见的错误都有 stableCode + recoverable + retryAfter。
9. **Clean-room**:不复制/链接/依赖 RustDesk(AGPLv3)任何代码;依据为 Windows 官方文档与本规格;第三方依赖许可证白名单 MIT/Apache-2.0/BSD/MS-PL(protobuf、coturn 等)。

---

# 3. 现状基线与资产去向

| 现有资产 | 位置 | 去向 |
| --- | --- | --- |
| WSS 控制连接(CHALLENGE/HELLO/心跳/退避) | agent/connect/ | **保留**,agent 继续持有;扩展 desktop 信令与 TURN 凭据下发 |
| 会话引擎 + exec/shell/file/tunnel/screen kind | agent/session/ | **保留框架**;exec/shell 后端迁往 xnc-shell(§12),screen 于 M2 退役 |
| ConPTY 自研封装(纯 syscall) | agent/session/conpty_windows.go | **移植**为 xnc-shell 核心(若 xnc-shell 选 Go,近零成本) |
| 多 shell exec 引擎(bash/pwsh/powershell/cmd 探测与参数构造) | agent/session/exec.go、machineinfo/ | **移植**入 xnc-shell |
| screen-helper(DDA/WGC + MF 软编 + pipe) | agent/screen-helper/ | **M2 退役**;dda.c 的 DXGI/COM 经验与 MF 编码逻辑移植入 xnc-desktop;`xnc screen --snap` 快照能力改经 `xnc-desktop --jpeg-single`(§20 M2) |
| xnc-dda.dll(C 实现 DXGI) | agent/screen-helper/dda/ | **吸收**:xnc-desktop 内直接编译(不再独立 DLL);「Go syscall vtable 在 DuplicateOutput 上系统性失败」的教训是选择 C++ 的直接证据 |
| ScreenStreamManager(单例共享/丢帧/I 帧缓存) | agent/session/screen.go | **思想继承**:订阅者模型移入 xnc-desktop DisplayVideoService;实现重写 |
| ScreenPreview(WebCodecs 播放) | web/src/pages/ | **演进**:新建 desktop 页(RTCPeerConnection + WebCodecs),ScreenPreview 保留至 screen 退役 |
| 自更新(bundle/manifest/staging/apply 舞/rollback) | agent/updater/ | **移交接**:agent 负责下载校验,文件落位改由 xnc-core ApplyUpdate 执行(§6.8) |
| 身份/注册(Ed25519 + DPAPI) | agent/identity/、enroll/ | **保留**在 agent;state 目录 ACL 调整为服务 SID |
| RDP tunnel | agent/session/tunnel.go | **保留**为兼容路径(`xnc rdp --native`) |
| 统一 session manager + pump(server) | server/internal/session/ | **保留**;新增 desktop kind 与 TURN 凭据签发,server 仍不解析媒体 |
| E2 已定位的关键帧风暴(MFT force-key 重复设置) | screen-helper | **教训固化**:force-next-keyframe 一次性消费语义写入编码器接口契约(§7.10) |

**现有缺口**(本重构要解决的):无输入、无 UAC/锁屏覆盖、无多显示器、无 GPU 硬编、exec 一律 SYSTEM、helper 普通用户令牌无法上安全桌面、无 QoS 反馈、单点进程无监督。

---

# 4. 进程模型与信任边界

## 4.1 总览

```text
                         Browser (Chrome/Edge, WebCodecs + RTCPeerConnection)
                              │  WSS(信令/会话) + DTLS/SRTP over TURN/TLS 443
                              ▼
                       xnc-server (Go, 公网)
                       REST + 会话中继 + TURN(coturn) + 签发 SessionTicket
                              │  WSS 443 出站(现状不变)
┌─────────────────────────────▼──────────────────────────────────────────┐
│ xnc-agent.exe — Go — NT SERVICE\XNCAgent(低权)— Windows 服务 XNCAgent │
│  控制连接 / 信令 / WebRTC(Pion, relay-only) / RTP 打包 / 输入转发      │
│  exec·shell·file·tunnel 会话(agent 直连 server,不经 xnc-core)       │
│  自更新下载与校验 / 遥测 / 监督请求发起                                  │
└──────────┬─────────────────────────────────────────────────────────────┘
           │ Named Pipe xnc-core(protobuf,双向认证)
┌──────────▼─────────────────────────────────────────────────────────────┐
│ xnc-core.exe — C++ — LocalSystem — Session 0 — Windows 服务 XNCCore    │
│  (无任何网络栈,无监听端口)                                             │
│  WTS 会话监控 / TokenManager / 进程监督·重启 / DisplaySupervisor        │
│  IDD 管理 / PowerManager / SendSAS / 更新落盘执行者                     │
└──────┬──────────────────────────────┬───────────────────────────────────┘
       │ spawn + 监督 pipe            │ spawn + 监督 pipe
┌──────▼──────────────────┐   ┌───────▼──────────────────────────────┐
│ xnc-desktop.exe         │   │ xnc-shell.exe                        │
│ C++                     │   │ Go                                   │
│ SYSTEM token @ 会话 N   │   │ 用户 token(user shell)              │
│ Desktop/Display/DXGI/   │   │ SYSTEM token(system shell)          │
│ GDI/光标/SendInput/     │   │ ConPTY + 多 shell exec 引擎          │
│ Encoder(GPU 热路径)   │   └──────────────────────────────────────┘
│ 多观众/单控制者          │
└──────┬──────────────────┘
       │ Named Pipe xnc-desktop-<pid>(压缩码流 + 输入 + 控制,直连 agent)
       ▼
  xnc-agent(RTP/DataChannel)──→ 网络
```

## 4.2 进程特权表

| 进程 | 语言 | 账户 | 会话 | 网络 | 生命周期 |
| --- | --- | --- | --- | --- | --- |
| xnc-agent.exe | Go | `NT SERVICE\XNCAgent`(虚拟账户,低权) | 0 | 仅出站 443(WSS + turns/TCP) | 常驻服务 |
| xnc-core.exe | C++ | LocalSystem | 0 | **无** | 常驻服务 |
| xnc-desktop.exe | C++ | SYSTEM 主令牌 + `TokenSessionId=N`(目标交互会话) | N | 无(经 pipe) | 会话级:活动会话存在即运行,会话结束退出 |
| xnc-shell.exe | Go | 用户令牌(user shell)/ SYSTEM(system shell) | N | 无(经 pipe) | 会话级,随 shell 会话 |

TokenManager 规则(xnc-core 内,细化初稿 §6):

- **xnc-desktop 令牌**:`WTSQueryUserToken(activeSession)` 取用户令牌仅为探测;实际用 **SYSTEM 主令牌复制 + `SetTokenInformation(TokenSessionId=N)`**,使其同时具备 SYSTEM 权限与目标会话桌面访问能力(可 OpenInputDesktop 上 UAC/锁屏桌面)。该模式已被 XNC E3 实验与 RustDesk 安装版行为双重验证。
- **user shell 令牌**:`WTSQueryUserToken(N)` 的用户令牌(需 `SE_ASSIGNPRIMARYTOKEN_NAME`,xnc-core 服务持有)。
- **system shell 令牌**:SYSTEM 主令牌 + SessionId=N(在用户会话内以 SYSTEM 运行,与 xnc-desktop 同法)。
- 所有 spawn 使用 `CreateProcessAsUser` + `lpDesktop = winsta0\default`;命令行不含敏感字段(会话票据与 pipe_secret 经 pipe 传递)。

## 4.3 安装布局与服务

```text
C:\Program Files\XNC\
  xnc-agent.exe            (服务 XNCAgent,Automatic,NT SERVICE\XNCAgent)
  xnc-core.exe             (服务 XNCCore,Automatic,LocalSystem;依赖 XNCAgent 先起)
  xnc-desktop.exe
  xnc-shell.exe
  (可选包) xnc-idd-driver\  (驱动 + WDF,单独安装,见 §13)

C:\ProgramData\XNCAgent\
  identity\     (Ed25519 私钥,DPAPI,ACL: 服务 SID + SYSTEM)
  updates\      (下载/staging,校验后才请求 xnc-core 落位)
  logs\
```

- 服务启动顺序:SCM 依赖 `XNCAgent` → `XNCCore`。xnc-core 起来后 agent 通过 pipe attach。
- xnc-desktop / xnc-shell 不是服务,由 xnc-core 按需 spawn,作为 Job 对象成员(xnc-core 退出时 workers 一并终止,防止孤儿 SYSTEM 进程)。

## 4.4 数据面路径一览

| 流 | 路径 | 格式 |
| --- | --- | --- |
| 视频下行 | xnc-desktop(GPU 编码)→ pipe → agent → Pion RTP → TURN → server → browser | H.264 Annex-B AU → RTP(packetization-mode=1) |
| 光标下行 | xnc-desktop → pipe → agent → DataChannel(位置走 `cursor`,形状走 `control` binary 子帧) | protobuf(CURSOR_SHAPE / CURSOR_POSITION) |
| 控制下行/上行 | browser ↔ agent DataChannel `control` | text=JSON 控制词汇,binary=protobuf(§10.5/§10.6) |
| 键鼠上行 | browser → agent DataChannel(`input` + `mouse`)→ pipe → xnc-desktop InputManager | protobuf(§11.2) |
| 终端/exec | browser → server WSS(kind=shell/exec)→ agent → pipe → xnc-shell → ConPTY | 现有协议不变 |
| 文件 | 现有 kind=file,不变 | 现有协议不变 |

> 说明:exec/shell/file 保持在现有 WSS 会话通道上(经 server 中继),**不**迁到 WebRTC DataChannel(DD-06)。终端对延迟不敏感,且复用现有 server 会话治理(RBAC、idle 治理、审计)零成本。

---

# 5. xnc-agent.exe 规格

## 5.1 职责

1. 设备身份、注册、控制连接(现状保留,含心跳/退避重连)。
2. 会话路由:SESSION_OPEN 按 kind 分发;desktop kind 走新路径,其余走现有 handler。
3. **WebRTC 引擎**(Pion):
   - `iceTransportPolicy: relay`,仅 TURN turns/TCP 443 候选;
   - 视频轨:接收 xnc-desktop 的 Annex-B AU → H264RTPPay(packetization-mode=1);
   - DataChannels:control / input / mouse / cursor(§10.5);
   - RTCP:PLI/FIR → 转发 KeyframeRequest 至 xnc-desktop;TWCC 带宽估计 → 驱动 QoS;
4. SessionTicket 验证(服务端签名,pinned server public key),并把 capability 集合连同票据转交 xnc-core/xnc-desktop 二次验证。
5. 输入转发与本地预校验:lease 校验、seq 单调、坐标夹紧、频控(§11.7)。
6. exec/shell/file/tunnel 会话面(现有);shell/exec 后端改经 xnc-core spawn xnc-shell(§12)。
7. 自更新:下载、整包 sha256、manifest 校验;落位交 xnc-core(§6.8)。
8. 遥测与结构化日志上报。

## 5.2 Must-Not

- 不加载 D3D/DXGI/任何图形栈;不接触桌面 API;
- 不直接 spawn SYSTEM 进程、不直接管理 token;
- 不接受任意可执行路径参与特权操作(profiles 由 xnc-core 决定,§8.2);
- 不解析视频内容(只透传/打包码流)。

## 5.3 账户迁移(现有装机)

- 服务重配置:`LocalSystem → NT SERVICE\XNCAgent`(sc sidtype unrestricted;授予 SeChangeNotifyPrivilege 默认集)。
- `%ProgramData%\XNCAgent` ACL 重写:服务 SID(FULL)+ SYSTEM(FULL),移除 Users。
- DPAPI 使用 `CRYPTPROTECT_LOCAL_MACHINE`(机器级,虚拟账户可解密,保持现状)。
- 升级安装器在一次 bundle apply 中完成重配置(原子,失败回滚)。
- **时点约束**:agent 低权化必须与 screen kind 退役同批(否则 legacy screen 的 helper spawn 路径破坏,见 §20 M2)。

---

# 6. xnc-core.exe 规格

## 6.1 职责

WTS 会话枚举与监控、console/活动会话检测、TokenManager(§4.2)、xnc-desktop / xnc-shell 的 spawn 与监督、DisplaySupervisor(拓扑,§13)、IDD 管理、PowerManager(SetThreadExecutionState / 重启关机策略)、SendSAS、更新落位执行者、本地 capability 规则执行。

## 6.2 Must-Not(攻击面,细化初稿 §45)

进程内禁止:HTTP/WebSocket/WebRTC/DTLS/SRTP 解析器、JSON 大 API、插件系统、任意命令/任意 DLL/任意注册表写入接口。xnc-core 是 small / strict / auditable / capability-oriented 的 RPC 端点,唯一入站面是一条 ACL 严格 + 双向认证的 named pipe。

## 6.3 Core RPC 目录

传输与帧格式见 §9。所有 RPC 均 request/response(带 request_id),另有单向事件流 `SubscribeEvents`。

```protobuf
// core.proto(节选,完整定义在仓库 proto/ipc/)
service Core {
  // ---- 查询 ----
  rpc GetHostState(GetHostStateRequest) returns (HostStateResponse);
  //    HostState: windows_sessions[], active_session_id, desktop_state,
  //    displays[](id,origin,w,h,primary,scale,rotation,virtual),
  //    capture{pid,backend,health,encoder,generation},
  //    core_version, agent_min_version
  rpc GetSessions(Empty) returns (SessionList);          // WTS 会话枚举
  rpc GetDisplays(Empty) returns (DisplayList);          // = HostState.displays

  // ---- 桌面 ----
  rpc StartCapture(StartCaptureRequest) returns (StartCaptureResponse);
  //    req: session_id(逻辑会话), wts_session_id(xnc-core 校验,见 §6.4),
  //         ticket(SessionTicket), video_config(codec,fps,max_w,max_h)
  //    resp: host_pid, pipe_name, pipe_secret, generation, displays
  rpc StopCapture(StopCaptureRequest) returns (Ok);      // 按 wts_session_id
  rpc SubscribeEvents(SubscribeEventsRequest) returns (stream CoreEvent);

  // ---- Shell ----
  rpc CreateShell(CreateShellRequest) returns (CreateShellResponse);
  //    req: session_id, token_kind(USER|SYSTEM), profile(CMD|POWERSHELL|PWSH|BASH),
  //         mode(INTERACTIVE|ONESHOT), params{cols,rows,cwd,env[],command,timeout_sec},
  //         ticket(必须含 shell.user 或 shell.system capability)
  //    resp: host_pid, pipe_name, pipe_secret
  rpc KillShell(KillShellRequest) returns (Ok);          // 按 host_pid/session_id

  // ---- 虚拟显示 ----
  rpc CreateVirtualDisplay(CreateVirtualDisplayRequest) returns (DisplayInfo);
  rpc RemoveVirtualDisplay(RemoveVirtualDisplayRequest) returns (Ok);

  // ---- 电源 ----
  rpc SetKeepAwake(BoolValue) returns (Ok);              // 见 §13.4
  rpc Reboot(RebootRequest) returns (Ok);                // capability: power.reboot
  rpc Shutdown(ShutdownRequest) returns (Ok);

  // ---- 特权杂项 ----
  rpc SendSas(TicketRequest) returns (Ok);               // capability: input.secure_attention

  // ---- 维护 ----
  rpc ApplyUpdate(ApplyUpdateRequest) returns (Ok);      // §6.8
  rpc RestartWorker(RestartWorkerRequest) returns (Ok);  // capability: agent.maintain
}
```

`CoreEvent`(agent 订阅,驱动 agent 对 server 上报与重连逻辑):

```text
WtsSessionChanged{event(WTS_CONSOLE_CONNECT/DISCONNECT/LOGON/LOGOFF...), session_id}
ActiveSessionChanged{old, new}            → agent 触发 xnc-desktop drain & respawn
DisplayTopologyChanged{displays}
DesktopHostExited{pid, exit_code, reason} // 监督事件(xnc-desktop)
ShellHostExited{pid, session_id, exit_code}
PowerEvent{suspend_resumed, ...}
UpdateProgress{phase}
```

## 6.4 参数二次验证(不信任 agent 传入)

| 参数 | 验证 |
| --- | --- |
| wts_session_id | 必须等于 `WTSGetActiveConsoleSessionId()`(**锁屏/WTSConnected 状态的 console 会话同样合法**——否则 Win+L 后无法继续控制)或为 WTSActive 的 RDP 会话;其余拒绝 |
| display_id | 必须 ∈ DisplaySupervisor 当前拓扑 |
| profile | 枚举白名单,xnc-core 自行解析为固定可执行路径(§8.2);**绝不接受路径参数** |
| ticket | xnc-core 用 pinned server public key 独立验签 + 过期 + nonce 一次性 |
| token_kind=SYSTEM | 要求 ticket capability 含 shell.system / capture 隐含 desktop 权限 |

## 6.5 WTS 会话监控

- `WTSRegisterSessionNotificationEx` + 500ms 轮询兜底(防漏通知)。
- 活动会话变化(console 切换、RDP 接入、注销、登录):发 `ActiveSessionChanged`;若 xnc-desktop 存在 → 对旧 xnc-desktop 发 `Drain`(停采集、释放按键、退出),在新会话 spawn 新 xnc-desktop。user-token 的 xnc-shell 实例随其所属会话自然终止,监督循环回收。
- 会话状态机:`BOOT → NO_USER → LOGON → ACTIVE → LOCKED → LOGOFF`(初稿 §38)。LOCKED 不销毁 xnc-desktop(要能看见/解锁锁屏),LOGOFF 才销毁该会话 workers。

## 6.6 进程监督

- 所有 workers 进 Job 对象 + `RegisterWaitForSingleObject` 退出回调。
- 退出即记 crash 记录,按 §15.2 的退避策略重启;重启前先读 workers 的 last-will 诊断(§16.1)。
- xnc-core 自身被 SCM 管理;agent 检测 core pipe 断开 → 重试 attach(指数退避 1s→30s),期间 desktop 会话拒绝新开并上报 server。

## 6.7 与 agent 的权限协作

xnc-core 不主动连接网络;一切由 agent 发起 RPC。xnc-core 对 agent 的信任仅限于「pipe 对端是 XNCAgent 服务 SID 且通过双向认证」,**不**信任其业务参数(§6.4)。

## 6.8 更新落位执行者

agent 下载并校验 bundle(sha256 + manifest)到 staging 后,调 `ApplyUpdate{version, staging_dir, manifest_sha256}`:

1. xnc-core 复核 manifest 签名与必需文件集(4 个 exe);
2. 通知 agent 进入 draining(会话按 §15.4 收尾);
3. spawn `xnc-core.exe --apply-update <staging> <parent_pid>`(独立进程执行 rename 换文件 + 服务重启,复用现有 apply 舞语义:`.old` 单代保留、connected 标记验证、超时回滚);
4. 新版本 xnc-core 起来后向 agent 发 `UpdateProgress{APPLIED|ROLLED_BACK}`。

> 为什么不让 agent 自己换文件:agent 低权写不了 Program Files;xnc-core 自更新则复用同一机制(apply 子进程模式),单一代码路径。

---

# 7. xnc-desktop.exe 规格

## 7.1 内部架构

```text
xnc-desktop.exe
├── IpcServer            (对 agent:控制 + 码流 + 输入;§9)
├── SessionContext       (票据、capabilities、generation、租约状态)
├── DesktopSupervisor    (input desktop 绑定与切换,§7.3)
├── DisplaySupervisor    (枚举/拓扑/分辨率/旋转/DPI;与 xnc-core 侧信息对齐,§13)
├── CaptureManager
│   ├── DxgiBackend      (DuplicateOutput1 → DuplicateOutput,§7.4)
│   ├── GdiBackend       (BitBlt 回退,§7.6)
│   └── WgcBackend       (仅单窗口共享与诊断,不承担无人值守,§7.7)
├── FrameCache           (LatestFullFrame + 帧状态机,§7.5)
├── CursorManager        (形状缓存 + 位置流,§7.8)
├── InputManager         (SendInput 注入 + 键鼠状态,§11)
├── DisplayVideoService ×N (每显示器:采集→转换→编码→订阅分发,§7.9)
│   └── EncoderManager
│       ├── MfD3d11Encoder   (设备 MFT:NVENC/QSV/AMF 由驱动提供)
│       ├── MfSoftwareEncoder(CMSH264EncoderMFT)
│       └── JpegFallback     (最终保底)
└── Supervisor           (自监控:watchdog 线程 + last-will 诊断文件)
```

## 7.2 线程模型

| 线程 | 职责 |
| --- | --- |
| IPC | pipe 读写,消息解码/编码 |
| Capture(每显示器 1) | AcquireNextFrame 循环,超时=帧间隔(静止桌面自然 0 采集) |
| Encode(每显示器 1) | GPU 转换 + 编码调用;输出 AU 入分发队列 |
| Input | 输入队列消费(有界 512),SendInput 注入;与网络/采集线程完全隔离 |
| Cursor | 光标轮询(8ms 高频仅在有观众时;GetCursorInfo/形状变更检测) |
| DesktopWatch | OpenInputDesktop 探测(500ms),检测 UAC/锁屏桌面切换 |
| Watchdog | 心跳;卡死(采集线程 >5s 无进展)时主动自杀重启 |

## 7.3 DesktopSupervisor(初稿 §9 细化)

- 绑定:启动时 `OpenInputDesktop()`(失败退 `OpenDesktop("default")`)→ `SetThreadDesktop()` 到采集与输入线程。
- DesktopWatch 每 500ms 探测 `OpenInputDesktop` 的桌面名:
  - `Default` ↔ `Winlogon`(UAC/锁屏)切换 = Desktop 事件 → 触发 CaptureReset(§7.5);
  - 状态机:`DEFAULT ⇄ WINLOGON`,中间态 `TRANSITION`(≤2s,期间容许黑帧,上报 SESSION_STATE)。
- 输入注入前同线程必须已 `SetThreadDesktop(当前 input desktop)`,SendInput 失败(错误桌面)→ 重绑 → 重试一次(初稿 §16)。
- **禁止假设一个 IDXGIOutputDuplication 能跨桌面存活**;ACCESS_LOST 与桌面切换走同一恢复路径。
- **已知风险与验证点**:DDA 在 Winlogon 安全桌面上的输出依赖驱动行为(部分机型黑帧),M2 首个里程碑即在真机验证 UAC/锁屏采集;若 DDA 与 GDI 均不可用,V1 降级为「UAC 期间画面冻结于最后一帧 + SESSION_STATE 提示」(与 2026-08 revert 记录的现状一致),不阻塞其余交付。

## 7.4 DXGI 采集管线(主路径)

```text
D3D11CreateDevice(adapter=输出所在适配器, VIDEO_SUPPORT | BGRA_SUPPORT)
  → IDXGIOutput5::DuplicateOutput1(B8G8R8A8)      // 失败降 DuplicateOutput()
  → AcquireNextFrame(timeout=帧间隔)
     ├── AccquiredBufferInfo.LastPresentTime==0 / WAIT_TIMEOUT → 无变化,不动
     ├── 首帧/重建后第一有效帧 → CopyResource 到 persistent texture(全量)
     └── 增量帧 → 应用 MoveRects + DirtyRects 到 persistent texture
  → persistent texture(始终持有最新全量帧 = LatestFullFrame GPU 版)
  → VideoProcessorBlt(旋转归一 + 缩放)→ NV12 texture
  → 硬件编码器输入(纹理直入,零 CPU 拷贝)
```

规则:

- `DuplicateOutput1` 优先(可指定格式,减少驱动转换);失败降级 `DuplicateOutput` 后仍以 BGRA 处理。
- 适配器选择:与目标输出绑定的适配器(避免跨适配器拷贝);编码器同适配器(§7.10)。
- ReleaseFrame 必须在处理完(或 CopyResource 之后)立即归还,不等编码完成(persistent texture 是拷贝目标)。
- ACCESS_LOST / DEVICE_REMOVED / DEVICE_HUNG → CaptureReset(§7.5)。
- 无变化帧(WAIT_TIMEOUT):**不编码、不发包**(静止桌面带宽≈0)。采集节奏:有订阅者时按 spf 轮询;HIBERNATE(无订阅)降为 1s 周期探活采集,为「随时可加入的观众」保持 LatestFullFrame 新鲜。
- 重复编码保活:仅当编码器有启动/内部延迟(软编冷启动,CMSH264EncoderMFT 实测 ~17 帧前瞻窗口)时,对同一帧重复送编**直至首个关键帧输出**(warm-up),上限 = 2× 窗口帧数或 2s 取小;期间绝不重发 force-key(ADR-017),warm-up 计数入日志。

## 7.5 FrameCache 与统一 CaptureReset(初稿 §12/§13/§32 合并)

帧状态机(每显示器):

```text
INIT → WAIT_BASE_FRAME → HAVE_BASE → INCREMENTAL
```

- 进入 HAVE_BASE 前只接受全量帧(CopyResource),**禁止只应用 dirty rect**;
- 新观众加入:取 LatestFullFrame → 请求 IDR → 从该 IDR 入流(等价 browser-desktop 设计的 needsKeyframe 合并语义);
- IDR 请求合并:同显示器多请求合并为一次;主动 IDR 请求最小间隔 500ms;周期性恢复 IDR 每 5s 最多 1 个;编码器重建/配置变化/generation 变化不受此限,但必须记录 reason。

**CaptureReset()**(一个函数处理所有重建,以下事件全部路由到它:桌面切换、UAC、锁/解锁、分辨率变化、显示器插拔、IDD 创建、GPU reset、ACCESS_LOST、D3D 设备移除、编码器降级重建、显示服务配置变更):

```text
Stop Acquisition → Release Duplication → Invalidate FrameCache
  → Invalidate Encoder References(Flush/重建)
  → Rebind Desktop(OpenInputDesktop/SetThreadDesktop)
  → Re-enumerate Outputs → Rebuild D3D Device(必要时)
  → Recreate Duplication → Acquire Base Frame(全量)
  → Force IDR → generation++ → Resume
```

复位期间:向观众发 `SESSION_STATE{recovering}`;复位超过 3s 未取得 base frame → 按 §15 降级(GDI 或 failed)。每次 CaptureReset 递增 capture generation 并记录原因(观测)。

## 7.6 GDI 回退(初稿 §30/§31)

- 触发:DXGI 健康分跌破阈值或 crash-loop 判定(§15.2/§15.3)。
- 路径:`GetDC(NULL) → BitBlt(SRCCOPY) → DIB → CPU 缩放/NV12 → 软件编码`,上限 15fps;**可操作的低帧率 > 黑屏**。
- 光标:GDI 模式下仍走独立光标通道(GetCursorInfo 可用),视频不烧录光标。
- 后台 30s 周期 DXGI probe,成功 → 恢复 DXGI → CaptureReset → Force IDR。
- 健康分:初始 100;WAIT_TIMEOUT 0;ACCESS_LOST −10(可自愈不扣);Create 失败 −40;GPU removed −50;连续无有效帧 −10/次;<60 触发 GDI(阈值可配置)。

## 7.7 WGC 后端

仅两个用途:① 单窗口共享(V2 feature,DataChannel 上行窗口列表选择);② 诊断工具。不作为无人值守桌面采集路径(ADR-006)。

## 7.8 CursorManager(初稿 §15)

- 形状:`GetFramePointerShape`(DXGI 路径)/`GetCursorInfo`(GDI/轮询路径)→ 转标准 RGBA + hotspot;形状缓存 by shapeId,仅变化时发 `CURSOR_SHAPE`。
- 位置:Cursor 线程 8ms 轮询(有观众时)→ 位置变化才发 `CURSOR_POSITION{displayId,x,y,visible}`,走 `cursor` 不可靠通道(旧位置自然被新位置覆盖)。
- 浏览器本地叠加渲染;视频永不烧录光标。
- 采集重启期间位置流不断(独立于视频管线),保证操作感知连续。

## 7.9 DisplayVideoService(共享视频服务)

每显示器一个服务,RustDesk 式订阅模型:

```text
Capturer → Converter/Scaler(GPU) → Encoder → Encoded AU
                                        ├→ Subscriber A(control lease)
                                        ├→ Subscriber B(view-only)
                                        └→ Subscriber C
```

- 订阅者集合 + newSubscribers(快照边界合入,不拼接旧 IDR 与新 P 帧);
- 无人订阅:进入 HIBERNATE——停止满频采集,保留 1s 探活(§7.4);最后一个订阅者离开即停编码;
- 每订阅者发送队列深度 3 帧(压缩后 AU):满 → 丢 delta → needsKeyframe → 合并触发新 IDR(继承 browser-desktop 设计 §8);
- 控制消息队列(256)与视频队列分离;控制队列持续拥塞 → 断开该订阅者 + 稳定错误码;
- **编码器按「活跃且可见观众的最高需求」运行**(分辨率取观众上限与配置上限的较小值,fps 取最高请求);慢观众通过**每订阅者分发采样(跳帧)**消化,不得拖低控制者体验(与 §10.8 一致)。

## 7.10 EncoderManager(初稿 §18/§19/§21 细化)

接口契约(C++):

```cpp
class IVideoEncoder {
public:
  virtual bool Init(const EncoderConfig&) = 0;
  // 输入 D3D11 纹理(GPU 路径)或 CPU NV12(GDI/软路径)
  virtual bool Encode(ID3D11Texture2D* tex, uint64_t capture_mono_us) = 0;
  virtual bool Encode(const uint8_t* nv12, size_t len, uint64_t capture_mono_us) = 0;
  virtual bool ForceNextIdr(const char* reason) = 0;   // 一次性:仅作用于下一次 Encode 输入
  virtual bool Reconfigure(const EncoderConfig&) = 0;   // 码率/fps 热调;失败返回 false 触发重建
  virtual void Flush() = 0;                             // 语义:drain 输出,不重复 force-key
  virtual std::vector<EncodedAU> TakeOutput() = 0;
  virtual EncoderDiagnostics Diagnostics() = 0;
};
```

**编码梯子(V1 实际落地)**:

```text
1. MfD3d11Encoder:适配器设备 MFT(枚举 MFT_ENUM_FLAG_HARDWARE,
   驱动自动映射 NVENC/QSV/AMF;D3D11 纹理直入)
2. MfSoftwareEncoder:CMSH264EncoderMFT(现有 screen-helper 逻辑移植)
3. JpegFallback:JPEG 帧流(最终保底,复用现有实现)
```

> 对初稿 §18 的细化:V1 不直接写 NVENC/QSV/AMF 原生 SDK 绑定——Windows 设备 MFT 已由驱动提供硬件编码器,统一 MF 接口即可拿到三家的 GPU 编码,复杂度和许可证面最小。原生 SDK 绑定留 V3 性能优化(DD-07)。

选择规则:按采集输出所在适配器枚举设备 MFT → 试初始化(编码 1 帧自检)→ 成功即用;失败逐级下探。跨适配器拷贝仅在「渲染 GPU ≠ 采集 GPU」且硬编不可用时发生,记入诊断。

**低延迟配置(V1 全档)**:B 帧=0;低延迟模式;小 VBV;无 lookahead;按需 IDR;frame dropping 允许。**拥塞时丢旧帧编码最新帧**(帧在编码前排队深度上限 1)。

**force-key 一次性语义(硬契约,防 E2 关键帧风暴)**:`ForceNextIdr` 置位后恰好作用于下一次 `Encode` 输入即清除;编码器无输出(内部缓冲)期间**禁止**重复置位;冷启动期用 `Flush/drain` 语义等待,不得以「尚未见到关键帧输出」为由重发 force-key。

码流整形(继承现状):IDR 前带 SPS/PPS、统一 4 字节 Annex-B 起始码、丢弃 AUD。

---

# 8. xnc-shell.exe 规格

## 8.1 定位

统一承载 exec(一次性命令)与 shell(交互终端),在**正确的令牌**下运行——修复现状「exec 一律 SYSTEM」的问题。两种 mode 共用 profiles 与 ConPTY 底座。

## 8.2 Profiles(xnc-core 白名单,xnc-core 自行解析路径)

| Profile | 可执行文件解析(xnc-core 侧) | 说明 |
| --- | --- | --- |
| POWERSHELL | `%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe` | 默认交互 shell |
| PWSH | `%ProgramFiles%\PowerShell\7\pwsh.exe`(探测,缺失则拒绝) | |
| CMD | `%SystemRoot%\System32\cmd.exe` | |
| BASH | 注册表/已知路径探测 Git Bash `bash.exe` | exec 用;交互 V1 可选 |

- agent/客户端只传 profile 枚举名;**任何可执行路径参数一律拒绝**(初稿 §36)。
- 交互模式注入一次性 prompt 函数重定义(`[主机名]` 前缀),不解析/不改写 VT 流(继承现状 §11)。

## 8.3 ConPTY 与线程

- 移植现有 `conpty_windows.go`(DD-03 已定:Go);两条硬不变量保留:lpValue 传 HPCON 句柄值、STARTF_USESTDHANDLES 必置。
- 至少双线程:stdin 写线程 / stdout 读线程,禁止单线程同步 Read/Write(初稿 §37);支持 `ResizePseudoConsole`。
- 输出 UTF-8 + VT 原样透传;working set 目标 < 60MB。

## 8.4 令牌语义

| mode | token | 授权 |
| --- | --- | --- |
| user shell / user exec | `WTSQueryUserToken(活动会话)` 用户令牌 | capability `shell.user`(默认) |
| system shell / system exec | SYSTEM + SessionId=N | capability `shell.system`(显式授予,默认拒绝) |

user shell 目标效果 `C:\Users\Alice>`;system shell 不得由 user shell 会话内自动提权获得(需新的、经授权的 CreateShell 调用)。

**无活动用户会话时**(登录界面/无人登录),`WTSQueryUserToken` 不可得:CreateShell(USER) 返回稳定错误码 `NO_ACTIVE_SESSION` 并提示改用 system,**不做隐式提权回退**。

## 8.5 数据协议(agent ↔ xnc-shell pipe)

继承现有 shell kind 词汇:SHELL_BEGIN / SHELL_RESIZE(text)、pty 裸 binary 双向流;exec 附加 EXEC_RESULT(退出码)。kill:pipe 关闭或 SESSION_CLOSE → 杀进程树(现有 taskkill /T /F 语义);xnc-core KillShell 兜底。

---

# 9. 本地 IPC 规格(初稿 §7 细化)

## 9.1 Pipes 与 ACL

| Pipe | 服务端 | ACL(允许连接) | 用途 |
| --- | --- | --- | --- |
| `\\.\pipe\xnc-core` | xnc-core | SYSTEM + `NT SERVICE\XNCAgent` | agent↔xnc-core RPC + 事件流 |
| `\\.\pipe\xnc-desktop-<pid>` | xnc-desktop | SYSTEM + `NT SERVICE\XNCAgent` | 控制 + 压缩码流(下行)+ 输入(上行) |
| `\\.\pipe\xnc-shell-<pid>` | xnc-shell | SYSTEM + `NT SERVICE\XNCAgent` | 终端流 |

- 所有 pipe:`FILE_FLAG_FIRST_PIPE_INSTANCE`,DACL 显式 deny Everyone else;随机后缀防抢占。
- worker pipe 由 worker 创建;**DACL 只允许 agent(服务 SID)连接**——即便 user-token 的 xnc-shell 也不放行对应用户 SID(用户不得伪造会话客户端;worker 是服务端,本就无需以客户端身份连自己的 pipe)。
- xnc-core 在 spawn 时生成 pipe 名与一次性 `pipe_secret`,经继承句柄传给 worker,并在 RPC 响应中带给 agent(§9.3)。

## 9.2 帧格式

小端二进制头 + protobuf payload:

```text
偏移  长度  字段
0     4    magic "XNIP"
4     1    protocolVersion(=1)
5     1    flags(bit0=response, bit2=event, bit4=error)
6     2    messageType(u16, 按 proto 枚举)
8     4    requestId(u32,请求-响应关联)
12    4    payloadLength
16    ..   payload(protobuf)
```

- 单帧上限 9MiB(覆盖 4K JPEG 帧);超过即协议错误断连。
- 流控:码流帧为高优先独立消息;pipe 缓冲由 OS 管理,agent 侧发送队列有界(视频 3 帧/订阅者语义在 xnc-desktop 内部完成,pipe 上不积压)。

## 9.3 认证握手

连接建立后第一帧必须是 `HELLO{pid, nonce}`:

- **身份**:双方各自用 `GetNamedPipeClientProcessId`/`ServerProcessId` 取对端 PID → `OpenProcess(QUERY_LIMITED_INFORMATION)` → 映像路径必须等于预期 exe 全路径(Program Files 下)且 Authenticode 签名有效(XNC 发布证书);
- **密钥**:challenge-response 证明 = `HMAC-SHA256(pipe_secret, nonce)`(密钥在前;RFC 4231 语义)。`pipe_secret` 由 xnc-core 在 spawn 时生成、仅发给该 worker 与 agent 两侧(经继承句柄 / RPC 响应),一次性、短 TTL——**不依赖 machine key**(user-token 的 xnc-shell 进程读不到 SYSTEM 才能读的机器密钥);
- xnc-desktop / xnc-shell 连接 agent 的 pipe 时同样双向校验。
- 任一校验失败:断连 + 审计事件 `ipc_auth_failed`。

## 9.4 xnc-desktop 侧消息目录(desktop.proto 节选)

```text
Agent→Host:
  ATTACH_SUBSCRIBER{session_id, ticket, capabilities, wants_control}
  DETACH_SUBSCRIBER{session_id}
  KEYFRAME_REQUEST{display_id, reason}
  SET_VIDEO_CONFIG{display_id, max_fps, max_width, target_bitrate_bps, quality}
  SET_ENCODER_BITRATE{bps}                     // QoS 热调
  INPUT_EVENT{seq, lease_id, event...}(§11.2)
  CONTROL_LEASE{grant|revoke, lease_id, session_id, expires_at}
  DRAIN{}
Host→Agent:
  HOST_HELLO{generation, displays[], encoder_backend, codec_config}
  ENCODED_FRAME{display_id, frame_seq, flags(key), captured_mono_us,
                encoded_mono_us, data(Annex-B AU)}
  CURSOR_SHAPE{shape_id, w, h, hotspot_x, hotspot_y, rgba}
  CURSOR_POSITION{display_id, x, y, visible}
  STATE_EVENT{stable_code, backend, recoverable, retry_after_ms, detail}
  DISPLAY_CHANGED{generation, reason}
  DESKTOP_STATE{DEFAULT|WINLOGON|TRANSITION}
```

---

# 10. 桌面会话协议(WebRTC 面)

## 10.1 传输决策(DD-01,初稿 §22 采纳 + 合规约束)

- **V1 数据面 = WebRTC(Pion @ agent / 原生栈 @ browser),ICE 强制 relay-only,TURN over TLS/TCP 443。**
- 理由:① 需要**不可靠无序通道**(mouse-fast/cursor),WSS 做不到;② RTP 自带 NACK/PLI/TWCC 拥塞控制与带宽估计,免自研;③ 浏览器原生 RTCPeerConnection + WebCodecs,零插件。
- 合规:节点与浏览器都只出站 TCP 443(turns)。媒体经 server 侧 TURN relay 中转,成本与现有 WSS relay 同量级。
- P2P(host/srflx 候选)为部署级策略开关(`cluster_config.allow_p2p`,默认 false),V2 起可按集群开启——开启后仍遵守「无入站监听」(ICE 只影响出站候选)。

## 10.2 信令与建连时序

复用现有控制连接与 SESSION_OPEN 框架:

```text
1. Browser  POST /api/nodes/{id}/desktop        (server: RBAC + 单控制者仲裁 + 审计)
2. Server   下发 SESSION_OPEN{kind=desktop, params{
              signaling(webrtc), capabilities[], ticket, turn_creds{username,credential,urls}}}
            经控制连接到 agent;同时向 Browser 返回 sessionId + clientWsUrl + turn_creds
3. Browser  生成 offer(RTCPeerConnection, iceTransportPolicy=relay,
            addTransceiver(video,recvonly), createDataChannel×4)
            → 经 client-side desktop WSS 发 OFFER{sdp}
4. Server   中继 OFFER 至 agent(desktop 会话 attach;server 不解析 SDP 媒体内容,仅尺寸限制)
5. Agent    校验 ticket → xnc-core.StartCapture(ticket)→ xnc-desktop 就绪(pipe ATTACH)
            → Pion setRemoteDescription → answer → SDP_ANSWER 经 server 回 Browser
6. ICE(双方均仅 turns:443?transport=tcp 候选)→ DTLS → SRTP/DataChannel 建立
7. Browser  CLIENT_HELLO(control 通道)→ 协商 H.264 profile → Host 发 CODEC_CONFIG + IDR
8. 媒体流开始;VIDEO_ACK 周期回传;输入按 lease 注入
```

- SDP 只经 server 中转(同现有 tunnel 透传立场);desktop WSS 断开 = 信令断,ICE 不依赖其存活(relay 媒体继续),server 侧逻辑会话保留 30s 供重附着(继承 browser-desktop 设计 §6)。
- 重附着:同一逻辑会话内 agent 侧 PeerConnection 保留,浏览器 ICE restart + 新 CLIENT_HELLO;xnc-desktop generation 不变(除非 Host 重建)。
- 多观众:每个观众一条独立 PeerConnection(agent 侧 Pion 多 PC 并存),共享同一 DisplayVideoService 编码输出并扇出 N 条 RTP 流;control lease 全局唯一(§11.1)。

## 10.3 TURN

- 部署:deploy/ 增加 coturn 容器,`listening-port=443`,tls-cert 与 xnc.app 同域(`turn.xnc.app`),`lt-cred-mech` + REST API(HMAC secret 与 xnc-server 共享)。
- 凭据:server 为每次 desktop 会话签发短时(≤会话 TTL)TURN 凭据,随 SESSION_OPEN 与 Browser 响应下发;凭据不落盘。
- 容量:relay 端口段仅 server 本机方向,节点/浏览器侧无感知。

## 10.4 RTP 视频

- codec:H.264(profile 经 CLIENT_HELLO 协商,首选 constrained-baseline,兼容性 fallback high);packetization-mode=1,FU-A 由 Pion 打包。
- RTP timestamp = captured_mono_us 换算 90kHz;启用 TWCC + transport-cc 扩展;abs-capture-time 扩展携带绝对采集时钟。
- 关键帧请求:浏览器 `RTCP PLI`(VideoDecoder 丢帧/入流)→ agent 转 `KEYFRAME_REQUEST{reason=pli}` → xnc-desktop 合并触发一次 IDR。
- 码率:Pion bandwidth estimator(TWCC)→ `SET_ENCODER_BITRATE` 热调;梯子与保底见 §10.8。

## 10.5 DataChannels

| 通道 | 特性 | 承载 |
| --- | --- | --- |
| `control` | reliable/ordered | **双载荷**:text = JSON 控制词汇(§10.6);binary = protobuf CURSOR_SHAPE(大体积低频,需可靠有序) |
| `input` | reliable/ordered | 键、按钮、滚轮、文本、lease(protobuf) |
| `mouse` | unreliable/unordered | 仅 POINTER_MOVE 合并流(protobuf) |
| `cursor` | unreliable/unordered | 光标位置 CURSOR_POSITION(下行;旧位置自然被覆盖) |
| `clipboard` | reliable/ordered | V1.5 预留,不创建 |

> 与初稿 §24 的差异:`shell` 通道不建(终端走既有 WSS relay,DD-06);光标拆为「位置(不可靠)+ 形状(control 二进制子帧,可靠)」两路。

## 10.6 控制词汇(control 通道 text 载荷,继承 browser-desktop 设计 §7.1)

保留:CLIENT_HELLO / DESKTOP_BEGIN / DISPLAY_INFO / DISPLAY_CHANGED / CODEC_CONFIG / SESSION_STATE / ERROR / VIDEO_ACK / REQUEST_KEYFRAME / VIEWPORT_STATE / lease 管理(LEASE_REQUEST / LEASE_GRANTED / LEASE_REVOKED)/ PING-PONG。

VIDEO_ACK 字段沿用:{generation, displayId, receivedSeq, decodedSeq, renderedSeq, decodeQueueSize, decodeFps, visible},500ms 累计 + 关键事件即时。

## 10.7 generation 语义

- xnc-desktop 每次重建(CaptureReset 只递增 capture 子版本;进程重启/编码器重建/分辨率变化)generation++,经 DISPLAY_CHANGED/DESKTOP_BEGIN 下发;
- 浏览器收到新 generation:flush decoder、清光标状态、重等 CODEC_CONFIG + IDR;QoS 与输入 seq 不跨 generation。
- 逻辑会话 30s 重附着窗口内 generation 保持,超时新会话。

## 10.8 QoS 控制器(agent 内,综合 RustDesk VideoQoS 与 browser-desktop §10)

输入信号:TWCC 估计带宽、RTCP RR RTT、VIDEO_ACK(decodedSeq 滞后、decodeQueueSize、decodeFps、visible)、编码器输入队列深度、capture→encode 耗时。

```text
FPS 梯子:60(协议上限)→ 30(V1 默认档,M2 端到端验证后放开 60)→ 15 → 7.5,
          最多每 1s 调一次;
码率梯子:TWCC 目标 × [0.5..1.0] 平滑,每 3s 一次,升档幅度 ≤ +1.5Mbps/次;
约束优先级:控制者体验 > 可见观众 > 隐藏观众(隐藏标签页立即退出全局样本);
解码拥塞(decodeQueueSize 持续 >2):先降该订阅者分发采样,再降全局 fps;
静止桌面:采集/编码自然 0fps(§7.4),bitrate 不动,光标与心跳继续。
```

阈值可配置,状态转换固定可观测;组件测试用确定性输入驱动(继承 browser-desktop §10)。

---

# 11. 输入系统

## 11.1 授权链

```text
Server(RBAC + capabilities + 单控制者仲裁)
  → SESSION_OPEN.ticket{capabilities: input.mouse, input.keyboard, ...}
  → agent 校验 ticket
  → xnc-core.StartCapture 转交 ticket(xnc-desktop 独立验签)
  → CONTROL_LEASE{grant, lease_id, TTL 60s, 续期}
  → INPUT_EVENT 必须携带有效 lease_id + 未过期 generation
```

- 每逻辑桌面会话同一时刻至多一个 control lease;Server 授予/续期/移交/撤销,browser 不得自行声明。
- 断线、generation 变化、lease 过期/撤销、Host 退出 → xnc-desktop **释放所有已记录按键与鼠标按钮**(RustDesk 卡键教训固化为硬规则)。

## 11.2 消息定义(input 通道;POINTER_MOVE 走 mouse 通道)

```protobuf
message InputEvent {
  uint64 seq = 1;             // 每 lease 单调递增,Host 检查(重复/回退丢弃)
  string lease_id = 2;
  oneof event {
    PointerMove    pointer_move = 10;     // mouse 通道,可合并
    PointerButton  pointer_button = 11;   // barrier
    Wheel          wheel = 12;            // barrier
    KeyPhysical    key_physical = 13;     // barrier
    KeyUnicode     key_unicode = 14;      // barrier
    Text           text = 15;             // IME 合成结果
    LockState      lock_state = 16;       // caps/num/scroll 同步
  }
}
message PointerMove   { int32 x=1; int32 y=2; uint32 buttons=3; uint32 display_id=4; }
    // x,y = 目标显示器逻辑像素;buttons 位:1L 2R 4M 8X1 16X2
message PointerButton { uint32 button=1; bool down=2; }
message Wheel         { sint32 dx=1; sint32 dy=2; bool trackpad=3; }
message KeyPhysical   { uint32 scan_code=1; bool down=2; bool extended=3; }
    // scan_code = Windows 扫描码(Set 1);浏览器 KeyboardEvent.code 映射表在 Host 维护
message KeyUnicode    { uint32 codepoint=1; bool down=2; }
message Text          { string s=1; }     // 一次注入一串,KEYEVENTF_UNICODE 序列
message LockState     { bool caps=1; bool num=2; bool scroll=3; }
```

## 11.3 坐标系

- 主控传**目标显示器逻辑像素**(display_id + 显示器内坐标);Host 换算虚拟桌面绝对坐标(`MOUSEEVENTF_ABSOLUTE|VIRTUALDESK` 0–65535 归一),天然支持负坐标多屏。
- Host 侧夹紧到显示器边界;越界 + 未按下状态 → 钳到最近边;MOVE_RELATIVE 的 delta 钳制 ±10000(RustDesk 教训:防恶意大跳)。
- DPI:DISPLAY_INFO 携带每显示器 scale,浏览器侧画布换算。

## 11.4 键盘模型(V1)

- **物理键**:KeyboardEvent.code → Windows 扫描码表(Host 维护布局无关映射),`KEYEVENTF_SCANCODE`(+extended 标志)注入——免布局位置注入。
- **文本**:TEXT/KeyUnicode 走 `KEYEVENTF_UNICODE`(代理对展开),承载 IME 与非 ASCII。
- **修饰键**:一律由显式 KeyPhysical 事件序列表达(Ctrl+Click = Ctrl down → click → Ctrl up);**Host 不做修饰键推断或临时按压**,仅在 drain/断连/lease 失效时强制释放全部已记录按键。Wheel V1 不携带修饰键字段。
- **LockState 同步**:Host 对比事件 lock_state 与 `GetKeyState` 实际状态,不一致先注入 CapsLock/NumLock 同步(RustDesk LockModesHandler 语义)。
- V1 不做完整键盘模式协商(legacy/map/translate);上述「物理键 + 文本」双通道即覆盖 95% 场景,复杂布局场景 V2 再议(DD-08)。

## 11.5 注入规则(InputManager)

- 全部 `SendInput`,**固定 dwExtraInfo 标记**(如 `0x584E4301`)——同时用于:① 自我注入识别(防回环);② 本地低级钩子可选择性屏蔽本地物理输入(privacy, V2)。
- 注入线程 = DesktopSupervisor 绑定线程(§7.3);SendInput 失败 → 重绑 input desktop → 重试一次 → 上报 `INPUT_DESKTOP_MISMATCH`。
- 输入队列:有界 512;POINTER_MOVE 在两个 barrier(按键/按钮/滚轮)之间可合并(保留最新);barrier 事件不丢弃不合并;队列溢出 → 撤销 lease(而非积压)。
- 卡键清理:Watchdog 每 10s 扫描已记录按下超过 30s 未释放的键/按钮强制 KeyUp(进程退出时同样执行)。
- 频控:move 事件 ≤ 500Hz 合并后;整体系 1000 events/s 上限,超限丢弃 move、保 barrier。

## 11.6 UAC / 安全桌面 / SAS

- UAC 弹出:DesktopWatch 检测 input desktop = Winlogon → CaptureReset → 继续采集安全桌面(SYSTEM-in-session 令牌可 OpenInputDesktop);输入同线程已绑定 → 直接注入 → **可远程点击 UAC**。
- Ctrl+Alt+Del / 锁屏 SAS:**不走伪装键盘**,独立 `SECURE_ATTENTION` 控制消息(需 capability `input.secure_attention`)→ agent → xnc-core `SendSas`(SendSAS,sas.dll,仅 LocalSystem 服务可调用)。
- `Win+L` 后:看到锁屏、可输入凭据解锁(锁屏亦在 Winlogon 桌面)。

## 11.7 agent 侧预校验

lease 有效性、seq 单调、坐标与按钮位合法性、消息大小(≤2KiB)、速率;不合法丢弃并计数(观测),不惩罚断连(避免误杀)。

---

# 12. RemoteShell 与 exec 迁移

- 协议面:kind=shell / kind=exec **不变**(browser/CLI/server 无感知);
- agent 内部:exec.go / shell.go 的进程创建部分移除,改为 `xnc-core.CreateShell(mode=ONESHOT|INTERACTIVE, token_kind, profile, params)` → 连 xnc-shell pipe 泵数据;
- token_kind 默认:exec/shell 默认 **USER**(与现状 SYSTEM 是行为变化,见 DD-09);`--system` / capability 显式要求时 SYSTEM;
- 多 shell 参数构造(bash -c / pwsh -Command 等)随 exec 引擎移入 xnc-shell;env/cwd/timeout/杀树语义保持;
- 交付顺序:M2(§20)与桌面解耦,可独立先行。
- **file kind 留在 agent 进程内执行**:agent 低权化后,受保护路径(C:\Windows、其他用户 profile 等)的 put/get 以 `PERMISSION_DENIED` 快速失败并提示;SYSTEM 级文件操作经 shell.system(copy/robocopy)完成。不为文件会话扩大 xnc-core 攻击面。

---

# 13. 显示器管理与无人值守

## 13.1 DisplaySupervisor(xnc-core 为主,xnc-desktop 对齐)

- 枚举:DXGI 输出 + EnumDisplayDevices 交叉(继承 RustDesk 经验:按 GDI 枚举顺序重排 DXGI,索引稳定);字段:{id, origin(虚拟桌面坐标, 可负), w, h, primary, scale, rotation, virtual}。
- 拓扑监控:WM_DISPLAYCHANGE 广播 + 500ms 轮询兜底;变化 → CoreEvent → xnc-desktop CaptureReset + DISPLAY_CHANGED。
- 状态机:`PHYSICAL_ACTIVE / PHYSICAL_OFF / NO_OUTPUT / VIRTUAL_CREATING / VIRTUAL_ACTIVE / TOPOLOGY_CHANGING`。

## 13.2 V1 多显示器策略

- V1:同时编码**主显示器/用户指定一台**;DISPLAY_INFO 全量下发,浏览器可发 SWITCH_DISPLAY{display_id}(SET_VIDEO_CONFIG)切换——切换即该显示器 DisplayVideoService 重置 + IDR。
- V2:每显示器一条视频轨,同时观看(DD-10)。

## 13.3 Headless 与 IDD(可选组件,初稿 §26–28)

- 触发:NO_OUTPUT(无输出)、合盖断面板、用户显式请求、云主机场景。
- 流程:`xnc-core.CreateVirtualDisplay{1920×1080@60}` → IDD 驱动插入 → 等 WM_DISPLAYCHANGE(≤5s)→ 重新枚举 → DXGI 采虚拟输出 → Force IDR。
- IDD 只提供显示器,不承担采集(ADR-008);单独安装包(`xnc-idd-driver`),未安装时 NO_OUTPUT 上报稳定错误码 `NO_DISPLAY_ADAPTER` 并提示。
- 物理显示器回插:策略 = 保留虚拟屏(V1,避免反复切换),用户可显式 RemoveVirtualDisplay。

## 13.4 PowerManager

- desktop 会话存在期间默认 `SetThreadExecutionState(ES_CONTINUOUS | ES_SYSTEM_REQUIRED | ES_DISPLAY_REQUIRED)`:禁系统睡眠、**保持屏幕唤醒**——合盖/面板断开会引发拓扑变化,误触发 §13.3 的 headless 路径,故 V1 控制期间不主动息屏。
- `allow_display_off` 为节点级配置项(默认关):开启时仅 SYSTEM_REQUIRED(允许息屏省电),需按机型实测 DXGI 息屏黑帧风险,出现即自动回退全保活并记诊断。
- 会话结束恢复默认;重启/关机走 capability RPC(power.reboot / power.shutdown),带 30s 可取消窗口与审计。

---

# 14. 安全模型

## 14.1 Capability 目录(V1)

```text
screen.view             观看桌面视频+光标(默认授予 operator)
input.mouse             鼠标注入
input.keyboard          键盘/文本注入
input.secure_attention  SAS/Ctrl+Alt+Del(默认仅 owner 角色)
shell.user              用户令牌 exec/shell(默认 operator)
shell.system            SYSTEM 令牌 exec/shell(默认仅 owner,显式授予)
file.read / file.write  现有文件会话
display.virtual         创建/删除虚拟显示器
power.keep_awake        (随 desktop 会话隐含)
power.reboot / power.shutdown
agent.update            触发更新(默认 owner)
agent.maintain          RestartWorker 等维护操作(owner)
(预留 V1.5/V2:clipboard.read, clipboard.write)
```

禁止「connected == full admin」;连接 ≠ 权限,每次会话携带最小 capability 集。

## 14.2 SessionTicket(初稿 §44 细化)

即现有 agentToken 信封的扩展,server 用**专用会话签名密钥**(Ed25519)签名,public key 在注册时 pin 到节点(agent 与 xnc-core 各存一份,可经签名轮转消息更新):

```json
{
  "device_id": "...", "session_id": "...", "kind": "desktop",
  "issued_at": 0, "expires_at": 0, "nonce": "...",
  "capabilities": ["screen.view", "input.mouse", "input.keyboard"]
}
```

- agent 收 SESSION_OPEN 时验签(签名、device_id、过期、nonce 一次性);
- xnc-core / xnc-desktop **各自独立再验**(不信任 agent 结论);
- 票据只经 pipe 与 TLS 通道传递,不落盘、不进命令行/环境变量。

## 14.3 攻击面与审计

- xnc-core 无网络栈(§6.2);xnc-desktop/xnc-shell 无监听;全部 pipe 双向认证(§9.3);
- workers 无永久凭据;票据短 TTL;lease 短 TTL;
- 审计事件(server 侧记录):desktop.open/close、观看者列表、lease 授予/移交/撤销、SAS 使用、SYSTEM shell 创建、虚拟显示器创建/删除、电源操作、更新执行、ipc_auth_failed、worker 崩溃与降级;
- 审计**不**记录:按键文本、鼠标逐事件、终端内容、帧内容;
- 硬上限:消息/帧尺寸、显示器数(≤8)、订阅者数(≤4)、输入频率、shell 并发(≤8)、控制队列长度。

---

# 15. 故障恢复与降级

## 15.1 监督关系

```text
SCM → XNCAgent(服务)          SCM → XNCCore(服务)
agent ←pipe 心跳→ xnc-core
xnc-core → Job 对象 + 退出回调 → xnc-desktop / xnc-shell × N
xnc-desktop 内部 Watchdog 线程(卡死自杀)
server 侧逻辑会话 30s 重附着窗口
```

## 15.2 重启策略

- worker 崩溃:记录 crash(退出码 + last-will 诊断)→ 指数退避重启(1s,2s,4s…封顶 60s);
- **Crash loop**:60s 内 5 次 → 停止自动重启,按类型降级:
  - xnc-desktop loop → 以 `--backend=gdi --encoder=software` 参数组重启(锁死最保守模式);
  - encoder 相关崩溃(诊断标记)→ 直接软编;
- xnc-core 崩溃:SCM 重启;workers 属 Job 随之终止,agent 在 xnc-core 回来后重建会话(逻辑会话未失,generation++);
- agent 崩溃:SCM 重启;xnc-core 检测 pipe 断开 → DRAIN 所有 workers(输入立即停,视频停,xnc-core 不持会话状态)。

## 15.3 失败矩阵(含稳定错误码)

| 失败 | 动作 | 码 |
| --- | --- | --- |
| DXGI WAIT_TIMEOUT | 继续(静止) | — |
| DXGI ACCESS_LOST / DEVICE_REMOVED | CaptureReset | CAPTURE_RESET |
| 桌面切换 / UAC / 锁 / 解锁 | CaptureReset(+桌面重绑) | DESKTOP_SWITCH |
| 分辨率变化 / 显示器插拔 / IDD 创建 | CaptureReset + DISPLAY_CHANGED | DISPLAY_CHANGED |
| 无输出 | CreateVirtualDisplay(可选包);未装 → 报错 | NO_DISPLAY |
| DXGI 反复失败(健康分<60 / crash-loop) | GDI 回退 | CAPTURE_DEGRADED |
| GDI probe 成功 | 恢复 DXGI + IDR | CAPTURE_RECOVERED |
| 硬件编码器失败 | MfSoftware 下探 + IDR | ENCODER_FALLBACK |
| 软编失败 | JPEG 帧流(屏幕类内容质量劣化但可用) | ENCODER_JPEG |
| 编码输入排队 >1 帧 | 丢旧帧编最新 | —(计数) |
| 新订阅者 / PLI / 队列溢出 | 合并触发 IDR | — |
| xnc-desktop 崩溃 | xnc-core 重启;browser 30s 重附着,generation++ | HOST_RESTARTING |
| xnc-shell 崩溃 | 仅重启对应 shell,桌面不受影响 | SHELL_EXITED |
| 用户注销 | 该会话 workers 销毁;agent/xnc-core 不动 | SESSION_ENDED |
| 新用户登录 | 新 xnc-desktop | SESSION_CHANGED |
| core pipe 断 | agent 退避重连;期间拒绝 desktop | CORE_UNAVAILABLE |
| TURN 不可达 | desktop 会话建立失败(快速失败+明确指引) | TURN_UNREACHABLE |
| USER shell/exec 但无活动用户会话 | 快速失败,提示 `--system` | NO_ACTIVE_SESSION |
| file 会话访问受保护路径(agent 低权) | 快速失败,提示走 shell.system | PERMISSION_DENIED |
| 网络拥塞 | 丢旧帧 + QoS 降档 | — |
| 输入队列溢出 | 撤销 lease | LEASE_REVOKED_OVERFLOW |

## 15.4 收尾语义(Drain)

任何组件退出前:释放全部按键/按钮 → 停采集 → 停编码 → flush 管线 → 断 pipe;中断(崩溃)路径由 Job 终止兜底 + 重启后无状态(票据全部过期作废)。

---

# 16. 可观测性

## 16.1 结构化日志(全组件统一字段)

```text
timestamp, process, pid, wts_session_id, logical_session_id, generation,
display_id, capture_backend, encoder_backend, codec, error_code(stableCode),
state_before, state_after, duration_ms
```

- xnc-desktop 崩溃前写 last-will 诊断文件(ProgramData,环形 3 份):最后 200 条日志 + 各状态机快照 + 计数器。
- 日志不含票据、按键文本、帧数据。

## 16.2 指标(经 agent 心跳上报 server;本地 `/logs` 可导出)

```text
capture_fps, encode_fps, send_fps(per-subscriber ack fps), dropped_frames,
idr_count{idr_request_reason}, dxgi_reset_count, gdi_fallback_count,
encoder_fallback_count, capture_to_encode_ms, encode_latency_ms,
bitrate_bps(twcc target vs actual), rtt_ms, packet_loss, nack_count, pli_count,
cursor_update_hz, input_queue_depth, input_dropped, stuck_keys_released,
subscriber_count, host_restart_count, shell_sessions, shell_rx/tx_bytes,
core_rpc{count, error, latency_ms} by method, update_phase
```

## 16.3 延迟预算(V1 LAN 目标,继承初稿 §48 + browser-desktop 门槛)

```text
输入: browser event → agent → pipe → SendInput      p95 < 30 ms
光标: 采样 → browser 渲染                            p95 < 50 ms
采集→编码: AcquireNextFrame → AU 产出                 p95 < 15 ms @1080p60
端到端鼠标感知(输入→画面反馈)                       p95 < 150 ms(验收门槛)
首帧: CLIENT_HELLO → 首可解码帧                      p95 < 1 s
WAN: 核心目标 = 延迟不积累(丢旧帧),不承诺固定 FPS
```

---

# 17. 性能目标

| 项 | V1 | V2 |
| --- | --- | --- |
| 分辨率×帧率 | 1080p60(fps 默认 30,M2 验证后放开 60;上限 1440p60) | 4K60 |
| 编码 | H.264 硬件(设备 MFT),软编兜底 | AV1/HEVC 协商,H.264 恒为 fallback |
| CPU | 典型现代 PC < 15%(1080p60 满频) | — |
| xnc-desktop 内存 | < 350MB,30min 无持续增长(browser-desktop 门槛沿用) | — |
| 静止桌面带宽 | ≈0(视频 0fps + 光标 + keepalive) | — |
| 码率范围 | 0.5–15 Mbps 自适应 | 25Mbps |

---

# 18. Server 侧与 wire 协议演进

1. 新 session kind `desktop`:SESSION_OPEN.params 扩展 `{engine:"webrtc", capabilities[], ticket, turn_creds}`;会话 token 60s TTL 不变。
2. 新 REST:`POST /api/nodes/{id}/desktop`(RBAC + 单控制者仲裁 + 审计)、`GET /desktop-creds`(会话内 TURN 凭据);权限模型新增 §14.1 各 capability(默认角色映射:operator→screen.view+input.*+shell.user;owner→全部)。
3. desktop WSS(信令中继):双侧 attach 后透传 OFFER/ANSWER/ICE/控制 JSON,server 仅做尺寸与会话状态校验,不解析 SDP 细节(立场同 tunnel)。
4. server 会话管理:desktop 逻辑会话 30s 重附着;idle 治理:无订阅者 5min 自动关闭(desktop)。
5. deploy/:新增 coturn(turn.xnc.app,443 turns,REST 凭据,共享 HMAC secret)。
6. 节点灰度开关:`desktop_engine = legacy | desktop`(节点级配置,随心跳/配置通道下发),按实验节点 → dev channel → 小比例 → 默认逐级放量,同时观察 EDR/AV 对 SYSTEM-in-session 采集注入进程的拦截情况,必要时提供企业环境白名单指引;`cluster_config.allow_p2p`(默认 false)。
7. legacy:`xnc screen`/screen kind 保留至 M2(xnc-desktop 全量后退役,`--snap` 由 `xnc-desktop --jpeg-single` 承接);`xnc rdp` 默认打开浏览器 desktop,`xnc rdp --native` 保留 mstsc tunnel。
8. CLI:`xnc exec --system` / `xnc shell --system` 显式 SYSTEM 令牌(默认 USER);`xnc rdp` 默认浏览器 desktop;`xnc screen --snap` 语义不变(实现切换为 xnc-desktop 快照模式)。

---

# 19. 工程结构(对初稿 §51 的 XNC 化)

```text
XNC/
├── proto/
│   ├── proto.go / session.go          (现有 wire 协议,扩展 desktop kind)
│   └── ipc/                           (新增:core.proto, desktop.proto, shell.proto,
│                                        protoc → Go(agent 侧) + C++(native 侧))
├── agent/                             (Go module,xnc-agent)
│   ├── connect/ identity/ enroll/ machineinfo/ svcapp/ updater/   (现有)
│   ├── session/                       (engine + exec/shell/file/tunnel 改造)
│   ├── coreclient/                    (新增:xnc-core pipe RPC 客户端)
│   ├── desktop/                       (新增:WebRTC 引擎、RTP、DataChannel、QoS、输入预校验)
├── shellhost/                         (新增 Go module,xnc-shell.exe)
├── native/                            (新增 C++ / CMake;MSVC v143;静态 CRT)
│   ├── core/                          (xnc-core.exe)
│   ├── desktop/                       (xnc-desktop.exe: dxgi/ gdi/ wgc/ cursor/ input/ encoder/ framecache/)
│   │   └── (吸收 screen-helper/dda 经验与 MF 编码逻辑)
│   └── common/                        (pipe、ipc 帧、日志、崩溃处理)
├── driver/idd/                        (V2,可选包)
├── server/  web/  cli/ deploy/        (现有 + §18 改动)
└── tools/screendiag/                  (现有诊断,扩展 xnc-desktop 诊断模式)
```

- C++ 依赖策略:**目标依赖仅 Windows SDK + protobuf**;构建脚本 build.py 扩展 MSVC 段;CI 增加 license 清单检查(MIT/Apache-2.0/BSD/MS-PL 白名单)。
- clean-room 检查进入 code review checklist(browser-desktop §19 全文适用)。

---

# 20. 实施阶段与验收

## M0 — 新架构地基(约 1–2 周)

- proto/ipc:XNIP 帧编解码(Go + C++ 双侧字节级一致)、握手(pipe_secret + HMAC-SHA256 双向证明)、core/desktop/shell schema 定稿(protobuf codegen M1 接入);
- xnc-core C++ 骨架:named pipe 服务端、双向认证握手、结构化日志、watchdog 线程、`--console` 诊断模式;
- agent 侧 coreclient:拨号、握手、PING/PONG;
- 跨语言 smoke:Go client ↔ xnc-core 完成握手与往返(自动化,CI 可跑)。

> **2026-08-22 owner 决策:不在旧 helper 上做任何修复或加层。** 原 M0 的「旧 helper 修关键帧风暴」与「legacy screen 之上的 browser desktop Phase 1 观测切片」撤销——E2 教训(force-key 一次性消费、MFT 时基对齐)直接作为 xnc-desktop 编码器契约实现(§7.10/ADR-017),不再回头修 legacy;DD-12 中「M0 在 legacy screen 上验证协议词汇」不再执行。legacy screen 维持现状(含已知关键帧风暴与高运动 ~5fps),直到 M2 被 xnc-desktop 替换;期间 screen 可用性问题以「提前 M2 交付」回应,而非修补旧路径。

## M1 — 垂直原型(初稿 Phase 0,约 4–6 周)

- xnc-desktop:DXGI 采集(移植 dda.c)+ FrameCache + **软编**(MF 移植;硬编设备 MFT 并行做,不阻塞);
- xnc-core:TokenManager + StartCapture + 监督最小集;
- agent:coreclient + Pion(relay-only)+ RTP + 4 条 DataChannel;
- web:desktop 实验页(RTCPeerConnection + WebCodecs + 光标叠加 + 输入发送);
- deploy:coturn。
- 输入采用简化控制权模型(单观众即控制者);完整 lease 状态机 M2 交付。
- **验收:LAN 1080p60 鼠标键盘可控制普通桌面,首帧 <1s,DXGI ACCESS_LOST 自动恢复。**软编达标允许以 CPU 余量为条件;60fps 档位以 M2 硬编验证为准(§10.8)。

## M2 — 可靠远控 = 第一个可交付版(初稿 Phase 1,约 6–8 周)

- DesktopSupervisor(UAC/锁屏/切换)、统一 CaptureReset、GDI 回退;
- control lease + 输入完整语义(lock 同步、卡键清理、安全桌面注入、SAS);
- 多观众 + QoS 完整档位 + 慢观众隔离;
- xnc-shell 迁移(exec/shell 令牌语义)+ worker 监督/退避/降参;
- 更新落位移交 xnc-core;
- **screen kind 退役 + agent 低权化同批执行**:`xnc screen --snap` 改经 xnc-core spawn `xnc-desktop --jpeg-single`(一次性快照模式),旧 screen-helper 下线(避免低权 agent 无法 spawn helper 的过渡期断档,§5.3)。
- **验收 = 初稿 §53 V1 DoD 全项**(逐条沿用,含 UAC 远程点击、Win+L、注销存活、分辨率/插拔恢复、降级链、崩溃恢复、新观众关键帧、shell 不越权、xnc-core 无任意执行面)。

## M3 — 无人值守(初稿 Phase 2)

- DisplaySupervisor 完整 + 多显示器切换、动态分辨率、IDD 可选包、合盖、PowerManager、crash-loop 降级参数组。
- **验收 = 初稿 §54 V2 DoD**(headless 启动即控、IDD 自动创建/删除/回迁、多显示器动态增删)。

## M4 — 性能(初稿 Phase 3)

- 硬件编码全量(设备 MFT 三家矩阵)、AV1/HEVC 协商、共享内存 IPC(帧面 pipe→ring buffer)、拥塞调优、QUIC 评估、(评估)NVENC/QSV/AMF 原生 SDK。

---

# 21. 决策记录(2026-08-22 owner 批准:全部采纳「决定」列)

| # | 决策 | 决定 | 备选(未采用,存档) |
| --- | --- | --- | --- |
| DD-01 | WebRTC V1 强制 relay-only(turns/TCP 443),P2P 为部署开关 | **采纳**(保合规红线) | 直接允许 P2P(放弃 §1.5 红线);或回到纯 WSS(放弃不可靠通道与 RTP 拥塞控制) |
| DD-02 | Host 语言 Rust(旧设计)→ C++(初稿) | **C++**(dda.c 教训:COM/图形 syscall 在非 C/C++ 栈系统性踩坑;团队已有 dda.c/MF-syscall 经验) | 维持 Rust |
| DD-03 | xnc-shell 语言:Go(移植现有 ConPTY/exec 引擎) | **Go**(近零迁移成本;安全边界在 token 与 xnc-core,与语言无关) | 按初稿用 C++ 重写 |
| DD-04 | xnc-desktop 常驻(会话级,1s 探活保帧)vs 仅订阅时存在 | **会话级常驻 + 无订阅 hibernate**(初稿 §12「必须持续 Capture」的省资源化) | 严格按初稿满频常驻 |
| DD-05 | M1 编码:软编先行,硬编并行(不阻塞垂直切片) | **采纳** | 按初稿 M1 即硬编 |
| DD-06 | exec/shell/file 留在 WSS relay,不迁 DataChannel | **采纳**(复用会话治理;终端对延迟不敏感) | 按初稿迁 shell DataChannel |
| DD-07 | V1 硬编只做设备 MFT,不写 NVENC/QSV/AMF 原生绑定 | **采纳**(V3 再议) | 初稿 §18 字面(四家原生) |
| DD-08 | 键盘 V1 = 物理键 + 文本双通道,不做 map/translate 全模式 | **采纳** | 完整模式集(RustDesk 式) |
| DD-09 | exec/shell 默认令牌 USER(现状 SYSTEM 是行为变化) | **USER 默认 + 显式 SYSTEM**(CLI `--system` 透出;无活动会话 → NO_ACTIVE_SESSION,§8.4) | 保持 SYSTEM 默认(兼容) |
| DD-10 | 多显示器 V1 单编码 + 切换,V2 多轨同编 | **采纳** | V1 即多轨 |
| DD-11 | agent 低权化时点:M2 一次到位(服务重配置+ACL+更新移交) | **M2,且与 screen kind 退役同批**(M1 原型期暂维持 SYSTEM,加 TODO 标记) | M1 即降权 |
| DD-12 | 旧 browser-desktop 设计(已批准)中 WSS+XNCD desktop 协议的地位 | 词汇库(control/ACK/lease)全部继承;XNCD 二进制头仅用于 WSS 回退模式,V1 不实现回退 | V1 同时实现 WSS 回退 |
| ~~DD-13~~ | **已定(2026-08-22)**:命名采用 core 家族——`xnc-core.exe`(服务 XNCCore)/ `xnc-desktop.exe` / `xnc-shell.exe`;pipes:`xnc-core` / `xnc-desktop-<pid>` / `xnc-shell-<pid>` | — | — |
| DD-14 | 剪贴板时点 | V1.5(DataChannel 通道名已预留;Get/SetClipboardData 在 xnc-desktop) | V1 即做 |

---

# 22. ADR 汇总

| ADR | 决策 |
| --- | --- |
| ADR-001 | Go/C++ 以进程 IPC 为边界,不用 CGO 承载实时图像 |
| ADR-002 | 公网 agent 不以 LocalSystem 运行(NT SERVICE 虚拟账户) |
| ADR-003 | LocalSystem xnc-core 不处理任何互联网协议 |
| ADR-004 | DXGI Desktop Duplication 为全桌面主采集后端 |
| ADR-005 | GDI 永久保留为兼容回退 |
| ADR-006 | WGC 仅窗口共享与诊断,不承担无人值守采集 |
| ADR-007 | IDD 为可选显示提供者,单独安装包 |
| ADR-008 | IDD 第一阶段不直接承担帧采集 |
| ADR-009 | 视频在 C++ 完成编码后才进入 Go |
| ADR-010 | RemoteShell 使用 ConPTY;exec/shell 统一由 xnc-shell 承载 |
| ADR-011 | user shell 与 system shell 为不同 capability |
| ADR-012 | 光标与视频独立传输(位置不可靠通道 + 形状 control 二进制子帧) |
| ADR-013 | 任何采集重建必须重建 Base Frame 并 Force IDR(generation 递增) |
| ADR-014 | 拥塞时丢旧帧,禁止累积显示延迟 |
| ADR-015 | Session/Desktop/Display/Capture/Encoder 独立状态机 |
| ADR-016 | WebRTC relay-only(新):媒体面经 TURN/TLS 443,P2P 为部署策略开关 |
| ADR-017 | force-next-keyframe 一次性消费(新,E2 教训) |
| ADR-018 | 更新落位统一由 xnc-core 执行(新) |
| ADR-019 | 跨进程票据双验:agent 与 xnc-core/xnc-desktop 各自独立验 SessionTicket(新) |
| ADR-020 | file 会话不设 SYSTEM 通道:agent 低权后受保护路径 PERMISSION_DENIED 快速失败,SYSTEM 文件操作走 shell.system(新) |
| ADR-021 | 无活动用户会话时 USER shell/exec 快速失败 NO_ACTIVE_SESSION,禁止隐式提权(新) |
