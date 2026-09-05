# CI、版本号与发布部署 设计（2026-09-05）

> 状态：设计稿（未实施）。四部分相互咬合：版本规则是地基，测试门禁是
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

- **git tag `vX.Y.Z[-dev]` 是唯一发版动作**：打 tag 触发发布流水线
  （§3）。tag 必须打在 main 上、不允许 force-push 已有 tag。
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
    # proto / cli / mockagent / shellhost / shellsmoke / tools/*
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
    # 保障 .iss/build.ps1 不烂；产物 sha256 上传 artifact 供人工核验

  native:              # windows-latest
    # native/core + native/desktop 的 build.bat 编译门禁（selftest 编译即可）
```

### 2.3 规则

- 各 job 缓存：`actions/setup-go` 自带模块缓存 + `actions/cache` 缓存
  `bin\`（native 编译产物按 key: hashFiles(native/**) 复用）。
- 失败输出：`if: failure()` 上传失败日志（go test 输出、build.log）。
- 禁止 CI 里出现任何凭据（测试全用 testcontainers/临时凭据）。
- 已知环境债：`agent/session` 的 GOOS=linux 构建失败是预存在问题，
  CI 的 linux job 对该包 `//` 排除并在 job 内注释引用问题编号（不修
  不挡——避免 CI 一上线就红）。

## 3. GitHub CI 发布规则（`.github/workflows/release.yml`）

### 3.1 触发与前置校验

- 触发：`push: tags: ['v*']`（仅 main 上的 tag——workflow 内校验
  `git merge-base --is-ancestor <tag> main`，否则 fail）。
- tag 格式校验：`^v\d{1,4}\.\d{1,4}\.\d{1,4}(-dev)?$`（段宽 ≤4 位，
  dev 仅裸后缀；`-dev` tag 发布到 dev 频道）。

### 3.2 流水线（单 job，windows-latest，串行步骤）

```
1. checkout(tag) + 校验 tag 在 main 上 + 格式合法
2. 装 Inno Setup 6（choco install innosetup）
3. build.ps1 -Version <tag去v前缀> -Channel <stable|dev 由 tag 推导>
   - 签名：XNC_CODESIGN_PFX（base64 secret → 文件）+
     XNC_CODESIGN_PASSWORD（env）→ build.ps1 自动签名（含时间戳探测）
   - 产物：bin/XNC-Installer-<v>[-dev].exe + .sha256
4. 上传 GitHub Release（softprops/action-gh-release）：
   附件 = 安装器 + .sha256；正文 = tag message（发版说明写进 tag）
5. 上传生产 release store：
   - Secrets: XNC_SERVER_URL=https://xnc.app, XNC_ADMIN_EMAIL/PASSWORD
   - 登录取 JWT → POST /api/admin/releases（version/notes/channel/setup=@）
   - 校验回读 /installer.json?channel= 的 version+sha256 与本地一致
6. 通知：job summary 列出版本、sha256、频道、下载页链接
```

### 3.3 发布规则条文

- **tag 即版本**：无 tag 不发布；`-dev` 标签发布到 dev 频道，其余
  发布到 stable。
- **不可变**：同 tag 重推被 CI 拒绝（步骤 1 校验 + GitHub tag 保护）；
  修坏版本 = 新 PATCH tag。
- **发布窗口自检**：上传成功 ≠ 完成——步骤 5 的回读校验失败即 job
  失败（并发出醒目 summary），防止"上传假成功"（部署事故教训）。
- 在线节点拉取路径（推送/轮询/看门狗回滚）全部既有，无需流水线干预。
- 仓库分支保护（配套设置，非 workflow）：main 需 CI 绿 + 禁止 force
  push；tag 保护规则 `v*` 禁止删除与移动。

## 4. Server 镜像化交付（GHCR + Watchtower）

**架构**：CI 构建版本镜像推 GHCR（GitHub 自带注册表）；服务器侧
Watchtower（compose 内独立容器）轮询注册表自动拉取重建。与 server
产品本身零耦合——不把基础设施交付嵌进产品 API。

### 4.1 构建推送（`build-server.yml`，push main / tag v* 触发）

- 镜像 `ghcr.io/xworks-studio/xnc-server`：
  - main push → 标签 `main`（浮动）+ `sha-<短哈希>`；版本 `main-<短哈希>`
  - v* tag → 标签 `vX.Y.Z` + `latest`；版本 = tag 版本
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

## 5. 配套设置清单（实施时一次做完）

- [ ] 分支保护：main（CI 必需检查 + 禁 force push）
- [ ] tag 保护：`v*`（禁删/移）
- [ ] Secrets：`XNC_CODESIGN_PFX`、`XNC_CODESIGN_PASSWORD`、
      `XNC_SERVER_URL`、`XNC_ADMIN_EMAIL`、`XNC_ADMIN_PASSWORD`、
      `SRV_HOST`、`SRV_SSH_USER`、`SRV_SSH_KEY`
- [ ] 签名私钥 pfx 的 base64 导出命令记录在 AGENTS.md 签名节
- [ ] AGENTS.md §3/§4/§5 增补：CI 门禁与发版/部署的触发方式
```
