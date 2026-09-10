# xncidd — vendored Indirect Display Driver (IDD)

"XWorks XNC Virtual Display"：XNC 的虚拟显示器（IDD）驱动与配套控制工具。

## 上游出处

| 组件 | 上游 | 许可 | 检出 |
|---|---|---|---|
| 驱动本体 | <https://github.com/rustdesk-org/RustDeskIddDriver>（基于微软官方 [Indirect Display Driver Sample](https://github.com/microsoft/Windows-driver-samples/tree/master/video/IndirectDisplay)，IddCx 1.4） | MS-PL | main @ 2026-09-10（`git clone --depth 1`） |
| 控制工具 | 同上 `RustDeskIddApp/`（IddController.c/h 原样保留，main.c 改写为非交互 CLI） | MS-PL | 同上 |

上游 LICENSE 见本目录 `LICENSE`（MS-PL，逐文件级 copyleft：对上游文件的修改
保留 MS-PL 声明；XNC 新增文件 `XncIddCtl/main.c` 为 XNC 自有代码）。

## XNC 相对上游的改动（最小化，便于回迁上游修复）

1. **身份重命名**：服务/硬件 ID/设备名/厂商串 → `XncIdd` / `ROOT\XncIdd` /
   "XWorks XNC Virtual Display" / "XWorks Studio"。
2. **新设备接口 GUID** `{0b6910e4-b09f-48a9-8513-6628415e1b45}`
   （`GUID_DEVINTERFACE_XNCIDD_DEVICE`）——与上游 RustDesk 驱动可共存一机。
3. **EDID 表替换**：单一 1920×1080@60 显示器（EDID 名 "XNC Virtual"，
   校验和 0xD5），保留表驱动 + IOCTL 热插拔架构不变。
4. **WPP 跟踪 GUID** 更换为 `65d5c69d-325c-4396-9865-586f8e18c8db`。
5. `XncIddCtl/main.c` 为非交互 CLI（上游为 `_getch` 交互式，无法远程驱动）。
6. 驱动输出名 `XncIdd.dll`（vcxproj `TargetName`）。

## 控制协议（与上游逐字节兼容，IOCTL 码不变）

- 设备由控制方经 `SwDeviceCreate`（硬件 ID `XncIdd`）动态创建；
  生产路径 agent 以 `SWDeviceLifetimeHandle` 持句柄（崩溃自动移除设备），
  测试工具 `XncIddCtl create` 用 `SWDeviceLifetimeParentPresent`（跨进程存活）。
- 显示器热插拔经设备接口 `CreateFile` + `DeviceIoControl`：
  `IOCTL_CHANGER_IDD_PLUG_IN`（`CtlPlugIn{ConnectorIndex, MonitorEDID, ContainerId}`）、
  `IOCTL_CHANGER_IDD_PLUG_OUT`、`IOCTL_CHANGER_IDD_UPDATE_MONITOR_MODE`。
  详见 `XncIddDriver/Public.h` 与 `XncIddCtl/IddController.c`。
- Go 侧镜像实现：`src/agent/display/idd_windows.go`。

## 构建与签名

- 构建：WDK 驱动工具集（WindowsUserModeDriver10.0）+ msbuild
  `-p:Configuration=Release -p:Platform=x64 -p:SignMode=off`（`TargetName=XncIdd`）。
- 签名：dll 内嵌签名 + `XncIdd.cat` 目录签名，用仓库自签 codesign.pfx
  （signtool，SHA256 + 时间戳）；信任经安装器现有 certutil Root +
  TrustedPublisher 分发。UMDF 为用户态驱动，走 Authenticode 链校验，
  无需 testmode（2026-09-10 XIAOXIN 无 testmode 真机验证通过）。
- 安装器集成：`src/installer/build.ps1` 的 `Build-IddDriver`；
  驱动包由 `pnputil /add-driver` 入 store（`src/installer/xnc.iss` idd 组件）。
