# AGENTS.md — XNC 仓库操作规范

面向在本仓库工作的 AI agent 与工程师。读完应能：正确开发、部署 server、发布安装器、
管理节点与用户安装流程，且不违反下述硬性契约。历史背景与完整设计见文末索引；
本文与 spec 冲突时以 spec 为准，但**硬性契约**一节优先于一切。

## 1. 仓库是什么

XNC = Windows 节点远程管理平台：Go server（xnc.app）+ Windows agent
（服务）+ Web UI + CLI（`xnc`）。安装器（Inno Setup）是唯一分发载体：安装、
修复、升级、卸载共用一个 setup.exe；更新由 agent 编排静默安装器完成。

模块地图（Go workspace，`go.work` 串联）：`proto`（协议）· `server`（控制面，
chi + sqlc + 真 PG 测试）· `agent`（节点侧，Windows 服务）· `cli` · `shellhost`
（ConPTY 宿主）· `mockagent`（负载/一致性测试用，**保留勿删**）· `shellsmoke` ·
`tools/{desktopreport,e2eviewer,nalcheck}`（桌面报告/e2e 回放/码流分析，均在职）。
`native/` 为 C++（core=SYSTEM 管道服务、desktop=桌面会话、common=共享头）。
`web/` 为 React+Vite（产物嵌入 server 二进制）。`installer/` 为 Inno Setup 打包。

## 2. 硬性契约（违反即事故）

1. **版本单一来源**：agent 版本只经构建期 ldflags 注入 machineinfo.Version；
   setup.json 的 version == agent 自报。发布前必须校验，不一致=发布失败。
2. **服务 argv 零密钥**：XNCAgent/XNCCore 的 binPath 里不得出现任何
   token/secret；凭据只经 StateDir 文件 + DACL。改服务=停→删→重建（无
   `sc.exe config` 路径；SID 由服务名派生，重建无损）。
3. **StateDir 布局**：机器级状态统一 `C:\ProgramData\XNC`
   （binding.json/identity.json/core-secret.hex/update-pending.json/
   staging\/installer-cache\/logs\）；用户会话在 `~/.xnc/`。用户 JWT 永不落
   ProgramData。
4. **环境来源**：core 用用户 token 拉起的子进程必须携带 token 派生的用户环境
   （CreateEnvironmentBlock）；子进程读 `XNC_*` 调试旋钮走注册表环境，服务
   环境不再透传（有意收窄）。
5. **更新契约**：看门狗计划任务名 `XNCRollbackWatchdog`；回滚=原地执行
   installer-cache 里的上一版 setup.exe（staging 只做下载）；安装器保证可重入。
   agent 不再解包搬文件——发现→sha256 校验→静默执行安装器→看门狗兜底。
6. **machineId 语义**：同 cluster 同 machineId 异 key 的注册=**adopt**（沿用原
   nodeId、重绑公钥、驱逐旧连接）；同 key=幂等；跨 cluster=409（出路是管理端
   删除旧记录后重注册）。
7. **凭据纪律**：`deploy/.env` 是唯一凭据源（SRV_*/NODE_*，gitignored）；
   严禁入库、严禁回显。测试/诊断脚本不落盘 token。

## 3. 开发流程

- **分支隔离**：任何实施在 `.worktrees/<name>`（gitignored）+ feature 分支上
  做，完成后合并回 main；不在 main 上直接开发。
- **构建/测试**：`go build ./...` 逐模块（根目录不是模块）；server 测试需真
  PG（testcontainers 自动拉起）；agent Windows 测试直接本机 `go test`。
  `make build/test/fmt` 覆盖 MODULES。
- **提交**：英文 conventional commits；代码注释中文；每个逻辑单元独立提交。
- **流程惯例**：多任务实施走 spec→plan→逐任务子代理+审查（
  docs/superpowers/ 下的模式）；带日期的 plans 文档是历史存档，**只读**。
- **实验机**：LABS-XIAOXIN（干净实验台）、LABS-TB16G7（用户机）经 PowerShell
  remoting（Invoke-Command，凭据在 deploy/.env 的 NODE_* 键）；**不要在开发
  机本机装 agent 做实验**。
- 遗留坑：`agent/session` 存在预存在的 GOOS=linux 构建失败（非 Windows 路径），
  与新改动无关时勿"顺手修"。

## 4. 部署流程（server → xnc.app）

顺序固定，缺一步即事故（每条都是实战教训）：

1. **本地构建 web**：`cd web && npm run build`（服务器不跑 npm）。
2. **同步代码**：`py deploy/deploy_srv.py push`（打包 proto/server/deploy/web
   上传解压）。
3. **⚠️ 删除型变更必须先清远端**：push 的 tar 解包**只覆盖不删除**。若本次
   变更删过文件/目录：先备份远端 `deploy/.env` → 清空远端
   `proto/ server/ deploy/ web/` → 重新 push → 恢复 `.env`。不清=新旧混编
   编译失败、旧二进制假上线（曾发生过）。
4. **版本**：改远端 `.env` 的 `XNC_VERSION`（`py deploy/deploy_srv.py env`
   只增不改已有键）。server 版本与 agent release 版本相互独立。
5. **构建上线**：远端 `docker compose -f deploy/docker-compose.yml build
   xnc-server && up -d`。**检查构建退出码**——build 失败时 up 会用旧镜像
   "成功"，必须看 BUILD_RC 或验证新行为（如新端点）真的生效。
6. **Caddyfile 变更**需 `docker compose up -d --force-recreate caddy`——
   push 用 tar 替换文件产生**新 inode**，restart 只重启进程、bind mount 仍
   指旧 inode；必须重建容器重绑挂载（域名翻转时实战踩过）。
7. 验证：`/api/health` 版本、新端点行为、SPA 哈希更新。
8. 本机**禁止**运行 xnc-server 栈（spec 红线）；server 只活在 SRV 的 docker。

## 5. 发布流程（安装器/更新）

1. 构建：`powershell installer/build.ps1 -Version <v> -Channel stable|dev`
   （或 `make installer VERSION=<v>`）——产 `bin/xnc-setup[-dev]-<v>.exe`
   + sha256。五二进制（agent/core/desktop/shell/CLI）版本同源注入。
2. 上传：admin 登录取 JWT → `POST /api/admin/releases`
   multipart：`version`/`notes`/`channel`/`setup=@...`。bundle 部分已退役
   （传了 400 是对的）。同版本重传=upsert 改道（注意）。
3. 生效即达：上传后 `/setup.json` 与下载页立即指向新版；在线 agent 经
   WS 推送/6h 轮询/`xnc upgrade` 升级（秒级中断，失败自动回滚+拉黑）。
4. **升级安全网**：看门狗 schtask + installer-cache 回滚源 + 24h 过期
   pending 兜底；发布坏版本的自愈路径已内建，无需人工回滚。
5. 当前线上锚点：下载页 `https://xnc.app/download`；短域
   `xnc.app/setup.exe`。

## 6. 用户安装 / 注册 / 卸载

- **安装**：下载页或 `curl -LO https://xnc.app/setup.exe` → 运行
  （管理员）→ 服务就位，**零凭据**，agent 空转 `awaiting registration`。
- **注册**：`xnc register --server https://xnc.app`（新机首跑需
  --server；TTY 交互登录+选 cluster，非 TTY 用 --email + stdin 密码 +
  --yes）。成功后秒级 online。重装/换机身份 → adopt 沿用原节点。
- **会话**：`xnc login/logout`（用户级）；`logout` 不影响节点在线。
- **反注册**：`xnc deregister`（需管理员终端；删服务端记录+本地绑定，
  保留身份）。WS 通道不通时改用服务端管理删除。
- **升级**：`xnc upgrade [--channel stable|dev]`。
- **卸载**：图形向导（数据默认保留，重装续用身份）或
  `unins000.exe /VERYSILENT /PURGEDATA=true`（彻底清零）。卸载不反注册。

## 7. 运维要点

- **节点管理**：`DELETE /api/nodes/{id}`（平台管理员）删孤儿/换主记录；
  节点离线回收策略尚无自动化（待办）。
- **诊断模板**：agent 日志 `C:\ProgramData\XNC\agent-service.log`。健康链
  `registered via agentctl → binding detected (woken) → control connection
  ready`；卡在 wake 前=管道/注册问题，卡在 dial=网络问题。
- **已知网络约束**：agent 需可建立 websocket 长连接；严格代理网络会以
  "handshake 200" 形式失败（注册能过、上线不能）——排查代理/DNS，长期
  待办是轮询兜底通道。
- **实验闭环惯例**：卸载→清残留（服务/进程/任务/目录/PATH/注册表八项核验）
  →全新安装→注册上线→验证→deregister→卸载→双端零残留。

### 代码签名（自签过渡期）

- **背景**：未签名 exe 会被 Defender 随机隔离（实战发生过：xnc.exe 消失）。
- **证书**：`installer/codesign.cer`（公钥，入库）+ `installer/codesign.pfx`
  （私钥，gitignored，**勿外传**）；密码在 `deploy/.env` 的
  `XNC_CODESIGN_PASSWORD`。3 年有效期，续期用 `installer/make-cert.ps1`
  重建并给全机群重导信任。
- **构建**：`build.ps1` 自动签名（五 exe 在 ISCC 前签、setup 在其后、
  sha256 覆盖签名后产物）；设 `XNC_CODESIGN_PASSWORD` 环境变量。时间戳
  多服务器回退，全败则免时间戳签名+告警。
- **新机入群**：导入 `codesign.cer` 到 LocalMachine 的 `Root` +
  `TrustedPublisher`（否则 Defender 照拦——自签信任靠自己分发）。
- **待办**：换正式 CA（EV）证书后，信任分发可整体退役。

## 8. 已知小缺口（勿重复发现，按需修）

- `HEAD /setup.exe` 落到 SPA（chi Get 不含 Head）。
- 安装器 PATH 写回会展开 REG_EXPAND_SZ 引用（触发于增删 XNC 项时）。
- 同版本重传 release 不删除缺席制品（旧 setup.exe 可能残留可下载）。
- `deploy_srv.py push` 未实现"先删后解"（本文 §4.3 的人工规程即其替代）。
- `.dockerignore` 使本地 compose build 失败（部署用远端构建不受影响）。

## 9. 索引

- 产品 spec：`spec.md`（权威）· 快速上手：`README.md`
- 安装器/更新/凭据设计：`docs/superpowers/specs/2026-09-03-innosetup-installer-unified-auth-design.md`
- 实施与真机验收记录：`docs/superpowers/plans/2026-09-03-…-plan.md`（Results 节）
- 开发拓扑/凭据布局：`scripts/dev-topology.md` · `scripts/dev-services.md`
