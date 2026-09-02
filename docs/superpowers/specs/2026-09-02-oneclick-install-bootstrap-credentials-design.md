# XNC 一键安装与引导凭据链路 系统设计

> 版本 v1.0 · 2026-09-02 · 状态：待审阅
> 一句话定位：让终端用户在新 Windows 机器上粘贴一行 PowerShell 命令即完成 agent + core 安装与注册，且引导链路中一次性凭据零持久化、服务 argv 零 secret。

## 1. 背景与问题

### 1.1 凭据体系总体规划（四子项目拆分）

「优化新机器安装流程 + 全面凭据管理」经拆分为四个子项目，各自独立走 spec → plan → 实现循环：

| # | 子项目 | 内容 | 依赖 |
|---|--------|------|------|
| 1 | **一键安装 + 引导凭据链路（本 spec）** | 修复 `/a/{token}` 全链路、状态目录统一、token 不泄露进服务 argv、core 服务自动安装 | 无 |
| 2 | 节点身份生命周期 | 重装/换机重新绑定、旧节点退役、身份备份恢复策略 | #1 的 enroll 接口 |
| 3 | 服务端密钥治理 | `XNC_JWT_SECRET` 等环境变量改文件管理、自动生成与轮换、admin 引导密码卫生 | 无（独立） |
| 4 | 用户会话与多用户授权 | JWT 会话加固（刷新/吊销）、角色权限、按用户划分节点访问 | #3 |

### 1.2 现状问题（探索结论）

1. **一键安装链路服务端即坏**：README 宣传的 `curl -sL xnc.app/a/<token> | cmd` 路径中，生成的脚本调用 `/install/bundle`、`/install/cli-binary`，这两个路由未在 `router.go` 注册（404 落进 SPA）；脚本还调用需要 JWT 的 `/api/cli/latest`（401）。
2. **生成脚本本身有语法/逻辑 bug**：`install_scripts.go` 的 sc.exe 段使用了 PowerShell 自动变量 `$args`（运行时为空）、同一行重复三次 `sc.exe config/create`、`$svcArgs` 含空格路径不加引号。
3. **一键流程从不安装 XNCCore 服务**：脚本复制 `xnc-core.exe` 但不建服务，`core-secret.hex` 永不生成，desktop/shell/exec 全部 `CORE_UNAVAILABLE`，需要人肉执行 `install-xnccore.ps1` 且 `-StateDir` 必须与 agent 状态目录手动对齐。
4. **注册令牌永久泄露进服务 argv**：一次性 enrollment token 写入 binPath（`sc qc XNCAgent` 任何本地用户可读）。
5. **状态目录三套约定**：agent 默认 `%ProgramData%\XNCAgent`、快捷脚本 `C:\ProgramData\XNC`、开发脚本 `%ProgramData%\XNCAgentDev`，core secret 对齐依赖人工。

### 1.3 成功标准

- 终端用户（本地管理员）粘贴一行命令，UAC 确认一次，2 分钟内双服务 Running、节点在服务端上线、desktop/shell/exec 可用。
- 全程任何文件、SCM argv、注册表中不存在 enrollment token；`sc qc XNCAgent` 与 `sc qc XNCCore` 输出无任何 secret。
- 整条命令可安全重复执行（幂等）；任一步失败在控制台输出人话错误与下一步动作。
- 新装机器状态目录唯一：`C:\ProgramData\XNC`；存量已部署机器升级不受影响。

## 2. 范围

### 2.1 做

- 入口命令改为 PowerShell 原生 `irm | iex`，`/a/{token}` 直接返回 PS 脚本。
- 服务端新增 `/install/bundle` 路由（enroll-token 鉴权、只校验不扣减）；重写 `install_scripts.go` 模板。
- agent 新增 `enroll` 一次性 CLI 子命令；扩展 `install` 子命令同时安装 XNCCore + XNCAgent，argv 去除 `--token`。
- 默认状态目录统一为 `C:\ProgramData\XNC`。
- 错误处理矩阵与端到端验收清单。

### 2.2 不做（明确出界）

- 代码签名 / SmartScreen 规避（独立子项目，先以文档提示绕过）。
- 换机/重装的身份迁移与自动 re-bind（子项目 2；本 spec 仅保证 409 错误信息可指引）。
- 服务端 secrets 治理、用户会话/RBAC（子项目 3、4）。
- CLI 安装路径（`/c`）的体验优化——仅保证其不被本变更破坏。
- 非管理员（per-user）安装模式。

## 3. 总体架构

### 3.1 角色分工原则

PS 脚本只做下载、解压、提权；凡涉及服务创建、凭据、文件保护的操作全部在 Go 二进制内（复用已验证的 `svcapp` 引号处理与 core secret DACL 机制）。

```
管理员                          终端用户新机器
  │                                │
  │ xnc token create <cluster>     │
  │ → 得到一行命令发给用户           │
  └────────────────────────────────▶ irm https://xnc.app/a/<token> | iex
                                        │
                                   PS 脚本（服务端生成）
                                   1. 检测管理员，否则 UAC 自提权重启
                                   2. GET /install/bundle?token&channel → sha256 校验 → 解压到 C:\Program Files\XNC
                                   3. xnc-agent.exe enroll --server --token --state-dir C:\ProgramData\XNC
                                   4. xnc-agent.exe install --server --state-dir C:\ProgramData\XNC   ← 无 token
                                   5. 轮询服务状态 + 节点上线 → 打印成功
```

### 3.2 关键不变量

1. **服务 binPath 零 secret**：enroll 在服务创建之前完成，token 消费后从流程消失；`install` 的 argv 不含 token。
2. **下载不烧 token**：`/install/bundle` 校验 token 有效性但不扣减使用次数，扣减仅发生在 `/api/agent/enroll`，下载重试安全。
3. **enroll 幂等可重试**：同 (cluster, machineId) + 同 pubkey 的重复 enroll 返回成功（复用服务端既有幂等语义），支持「enroll 成功但 install 失败后整命令重跑」。

## 4. 服务端设计

### 4.1 路由与 handler

| 路由 | 变更 | 说明 |
|---|---|---|
| `GET /a/{token}` | 改为直接返回 PS 脚本文本（`Content-Type: text/plain; charset=utf-8`） | 不再生成 .cmd 中转；`/a-dev/{token}` 渠道变体保留 |
| `GET /install/bundle?token=&channel=` | **新增** | enroll-token 哈希校验（存在、未过期、未用尽），不扣减；流式返回与 `/api/agent/bundle` 相同的 release zip（按 channel 选择 release） |
| `GET /c`、`GET /c-dev` | 保留 | CLI 安装变体，不优化仅不破坏 |
| `/api/cli/latest` | 脚本不再调用 | 版本信息由 bundle 内 manifest 提供，路由鉴权问题自然消失 |

token 创建流程不变：`xnc token create <cluster>`，30 分钟 TTL（`XNC_ENROLL_TOKEN_TTL`）、默认 maxUses 1、DB 仅存 sha256 哈希。

### 4.2 脚本模板重写（install_scripts.go）

新模板职责收敛为五步：提权检测与 UAC 自重启、下载 bundle（带重试与进度）、sha256 校验、解压到 `C:\Program Files\XNC`、顺序调用 agent 子命令并转发退出码、最终验证与结果打印。明确禁止：模板内出现任何 `sc.exe` 调用、token 落盘、`$args` 自动变量使用。

最终验证的定义：双服务 SCM 状态为 Running，且 agent 首次成功建立服务端连接后在 `<StateDir>` 写入 `install-connected.ok` 标记（复用更新流程的 marker 模式），脚本对该标记做有界等待；等待超时输出日志指引，不视为安装失败（服务已装好，连接问题另行排查）。

## 5. Agent 设计

### 5.1 新增 `enroll` 子命令（一次性 CLI）

- 参数：`--server`、`--token`、`--state-dir`。
- 行为：与 `EnsureEnrolled` 相同逻辑（生成 Ed25519 密钥 → `POST /api/agent/enroll` → 写 `identity.json`，DPAPI LOCAL_MACHINE 加密、0600、目录 0700）。
- 退出语义：成功打印节点名退出 0；identity 已存在且 pubkey 相同则幂等成功；409 打印「机器已注册但本地凭据丢失，请联系管理员重置节点」退出非 0。
- 非交互、无服务依赖，可在服务安装前独立执行。

### 5.2 扩展 `install` 子命令（装双服务）

- **先装 XNCCore**：binPath = `"<InstallDir>\xnc-core.exe" --service XNCCore --secret-file "<StateDir>\core-secret.hex"`（`install-xnccore.ps1:75-79` 逻辑的原生移植，含空格路径引号处理）。core 首启自建并 DACL 锁定 secret 文件（机制不变）。
- **再装 XNCAgent**：argv 为 `["run", "--server=...", "--state-dir=..."]`，**不含 `--token`**；`buildServiceArgs` 删除 token 参数。
- **幂等**：服务已存在 → 校验 binPath 与期望一致（不一致则输出警告与处理指引，不静默改写）→ 确保运行。
- 存量机器 binPath 中残留的旧 `--token=` 参数：agent 启动时忽略该参数（标记 deprecated），不做自动清理。

### 5.3 默认状态目录统一

`%ProgramData%\XNCAgent` → `C:\ProgramData\XNC`（`defaultStateDir`）。存量机器服务 argv 已显式携带 `--state-dir`，不受默认值变化影响；`install-dev-agent.ps1` 的 `XNCAgentDev` 隔离目录不变；`scripts/install-xnccore.ps1` 保留为运维手动路径。

## 6. 凭据不变量与生命周期

| 凭据 | 生成方 | 存储位置 | 生命周期 |
|---|---|---|---|
| Enrollment token | 服务端 | 仅存在于：管理员发出的命令文本、用户终端历史、安装进程内存。不出现在终端机器的任何文件、argv、注册表（服务端访问日志除外，见 6.1） | enroll 成功即扣减；过期/用尽自然失效 |
| 节点身份（Ed25519） | agent | `<StateDir>\identity.json`，DPAPI LOCAL_MACHINE + 0600 | 与机器共存亡；迁移场景留给子项目 2 |
| Core pipe secret | xnc-core | `<StateDir>\core-secret.hex`，DACL SYSTEM+Admins | core 首启创建；StateDir 由 install 子命令保证与 agent 对齐 |
| 服务 binPath | — | SCM | 零 secret 不变量，升级/更新流程沿用 |

### 6.1 已知并接受的暴露面

1. token 出现在 `/install/bundle` 的 URL query，会进服务器访问日志。30 分钟 TTL + 哈希存储 + 单次扣减下风险可接受；改 POST body 会复杂化 `irm | iex` 入口，本轮不做。
2. `irm | iex` 命令含 token，留存于 PowerShell 历史。与命令文本本身含 token 等价，TTL 兜底。

## 7. 错误处理与重试矩阵

原则：任何一步失败立即在控制台输出人话错误 + 下一步动作；整条命令可安全重跑。

| 失败点 | 用户看到的 | 幂等性/重试 |
|---|---|---|
| 非管理员且拒绝 UAC | 提示需要管理员权限 | 重跑命令 |
| 下载失败（网络/404） | HTTP 状态 + 重试提示 | 重跑；下载不扣 token 次数 |
| token 过期/用尽 | 「注册链接已失效，请联系管理员重新生成」 | 管理员发新 token |
| enroll 409（同机新钥，如 StateDir 被清空） | 「此机器已注册但本地凭据丢失，请联系管理员重置节点」 | 手动清理节点（现状）；自动 re-bind 留给子项目 2 |
| enroll 成功但 install 失败 | 服务安装具体错误（权限/SCM 错误码） | 重跑：同 pubkey enroll 幂等成功，install 幂等 |
| 服务启动/验证超时 | 哪个服务、SCM 状态、日志路径 | 重跑；无需预清理 |
| 重复运行整条命令 | 已装则全幂等通过，打印成功 | — |

## 8. 测试策略

- **服务端**：`/install/bundle` handler 测试——有效 token 放行；过期/用尽拒绝；**校验不扣减**（同一 token 连续下载 N 次后仍可 enroll 一次）；channel 选择正确。`/a/{token}` 返回内容用 PowerShell Parser（`ParseInput`）做语法校验，防语法级 bug 再次上线。
- **agent**：`enroll` 子命令（成功/幂等/409 的退出码与输出）；`install` 子命令沿用 `svcapp` 现有测试模式，新增断言：XNCCore binPath 含空格路径正确加引号、XNCAgent argv 无 token、幂等重装、已存在服务 binPath 不一致时的警告行为。
- **脚本模板**：golden-file 测试钉住生成内容；channel 变体（dev）参数化覆盖。
- **手动端到端**（干净 VM）：`token create` → `irm | iex` 全程 → 验收清单：双服务 Running、节点上线、desktop/shell/exec 可用、重跑幂等、`sc qc` 两服务输出无 token、`C:\ProgramData\XNC` 下 identity.json 与 core-secret.hex 权限正确。

## 9. 交付物清单

| 层 | 文件 | 变更类型 |
|---|---|---|
| server | `server/internal/api/router.go` | 注册 `/install/bundle` |
| server | `server/internal/api/install_handlers.go` | `/a/{token}` 返回 PS 文本 |
| server | `server/internal/api/install_scripts.go` | 模板重写 |
| agent | `agent/cmd/xnc-agent/main.go`、`main_windows.go` | 新增 `enroll` 子命令；`install` 扩展；argv 去 token；默认目录 |
| agent | `agent/svcapp/service_windows.go` | XNCCore 安装支持 |
| docs | `README.md` | 入口命令更新为 `irm \| iex` |
