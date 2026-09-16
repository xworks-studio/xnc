# AGENTS.md — XNC 仓库操作规范

> 2026-09-11 架构基线：主站（xnc.app）= web/认证/编排/票据签发，**零数据流**
> （XNC_RTV_EMBEDDED 默认 false）；媒体 + 会话数据（exec/shell/file/tunnel）
> 全部经外置 xnc-relay（r1/r2.xnc.app，节点粘性 + 负载评分 + 健康探测）。
> 存量客户端零断代（旧 CLI 经 UA 门禁回落主站旧路径）。详见
> docs/ci-release-and-deploy.md §6/§7。

面向在本仓库工作的 AI agent 与工程师。读完应能：正确开发、部署 server、发布安装器、
管理节点与用户安装流程，且不违反下述硬性契约。历史背景与完整设计见文末索引；
本文与 spec 冲突时以 spec 为准，但**硬性契约**一节优先于一切。

## 1. 仓库是什么

XNC = Windows 节点远程管理平台：Go server（xnc.app）+ Windows agent
（服务）+ Web UI + CLI（`xnc`）。安装器（Inno Setup）是唯一分发载体：安装、
修复、升级、卸载共用一个安装器；更新由 agent 编排静默安装器完成。

### 系统架构（固定基线，细节见 spec 与设计文档索引）

```
                 ┌─ SRV（阿里云，xnc.app）docker：caddy(TLS) ─ xnc-server ─ postgres
                 │                                    cert-sync(仅内嵌过渡)  watchtower(已停用)
   浏览器 ───────┤  TCP443(caddy)：Web UI / REST / 会话编排（认证+控制面；
                 │    XNC_RTV_EMBEDDED=false 默认 → 主站不跑媒体面/不挂 /ws）
                 │  分发：/installer + /installer.json + /download 页（无认证）
                 │
   Windows 节点 ─┤  纯出站 wss 控制连接（agent 自报版本，server 推送更新）
     XNCAgent(SYSTEM) + XNCCore(spawn 桥) + xnc-host.exe(Rust 采集/编码/
       RS FEC/QUIC 直连 relay UDP4433，HostToken/certSha256 经 stdin 下发)
     媒体面（RTV，2026-09-11 起 relay-only 默认）：host→xnc-relay 字节扇出
       →浏览器 WebCodecs 硬解。relay = 外置 systemd 二进制（UDP443 WT 自签
       钉扎 + UDP4433 host 腿 + HTTP /ws 兜底经 relay 主机 caddy 前置）；
       server rtvpool 注册/审批/健康探测/节点粘性+负载评分分配；无可用
       relay → desktop 503 RTV_NO_RELAY；内嵌 relay-0 仅 dev/过渡
       （XNC_RTV_EMBEDDED=true）
     会话数据面（Stage B，2026-09-11）：exec/shell/file/tunnel 的 WS 双腿
       走 relay session router（sdata 票据；域名+caddy 前置为激活前置，
       未宣告时回落主站旧路径——存量端零改动）
     状态：ProgramData\XNC（binding/identity/回滚缓存）；用户会话：~/.xnc
     用户流：装安装器（零凭据）→ xnc register（登录→选 cluster→秒级上线）
```

- **交付面**：agent/CLI 经 Inno Setup 安装器（**现行路径：本地
  `src/installer/build.ps1` 签名构建 → 管理员 API `POST /api/admin/releases`
  直传生产 release store**；CI publish.yml 因 vcpkg 陈旧 ffmpeg 基线暂不可
  用，修复后恢复"tag+GitHub Release+installersync 拉回"的正规路径）；
  server 经 `deploy/build-server-local.ps1`（本地 docker build → save →
  SFTP → SRV load + `up -d --no-pull`；watchtower 已在 SRV 停用，CI
  build-server.yml → GHCR 保留为备用通道）。
- **信任**：自签 Authenticode（安装器内置信任装卸）；更新 sha256 强校验 +
  回滚看门狗；凭据唯一源 `deploy/.env`。
- **开发流**：feature 分支直接开发（2026-09-10 起弃用 worktree：`git switch
  -c feature/<name>` 从 main 切出）→ PR（ci.yml 六矩阵门禁：go-linux/
  go-windows/web/native/host-rust/installer-dryrun）→ merge 回 main。实验机
  XIAOXIN/TB16G7（PS remoting），本机不装产品组件。

模块地图（2026-09-09 起全部代码模块在 `src/` 下；`deploy/` 运维、`docs/`
文档、`scripts/` 脚本、`bin/` 产物池留在仓库根。Go workspace 由仓库根
`go.work` 串联）：`src/proto`（协议）· `src/server`（控制面，
chi + sqlc + 真 PG 测试）· `src/agent`（节点侧，Windows 服务；`display` 子包
= IDD 虚拟显示器控制编排）· `src/cli` · `src/shellhost`
（ConPTY 宿主）· `src/mockagent`（负载/一致性测试用，**保留勿删**）· `src/shellsmoke` ·
`src/rtv`（RTV 中继数据面库：Hub 扇出/票据/仲裁/三腿，server 内嵌与外置中继共用）·
`src/relay`（外置中继二进制 xnc-relay）·
`src/tools/{desktopreport,rtvload,nalcheck}`（桌面报告/RTV 合成查看器/码流
分析，均在职）。`src/native/` 为 C++（core=SYSTEM 管道服务、common=共享头；
desktop 已随 RTV 重构删除）。`src/host/` 为 Rust crate `xnc-host`（桌面采集/
编码/FEC/QUIC 发送），vendor 依赖在 `src/third_party/scrap`（rustdesk fork 裁剪）。
`src/third_party/xncidd/` 为 IDD 虚拟显示器驱动（UMDF/IddCx 1.4，C++，
vendored RustDeskIddDriver + 微软 IddSample 基底，MS-PL；控制侧在
`src/agent/display`，详见其 VENDOR.md）。
`src/web/` 为 React+Vite（产物嵌入 server 二进制）。`src/installer/` 为 Inno Setup 打包。

## 2. 硬性契约（违反即事故）

1. **版本单一来源**：agent 版本只经构建期 ldflags 注入 machineinfo.Version；
   installer.json 的 version == agent 自报。发布前必须校验，不一致=发布失败。
2. **服务 argv 零密钥**：XNCAgent/XNCCore 的 binPath 里不得出现任何
   token/secret；凭据只经 StateDir 文件 + DACL。改服务=停→删→重建（无
   `sc.exe config` 路径；SID 由服务名派生，重建无损）。
3. **StateDir 布局**：机器级状态统一 `C:\ProgramData\XNC`
   （binding.json/identity.json/core-secret.hex/update-pending.json/
   staging\ /installer-cache\ /tmp\（会话临时脚本）/ logs\（全部运行
   日志：agent-service.log、xnc-core-service.log、xnc-host.log、
   xnc-shell.log，均 8MB×3 份大小轮转，2026-09-15 规范））；用户会话在
   `~/.xnc/`。用户 JWT 永不落 ProgramData。
4. **环境来源**：core 用用户 token 拉起的子进程必须携带 token 派生的用户环境
   （CreateEnvironmentBlock）；子进程读 `XNC_*` 调试旋钮走注册表环境，服务
   环境不再透传（有意收窄）。
5. **更新契约**：看门狗计划任务名 `XNCRollbackWatchdog`；回滚=原地执行
   installer-cache 里的上一版安装器（staging 只做下载）；安装器保证可重入。
   agent 不再解包搬文件——发现→sha256 校验→静默执行安装器→看门狗兜底。
6. **machineId 语义**：同 cluster 同 machineId 异 key 的注册=**adopt**（沿用原
   nodeId、重绑公钥、驱逐旧连接）；同 key=幂等；跨 cluster=409（0005 起
   machine_id 全局唯一索引硬约束；出路 = **双边 owner 的 `POST
   /api/nodes/{id}/move`**（nodeId/公钥/在线连接保留，`xnc node move`）或
   管理端删除旧记录后重注册）。另：平台 admin 是显式 `users.is_admin`
   （0005 起），**不再是**"任一 cluster owner 即 admin"——建 cluster 不升权；
   每用户建号即得 personal 默认 cluster（migration 0005 + 设计
   docs/superpowers/specs/2026-09-14-user-cluster-self-service-design.md）。
7. **凭据纪律**：`deploy/.env` 是唯一凭据源（SRV_*/NODE_*，gitignored）；
   严禁入库、严禁回显。测试/诊断脚本不落盘 token。

## 3. 开发流程

- **分支隔离**：任何实施在 feature 分支上做（`git switch -c feature/<name>`，
  不再使用 worktree——同一工作区切换分支），完成后合并回 main；不在 main
  上直接开发。
- **构建/测试**：`go build ./...` 逐模块（根目录不是模块）；server 测试需真
  PG（testcontainers 自动拉起）；agent Windows 测试直接本机 `go test`。
  `make build/test/fmt` 覆盖 MODULES。
- **提交**：英文 conventional commits；代码注释中文；每个逻辑单元独立提交。
- **流程惯例**：多任务实施走 spec→plan→逐任务子代理+审查（
  docs/superpowers/ 下的模式）；带日期的 plans 文档是历史存档，**只读**。
- **实验机**：LABS-XIAOXIN（干净实验台）、LABS-TB16G7（用户机）经 PowerShell
  remoting（Invoke-Command，凭据在 deploy/.env 的 NODE_* 键）；**不要在开发
  机本机装 agent 做实验**。
- 遗留坑：`src/agent/session` 存在预存在的 GOOS=linux 构建失败（非 Windows 路径），
  与新改动无关时勿"顺手修"。

## 4. 部署流程（server → xnc.app，本地构建直推）

**部署（现行）= 本地跑 `deploy/build-server-local.ps1`**：本地 docker
build（web dist + 版本镜像）→ `docker save` → SFTP 上 SRV → `docker load`
+ tag `latest` → `compose up -d --no-pull`。SRV 上的 watchtower 已停用
（2026-09-08 运维决策：不走远端构建/注册表中转，省时且可控）；CI 的
`build-server` workflow（→ GHCR `v<版本>+latest+sha`）保留为备用通道，
启用需在 SRV 重启 watchtower。server 发布节奏由人决定，不随 push main
自动出。

- **本地路径**：`powershell deploy/build-server-local.ps1 -ServerVersion <v>`
  （参数名刻意避开 -Version——powershell.exe -File 会把它吞作引擎参数；
  远端推送经 deploy/push_server_image.py 走 paramiko 密码认证，凭据读
  `deploy/.env` 的 SRV_* 键）。
- **备用 CI 路径**：Actions → build-server → Run workflow → 填版本号
  （须先合入 main；格式 X.Y.Z[-dev]，段 ≤4 位）→ 等 watchtower 轮询换版。
- **验证**：`curl https://xnc.app/api/health` 的 version == 输入的版本号。
- **锁版本/回滚**：改 compose 的镜像 tag 为 `:vX.Y.Z` 或 `:sha-<哈希>`
  后 `up -d`（SRV 上直接改，改完记得回改仓库保持一致）。
- **Caddyfile 变更**仍需 SSH：替换文件后 `docker compose up -d
  --force-recreate caddy`（tar/编辑器换文件产生新 inode，restart 不够，
  必须重建容器重绑挂载——域名翻转时实战踩过）。
- **turnserver/coturn 已退役**（2026-09-08 随 RTV 重构移出 compose）：RTV
  走 QUIC/WT，TURN 无用户。`deploy/turnserver.conf` 模板与 `deploy_srv.py`
  的渲染逻辑仅为 pre-RTV 回滚窗口保留；compose 里 coturn 服务已删，回滚需
  先恢复旧 compose 再渲染（`${VAR}` 模板不能直用，2026-09-06 踩过）。
- 服务器上**没有源码、没有脚本、没有 cron**：`/opt/xnc` 只有 `deploy/`。
  `deploy_srv.py` 的 push/up 等命令已退役为应急手段（远端无构建上下文）。
- GHCR 拉取授权：SRV root 的 docker config（PAT read:packages）；
  watchtower 挂载同一 config（已停用，重启容器前先确认要不要恢复自动换版）。
- 本机**禁止**运行 xnc-server 栈（spec 红线）；server 只活在 SRV 的 docker。

## 5. 发布流程（安装器/更新）

**发布（现行）= 本地构建 + 管理员 API 直传**：`powershell
installer/build.ps1 -Version <v> -Channel stable|dev` 签名构建 →
`POST /api/admin/releases`（multipart 上传 exe + .sha256，生产 admin
JWT 鉴权）写入生产 release store。在线 agent 经 WS 推送秒级升级，语义
与正规路径完全一致。

**正规路径（CI，暂不可用）**：Actions → publish → Run workflow（原
release.yml，2026-09-08 因步骤名未引号冒号损坏注册表而重命名）→ 在
main HEAD 创建并推 tag `v<版本>`（**构建成功后才打 tag**；tag 已存在
即拒绝 = 版本不可变）→ 签名构建 → GitHub Release。当前卡点：CI 的
vcpkg 无基线锁定，ffmpeg 头漂移（FF_PROFILE_* 枚举缺定义）编译失败
——修复（锁 vcpkg baseline 或自带 vendor）前用本地直传。注意：本地
直传不经 tag，**版本不可变靠自觉**（同版本禁止重传不同内容）。

1. installersync 拉回路径仍在运行（默认 5 分钟轮询
   `XNC_INSTALLER_SYNC_REPO`；`XNC_INSTALLER_SYNC_INTERVAL` / `XNC_GITHUB_TOKEN`
   可调；sha256 边车强校验 + 事务入库，半截行自愈）——CI 恢复后即接回
   "GitHub Release → 生产" 的自动链。`/installer.json` 与下载页随后更新。
   在线 agent 经 WS 推送/6h 轮询/`xnc upgrade` 升级（秒级中断，失败自动
   回滚+拉黑）。
2. 本地构建：`make installer VERSION=<v>` 等价。五二进制（agent/core/
   host/shell/CLI，xnc-desktop 已换 xnc-host）版本同源注入；host 为
   cargo 构建（需 VCPKG_ROOT/LIBCLANG_PATH，`-ReuseNative` 可复用缓存）。
3. **升级安全网**：看门狗 schtask + installer-cache 回滚源 + 24h 过期
   pending 兜底；发布坏版本的自愈路径已内建（修复 = 新 PATCH 版本，同版本
   禁止重发）。
4. 当前线上锚点：下载页 `https://xnc.app/download`；短域
   `xnc.app/installer`。

## 6. 用户安装 / 注册 / 卸载

- **安装**：下载页或 `curl -LO https://xnc.app/installer` → 运行
  （管理员）→ 服务就位，**零凭据**，agent 空转 `awaiting registration`。
- **注册**：`xnc register`（server 固定 https://xnc.app，不再询问；
  --server/XNC_SERVER 仅为开发保留且已从帮助隐藏。TTY 交互登录+选
  cluster，非 TTY 用 --email + stdin 密码 + --yes）。成功后秒级 online。
  重装/换机身份 → adopt 沿用原节点。
- **会话**：`xnc login/logout`（用户级）；`logout` 不影响节点在线。
- **反注册**：`xnc deregister`（需管理员终端；删服务端记录+本地绑定，
  保留身份）。WS 通道不通时改用服务端管理删除。
- **升级**：`xnc upgrade [--channel stable|dev]`。
- **虚拟显示器（默认停用，2026-09-11）**：`xnc display on|off|status`（IDD 可选组件，Win10 19041+；
  策略 = RTV 会话接入且盒盖/无物理输出时自动建屏、会话结束移除、agent
  崩溃自动消失；实现见 `src/agent/display` + `src/third_party/xncidd`，
  验收记录 `docs/superpowers/plans/2026-09-10-idd-virtual-display-plan.md`）。
- **卸载**：图形向导（数据默认保留，重装续用身份）或
  `unins000.exe /VERYSILENT /PURGEDATA=true`（彻底清零）。卸载不反注册。

## 7. 运维要点

- **节点管理**：`DELETE /api/nodes/{id}`（平台管理员）删孤儿/换主记录；
  节点离线回收策略尚无自动化（待办）。
- **诊断模板**：agent 日志 `C:\ProgramData\XNC\logs\agent-service.log`
  （2026-09-15 规范起运行日志统一归 logs\；host/core/shell 同目录；
  安装目录下的同名 .log 是旧版残留）。健康链
  `registered via agentctl → binding detected (woken) → control connection
  ready`；卡在 wake 前=管道/注册问题，卡在 dial=网络问题。
- **已知网络约束**：agent 需可建立 websocket 长连接；严格代理网络会以
  "handshake 200" 形式失败（注册能过、上线不能）——排查代理/DNS，长期
  待办是轮询兜底通道。
- **实验闭环惯例**：卸载→清残留（服务/进程/任务/目录/PATH/注册表八项核验）
  →全新安装→注册上线→验证→deregister→卸载→双端零残留。

### 代码签名（自签过渡期）

- **背景**：未签名 exe 会被 Defender 随机隔离（实战发生过：xnc.exe 消失）。
- **证书**：正式主体 `CN=XNC Code Signing, OU=Release Engineering,
  O=XWorks Studio, C=CN`；`src/installer/codesign.cer`（公钥，入库）+
  `src/installer/codesign.pfx`（私钥，gitignored，**勿外传**）；密码在
  `deploy/.env` 的 `XNC_CODESIGN_PASSWORD`。3 年有效期，续期用
  `src/installer/make-cert.ps1` 重建、重签并给存量机器重导信任。
- **构建**：`build.ps1` 自动签名（五 exe 在 ISCC 前签、setup 在其后、
  sha256 覆盖签名后产物）；设 `XNC_CODESIGN_PASSWORD` 环境变量。时间戳
  多服务器回退，全败则免时间戳签名+告警。指纹经 `/DCertThumb` 传给
  xnc.iss，供安装器装卸信任。
- **信任分发（安装器内置）**：安装器运行时自动 `certutil -addstore`
  Root + TrustedPublisher（xnc.iss `InstallSignTrust`），卸载时按指纹
  `delstore`。**新机零手工**；仅存量老安装需手动导入一次 cer。首装时
  首装的 installer 自身仍显示未知发布者（自签引导的固有鸡生蛋，正式 CA 后消失）。
- **待办**：换正式 CA（EV）证书后，信任分发整体退役。

## 8. 已知小缺口（勿重复发现，按需修）

- `xnc exec` 的 `--cwd`/`--env` 值仍走 core argv（BuildChildCommandLine
  按契约拒绝内嵌双引号/尾反斜杠 → BAD_PAYLOAD）；命令本体已改经 stdin
  帧传输（core 写 secret 行后 u32LE 长度前缀帧，xnc-shell `--command-stdin`
  读），节点拒绝一律以 CLI 退出码 247 + 稳定码可见报错（2026-09-14
  静默 243 引号事故，fix/exec-inline-quote-reject）。
- `HEAD /installer` 落到 SPA（chi Get 不含 Head）。
- 安装器 PATH 写回会展开 REG_EXPAND_SZ 引用（触发于增删 XNC 项时）。
- 同版本重传 release 不删除缺席制品（旧制品可能残留可下载）。
- `deploy_srv.py push` 未实现"先删后解"（push 已退役为应急手段）。
- `.dockerignore` 使本地 compose build 失败（`build-server-local.ps1` 走
  同一 Dockerfile，构建前临时清掉即可；CI 远端构建不受影响）。
- CI publish.yml 的 vcpkg 无基线锁定：ffmpeg 头漂移（FF_PROFILE_* 枚举
  缺失）致 host 编译失败——安装器发布暂走本地构建直传。
- 阿里云安全组（主站）未放行 UDP443：仅影响内嵌/开发形态的 WT 腿
  （compose `XNC_RTV_WT_PORT` 过渡 14433/udp）；relay-only 生产形态主站
  不跑媒体腿，媒体在 relay 主机（r1/r2 已放行 UDP443/4433）。
- **2026-09-14 web terminal 经 relay 断连（根因已修正：会话路由器 Origin
  缺陷，非备案拦截）**：xnc-relay 的 session router `websocket.Accept`
  零值走同 Host Origin 校验——浏览器腿（页面源 xnc.app → relay 域
  r*.xnc.app）恒 403，CLI/agent 腿（无 Origin 头）恒通。Stage B
  （2026-09-11）接线时漏传 `--allow-origin`，即 web terminal 经 relay
  从未通过；当日并发的大陆备案拦截（80 端口 beian-block 页持续存在、
  443 SNI 级 RST 间歇波动）掩盖了判读。修复 `fix/relay-session-origin`
  （dc99d59）：patterns 接入双腿 Accept + 跨源回归测试；已直推双 relay
  （二进制换 `/usr/local/bin/xnc-relay`，备份 `.bak-origin`），浏览器
  经 `wss://r2.xnc.app:443` 全链验证通过。**现行**：sdata 已在 443 恢复
  宣告；caddy `:8443` 站点块双机已预置（域名证书，非标端口不经备案
  SNI 拦截），**安全组已放行 8443/TCP（2026-09-14 验证：浏览器经
  `wss://r2.xnc.app:8443` 真实终端会话全链通过）；逃生开关 = 改单元
  `--session-port 8443` + 重启 xnc-relay**（约 30 秒）。**2026-09-14 晚：
  安全组已移除 22/TCP（TCP 半开、SSH 不可达）——逃生开关与 push_relay.py
  均依赖 SSH，届时须先经阿里云控制台 Workbench/VNC 恢复 22（建议限源）
  再操作**。**遗留风险**：
  relay 域名证书（LE，至
  2026-12-10）续期 ~11-10 走 80/443 挑战均被备案拦截所阻，需提前改
  dns-01；主站 xnc.app 同域名在大陆，80/443 目前正常但风险同源。
  relay 主机 SSH 凭据在 deploy/.env 的 `RELAY1_*/RELAY2_*` 键。
- web HUD 码率/收包显示累计值（未做每秒差分）；e2e 显示跨时钟偏差。
- **显示器休眠（非无输出）→ RTV 采集全黑**（2026-09-14 真机实测）：物理
  屏休眠时 DXGI 采集内容为黑、变化驱动编码器近似零流量（relay ~53s
  idle-timeout 收会话）；IDD auto 策略只认盒盖/无物理输出，不认屏休眠。
  手动出路：`XNC_IDD_ENABLED=1`（机器环境）+ 重启 agent + `xnc display on`。
  待办：auto 建屏条件纳入屏休眠/黑帧检测，或 RTV 会话期间
  SetThreadExecutionState 保活显示。

## 9. 索引

- 产品 spec：`spec.md`（权威）· 快速上手：`README.md`
- CI/版本号/发布/部署设计：`docs/ci-release-and-deploy.md`（实施规格）
- 安装器/更新/凭据设计：`docs/superpowers/specs/2026-09-03-innosetup-installer-unified-auth-design.md`
- 实施与真机验收记录：`docs/superpowers/plans/2026-09-03-…-plan.md`（Results 节）
- 开发拓扑/凭据布局：`scripts/dev-topology.md` · `scripts/dev-services.md`
