# 2026-09-10 IDD 虚拟显示器（"XWorks XNC Virtual Display"）实施记录

## 目标与策略（用户确认）

- 笔记本盒盖后 RTV 远程会话仍能看到可用桌面：引入 IDD 虚拟显示器驱动。
- **生命周期策略（2026-09-10 终版）**：
  - **设备常驻后台**：SW 设备在 agent 生命周期内创建一次，不随会话拆建
    （Handle 生命周期——agent 崩溃/退出自动移除）；
  - **屏幕仅在有控制请求接入后才创建**：desktop（RTV）会话活跃 且
    （盒盖 ∨ 无物理输出 ∨ `XNC_IDD_FORCE_LID` 调试旋钮）时插屏；会话
    结束自动移除（仅自动创建的）；手动 `xnc display on` 保持到 off。
    无会话时盒盖不产生任何屏幕。
- 首期 = 端到端最小闭环：驱动 + 安装器可选组件 + agent 会话钩子 +
  `xnc display on|off|status`。

## 交付物

| 组件 | 位置 | 说明 |
|---|---|---|
| IDD 驱动（UMDF/IddCx 1.4，C++） | `src/third_party/xncidd/`（vendored [RustDeskIddDriver](https://github.com/rustdesk-org/RustDeskIddDriver) + 微软 [IddSample](https://github.com/microsoft/Windows-driver-samples/tree/master/video/IndirectDisplay) 基底） | "XWorks XNC Virtual Display"，1920×1080@60 EDID，IOCTL 热插拔；**XNC 增补 `IOCTL_CHANGER_IDD_GET_STATUS`**（插拔/swapchain 激活状态，session 0 唯一权威信号） |
| 控制编排（Go） | `src/agent/display/` | SwDeviceCreate（Handle 生命周期：agent 崩溃自动消失）+ IOCTL 插拔 + 盒盖监听（RegisterPowerSettingNotification + message-only 窗口）+ 3s 轮询兜底 + 状态机 |
| 会话钩子 | `src/agent/desktop/handler.go` | SessionStart 先建屏后 StartCapture；SessionEnd 先 StopCapture 后拆屏 |
| 控制面 | agentctl 新 op `display`（on/off/status）+ CLI `xnc display` | 注册态无关，本地管道 |
| 安装器 | `build.ps1`（WDK msbuild + signtool 签名 dll/cat）+ `xnc.iss` 组件 `idd`（MinVersion 10.0.19041；ssPostInstall pnputil /add-driver；卸载 CurUninstallStepChanged pnputil /delete-driver + /remove-device） | 驱动包随安装器分发 |

## 关键实现事实（踩坑记录，供后续维护）

1. **签名路线验证**：UMDF 是用户态驱动，自签 Authenticode + Root/
   TrustedPublisher 信任即可在无 testmode 的 Win11 上加载（XIAOXIN 实测；
   与 RustDesk 官方 test-sign 分发同机制）。
2. **SwDeviceCreate 在 cfgmgr32**：现代 Windows System32 无实体
   `swdevice.dll`（SDK 的 swdevice.lib 也转发到 cfgmgr32）——Go 侧必须从
   `cfgmgr32.dll` 解析 SwDevice*，否则 LazyProc panic 杀进程（已加
   recover 兜底：display op 永不杀死 agent）。
3. **CapabilityFlags 真值** 0x01/0x02/0x08（Removable/SilentInstall/
   DriverRequired），含未定义位 → E_INVALIDARG。
4. **IOCTL 码 12 位截断**：CTL_CODE 把 Function 截到 12 位——PLUG_IN/
   PLUG_OUT/GET_STATUS 实际为 0x0030C004/0x0030C008/0x0030C010
   （C 探针编译 Public.h 打印验证）。
5. **session 0 无显示枚举**：服务会话里 EnumDisplayDevicesW 恒空、DXGI
   枚举 NOT_FOUND——物理屏判定改为"空枚举=未知→保守不触发"（防桌面机
   误建虚拟屏），虚拟屏激活判定改走驱动 GET_STATUS。盒盖事件是 session
   无关的，为笔记本场景主触发。
6. **UTF16 multi-sz**：`UTF16PtrFromString` 拒绝内嵌 NUL，需从干净串
   构造后手动补双终止符。
7. **SwDevice 幻影实例**：失败尝试会在 SWD 枚举器留下半死实例（后续
   create 报"已存在"且无法 remove）——需重启清理；agent 侧 Handle 生命
   周期在正常路径不会产生（崩溃即自动移除，已验证）。
8. **安装器不能跑在 exec 会话里**：安装器停掉 agent 即杀死自己宿主——
   必须经计划任务（schtask，SYSTEM、脱离会话）执行（生产更新编排本就
   如此；实验脚本已固化此模式）。

## 验收结果（XIAOXIN = 小新 Pro 14 IAH10，合盖真机，XNC 通道全程验证）

- **签名可行性（T0）**：pnputil 接受自签驱动包（oem109），无 testmode
  加载成功，虚拟屏 active+primary+attached。
- **手动路径（T5）**：`xnc display on` → 设备创建 → 插屏 → 驱动确认
  swapchain 激活（virtual active: yes）；`off` → 干净拆除；`status` 全字段。
- **会话自动触发（T5）**：xnc.app 面板 → 节点 → Remote Desktop →
  agent 自动建屏（auto: yes, virtual active: yes）；RTV 链在线
  （H.264 h264_qsv 硬编、WebTransport、完整帧 46、中继 cn-hangzhou）。
- **崩溃安全（T5）**：agent 重启 → Handle 生命周期自动移除虚拟屏，
  零幻影残留。
- **安装器路径（T5）**：0.10.13 同版本安装器（含 idd 组件）经计划任务
  静默安装成功：pnputil 入 store、服务重建、节点回归在线。
- **待补**：卸载零残留证据采集（机器等 23:59 自动重装恢复后补跑一条龙
  任务：卸载→残留核验→重装）；真实 lid 开盖路径等你回机器后补验。

## 与既有 spec 的有意偏差（记录在案）

1. §13.3 IDD 管理原定 xnc-core（core.proto CreateVirtualDisplay）——
   RTV 重构后控制面在 Go agent，实现于 `src/agent/display`；core.proto
   条目保持遗留未实现。
2. 触发策略更克制（会话驱动，非常驻）。
3. "单独安装包" → 统一安装器可选组件（安装器是唯一分发载体）。
4. 验证通道 PS remoting → XNC 自身（用户指定；TB16G7 已弃用）。

## 盒盖过渡优化（2026-09-10 追加，用户实测反馈）

开盖远控中合盖 → 视频流恢复的过渡链优化（延迟来源 → 措施）：

| 延迟来源 | 措施 |
|---|---|
| lid 事件后 3s 轮询才反应 | lid 电源事件经 notify 通道即时信号 Manager（`reconcile` 不等轮询） |
| 触发时才创建设备（异步回调 + 接口就绪重试） | **会话开始即预创建设备**（无显示器），合盖时只剩插屏 + OS 模式提交；接口重试 25×1s → 200ms×10 + 1s×5 |
| 激活等待轮询粒度 | 200ms → 100ms |
| host 拓扑巡检 1s | 500ms |
| 面板消亡错误先切 GDI 掩盖一轮 | `ConnectionReset/ConnectionAborted`（ACCESS_LOST/会话断开）跳过 GDI 掩盖直接重建重枚举 |
| pipeline 重建退避 200ms 起步 | 100ms 起步（封顶 3s 不变） |

host 侧改动本地无 vcpkg 无法编译验证——由 CI host-rust 矩阵把关（PR 门禁）。

## 后续 PATCH（不在本期）

- 多显示器采集源切换（SWITCH_DISPLAY）：覆盖"盒盖但面板仍在线"场景。
- 远程开关：Web UI/API/审计/capability `display.virtual`。
- CI 增加 WDK 驱动构建矩阵（现 build.ps1 缺 WDK 告警跳过）。
- EV 证书 Attestation 迁移（当前实验用一次性 spike 证书；**生产签名需
  恢复真实 codesign.pfx——本机 deploy/.env 与 pfx 已被清理**）。
- 盒盖面板在线时的触发语义（lid 事件 vs 无输出）按真实开盖测试校准。
