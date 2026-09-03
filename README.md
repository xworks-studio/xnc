# XNC v2

Windows 节点统一运维入口：出站 443 反向连接（agent 主动连 server，不开入站端口）。为小团队和 AI Agent 设计。

[完整规格](spec.md) · [计划文档](docs/) · [部署](deploy/)

## 安装

### 装 Agent + CLI（安装器，推荐）

```cmd
curl -LO https://xnc.app/setup.exe && setup.exe
```

或浏览器打开 <https://xnc.app/setup.exe> 下载后双击。Inno Setup 安装器一次装齐 agent（Windows 服务 `XNCAgent`，Automatic）与 CLI（自动加 PATH）；安装期零凭据，装完服务空转等待注册（无 token、无需预先 admin 介入）。

首次使用在目标机执行（login → 选 cluster → 本机注册为节点，约 30 秒内上线）：

```bash
xnc register
```

### Dev 频道

```cmd
curl -LO "https://xnc.app/setup.exe?channel=dev" && setup.exe
```

### 一行流（deprecated）

```cmd
curl -sL xnc.app/a/<enrollment-token> | cmd    :: agent（deprecated）
curl -sL xnc.app/c | cmd                       :: CLI（deprecated）
```

已被 setup.exe + `xnc register` 取代（CLI 随安装器分发，无单独安装步骤）；保留 2 个 release 周期后删除。迁移细节见[统一认证设计 §14](docs/superpowers/specs/2026-09-03-innosetup-installer-unified-auth-design.md)。

## 快速上手

```bash
xnc login                            # 连接 server
xnc register                         # 本机注册为节点（未装 agent 时提示装 setup.exe）
xnc node list                        # 查看节点
xnc exec <node> "hostname"           # 执行命令（自动选 shell）
xnc exec <node> --shell bash "ls"    # bash（引号最简）
xnc exec <node> --shell cmd "dir"    # cmd.exe
xnc exec <node> --file deploy.ps1    # 脚本文件
xnc shell <node>                     # 交互终端（~. 断开）
xnc put <node> <local> <remote>      # 上传（sha256 校验）
xnc get <node> <remote> <local>      # 下载
xnc screen <node> --snap out.jpg     # 屏幕截图
xnc screen <node> --open             # 实时画面（浏览器）
xnc rdp <node>                       # 远程桌面
xnc update                           # CLI 自更新
```

## 多 Shell 执行

`xnc exec` 支持 5 种 shell + 环境变量 + 工作目录：

```bash
xnc exec node1 "hostname"                          # auto（探测最佳）
xnc exec node1 --shell bash "grep x *.log"         # Git Bash / WSL
xnc exec node1 --shell pwsh "Get-Process"          # PowerShell Core
xnc exec node1 --shell powershell "Get-Service"    # Windows PS 5.1
xnc exec node1 --shell cmd "ver"                   # cmd.exe
xnc exec node1 --file deploy.ps1                   # 脚本文件
cat script.sh | xnc exec node1 -                   # stdin 管道
xnc exec node1 --cwd C:\xnc --env DEBUG=1 "tool"   # env + cwd
```

## 发布频道与自更新

双频道：`stable`（正式）/ `dev`（开发测试）。Agent 收到推送后经安装器静默自更新（下载 setup.exe → sha256 校验 → 静默安装 → 回滚保护），零手工干预；`xnc upgrade` 可随时手动触发。存量 bundle 节点经最后一个 bundle 升级一次后切换到安装器更新（[迁移说明](docs/superpowers/specs/2026-09-03-innosetup-installer-unified-auth-design.md) §14）。

| 操作 | 命令 |
|---|---|
| 上传 release | `curl -X POST /api/admin/releases -H "Auth: Bearer $T" -F version=X -F setup=@xnc-setup-X.exe -F bundle=@... -F cli=@...` |
| 灰度单节点 | `curl -X POST /api/admin/rollout -d '{"version":"X","nodeId":"..."}'` |
| 切节点频道 | `curl -X POST /api/admin/rollout -d '{"nodeId":"...","channel":"dev"}'` |
| 手动升级本机 | `xnc upgrade [--channel dev]` |
| CLI 自更新 | `xnc update [--channel dev]` |

## 架构

```
Browser ──── Web UI / WebCodecs
    │
Server (Go) ─── REST + WS relay + release store + install endpoints
    │ (agent 主动出站 443)
Agent (Windows 服务)
    ├─ exec engine (bash/pwsh/powershell/cmd)
    ├─ shell (ConPTY)
    ├─ screen (DXGI Desktop Duplication → H.264)
    └─ self-updater (installer-orchestrated + rollback)
```

## 开发

```bash
cd deploy && docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d --build
(cd cli && go build -o ../bin/xnc.exe .)
(cd agent && go build -o ../bin/xnc-agent.exe ./cmd/xnc-agent)
```

## 构建安装器与版本注入（版本单一来源）

版本号只在构建时注入（agent 自报 / bundle manifest / 安装器同一来源；未注入回落
`0.0.0-dev`）。安装器：`make installer VERSION=<v> [CHANNEL=stable|dev]`（构建五个
exe 到 `bin/` 后经 Inno Setup 打包 `xnc-setup[-dev]-<v>.exe`）。

### 最后过渡 bundle（遗留，§14 迁移期）

仅供存量 bundle 节点的最后一次升级发布（bundle 携带编排版 agent，节点经 bundle
升级一次后永久切换到安装器更新）：

```bash
cd agent && go build -ldflags "-X xnc/agent/machineinfo.Version=<v>" -o ../bin/xnc-agent.exe ./cmd/xnc-agent
cd .. && go run scripts/build-bundle.go bin <v> bin/bundle-<v>.tar.gz   # 校验 agent 自报 == <v>
```

bundle 含 4 个 exe（xnc-agent + xnc-core/desktop/shell 三件套；screen-helper
已退役，0.4.6 起不再分发）。全网切换后本工具随 bundle 通道一并退役。

server 构建版本同理（`/api/health` 上报）：`deploy/.env` 设 `XNC_VERSION=<v>`
后 `py deploy/deploy_srv.py env && py deploy/deploy_srv.py up`，compose 经
Dockerfile `ARG XNC_VERSION` 注入；未设回落 `0.0.0-dev`。

## 测试设备凭据

`deploy/.env`（gitignored）：`cp .env.example .env` 后填入。server 端仅经 `py deploy/deploy_srv.py` 部署到 control.xnc.app，本机不部署 server。

## 负载 smoke

```bash
make load N=1000
# xnc node list --json | grep -o '"status":"online"' | wc -l
```
