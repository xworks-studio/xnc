# XNC 浏览器远程桌面架构设计

- 日期：2026-08-22
- 状态：已确认（架构、协议、视频、输入、安全、恢复、验收和迁移均已逐节批准）
- 技术取舍：RustDesk 架构经验优先于 XNC 现有 screen helper，但不复制、修改或链接 RustDesk AGPLv3 主仓库代码
- 客户端边界：桌面版 Chrome/Edge 是第一阶段唯一正式支持的控制端

---

# 1. 背景与决策

XNC 当前有两条割裂的桌面路径：

1. xnc rdp 通过 Server 中继原生 RDP TCP 流，具备完整控制能力，但依赖节点启用 RDP、Windows 凭据和 TermService。
2. xnc screen 通过用户会话 helper 采集并编码 H.264，浏览器可零安装观看，但没有输入、反馈式 QoS 和完整的高权限桌面覆盖。

实验表明，现有 screen 路径首帧可在约 0.43–0.54 秒到达，但高运动场景只有约 5 FPS，并出现关键帧风暴、长停顿和较高内存占用。继续只修补编码器，不能解决普通用户 helper 无法可靠覆盖 UAC、安全桌面和会话切换的结构性问题。

本设计作出以下决定：

- 浏览器继续作为主要入口，节点之外不分发原生客户端。
- XNC 的节点身份、RBAC、审计、REST API 和中心 Server relay 保留。
- 新增独立 Rust Desktop Host，采用 RustDesk 安装版所体现的进程边界和视频服务思想，但完全独立实现。
- Windows Service Agent 负责监督、授权和非桌面能力；活动交互会话中的高权限 Host 负责采集、编码、输入和桌面会话网络。
- 所有连接继续主动出站 443；第一阶段不引入 WebRTC、P2P、TURN 或 SFU。
- 原生 RDP tunnel 保留为兼容和应急路径，浏览器桌面最终成为 xnc rdp 的默认入口。

本设计覆盖一个完整子系统，后续实施必须按阶段拆解，不能一次性替换生产路径。

# 2. 目标与非目标

## 2.1 目标

- Chrome/Edge 无插件观看和控制当前 Windows 交互桌面。
- 不依赖目标机启用 3389、TermService 或提供 Windows 账号密码。
- 支持普通桌面、管理员窗口、锁屏、UAC 安全桌面和活动会话切换。
- 每个显示器只采集、编码一次，多个观众共享编码结果。
- 视频具有帧序号、时间戳、ACK、解码反馈、反压和自适应 QoS。
- 多观众可以同时观看，同一时间只有一个控制者可以注入输入。
- Host 崩溃或用户会话切换时，浏览器可在恢复窗口内重新附着。
- 保留节点级灰度开关和旧路径回退能力。

## 2.2 非目标

- 第一阶段不支持 Firefox、Safari 或移动浏览器。
- 第一阶段不实现音频、文件拖放、打印机、USB、摄像头和完整 RDP 设备重定向。
- 第一阶段不同时控制同一节点上的多个 Windows 用户会话。
- 第一阶段不实现 WebRTC 直连或服务端视频转码。
- 不追求 RustDesk 协议兼容，也不直接使用 RustDesk 主仓库代码。
- 不在视频验收前开放远程输入。

# 3. 代码研究依据

## 3.1 RustDesk 的关键结构

本地 RustDesk 源码研究得到以下架构事实：

- src/platform/windows.rs 的 Windows Service 监测活动会话，并在目标会话启动高权限 --server 进程；会话变化时重新启动。
- src/server/portable_service.rs 的便携模式把主屏采集、光标和输入放入 SYSTEM helper，像素通过共享内存返回，输入通过认证 IPC 进入 helper。
- src/server/service.rs 使用 GenericService、Subscriber、snapshot 和休眠机制管理共享服务。
- src/server/video_service.rs 为每个显示器建立单一 capturer/encoder，并向多个订阅者广播编码帧。
- src/server/video_service.rs 和 src/server/video_qos.rs 跟踪每个连接的取帧、延迟和质量反馈，以调节 FPS 与质量。
- src/server/connection.rs 为视频与普通控制消息使用独立发送通道，并把键鼠事件交给独立输入线程。
- src/client/io_loop.rs 和 src/client.rs 使用有界视频队列、独立解码线程、解码 FPS 和队列长度反馈。
- libs/enigo 的 Windows 实现最终使用 SendInput、virtual desktop 坐标、扫描码、Unicode 和 dwExtraInfo。

这些事实只作为行为与边界参考。XNC 不复制其实现。

## 3.2 XNC 当前结构

- agent/session/screen_windows.go 从 Session 0 使用 WTSQueryUserToken 和 CreateProcessAsUser 启动普通用户 helper。
- agent/screen-helper 完成 DDA/WGC 采集、BGRA/NV12 转换和 Media Foundation H.264 编码。
- agent/session/screen.go 用单例 ScreenStreamManager 广播帧，慢观众队列满时静默丢帧。
- server/internal/session/pump.go 只做通用 WebSocket 双向转发，不理解视频 ACK、优先级或恢复。
- web/src/pages/ScreenPreview.tsx 使用 WebCodecs 解码，但只回传解码错误，不反馈解码 FPS、队列和渲染延迟。

当前 helper 解决了“进入普通用户会话”，但没有形成 RustDesk 式高权限桌面 Host、反馈闭环和恢复模型。

# 4. 方案比较

| 方案 | 说明 | 结论 |
| --- | --- | --- |
| 独立 Rust Desktop Host | Go Agent 监督；Rust Host 在活动会话负责桌面数据面 | 采用。最接近已验证的 RustDesk 边界，并适合 Windows 图形与并发代码 |
| 增强现有 Go helper | 提升 helper 权限并继续扩展采集、输入、QoS | 不采用为目标架构。会延续当前 COM/MFT 风险和职责堆叠 |
| WebRTC 数据面 | 使用 WebRTC 传输视频与输入 | 暂不采用。首期基础设施和调试成本过高，且偏离 RustDesk 数据管线 |
| 直接复用 RustDesk | fork、链接或复制主仓库模块 | 禁止。XNC 不能接受 AGPLv3 衍生约束 |

# 5. 进程与信任边界

~~~text
Session 0
  xnc-agent.exe / Windows Service
    - 节点身份、控制连接、RBAC 结果执行
    - exec、shell、file、update
    - Windows 会话监测与 Desktop Host 监督
    - Host 短期租约和一次性桌面会话授权

活动交互会话
  xnc-desktop-host.exe / SYSTEM / 无 UI
    - 桌面、显示器和 input desktop 绑定
    - 采集、缩放、色彩转换和编码
    - 光标服务
    - 单控制者输入服务
    - 每个浏览器会话独立出站 WSS

中心服务
  xnc-server
    - REST、身份、RBAC、审计
    - 逻辑桌面会话和短期重新附着
    - 不透明 WSS relay

用户端
  Chrome/Edge
    - WebCodecs、Canvas、光标叠加
    - 键鼠与文本采集
    - ACK、解码 FPS、队列和页面状态反馈
~~~

Host 是节点级长期进程，不是每个观众启动一个进程。无人观看时 Host 保持就绪但视频服务休眠。高码率视频不经过 Agent 的本地 IPC。

Agent 选择当前活动交互会话。第一阶段同一时刻只服务一个会话；控制台或活动 RDP 会话变化时，旧 Host 进入 Draining，撤销输入、释放按键、停止采集，然后退出。Agent 再在新会话中启动 Host。

Host 使用分配到目标会话的 SYSTEM token，并绑定正确的 window station 和 desktop。Host 不显示 UI，不监听 TCP 端口，所有网络均主动出站。

## 5.1 本地 IPC

- 使用随机命名、严格 ACL 的 Windows named pipe。
- 仅允许 SYSTEM 连接；Agent 和 Host 双向验证对端 PID、会话 ID、映像路径及发布身份。
- Agent 创建 pipe 并启动 Host，Host 必须在启动期限内完成 challenge-response。
- pipe 只承载租约、会话授权、关闭和少量诊断，不承载视频。
- 会话 token 不出现在命令行、环境变量或普通用户可读文件中。
- Agent 控制连接失效或租约过期后，Host 停止输入并关闭所有桌面会话。

# 6. 服务端与会话生命周期

新增 desktop session kind。REST 创建桌面会话后：

1. Server 完成用户认证、RBAC、单控制者仲裁和审计。
2. Server 经 Agent 控制连接发送 SESSION_OPEN，包含短期 Host 侧授权。
3. Agent 校验目标活动会话和 Host 租约，通过本地 IPC 把授权交给 Host。
4. Host 直接拨号 Server 的 agent-side desktop WSS。
5. Browser 拨号 client-side WSS。
6. Server 粘合两侧并维持逻辑会话。

逻辑状态：

~~~text
Created → Opening → Open → Recovering → Open
                    └───────────────→ Closed
~~~

一侧断开后 Server 保留逻辑会话 30 秒。Host 崩溃或 Windows 会话切换时，Browser 保持连接并收到 recovering。Agent 启动新 Host 后，为同一逻辑会话签发新的单次授权。

每次 Host 重新附着递增 generation。Browser 收到新 generation 后清空 WebCodecs 队列和旧光标状态，重新等待 DISPLAY_INFO、CODEC_CONFIG 和 IDR。QoS 与输入状态不得跨 generation 继承。

Browser 短暂断线也可在 30 秒内重新附着；超过窗口后必须建立新逻辑会话。

# 7. 桌面协议

一条 desktop WSS 内同时包含控制和视频，但发送端必须使用独立逻辑队列和单一串行 writer：

- 控制队列：最多 256 条，可靠、优先发送。
- 视频队列：每个订阅者最多 3 帧，允许丢弃 delta。
- 输入与视频方向相反，浏览器输入不会排在 Host 的视频发送队列后。

## 7.1 控制消息

控制消息继续使用版本化 JSON text frame。主要消息如下：

| 方向 | 类型 | 关键字段 |
| --- | --- | --- |
| Browser → Host | CLIENT_HELLO | protocolVersion、supportedH264Profiles、maxWidth、maxHeight、maxFps |
| Host → Browser | DESKTOP_BEGIN | protocolVersion、generation、capabilities、hostMonoOriginUs |
| Host → Browser | DISPLAY_INFO | displayId、origin、width、height、scale、primary |
| Host → Browser | DISPLAY_CHANGED | generation、reason |
| Host → Browser | CODEC_CONFIG | codec、profile、width、height |
| Host → Browser | CURSOR_SHAPE | shapeId、hotspot、尺寸、RGBA |
| Host → Browser | CURSOR_POSITION | displayId、x、y、visible |
| Host → Browser | SESSION_STATE | state、reason、retry |
| Host → Browser | ERROR | stableCode、message、recoverable |
| Browser → Host | VIDEO_ACK | generation、displayId、receivedSeq、decodedSeq、renderedSeq、decodeQueueSize、decodeFps、visible |
| Browser → Host | REQUEST_KEYFRAME | generation、displayId、reason |
| Browser → Host | VIEWPORT_STATE | visible、canvasWidth、canvasHeight、devicePixelRatio |
| Browser → Host | INPUT_* | leaseId、seq、具体输入字段 |
| 双向 | PING / PONG | id、monotonic timestamp |

Browser 约每 500ms 发送累计 VIDEO_ACK；关键错误和 keyframe 请求立即发送。

Browser 连接后的第一条消息必须是 CLIENT_HELLO。它先用 VideoDecoder.isConfigSupported 探测约定的 H.264 profile 列表；Host 选择双方交集后才启动编码并发送 DESKTOP_BEGIN 与 CODEC_CONFIG。没有交集时返回 CODEC_UNSUPPORTED 并关闭会话。

## 7.2 视频二进制帧

视频帧使用固定 40 字节 little-endian header：

| 偏移 | 长度 | 字段 |
| ---: | ---: | --- |
| 0 | 4 | magic = XNCD |
| 4 | 1 | protocolVersion |
| 5 | 1 | messageKind，v1 中 1 = video |
| 6 | 2 | flags，bit 0 = keyframe |
| 8 | 4 | generation |
| 12 | 2 | displayId |
| 14 | 2 | reserved，必须为 0 |
| 16 | 4 | frameSeq |
| 20 | 8 | capturedMonoUs |
| 28 | 8 | encodedMonoUs |
| 36 | 4 | payloadLength |

header 后是完整 H.264 access unit，采用 Annex-B。每个 IDR 必须携带 SPS/PPS。frameSeq 在每个 generation、每个 display 内从 1 递增。

Browser 使用 Host 时间戳相对 DESKTOP_BEGIN 的时间原点构造 EncodedVideoChunk timestamp，不再用收到帧时的 performance.now。

Server 限制 text 和 binary frame 大小并验证会话状态，但不解析 H.264 payload。

# 8. 共享视频服务

每个显示器对应一个 DisplayVideoService：

~~~text
Capturer → Converter/Scaler → Encoder → Encoded AU
                                      ├→ Subscriber A
                                      ├→ Subscriber B
                                      └→ Subscriber C
~~~

服务具有 subscribers、newSubscribers、options 和 hibernation 状态。首个订阅者启动管线，最后一个离开后停止采集和编码。

新订阅者不能只接收一个历史 IDR 后直接跳到当前 P 帧。它进入 needsKeyframe 状态，Host 合并同一显示器上的多个请求，并只对下一次编码输入请求一次 IDR。订阅者从该新 IDR 开始入流。

同一编码器的主动关键帧请求至少间隔 500ms；正常流每 5 秒最多安排一个恢复性 IDR。编码器重建、配置变化和 generation 变化不受该间隔限制，但都必须记录请求原因。

每个订阅者持有小容量视频队列。队列满时：

1. 丢弃尚未发送的 delta。
2. 标记 needsKeyframe。
3. 停止向该订阅者发送 delta。
4. 合并触发全局新 IDR。
5. 从新 IDR 恢复。

控制消息不得静默丢弃。控制队列持续拥塞时关闭该订阅者并给出稳定错误码。

# 9. 采集与编码

## 9.1 采集梯子

1. DXGI Desktop Duplication 主路径。
2. 可恢复错误先重建 DDA device、output duplication 和 display binding。
3. 连续失败后切换 GDI。
4. 检测 input desktop 变化，并在普通桌面、锁屏和 UAC 桌面之间重新绑定。
5. WGC 不作为第一阶段生产主路径，只允许保留为诊断后端。

采集器输出统一 FrameRef，可表示 D3D11 texture 或 CPU BGRA buffer。DisplayVideoService 不依赖具体后端。

## 9.2 编码梯子

- 主路径：D3D11 texture → GPU 缩放与 BGRA/NV12 转换 → hardware Media Foundation H.264。
- 回退：CPU BGRA/NV12 → software Media Foundation H.264。
- Encoder 接口必须暴露 codec config、latency mode、bitrate、force-next-keyframe、drain、flush 和 backend diagnostics。
- v1 唯一必选 codec 是 H.264。VP9/AV1 仅保留接口扩展点。
- force-next-keyframe 在一次 ProcessInput 后立即消费，不能等待看到关键帧输出后才清除。
- 编码器有内部延迟时使用 drain/output 语义处理，不反复对缓存帧设置 force-key。

光标独立于视频传输和浏览器叠加。光标移动不触发全桌面编码。静止桌面允许视频降到 0 FPS，控制、光标和心跳继续运行。

分辨率、显示器、采集后端或编码器变化时递增视频配置版本，通知 Browser，flush decoder，并以新 IDR 恢复。

# 10. QoS 与反压

Host 为每个订阅者维护：

- 最近 received、decoded、rendered frameSeq
- send-to-ack 与估算 RTT
- decodeQueueSize 与 decode FPS
- 页面可见性
- 丢帧、等待关键帧和恢复次数

控制策略：

- 冷启动使用 15 FPS 和保守码率，稳定后升至目标 30 FPS。
- 协议允许最高 60 FPS，但第一阶段默认上限仍是 30 FPS。
- FPS 最多每秒调整一次；码率约每 3 秒调整一次。
- 活跃且可见观众中的最差反馈约束共享编码器。
- 隐藏标签页立即退出全局 QoS 样本；可见观众连续 3 秒落后时先降低其分发采样率，再进入 needsKeyframe。它不能无限期降低当前控制者体验。
- 网络延迟、浏览器解码不足和后台限速必须通过 RTT、ACK、decodeQueueSize 和 visible 分开判断。

QoS 算法在组件测试中使用确定性输入，具体阈值可配置，但状态转换和上限必须固定并可观测。

# 11. 输入与控制权

多人可以同时观看，但每个逻辑桌面会话同一时间只有一个 control lease。Server 决定授予、续期、移交和撤销；Browser 不能自行声明权限。

授权能力至少包含：

- desktop.view
- desktop.pointer
- desktop.keyboard
- desktop.secure_attention

Host 同时验证 Agent 下发的 capability、control lease、事件序号、generation 和过期时间。

## 11.1 输入类型

- POINTER_ABSOLUTE：显示器 ID、逻辑坐标、按钮状态。
- POINTER_RELATIVE：dx、dy、按钮状态。
- POINTER_BUTTON：按钮、down/up。
- POINTER_WHEEL：水平和垂直滚动。
- KEY_PHYSICAL：KeyboardEvent.code 对应的物理扫描码、down/up、repeat、modifier。
- TEXT_INPUT：Unicode 文本。
- SECURE_ATTENTION：独立授权的安全操作，不伪装为普通键盘组合。

Host 使用最多 512 条事件的独立有界输入队列。鼠标移动可以在两个 barrier 事件之间合并；按键、按钮和滚轮是 barrier，不允许丢弃或跨越合并。队列无法及时消费时撤销控制权，而不是无限积压。

绝对坐标映射到 Windows virtual desktop，支持负坐标、不同 DPI 和多显示器 origin。注入事件使用固定 dwExtraInfo 标记。

断线、generation 变化、控制权移交、租约过期或 Host 退出时，Host 必须释放所有记录中的按键和鼠标按钮。

# 12. 安全与审计

- Server 在创建会话前执行 RBAC，Host 只接受 Agent 转交的短期授权。
- Agent 与 Host 的本地 IPC 双向认证；任何跨会话、错误 PID、错误映像或过期 challenge 都拒绝。
- Host 无本地 TCP listener，无永久远程 token。
- control lease、Host lease 和 session token 都有明确 TTL、单一用途和撤销路径。
- Server 审计会话开始/结束、观看者、控制权授予/移交/撤销、UAC/SAS 等敏感操作。
- 审计不记录具体键盘文本、剪贴板内容或每次鼠标移动。
- 二进制帧、控制消息、队列、显示器数量、分辨率和恢复次数都有硬上限。
- Host 依赖只允许经审核的宽松许可证，例如 MIT、Apache-2.0 或 BSD；CI 运行许可证清单检查。

# 13. 错误处理与恢复

Agent 监督状态：

~~~text
NoSession → Starting → Ready → Draining
                 └→ Failed → BoundedBackoff → Starting
~~~

Host 桌面状态：

~~~text
idle
starting
capturing
recovering
no_session
permission_denied
encoder_fallback
failed
~~~

禁止用 capturing 占位但不产出画面。错误必须包含 stableCode、backend、lastSuccessfulFrame、recoverable 和 retryAfter。

局部降级顺序固定为：

1. 重建 DXGI capturer。
2. DXGI 切换 GDI。
3. 重建硬件编码器。
4. 硬件切换软件编码器。
5. 降低分辨率、FPS 和码率。
6. 报告 failed 并停止视频。

Host 重启采用有界指数退避，避免桌面或驱动异常造成重启风暴。恢复后必须使用新 generation 和新 IDR。

# 14. 可观测性

Host 和 Browser 至少记录：

- logicalSessionId、generation、displayId
- Host PID、Windows session ID、input desktop
- capture backend、encoder backend、codec profile、分辨率
- capture FPS、encode FPS、send FPS、decode FPS、render FPS
- bitrate、frameSeq、IDR 数量与请求原因
- subscriber queue、decodeQueueSize、drop count、needsKeyframe
- capture-to-encode、send-to-ack、decode、render 和估算端到端延迟
- Host restart、capturer rebuild、encoder fallback、session reattach

日志不包含授权 token 或具体输入文本。

# 15. 验证与验收

第一阶段正式测试桌面版 Chrome 和 Edge。建立会话前调用 WebCodecs codec capability probe；不支持所需 H.264 profile 时明确失败，不做服务端转码。

## 15.1 测试层次

- 单元：协议、header、generation、队列、关键帧合并、QoS、坐标和键盘映射。
- 组件：fake capturer、encoder、subscriber、Browser ACK 和 session reattach。
- Windows 集成：DDA、GDI、硬件/软件 MFT、SendInput、锁屏、UAC、显示器与 DPI。
- E2E：真实 XNC Server relay、真实 Chrome/Edge、真实节点 Agent/Host。
- 故障：杀 Host、切会话、断 Agent 控制连接、阻塞 Browser、编码器失败。
- 安全：token 重放、跨会话 IPC、普通用户伪造 Host、过期租约继续输入。

统一 HTML 测试动画包含 60Hz 大面积运动、颜色变化、帧计数器、单调时间、可点击目标、键盘回显和静止/低运动/高运动模式。

## 15.2 首期门槛

在实验节点、1920×1080、目标 30 FPS、网络 RTT 不高于 20ms 条件下：

- 首个可见解码帧 p95 不高于 1 秒。
- 高运动场景实际渲染不低于 27 FPS。
- 指针输入到画面反馈 p95 不高于 150ms。
- decodeQueueSize 不持续增长，稳态通常不超过 2。
- 正常 30 秒运行不存在 IDR 风暴；IDR 仅来自配置的周期策略、入流或恢复。
- 静止桌面稳定后视频接近 0 FPS，光标仍独立流畅移动。
- 慢观众在数秒内被降采样或等待 IDR，不长期降低控制者体验。
- Host working set 低于 350MB，连续运行 30 分钟无持续增长。
- Host 崩溃或 Windows 会话切换后，Browser 在 5 秒内进入新 generation 并恢复。
- UAC、锁屏、负坐标显示器和不同 DPI 下可正确观看和输入。

每次性能测试记录硬件、Windows 版本、采集/编码后端、浏览器版本、RTT 和原始统计。

# 16. 分阶段迁移

## 16.1 协议与观测基础

新增 desktop 协议、generation、ACK、Browser 解码指标和测试动画。可以暂接现有 screen 视频进行反馈链路实验，但不改变生产默认入口。

## 16.2 Host 进程骨架

建立独立 Rust workspace，完成 Host 启动、Windows 活动会话监督、受保护 IPC、租约和一次性 WSS 授权。只运行诊断，不采集或输入。

## 16.3 视频引擎

完成 DDA/GDI、H.264 encoder、DisplayVideoService、关键帧控制、单观众 Browser 闭环，然后增加多观众和 QoS。

## 16.4 输入与光标

完成独立光标、单控制者租约、键鼠/文本、断线释放和 UAC/锁屏验证。视频门槛未通过时，生产配置不得开放输入。

## 16.5 恢复与性能

完成 Host 重启重新附着、Windows 会话切换、GPU 主路径、慢观众隔离和长时间稳定性。

## 16.6 灰度切换

节点级配置：

~~~text
desktop_engine = legacy | rust_host
~~~

按实验节点、dev channel、小比例节点、默认路径逐级放量。每级都有回退开关。旧 helper 在全部门槛通过前保留。

# 17. 产品入口与兼容

- 新增 POST /api/nodes/{id}/desktop 和 desktop session kind。
- Web UI 节点页新增“远程桌面”，Browser 完成观看和控制。
- xnc screen 继续提供只读预览和 JPEG 快照。
- xnc rdp 最终默认打开 Browser desktop。
- xnc rdp --native 保留现有 mstsc/RDP tunnel。
- 新增 desktop.view 和 desktop.control 权限；secure attention 需要更高的显式权限。

# 18. 对现有设计的覆盖

本设计仅对桌面子系统作例外覆盖：

- 覆盖统一会话设计中 screen 只传 JPEG/H.264 预览、无输入的限定。
- 覆盖此前“全栈 Go 单语言”的绝对约束；Rust 只用于隔离的 Windows Desktop Host，Server、Agent 控制面、CLI 和 Web 继续保持 Go/TypeScript。
- 不改变中心 Server 中转、仅出站 443、节点身份、统一会话、RBAC、审计和发布体系。

# 19. Clean-room 规则

- 不从 RustDesk 主仓库复制源文件、函数、测试、协议定义或注释。
- 不把 RustDesk 仓库内 crate 作为 path、git 或二进制依赖。
- 不追求 RustDesk wire protocol 兼容。
- 实现依据为 Windows 官方 API 文档、本设计定义和独立编写的行为测试。
- 任何第三方 crate 必须从其独立上游取得并完成许可证审核。
- 代码评审需确认新增实现没有来自 AGPL 主仓库的逐行或结构性复制。
