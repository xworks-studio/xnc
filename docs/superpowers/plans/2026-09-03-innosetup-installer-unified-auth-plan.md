# Inno Setup 统一安装器与凭据链路 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers: subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** setup.exe 成为唯一分发载体：安装、修复、升级、卸载共用同一安装器；安装零凭据；首次使用经 `xnc register`（登录 → 选 cluster → 经 agentctl 管道注册）上线；更新 = agent 编排静默安装器 + 看门狗回滚；服务注册为可抛弃派生态（停→删→建）。

**Architecture:** server 新增 `/setup.exe`、`/setup.json`（release store 存安装器）与 `POST /api/clusters/{id}/nodes/register`（用户 JWT 授权注册）；agent 新增 `\\.\pipe\xnc-agentctl` 管道（register/deregister/status）与安装器更新编排（替代 bundle apply）；Inno Setup 打包五程序、无状态重建服务、缓存上一版安装器作回滚源。

**Tech Stack:** Go（chi + sqlc + testify）、Inno Setup 6（ISCC.exe、Pascal [Code]）、Windows SCM（golang.org/x/sys/windows/svc/mgr）、schtasks。

**Spec:** `docs/superpowers/specs/2026-09-03-innosetup-installer-unified-auth-design.md`

## Global Constraints

- 服务 argv 零密钥；一切密钥经 StateDir 文件 + DACL（沿用既有原则）。
- 机器级状态目录统一 `%ProgramData%\XNC`；用户级会话留 `~/.xnc/config.json`，用户 JWT 永不写入 ProgramData。
- 节点私钥只在 agent（SYSTEM）进程内生成/使用；CLI 经管道间接驱动（DPAPI SYSTEM + DACL）。
- 版本单一来源：`-ldflags` 注入 == setup.json.version == agent 自报；不一致视为更新失败走回滚。
- 服务注册一律停→删→建（无 `sc.exe config`/binPath 修改路径）；建序 XNCCore → XNCAgent，删序反序；创建后容忍 StartPending（吸收分支 8758b82 的容错）。
- server 测试依赖真实 PG（`db.OpenTestStore` / TestEnv 模式）；agent Windows 测试在 Windows 主机 `go test` 跑。
- 每任务独立提交，英文 conventional commit。
- **存量决策**：主工作区 updater WIP（apply_windows.go 等 5 文件）与分支 `feature/oneclick-install` 的修复由 Task 7 吸收或废弃——动手前先核对差异，避免重复实现。

---

### Task 1: server 安装器分发端点 `/setup.exe` + `/setup.json`

**Files:**
- Create: `server/internal/api/setup_handlers.go`
- Create: `server/internal/api/setup_handlers_test.go`
- Modify: `server/internal/api/router.go`（`/a/{token}` 路由块旁）

**Interfaces:**
- Consumes: `GetLatestReleaseByChannel(ctx, channel)`、`GetArtifact(ctx, {ReleaseID, Name})`（artifact 名常量 `setupArtifactName = "setup.exe"`；manifest 无需入库——`setup.json` 由 handler 动态生成）。
- Produces: `GET /setup.exe?channel=stable|dev` → 302 至 release store（或直接 200 流式 + `X-Xnc-Sha256`）；`GET /setup.json?channel=` → `{version, url, sha256, size, releasedAt}`；无 release → 404。
- 测试用例：双频道各两版本取最新；channel 缺省 stable；无 release 404；setup.json 字段与 artifact sha256/size 一致。

- [ ] Step 1: 失败测试（seedChannelRelease 模式，参照 install_bundle 旧计划测试）
- [ ] Step 2: 实现 handler + 路由；`go test ./...`

### Task 2: server 用户 JWT 授权注册端点

**Files:**
- Create: `server/internal/api/node_register_handlers.go`（+ `_test.go`）
- Modify: `server/internal/api/router.go`（clusters 块内）
- Modify: `proto/`（错误码 `CLUSTER_FORBIDDEN`、`MACHINE_ID_CONFLICT` 若无则加）

**Interfaces:**
- Consumes: `auth.UserFrom(ctx)`、既有 membership 判定（`isAdminUser`/cluster 成员查询）、enroll 落库路径（machineId 去重、公钥绑定、NodeID 分配——复用 `ConsumeEnrollmentToken` 之后的同一事务逻辑，抽出共函数）。
- Produces: `POST /api/clusters/{id}/nodes/register`，body 同 `/api/agent/enroll`（除 token），Authorization: 用户 JWT → `{nodeId}`；403 非成员；409 machineId 已属其他 cluster（响应含冲突 cluster 名）。
- 测试用例：成员注册成功且 audit 记 `register {userId, clusterId}`；非成员 403；machineId 冲突 409；token 参数被拒（400）。

- [ ] Step 1: 抽取 enroll 共用落库函数（现有 enroll handler 改为薄壳，先补回归测试）
- [ ] Step 2: 失败测试 → 实现新端点 → `go test ./...`

### Task 3: agent 机器级绑定与未注册空转

**Files:**
- Modify: `agent/agent.go`（binding.json 读写、unconfigured 循环）
- Create: `agent/binding/binding.go`（+ `_test.go`）
- Modify: `agent/cmd/xnc-agent/main_windows.go`（defaultStateDir → `ProgramData\XNC`；首启迁移旧 `XNCAgent` 目录）

**Interfaces:**
- `binding.Binding{Server, ClusterID, NodeID, RegisteredAt, Channel}`，`Load/Save(dir)`（原子写：tmp+rename）；DACL 继承 StateDir（SYSTEM+Admins）。
- Agent 启动：无 binding → 5s 轮询等待（日志 `awaiting registration`）+ 管道可注册（Task 4）；有 binding → 现行 connect 流程。updater 在无 binding 时不检查更新。
- 测试：binding 原子写/读回；旧目录迁移（含 identity.json 保留）；空转日志。

- [ ] Step 1: 失败测试 → binding 包
- [ ] Step 2: agent.go 接入 + StateDir 迁移；Windows 主机 `go test ./...`

### Task 4: agentctl 命名管道

**Files:**
- Create: `agent/agentctl/pipe_windows.go`（+ `_test.go`）、`agent/agentctl/pipe_other.go`
- Modify: `agent/agent.go`（挂管道）

**Interfaces:**
- 管道 `\\.\pipe\xnc-agentctl`，DACL：SYSTEM+Administrators+Interactive（deregister 校验 Administrators，register 允许 Interactive）。
- 单行 JSON 协议：`{op:register, server, clusterId, jwt}` → 调 Task 2 端点（公钥来自/生成于本机 identity）→ 原子写 binding → 触发 connect → `{ok, nodeId}`；`{op:deregister}` → 断 WS + server 注销（经既有 agent WS 控制语义，不新增 HTTP 凭据）+ 删 binding（保留 identity）→ `{ok}`；`{op:status}` → `{state, nodeId, server, clusterId, version}`。jwt 内存传递用后即弃。
- 测试：管道往返（真机 Windows 测试起 agent 进程）；DACL 拒绝非提权 deregister；register 幂等（已注册 → 冲突错误码）。

- [ ] Step 1: 失败测试 → 管道实现
- [ ] Step 2: agent 集成（register 成功即上线）；真机验证

### Task 5: CLI register / deregister / status

**Files:**
- Create: `cli/cmd_register.go`（+ `_test.go`）
- Modify: `cli/cmd_auth.go`（login 后 unregistered 提示）、`cli/main.go`（命令注册）

**Interfaces:**
- `xnc register`：无 JWT → 内联 login；`GET /api/clusters` 列表交互选择（单个确认即过）→ 连管道发 register → 轮询 `GET /api/nodes/{id}` 至 online（≤10s）→ 打印节点与面板入口。`--force` = deregister+register。
- `xnc deregister`（要求 admin 提权，管道校验）；`xnc status`：本机块（管道 status）+ 会话块。
- server 冲突展示：409 时提示冲突 cluster 名与 `--force`。
- 测试：命令流（mock 管道 + httptest server）；集群选择交互。

- [ ] Step 1: 失败测试 → 实现；真机 e2e 手跑一遍

### Task 6: Inno Setup 安装器

**Files:**
- Create: `installer/xnc.iss`（源码进库）
- Create: `installer/build.ps1`（ISCC 调用 + 版本注入 + sha256 输出）
- Modify: `Makefile`（`installer` 目标：build 五二进制 → ISCC → 产 `bin/xnc-setup-<ver>.exe`）

**要点（对照 spec §3/§8/§9.3）：**
- `PrivilegesRequired=admin`、固定 AppId、`DefaultDirName={autopf}\XNC`；组件：agent+CLI 必选，desktop/shell 可选。
- `[Code] PrepareToInstall`：停 XNCAgent→XNCCore（容忍不存在）→ 结束安装目录内 desktop/shell 进程 → rename 占用中的 `xnc.exe` 为 `.old`（清历史 .old）→ 删两服务（先 STOP）；`RegisterPreviousData`/安装尾段：`New-Service` 重建 XNCCore→XNCAgent（全量声明 binPath/start=auto/恢复策略；StartPending 容忍）→ 写 `installer-cache\` 当前安装器副本（仅留 1 份）→ 启动。
- 卸载序：清看门狗 schtask + update-pending + installer-cache/staging/.old → 停删服务（反序）→ 杀残留进程 → 删文件/PATH/注册表 → 数据询问（默认保留 ProgramData；`/PURGEDATA` 强删）。
- `/VERYSILENT /SUPPRESSMSGBOXES /NORESTART` 全程无 UI 依赖（session 0 可跑）。
- 验证：真机装→卸→重装→静默升级（同版本重跑）四连；`/SILENT` 卸载默认保留数据。

- [ ] Step 1: xnc.iss + build.ps1 + Makefile 目标
- [ ] Step 2: 真机四连验证，结果记录到本文件 Results 节

### Task 7: agent 安装器更新编排（替代 bundle apply）

**Files:**
- Rewrite: `agent/updater/updater.go`（编排：发现/下载/校验/执行/看门狗；删除 bundle apply 路径）
- Delete: `agent/updater/apply_windows.go` 中 bundle 搬移逻辑（保留可复用片段：进程清理、重试模式）
- Create: `agent/updater/orchestrate_windows.go`（+ `_test.go`）、`agent/updater/watchdog_windows.go`
- Modify: `proto/`（WS 推送 `update_available {version, sha256, url}` 若无）

**流程（spec §9）：** 触发（WS 推送 + 6h 轮询 setup.json + 管道 upgrade op）→ 黑名单/退避（1h/4h/24h）→ staging 下载 + sha256 → 确认 installer-cache 回滚源（缺失先补当前版安装器）→ 写 `update-pending.json {from,to,startedAt,deadline=+15min}` + 注册一次性 schtasks 看门狗 → SYSTEM 执行 `setup.exe /VERYSILENT …` → 新 agent 自检（版本==to、服务健康、WS 可达）→ 删 pending+看门狗、写 cache、audit `update_ok`。
**回滚：** 自检不过主动回滚；看门狗 deadline 触发（先重启服务、仍败则静默重跑 cache 旧安装器）；退出码非 0 由看门狗收尾；过期 24h 的 pending 在任意成功启动时触发。audit `update_rollback {from,to,reason}`。
**存量吸收：** 对比主工作区 updater WIP 与分支 8758b82/9a8efb6 的停服务/文件占用处理，可复用的并入 orchestrate（如按镜像路径杀进程、StartPending 容忍）。
**测试：** 编排状态机单测（假安装器脚本：成功/退出码非 0/超时三剧本）；看门狗注册与触发（真机）；installer-cache 补源路径。

- [ ] Step 1: 核对 WIP 与分支差异，定吸收清单（写进本任务）
- [ ] Step 2: 状态机失败测试 → 实现
- [ ] Step 3: 真机剧本验证（含拔网线自检失败→主动回滚）

### Task 8: CLI upgrade --channel + 管道 upgrade op

**Files:**
- Modify: `cli/cmd_admin.go` 或新建 `cli/cmd_upgrade.go`；`agent/agentctl`（加 `{op:upgrade, channel?}`）
**Interfaces:** `xnc upgrade [--channel stable|dev]` → 管道触发即时检查并应用，阻塞显示进度（管道进度事件或轮询 status）；跨频道改 binding.channel 并 audit。
**测试：** 管道 op 往返；channel 切换改绑定。

- [ ] Step 1: 失败测试 → 实现；真机跑一次跨频道切换

### Task 9: 退役与迁移

**Files:**
- Delete: `scripts/build-bundle.go`；server bundle 发布/下载面（update 检查端点改读 setup 制品或保留兼容一个版本周期——按 spec §14：最后一个 bundle 携带编排版 agent 后关闭）。
- Modify: `README.md`（入口换 setup.exe + register 流程）、`spec.md`（§14/§45/§46/§57/§64 对应节）、`install_handlers.go` 全套加 deprecated 头（两版本周期后删）。
- Release: 打 **最后一个 bundle**（编排版 agent），发布说明写明切换判据。

- [ ] Step 1: 退役删除 + 文档改写
- [ ] Step 2: 最后 bundle + setup.exe 同版发布演练

### Task 10: 全链路验收（真机）

- [ ] 全新机器：setup.exe 装 → 空转 → register → online → exec/shell/screen 会话
- [ ] 更新：发新版 → 推送升级 → 秒级中断 → online@新版本；坏版本（自检不过）→ 自动回滚@旧版本
- [ ] 卸载：默认保留数据重装复用 identity；`/PURGEDATA` 清除
- [ ] 结果与遗留问题记录进本文件 Results 节

## Results

### Task 6 (2026-09-03): Inno Setup 安装器 — real-machine verification, all green

Artifacts: `installer/xnc.iss`, `installer/build.ps1`, Makefile `installer` target (CHANNEL/VERSION passthrough). Built `bin/xnc-setup-0.6.1.exe` (sha256 `2d39241788433450bf3f3cabb18fff68164287281d4fa0c4e4a0002e50be3c49`, sidecar `bin/xnc-setup-0.6.1.exe.sha256`) via build.ps1 end-to-end (agent 0.6.1 self-report check passed; native build.bat core+desktop; ISCC 6.7.3). Dev channel naming verified: `/DChannel=dev` → `xnc-setup-dev-<ver>.exe`.

Pre-state: `XNCCore`/`XNCAgent` services both absent (sc error 1060) — machine left post-uninstall at the end, matching pre-state.

Cycles (each `/VERYSILENT /SUPPRESSMSGBOXES /NORESTART`, elevated, session-safe, no UI):
1. **install** exit 0 → XNCCore+XNCAgent exist, AUTO_START, RUNNING, recovery restart/5000×3 (reset 86400); 5 files in `C:\Program Files\XNC`; binPaths `..."xnc-agent.exe" run --server= --state-dir="C:\ProgramData\XNC"` (zero secrets) / `"xnc-core.exe" --service XNCCore --secret-file "C:\ProgramData\XNC\core-secret.hex"`; HKLM PATH appended; installer-cache holds exactly 1 exe; uninstall registry Channel=stable, DisplayVersion=0.6.1, InstallDate=20260903; core-secret.hex self-created by XNCCore first start. 21/21 asserts.
2. **uninstall** exit 0 → services absent, app dir gone (incl. runtime `xnc-core-service.log` via [UninstallDelete]), PATH entry gone, _is1 key gone, `C:\ProgramData\XNC` KEPT (default keep). 6/6.
3. **reinstall** exit 0 → full installed asserts green again (21/21).
4. **silent same-version upgrade re-run** (services RUNNING + a held-open `xnc.exe` login prompt) exit 0 → prep log: stopped+deleted RUNNING XNCAgent→XNCCore, `renamed xnc.exe -> xnc.exe.old (in-use swap)`; post log: cache rewritten (count 1), XNCCore→XNCAgent recreated + Running. 21/21.
5. **final `/SILENT` uninstall** exit 0 → defaults KEEP (no data prompt), ProgramData intact. 6/6.
6. **`/PURGEDATA=true` cycle** (reinstall then purge-uninstall) exit 0 → `C:\ProgramData\XNC` fully deleted. 4/4.

Post-run: no xnc processes, no leftover schtasks. Watchdog task name contract for T7: `XNCRollbackWatchdog` (uninstalled best-effort, tolerates absence).

Fixes made to the inherited partial work (found by real-machine run): PS `[Parameter(Mandatory)]` on embedded scripts prompted in the hidden window (Mandatory ignores defaults) → hang; removed, values baked as param defaults. Inno 6.7 API drift: `HWND_BROADCAST` now predefined, `WPARAM` unknown → `UINT_PTR`, `CreateCustomForm` now takes 4 args. `[Registry]` Channel entry could never survive (Inno deletes pre-existing `_is1` key when saving uninstall info — after [Registry] created it) → written from ssPostInstall instead. Uninstall "Cancel" on the data dialog aborted nothing → now raises and stops the uninstall. Runtime `*.log`/`*.old` under {app} added to [UninstallDelete].
