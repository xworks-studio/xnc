# Inno Setup 统一安装器与凭据规范（设计稿）

- 日期：2026-09-03
- 状态：设计稿（未评审）
- 取代：`2026-09-02-oneclick-install-bootstrap-credentials-design.md` 中"安装期
  enrollment token 引导"的部分——token 在安装期不再出现，注册整体后移到首次使用

## 1. 背景与目标

当前安装链路（`/a/{token}` 一行命令 → server 生成 PowerShell → 下载 bundle →
sc.exe 建服务）有三个问题：

1. **凭据前置**：安装期就要拿到 enrollment token，管理员须先建 token 再到
   机器上操作；token 泄漏面大（聊天/工单里传递）。
2. **CLI 与 agent 割裂**：两套安装入口（`/a` 与 `/c`）、两套凭据存储
   （`%ProgramData%\XNCAgent` 与 `~/.xnc/`），同一台机器装两次、互不相认。
3. **脚本安装无卸载**：没有标准卸载器，服务/文件/防火墙规则靠手工清理。

本规范的目标：

- **G1** 用 Inno Setup 打包全部程序（xnc-agent / xnc-core / xnc-desktop /
  xnc-shell / xnc CLI），一个安装器、一个卸载器。
- **G2** agent 与 CLI 默认同装、共用机器级凭据（server 绑定 + 节点身份），
  装完即可用，无需二次配置。
- **G3** 安装全程**零凭据**：不验证、不输入、不落盘任何 token/密码。
- **G4** 首次使用时经 CLI 交互完成：账号密码登录 → 选择 cluster → 注册本机
  节点；登录/退出登录/注册/反注册语义统一、可逆。
- **G5** 安装器是**唯一**分发与更新载体：安装、修复、升级、卸载全部使用
  同一个 installer；现有 bundle 自更新机制（build-bundle.go + agent/updater
  的 tar.gz apply）整体退役。

非目标：

- 不做 macOS/Linux 安装器（CLI 单二进制分发维持现状）。
- 不引入 OAuth/SSO；账号密码登录沿用 `/api/auth/login`。
- 不改 agent↔server 的 WS 协议与会话模型。
- 不做静默批量部署（无人值守 `/SILENT` 由 Inno Setup 天然支持，但凭据后移
  后批量场景改为"装完后统一 register"，编排工具另行设计）。

## 2. 总览

```
管理员/用户                         XNC Server                    本机
──────────────                     ────────────                  ──────────────
1. 下载 installer ←─ /installer?channel=…（频道最新直流）
2. 双击安装（admin）                                              Inno: 复制文件
                                                                 注册 XNCAgent
                                                                 注册 XNCCore
                                                                 加 PATH
                                                                 （零凭据，服务
                                                                   进入未注册空转）
3. 开终端 xnc register
   ├─ xnc login ────────────────→ /api/auth/login ─→ JWT(24h)
   ├─ 列出 clusters ←──────────── /api/clusters
   ├─ 选 cluster（交互）
   ├─ JWT+cluster 经命名管道 ──────────────────────────────────→ agentctl pipe
   │                                                            agent: 生成身份
   │                            ←─ /api/clusters/{id}/nodes/register（JWT 授权）
   │                                                            绑定 server/cluster
   │                                                            建立 WS 连接
   └─ 节点上线确认 ←──────────── /api/nodes/{id}
```

日常使用：`xnc <任意命令>` 复用机器级绑定 + 用户级会话。登出只清用户会话，
节点照常在线。

更新闭环（安装器即更新器，详见 §9）：server 发布新 installer → agent 经
WS 推送/周期轮询发现 → 下载校验 → 静默执行安装器完成升级（含回滚看门狗）。

## 3. 打包规范（Inno Setup）

### 3.1 产物与命名

| 频道 | 文件名 | 说明 |
|------|--------|------|
| stable | `XNC-Installer-<version>.exe` | 生产频道 |
| dev | `XNC-Installer-dev-<version>.exe` | 开发频道（对应现有 `-dev` bundle 频道） |

`<version>` 与 agent 自报版本同源（构建期 `-ldflags` 注入
`xnc/agent/machineinfo.Version`）。安装器是**唯一**发布产物：CI 构建五个
二进制后仅打包 installer（无 bundle），同一文件服务首装、修复、升级与
卸载；升级即"同 AppId 静默重跑"，见 §9。

### 3.2 安装器参数（Inno Setup 脚本要点)

- `PrivilegesRequired=admin`，per-machine 安装（服务需要）。
- `DefaultDirName={autopf}\XNC`（`C:\Program Files\XNC`）。
- 组件：默认全选；`xnc-core/xnc-desktop`（远程桌面）与 `xnc-shell`（ConPTY
  shell）允许按需裁剪，`xnc-agent + xnc CLI` 不可拆（G2）。
- 服务注册是**可抛弃的派生态**：安装/升级/卸载统一为"停止 → 删除 → 重建"，
  不做存量服务的配置迁移（无 `sc.exe config`/binPath 修改路径——PS 5.1 对
  原生 exe 引号的破坏历史上踩过，见 install-xnccore.ps1 头注释）。删除重建
  不改变 `NT SERVICE\<name>` 身份（SID 由服务名派生），ProgramData 的 DACL
  全部继续有效。顺序：建 `XNCCore` → `XNCAgent`（agent 依赖 core 管道），
  卸载反序；重建时全量声明 binPath/启动类型/恢复策略。**argv 零密钥**原则
  不变，全部经 StateDir 文件 + DACL。
- PATH：写 `HKLM\...\Environment\Path`（追加 `C:\Program Files\XNC`），广播
  `WM_SETTINGCHANGE`；卸载时移除。
- 防火墙：出站 443 主动连接，无需入站规则（agent 是纯出站客户端）。
- `[Setup]` 版本信息（VersionInfoVersion/ProductName）与频道写入注册表卸载项，
  `Uninstall` 项里记录 `Channel` 与 `InstallDate`。

### 3.3 目录布局（安装后）

```
C:\Program Files\XNC\
  xnc-agent.exe      # XNCAgent 服务主体（enroll/connect/exec 引擎/updater）
  xnc-core.exe       # XNCCore 服务（SYSTEM 控制台管道服务，桌面采集宿主）
  xnc-desktop.exe    # 桌面会话进程（core 拉起）
  xnc-shell.exe      # shellhost（ConPTY）
  xnc.exe            # CLI
C:\ProgramData\XNC\                # 机器级状态（见 §5）
  identity.json      # 节点 Ed25519 身份（SYSTEM DPAPI）
  binding.json       # server/cluster 绑定
  core-secret.hex    # core 管道共享密钥（core 自建，DACL: SYSTEM+Admins）
  update-pending.json  # 更新进行中标记（§9.3，回滚看门狗依据）
  logs\  staging\        # 运行日志 / 更新包下载暂存
  installer-cache\      # 上一版本 installer（回滚源，仅保留 1 份，§9.2）
%USERPROFILE%\.xnc\config.json     # 用户级会话（见 §5.3）
```

> 状态目录统一为 `C:\ProgramData\XNC`（分支 `feature/oneclick-install` 已完成
> 的统一在此沿用；main 现状 `XNCAgent` 在安装器首启时迁移）。

## 4. 下载分发

- `GET /installer?channel=stable|dev` → `302` 到当前频道最新
  `XNC-Installer[-dev]-<version>.exe`（release store 存放，与 bundle 同库）。
- `GET /installer.json?channel=…` → 版本清单：`{version, url, sha256, size,
  releasedAt}`，供 `xnc upgrade --check`、CI 与编排工具消费。
- 官网/README 的入口统一为 `xnc.app/installer`；`curl -LO https://xnc.app/installer
  https://xnc.app/installer` 与浏览器直下均可。
- 安装器完整性：`installer.json` 提供 sha256；生产频道安装器目标为 Authenticode
  签名（构建侧接入，未签名期间以 sha256 为准，见 §13）。

## 5. 状态与凭据模型

### 5.1 两级凭据，职责单一

| 级别 | 内容 | 位置 | 归属 |
|------|------|------|------|
| 机器级（共用） | server 绑定、cluster、节点身份私钥、core 密钥 | `C:\ProgramData\XNC` | SYSTEM 持有；DACL 仅 SYSTEM+Administrators |
| 用户级（会话） | 登录 JWT、 remembered email、频道偏好 | `%USERPROFILE%\.xnc\config.json` | 各交互用户私有 |

**"共用凭据"的准确定义（G2）**：CLI 与 agent 共享机器级绑定与节点身份——CLI
在这台机器上天然以该节点身份上下文工作（`xnc status`/本地诊断/注册管理），
无需独立注册流程；但**用户登录会话是个人的**，不进入机器级存储：一台机器可
以有多个用户各自 `xnc login`，节点只有一个。用户 JWT 永不持久化到
ProgramData。

### 5.2 机器级文件与保护

- `identity.json`：Ed25519 私钥，Windows 上 DPAPI（SYSTEM 作用域）+ 文件
  DACL（继承自 StateDir，仅 SYSTEM/Admins）。私钥由 agent 服务进程生成与
  使用，**CLI 永不直接读取**（经 §6 管道间接驱动）。
- `binding.json`：`{server, clusterId, nodeId, registeredAt, channel}`，公开
  字段，无密钥。CLI 只读此文件判断"本机已注册到哪"。
- `core-secret.hex`：沿用现状（core 首启生成、DACL 锁定、agent 读取）。

### 5.3 用户级会话

`~/.xnc/config.json` 结构不变：`{server, token, remembered_email, channel}`。
`server` 字段以机器级 `binding.json` 为准（CLI 启动时若二者冲突，以 binding
为准并提示）。JWT 有效期沿用 24h，无 refresh token（开放问题 §15-3）。

## 6. 注册流程（首次使用）

### 6.1 未注册空转态（unconfigured）

安装完成即启动 `XNCAgent`，但无 `binding.json` 时进入空转：

- agent 周期性（5s）检查 StateDir；同时监听控制管道等待注册指令。
- 不发起任何外联（无 server 可连）；日志明确输出 `awaiting registration`。
- self-updater 在未注册时**不检查更新**（更新源即 server，无绑定即无源；
  更新机制见 §9，下同）。
- `xnc status` 显示 `installed, unregistered`。

### 6.2 `xnc register`（交互，一步完成）

1. **登录**：无有效 JWT 则内联走 login 流（email+password → `/api/auth/login`，
   JWT 存用户级 config；已有未过期 JWT 则复用，提示当前账号）。
2. **选 cluster**：`GET /api/clusters`（JWT 授权）列出用户可见 cluster；
   仅一个时确认即过，多个时编号选择。
3. **本机注册**：CLI 连 `\\.\pipe\xnc-agentctl`（见 6.3），发送
   `{op: "register", server, clusterId, jwt}`；agent 收到后：
   - 无身份则生成 Ed25519（SYSTEM/DPAPI）；
   - `POST {server}/api/clusters/{clusterId}/nodes/register`，body 同现有
     `/api/agent/enroll`（hostname/machineId/osVersion/agentVersion/publicKey），
     **Authorization: 用户 JWT**；成功返回 `{nodeId}`；
   - 原子写 `binding.json` → 立即建立 WS 连接 → 上线；
   - 应答 CLI `{ok, nodeId}`，JWT 用后即弃（内存中，不落盘）。
4. **确认**：CLI 轮询 `GET /api/nodes/{nodeId}`（JWT）至 `online`（≤10s），
   打印节点名与面板入口。

重复 register（已注册）：提示当前绑定，`--force` 走 deregister+register。

### 6.3 agent 控制管道（新增，本规范唯一新本地面）

- 名：`\\.\pipe\xnc-agentctl`；DACL：SYSTEM + Administrators + Interactive
  （注册需要，普通用户可注册但不能反注册/改绑定——反注册要求 Admins）。
- 协议：单行 JSON 请求/响应（与 core 管道风格一致）：
  - `{op:"register", …}` → `{ok, nodeId}` / `{error}`
  - `{op:"deregister"}` → `{ok}`（admin only）
  - `{op:"status"}` → `{state: unregistered|registered|online, nodeId,
    server, clusterId, channel, version, update?}`（`xnc status` 本机部分
    的数据源；update = 在途更新进度 `{phase: checking|applying, from, to}`，
    仅供 `xnc upgrade` 轮询，可缺省）
  - `{op:"upgrade", channel?}` → `{ok, triggered}`；channel 仅 stable|dev
    （白名单外 bad_request），非空且异于绑定时先原子改 binding.channel
    （§9.5）再按新频道立即检查应用。已在途（pending 或一次触发未收线）
    → `{ok:true, triggered:false, note:"update already in progress"}`（非
    错误，CLI 渲染 note 后转入 status 轮询）。无 admin 门（与 register
    同级）。
- 服务端点见 §11；管道本身只做转发与身份持有，不含业务逻辑。

### 6.4 server 侧新端点

`POST /api/clusters/{id}/nodes/register`（新）：

- 授权：用户 JWT + 对该 cluster 的成员关系（沿用现有 membership 判定；是否
  限定 admin/owner 角色 → 开放问题 §15-2）。
- 语义：等价于"服务端铸造一次性 enrollment token 并在同一事务内消费"——
  校验成员 → 复用现有 enroll 落库路径（machineId 去重、公钥绑定、NodeID 分配）
  → 写 audit（`register` 事件，含 userId 与 clusterId，天然可审计，取代现在
  token 创建+使用两条审计的拼接）。
- 错误码：403 非成员 / 409 machineId 已注册于其他 cluster（返回冲突 cluster
  名，CLI 提示走 `--force` 或管理端处理）/ 400 参数缺失。

## 7. 登录 / 登出 / 注册 / 反注册语义

| 命令 | 作用域 | 行为 | 对节点的影响 |
|------|--------|------|--------------|
| `xnc login` | 用户会话 | email+password → JWT(24h)，存 `~/.xnc/`；记住邮箱 | 无 |
| `xnc logout` | 用户会话 | 删 JWT（保留 remembered_email 与 channel） | **无**：节点继续在线 |
| `xnc register` | 机器 | 登录（如需）→ 选 cluster → 经管道注册本机 | 节点上线 |
| `xnc deregister` | 机器 | 经管道：agent 断开 WS、调 server 注销节点、删 `binding.json`（**保留** identity，重注册复用）；要求 admin | 节点下线并从 cluster 移除 |
| `xnc status` | 只读 | 本机安装/注册/在线状态 + 当前用户会话 | 无 |

- server 注销端点：`DELETE /api/nodes/{id}`（现有管理端点授权规则不变；
  agent 侧调用发生在 deregister 流程内，以机器身份签名，具体复用 agent WS
  控制通道下发，不在 HTTP 面新增 agent 凭据）。
- `logout` 明确**不等于** deregister——规范级区分：会话（人）与注册（机器）。

## 8. 卸载规范（Inno Setup uninstaller）

顺序（`[UninstallRun]` + `[Code]`）：

1. 清理更新态：删除回滚看门狗计划任务与 `update-pending.json`（取消任何
   进行中/待回滚的更新）、`installer-cache\`、`staging\`、`xnc.exe.old`。
2. 停止并删除服务：`XNCAgent` → `XNCCore`（反序）；容忍"已不存在"。
3. 结束残留 `xnc-desktop.exe` / `xnc-shell.exe` 子进程（按镜像路径匹配）。
4. 删除 `C:\Program Files\XNC`、PATH 项、注册表卸载项。
5. **数据询问**（卸载向导单选，默认"保留"）：
   - 保留（默认）：`C:\ProgramData\XNC` 原样保留（identity 在，重装后
     `xnc register` 直接复用身份，节点历史可延续）；
   - 清除：删除 ProgramData\XNC（含日志）。**卸载器不反注册**——server 上的
     节点记录由 deregister（主动）或 server 离线回收策略（被动）处理；卸载
     确认页明示"节点仍登记在 server，如需移除请先运行 xnc deregister"。
6. 静默卸载（`/SILENT`）默认走"保留"（`/PURGEDATA` 自定义开关强制清除）。

修复安装：installer 检测到已安装同版本 → Inno 内建 repair；跨版本直接装
（升级见 §9）。

## 9. 更新流程（安装器即更新器）

installer 是唯一分发载体：安装、修复、升级、卸载共用同一安装器与同一版本
线。agent 的职责从"解包搬文件"退化为**更新编排**：发现 → 下载校验 → 静默
执行安装器 → 看门狗兜底回滚。agent/updater 中 bundle 下载/校验/apply/回滚
代码删除，保留编排骨架。

### 9.1 触发

- **WS 推送**：server 发布新版本后经 agent 控制通道下发
  `update_available {version, sha256, url}`，agent 立即执行一轮检查。
- **周期轮询**：agent 每 6h `GET /installer.json?channel=<绑定频道>` 兜底。
- **手动**：`xnc upgrade [--channel stable|dev]` 经 agentctl 管道要求立即
  检查并应用。
- 节流：同一版本失败后指数退避（1h → 4h → 24h），成功清零；校验失败或
  安装器退出码非 0 的版本记入本地黑名单，直至 installer.json 出现新 version。

### 9.2 下载与校验

1. 按 installer.json 的 `url + sha256` 下载到 `ProgramData\XNC\staging\`。
2. SHA-256 比对；安装器完成 Authenticode 签名后追加验签（§13）。
3. 回滚源确认：`installer-cache\` 中必须存在**当前版本**的安装器
   （首装与每次成功更新后由安装器/agent 写入，仅保留最近 1 份）；缺失则
   先补拷（从 staging 或 server 重新下载当前版本安装器）再继续。

### 9.3 应用（静默安装）

agent（SYSTEM）执行：

```
<installer> /VERYSILENT /SUPPRESSMSGBOXES /NORESTART /DIR="C:\Program Files\XNC"
```

同一 AppId 检测到已安装 → 安装器走升级路径（`[Code]`）：

1. **agent 侧预备**（执行安装器之前）：写 `update-pending.json`
   `{from, to, startedAt, deadline = now+15min}`；注册一次性回滚看门狗
   计划任务（§9.4）。
2. `PrepareToInstall`：停止 XNCAgent → XNCCore；结束以安装目录为镜像的
   残留 `xnc-desktop.exe` / `xnc-shell.exe` 进程；对正被用户占用的
   `xnc.exe` 先改名 `xnc.exe.old`（Windows 允许 rename 运行中的 exe，
   用户会话不受影响，下次启动即新版），清除历史 `.old`。
3. 覆盖复制五个程序文件（服务已停，无占用冲突）。
4. **删除并重建服务** XNCCore → XNCAgent（§3.2 派生态原则：首装与升级同一
   条路径，无"已存在则确保"分支；STOP 后再删，避免删除标记挂起导致同名
   `New-Service` 失败——创建带短重试）。
5. 启动服务；退出码回传发起者。

远程会话中断窗口 = 停服务到新 agent 上线（秒级，与原 bundle apply 同级）；
升级后 agent 重连 server，会话恢复语义不变。升级期间对同一节点串行化：
pending 存在时拒绝再次触发更新。

### 9.4 回滚

原则：**回滚 = 静默重跑 installer-cache 中的旧版安装器**，无独立搬移逻辑。

- 新 agent 启动自检：版本自报 == pending.to、服务健康、WS 可达 server；
  通过 → 删 `update-pending.json` 与看门狗任务、写 installer-cache、
  audit `update_ok`，更新完成。
- 三类失败的兜底：
  1. **新 agent 起不来**：SCM 服务恢复策略先重启若干次；看门狗（schtasks
     一次性任务，SYSTEM，deadline 触发）发现 pending 未清 → 直接静默执行
     旧版安装器回滚；
  2. **起来了但自检不过**：新 agent 主动执行回滚（不等人）；
  3. **安装器中途失败**（退出码非 0，pending 未清）：看门狗回滚。"服务已删
     但新装失败"的中间态同样被覆盖——安装器本就无条件重建服务（§3.2）。
- 回滚后 audit `update_rollback {from, to, reason}`；该版本进入 §9.1 黑名单。
- 看门狗自身不可靠的兜底：pending 超过 deadline 24h 仍未清理（极端：schtasks
  被禁用），agent 任意一次成功启动时检测到过期 pending 也触发回滚。

### 9.5 降级与跨频道

- 常规路径拒绝降级（版本比较）；看门狗回滚不受此限（直执旧安装器）。
- 跨频道切换仅经 `xnc upgrade --channel`：改绑定频道 → 按新频道清单检查
  并应用，audit 记维护操作；不允许自动跨频道。

### 9.6 版本一致性

installer.json 的 `version` == agent 自报版本（`-ldflags` 注入，版本单一来源
不变）；安装器完成标记与 agent 自报不一致视为失败，走回滚。

## 10. CLI 命令面变更汇总

| 命令 | 变更 |
|------|------|
| `xnc login/logout` | 不变；login 后若本机 unregistered，尾行提示 `xnc register` |
| `xnc register`（新） | §6.2 |
| `xnc deregister`（新） | §7，要求 admin |
| `xnc status`（增强） | 增加本机安装/注册块（经 agentctl pipe） |
| `xnc upgrade [--channel]`（增强） | 经 agentctl 管道触发 agent 立即检查并静默应用更新（§9.1）；CLI 自身不替换文件 |

## 11. Server API 变更汇总

| 端点 | 类型 | 说明 |
|------|------|------|
| `GET /installer?channel=` | 新 | 302 最新安装器 |
| `GET /installer.json?channel=` | 新 | 版本清单（version/sha256/size/releasedAt） |
| `POST /api/clusters/{id}/nodes/register` | 新 | 用户 JWT 授权的节点注册（§6.4） |
| `GET /a/{token}` `/a-dev/…` `/c` `/c-dev` `/install/*` | 已移除 | 未上线直采终态（§14）：一行流在上线前整体删除，无存量迁移 |

## 12. 构建与发布流水线（增量）

1. CI 构建五个二进制（版本注入）。
2. Inno Setup（`ISCC.exe`）打包 `XNC-Installer[-dev]-<version>.exe`（唯一发布
   产物；签名接入后同一产物先签再上传）。
3. release store 上传：installer 制品 + installer.json 清单（同一次发布原子提交，清单为动态端点）。
4. `build-bundle.go` 与 server 的 bundle 发布/下载面**退役删除**；
   `deploy_srv.py` 无变化（release store 已由其管理）。

## 13. 安全模型

| 威胁 | 对策 |
|------|------|
| 安装包被替换/中间人 | HTTPS + installer.json sha256；目标态 Authenticode 签名（未签名期 sha256 为唯一手段，installer.json 由 server 动态生成） |
| 安装期凭据泄漏面 | **安装期零凭据**（G3）：无 token、无密码、无绑定 |
| 节点私钥被本机其他用户读取 | 私钥仅 SYSTEM 生成/持有（DPAPI SYSTEM + DACL），CLI 经管道间接使用 |
| 用户 JWT 泄漏到机器级存储 | JWT 仅存用户 profile；register 时经管道内存传递、用后即弃 |
| 恶意本地用户把陌生 server 注册到本机 | agentctl register 允许 Interactive（注册本身要改绑定需 admin？——注册新绑定允许普通用户，**改绑/反注册仅 Admins**；`--force` 重绑走 Admins） |
| 恶意/损坏更新包在 SYSTEM 执行 | 下载后 sha256 强校验（installer.json 由 server 动态生成）+ 目标态 Authenticode 验签；执行前强制确认 installer-cache 回滚源就位（§9.2） |
| 旧 token 流残留 | 未上线直采终态：一行流端点已在上线前删除（§14）；token 创建 API 保留给编排场景 |

## 14. 迁移与兼容（未上线直采终态）

本规范在系统**上线前**直接采纳为终态：不存在已上线的 token 安装节点，
也没有存量 bundle 自更新节点，因此**无任何迁移路径**——一行流
（`/a/*`、`/c*`、`/install/*.ps1`）与遗留 bundle 更新通道（`UPDATE_OFFER`、
`/api/agent/bundle`、上传端点的 bundle 必填部件、`build-bundle.go`）
在上线前已整体移除，未经历"deprecated 2 个版本周期"的过渡期。
release 上传仅接受 `setup`（必填）+ `cli`（可选）；仍传 bundle 部件
会得到明确的 400（bundle channel retired）。

- **CLI 老用户**（`~/.xnc` 已有 JWT）：装安装器后无需任何动作，config 继续
  有效；`server` 字段冲突时以 binding 为准。
- **`feature/oneclick-install` 分支**：其 install/bundle 端点与 enroll 子命令
  被本规范取代（register 管道方案替代 agent 内置 enroll）；分支中服务布局、
  StateDir 统一、停服务换文件等修复**保留吸收**。

## 15. 开放问题

1. `register` 是否要求 cluster admin 角色，还是所有成员可注册自己管理的机器
   （倾向：成员即可，audit 记 userId；限额策略后置）。
2. machineId 冲突（同机重装后 OS 重装导致 machineId 变化）的身份延续策略：
   identity.json 保留时按 nodeId 复用，还是永远新节点？
3. JWT 24h 无 refresh：CLI 静默过期体验差，是否加 refresh token 或设备授权
   （建议后续独立规范）。
4. 安装器签名证书选型与 CI 接入（Windows EV 证书 vs Azure Trusted Signing）。
5. 更新的分批策略：按 cluster 灰度/百分比放量、维护窗口（延迟到指定时段）
   是否需要（当前设计为全量即时 + 失败黑名单回滚，server 侧仅推不控）。
