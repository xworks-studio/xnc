# CI、版本号与发布部署 设计（2026-09-05）

> 状态：已实施并投产（2026-09-08 随 RTV 重构更新运维现状：安装器发布走
> 本地构建 + 管理员 API 直传、server 走本地镜像直推、watchtower 停用——
> 详见 §3.5/§4.4）。四部分相互咬合：版本规则是地基，测试门禁是
> 发布前置，发布流水线消费版本规则，手动部署是 server 侧的受控出口。
> 实施落点：`.github/workflows/`；本文即实施规格。

## 1. 版本号规则（含扩容）

### 1.1 格式

```
MAJOR.MINOR.PATCH[-dev]
段内 1–4 位数字；每段独立计数、独立扩容。
例：0.8.0 / 0.8.1123 / 1.25.3386 / 0.9.0-dev
```

- **段宽 4 位（上限 9999/段）**：选型依据 Windows 版本资源的
  VS_FIXEDFILEINFO 每段上限 65535——4 位留足余量且永不越界；regex
  `^\d{1,4}\.\d{1,4}\.\d{1,4}(-dev)?$`（tag 带 `v` 前缀）。
- **dev 后缀为裸 `-dev`，不带任何序号/其他字符**。dev 频道的迭代
  靠**递增 PATCH**（0.9.0-dev → 0.9.1-dev → …），禁止同版本重发：
  在线 agent 以版本号判断新旧，同号重传的内容变化不被感知（upsert
  只影响新下载者）——该约束为硬规则。
- 比较语义沿用 agent 现有 CompareVersions（无后缀 > `-dev`；数字段
  任意长度数值比较——当前实现已满足本方案，无需改动）。

### 1.2 语义（何时 bump 谁）

| 段 | 何时递增 | 说明 |
|---|---|---|
| MAJOR | 协议/proto 破坏性变更 | agent↔server 不兼容窗口出现时；GA 前允许 0→1 一次性跨越 |
| MINOR | 功能发布 | 跨组件新能力（安装器/CLI/server 联动） |
| PATCH | 修复 | 单组件缺陷修复、重打包 |
| `-dev` 后缀 | dev 频道标记 | 裸后缀；该频道每次发布递增 PATCH |

- **0.x 期（当前）**：MINOR 承担功能演进（0.8、0.9 …），PATCH 修缺陷；
  `1.0.0` 留给 GA（对外承诺兼容性之时）。段内 4 位容量即"扩容"答案：
  无需跳位规则，每段自然增长到 9999。
- 版本单一来源铁律不变：git tag → 构建注入（agent ldflags / server
  XNC_VERSION / 安装器 `/DVersion`）== `installer.json.version`。

### 1.3 版本的载体与流转

- **手动发版是唯一动作**（§3 CI publish.yml；CI 修复前为 §3.5 本地
  直传）：CI 在当前 main HEAD 创建并推 tag `vX.Y.Z[-dev]` 锚定版本——tag
  仍由 CI 产生且不可移动，但发版节奏由人经 Actions 触发决定。
- server 版本与 agent 版本**独立但 MINOR 对齐**（同一批发布的 server
  版本号 = agent 正式版号；PATCH 允许各自独立）。
- 回滚 = 发一个新的 PATCH 版本，**永不**删除/移动已发布 tag 或同版本
  重传不同内容（同版本 upsert 会改道——已知陷阱，规则性禁止依赖它）。

## 2. CI 测试工作流（`.github/workflows/ci.yml`）

### 2.1 触发与门禁

- `pull_request`（目标 main）+ `push`（main）。PR 合并的门禁 = 全绿。
- concurrency：同 PR 取消旧跑，省额度。

### 2.2 矩阵

```yaml
jobs:
  go-linux:            # ubuntu-latest
    # proto / cli / shellhost / shellsmoke / tools/*（含 rtvload）
    # go build + go test + go vet；server 用 PG service container
    services: postgres:16（POSTGRES_USER/PASSWORD/DB 注入）
    steps: go test ./... （server 的 testcontainers 在 ubuntu docker 可用）

  go-windows:          # windows-latest
    # agent 全量（含 updater/orchestrate 真机语义测试）+ 签名自测不跑
    # （无 pfx），shellhost integration_windows_test
    steps: go test ./... （agent、shellhost 模块）

  web:                 # ubuntu-latest, node 22
    # npm ci → oxlint → vitest → tsc -b && vite build

  installer-dryrun:    # windows-latest，仅 main push（PR 可选）
    # 无证书构建：ISCC 装机 + build.ps1 走到 XNC-Installer-<ver>.exe 产出
    # 保障 .iss/build.ps1 不烂；native/host 产物按 hashFiles 缓存复用
    # （bin/xnc-core.exe + bin/xnc-host.exe）

  native:              # windows-latest
    # native/core 的 build.bat 编译门禁（desktop 已随 RTV 重构删除）

  host-rust:           # windows-latest（2026-09-08 新增）
    # xnc-host（Rust 采集/编码端）：cargo test（RS 黄金向量/组帧契约）
    # + cargo build --release 编译门禁
    # vcpkg 重依赖（ffmpeg[amf,nvcodec,qsv] + libyuv，x64-windows-static），
    # VCPKG_DEFAULT_BINARY_CACHE + actions/cache 按 workflow 哈希复用
```

### 2.3 规则

- 各 job 缓存：`actions/setup-go` 自带模块缓存 + `actions/cache` 缓存
  `bin\`（native 编译产物按 key: hashFiles(src/native/**) 复用）。
- 失败输出：`if: failure()` 上传失败日志（go test 输出、build.log）。
- 禁止 CI 里出现任何凭据（测试全用 testcontainers/临时凭据）。
- 已知环境债：`src/agent/session` 的 GOOS=linux 构建失败是预存在问题，
  CI 的 linux job 对该包 `//` 排除并在 job 内注释引用问题编号（不修
  不挡——避免 CI 一上线就红）。

## 3. GitHub CI 发布规则（`.github/workflows/publish.yml`）

> 2026-09-08：原 release.yml 因步骤名含未引号冒号损坏 workflow 注册表
> （dispatch 422），重命名为 publish.yml 并修复；随后又暴露 vcpkg 陈旧
> ffmpeg 基线问题（FF_PROFILE_* 枚举缺定义编译失败）——**CI 发布暂不可
> 用，现行安装器发布走 §3.5 本地直传**。CI 恢复条件：锁 vcpkg baseline
> 或 vendor 化 ffmpeg 头。

### 3.1 触发与前置校验

- 触发：`workflow_dispatch` 输入 `version`（X.Y.Z[-dev]，段 ≤4 位）；
  须选 main 分支——workflow 校验 HEAD == origin/main，否则 fail。
- 版本不可变预检：`v<version>` tag 已存在即拒绝（`git ls-remote` 预检；
  修复 = 新 PATCH 版本）。
- `-dev` 后缀发布到 dev 频道，其余 stable（与触发方式无关，按输入推导）。

### 3.2 流水线（单 job，windows-latest，串行步骤）

```
1. checkout(main, fetch-depth 0) + 校验版本格式 / HEAD == origin/main /
   tag 未占用；Release 正文取 main HEAD 提交信息（手动触发无
   head_commit 事件，经 GITHUB_ENV 多行段传递）
2. 装 Inno Setup 6（choco install innosetup）
3. build.ps1 -Version <输入> -Channel <stable|dev 由输入推导>
   - 签名：XNC_CODESIGN_PFX（base64 secret → 文件）+
     XNC_CODESIGN_PASSWORD（env）→ build.ps1 自动签名（含时间戳探测）
   - 产物：bin/XNC-Installer-<v>[-dev].exe + .sha256
4. 在 main HEAD 创建并推 tag v<version>（annotated，xnc-release-bot
   身份）——构建成功后才打：构建/签名失败不产生孤儿 tag，可直接重触发
5. GitHub Release（softprops/action-gh-release，tag_name 显式指向新
   tag——手动触发的 ref 是 main 而非 tag）：
   附件 = 安装器 + .sha256；正文 = main HEAD 提交信息；
   dev 频道标 prerelease（GitHub "Latest" 只指向 stable，与下载页一致）
6. 通知：job summary 列出版本、sha256、频道与验证命令
   （curl /installer.json?channel=，约一个同步间隔内生效）
```

### 3.3 发布规则条文

- **手动触发即发布**：无 workflow_dispatch 不发布；发布节奏由人决定，
  与 build-server（server 镜像）一致。
- **不可变**：tag 已存在即被预检拒绝（+ GitHub tag 保护）；修坏版本 =
  新 PATCH 版本。极端恢复：tag 已推而 Release 步骤失败时，管理员删 tag
  后重触发，或直接发下一 PATCH 版本。
- **CI 不触碰生产**：发布终点是 GitHub Release（唯一事实源）；
  release.yml 不再持有任何 SRV_ 生产凭据，生产生效由 server 侧
  installersync（§3.4）负责——拉取失败在 server 日志告警并按间隔
  重试，无需流水线干预。
- 在线节点拉取路径（推送/轮询/看门狗回滚）全部既有，无需流水线干预。
- 仓库分支保护（配套设置，非 workflow）：main 需 CI 绿 + 禁止 force
  push；tag 保护规则 `v*` 禁止删除与移动（CI 创建的 tag 同受保护）。

### 3.4 生产侧拉取（installersync，server 内置）

`src/server/internal/installersync`：GitHub Releases → 本地 release store
的定时拉取，/installer 与 /installer.json 服务路径与 agent 更新契约
零改动。

- **频道判定按 tag**（`vX.Y.Z` → stable，`vX.Y.Z-dev` → dev），不依赖
  release 的 prerelease 标记——标记配错不影响分发正确性。
- **同步范围**：各频道最新一条（列表端点一次调用双频道共用）；本地
  已有（release 行 + setup 制品俱在）则跳过，release 行在而制品缺的
  半截行会触发重新拉取补全（自愈直传时代遗留形态）。
- **校验**：.sha256 边车强校验 + MZ 魔数 + 128MiB 上限（与 admin 上传
  路径同限）；入库走事务（半截提交会让 latest 命中无制品行）。
- **API 配额**：ETag 条件请求（304 不计限额）；匿名 60 req/h 对分钟级
  轮询足够，`XNC_GITHUB_TOKEN` 可选兜底。
- **旋钮**（deploy/docker-compose.yml 透传）：`XNC_INSTALLER_SYNC_REPO`
  （默认 xworks-studio/xnc，空 = 关闭）、`XNC_INSTALLER_SYNC_INTERVAL`
  （默认 5m，下限 1m）、`XNC_GITHUB_TOKEN`。
- `POST /api/admin/releases` 上传端点保留为人工应急通道（语义不变）。

### 3.5 本地构建直传（现行发布路径）

CI publish.yml 修复前的实际路径（与正规路径的节点升级语义完全一致，
仅缺 tag 锚定——版本不可变改靠发布纪律）：

```
1. powershell src/installer/build.ps1 -Version <v> -Channel stable|dev
   （本地签名：XNC_CODESIGN_PASSWORD 环境变量；产 bin/XNC-Installer-<v>.exe + .sha256）
2. POST /api/admin/releases （multipart：file + sha256 边车 + channel/version
   字段；生产 admin JWT）→ 直写生产 release store
3. 在线 agent 经既有 WS 推送秒级升级；/installer.json 与下载页即时更新
```

注意：直传不产生 git tag 与 GitHub Release——发布记录以生产 release
store 为准；同版本禁止重传不同内容（agent 侧版本比较感知不到同号变化）。

## 4. Server 镜像化交付（本地直推为主，GHCR + Watchtower 备用）

**架构（原设计）**：CI 构建版本镜像推 GHCR（GitHub 自带注册表）；服务器侧
Watchtower（compose 内独立容器）轮询注册表自动拉取重建。与 server
产品本身零耦合——不把基础设施交付嵌进产品 API。

### 4.4 现行路径：本地构建直推（2026-09-08 运维决策）

远端构建/注册表中转耗时不做——`deploy/build-server-local.ps1
-ServerVersion <v>`（参数名避开 -Version：powershell.exe -File 引擎参数
冲突）：

```
本地 docker build（web dist 先入上下文，XNC_VERSION 注入）→ docker save
→ deploy/push_server_image.py（paramiko 密码认证，.env 的 SRV_*）：
SFTP 上 SRV → docker load + tag ghcr.io/...:latest → docker compose up -d
```

- 凭据读 `deploy/.env` 的 `SRV_HOST/SRV_SSH_USER/SRV_SSH_KEY`。
- SRV 上 watchtower **已停用**（容器存在但不跑）——重启它等于恢复
  §4.2 的自动换版，本地直推期间保持停用。
- 验证/回滚姿势不变：`/api/health` version 比对；回滚 = load 旧 tar
  或改 compose tag 为 `:vX.Y.Z` 后 `up -d`。

### 4.1 构建推送（`build-server.yml`，**手动触发 + 版本号输入，现为备用**）

- workflow_dispatch 输入 `version`（X.Y.Z[-dev]，段 ≤4 位；须先合入
  main——构建的是当前 main 代码）。server 发布节奏由人决定，不随
  push 自动出（与 agent 发版对齐）。
- 镜像 `ghcr.io/xworks-studio/xnc-server`：标签 `v<版本>` + `latest`
  （Watchtower 跟踪）+ `sha-<短哈希>`（溯源）。
- 多阶段构建沿用 `deploy/Dockerfile`（workflow 先构建 web dist 进上下文；
  服务器从不跑 npm），GITHUB_TOKEN 推送，buildx GHA 缓存。

### 4.2 服务器侧（Watchtower，compose 内独立服务）

- 仅管理带 `com.centurylinklabs.watchtower.enable=true` 标签的服务
  （当前只有 xnc-server）；5 分钟轮询；`--cleanup` 清旧镜像层。
- compose 引用 `ghcr.io/...:main`；**锁版本/回滚** = 改成 `:vX.Y.Z`
  或 `:sha-<哈希>` 后 `up -d`（版本不可变原则下的回滚姿势）。
- `/opt/xnc` 只有 `deploy/`——无源码、无脚本、无 cron。
- Caddyfile 变更仍 SSH 手工（换 inode 需 `--force-recreate`）。

### 4.3 GHCR 私有包的服务器授权（一次性）

服务器 `docker login ghcr.io` 需要只读凭据，二选一：
- **PAT（推荐）**：GitHub → Settings → Developer settings →
  Fine-grained PAT，仅 `contents/packages: read`，登 SRV 执行
  `docker login ghcr.io -u <user>` 粘贴 PAT（存 /root/.docker/config.json，
  Watchtower 挂载同一文件）。
- **公开包**：首次推送后在包设置改 public，服务器匿名拉取
  （代价：server 二进制公开可下载）。

## 5. 配套设置清单（已实施核对：2026-09-08）

- [x] 分支保护：main（CI 必需检查 + 禁 force push）
- [x] tag 保护：`v*`（禁删/移）
- [x] Secrets：`XNC_CODESIGN_PFX`、`XNC_CODESIGN_PASSWORD`、
      `XNC_SERVER_URL`、`XNC_ADMIN_EMAIL`、`XNC_ADMIN_PASSWORD`、
      `SRV_HOST`、`SRV_SSH_USER`、`SRV_SSH_KEY`
- [x] AGENTS.md §3/§4/§5 增补：CI 门禁与发版/部署的触发方式

## 6. xnc-relay 外置中继部署（2026-09-11 Stage A：媒体面 relay-only）

主站默认 `XNC_RTV_EMBEDDED=false`（compose 同步默认）：主站不跑任何媒体
流量——不创建内嵌 rtv.Server、不挂 `/ws`、不启 ACME。桌面媒体只经外置
xnc-relay；relay 池无可用者时 desktop 直接 `503 RTV_NO_RELAY`（不做内嵌
兜底）。过渡期（首台 relay 就位前）可在 SRV 的 compose 临时
`XNC_RTV_EMBEDDED=true` 保内嵌运行。

### 6.1 relay 主机要求与端口

- 二进制：`deploy/build-relay.ps1`（linux/amd64 交叉构建）+ `push_relay.py`
  推送（凭据 `deploy/.env` 的 `RELAY1_*` 键）；systemd 单元
  `deploy/xnc-relay.service`（参数模板渲染）。
- 端口：UDP 443（viewer WT 主路，自签证书 + 浏览器 serverCertificateHashes
  钉扎）、UDP 4433（host 腿 QUIC，自签证书 + server 下发 certSha256 钉扎）、
  TCP 443（可选：caddy 前置 → relay `127.0.0.1:8080` HTTP 腿 `/ws` 浏览器
  兜底 + `/healthz`）。
- **浏览器 WS 兜底腿的域名前置**：WS 无法钉扎自签证书——`/ws` 兜底要
  caddy（或 `--http-cert/--http-key` CA 证书）直启 TLS；纯 IP relay 的
  浏览器路径只有 WT（钉扎），`/ws` 候选握手必败后 viewer 自然回落，无害。

### 6.2 首次接入与审批

1. SRV 配 `XNC_RTV_SIGNING_KEY`（ed25519 64B hex，持久；空 = 进程内临时
   生成，server 重启后全部 relay 票据作废需重连重同步）。
2. relay 启动后自动经 `wss://xnc.app/api/relay/connect` 注册（ed25519
   挑战-应答）。首注册落 `pending`；`GET/PATCH /api/admin/relays` 审批为
   `active`，或预先在 `XNC_RTV_RELAY_ALLOWLIST` 放公钥（hex，逗号分隔）
   注册即 active。
3. relay 认证通过即上报 `RELAY_RECONCILE`（在服会话集）；心跳 30s、统计
   10s（含 ActiveSids → server 代 Touch 活跃会话）；server 下发
   `RELAY_CONFIG`（票据签名公钥）、`RELAY_SESSION_KILL`（撤销）与
   `RELAY_SESSION_GRANT`（被动首约，外部会话与内嵌 UX 一致）。

### 6.3 多 relay 负载均衡（现行设计）

- **分配**（`rtvpool.Assign`）：**节点粘性**——节点首个会话选中后沿用
  （host 张票 (node,relay) 字节等值、core 幂等复用 host 的前提）；粘性
  失效（relay 不合格）时在合格集内选 **负载评分最低** 者
  （loadNorm = sessions/maxSessions）。
- **合格性**：status=active + 90s 内有心跳 + host 腿健康探测（30s 一探，
  连续 2 败出局）+ 时钟漂移 <5min。
- **摘除/缩容**：admin `PATCH /api/admin/relays` 置非 active 状态即停止
  新分配（存量会话自然收敛；drain 编排列后续）。
- 观测：`GET /api/rtv/stats`（JWT）的 `relays` 数组 + web Monitor 页
  relay 池表格。

### 6.4 主站侧配置速查（relay-only 生产形态）

```
XNC_RTV_EMBEDDED=false        # 默认；主站不跑媒体
XNC_RTV_SIGNING_KEY=<hex64>   # 必须（持久票据签名）
XNC_RTV_RELAY_ALLOWLIST=<hex> # 可选（免审批）
# XNC_RTV_ENDPOINT/HOST_ADDR/WT_ADDR/CERT_FILE/KEY_FILE/WT_PORT 均仅
# 内嵌模式生效，relay-only 下忽略（配置了会告警提示）
```

## 7. 会话数据面走 relay（2026-09-11 Stage B）

exec/shell/file/tunnel 的会话 WS 双腿也迁到 xnc-relay（主站彻底只剩
web/认证/编排信令）：

- **协议**：relay HTTP 腿新增 `/api/agent/session?token=`（agent 腿）与
  `/api/session/{sid}?token=`（客户端腿），路径与主站同形——agent 引擎、
  CLI、web（toWsUrl 透传绝对地址）对 URL 形态零假设，**存量端零改动**。
  准入 = sdata 票据（ed25519，sid 粒度，sub=agent/client 区分腿）；
  粘合后帧不透明双向泵（复刻主站 pump 语义：8MiB 读限、任一腿断即终局）。
- **活跃/终局**：relay STATS 的 ActiveSids 并入会话数据 sid → server 代
  Touch（idle 治理）；泵终局上报 RELAY_SESSION_CLOSED → server
  NotifyClose（peer-disconnect 等价）；server 撤销（SessionKill）同步关
  relay 侧双腿 + 墓碑。
- **server 双路径**：startSession 对非 desktop kind 经 pool.Assign 找
  sdata 端点——有则签发双腿票据、SESSION_OPEN.WsURL 与 202 websocketUrl
  改指 relay（MarkRelayRouted：Opening TTL 停臂，双腿不回主站）；无
  （relay 未宣告/池空）= 主站旧路径，行为不变。节点粘性与桌面媒体共享
  （同 node 全 kind 同 relay）。
- **激活前置（硬性）**：浏览器/CLI 的 WS 腿无法钉扎自签证书——relay 需
  **域名 + caddy（或 --http-cert CA 证书对）**。DNS（如 r1.xnc.app →
  relay IP）就绪后在 relay 主机装 caddy 反代 TCP443 → 127.0.0.1:8080，
  再以 `--session-host r1.xnc.app` 重推 relay（push_relay.py 参数）。
  未宣告 sdata 前一切会话走主站旧路径——**合并/部署零风险，DNS 就绪即
  接通**。
- **票据 TTL**：sdata 1h（只在双侧拨号窗口消费）；撤销墓碑复用
  RELAY_SESSION_KILL 通道。
```
