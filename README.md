# XNC v2

Windows 节点统一运维入口：出站 443 反向连接（agent 主动连 server，不开入站端口）。为小团队和 AI Agent 设计。

[完整规格](spec.md) · [计划文档](docs/) · [部署](deploy/) · [操作规范](AGENTS.md)

## 安装

### 装 Agent + CLI（安装器，唯一入口）

浏览器打开 **<https://xnc.app/download>** 下载安装器并运行（需管理员）。

Inno Setup 安装器一次装齐 agent（Windows 服务 `XNCAgent`，Automatic）与 CLI（自动加 PATH），并自动安装代码签名信任；安装期零凭据，装完服务空转等待注册（无 token、无需预先 admin 介入）。

首次使用在目标机执行（login → 选 cluster → 本机注册为节点，秒级上线；重装/换机沿用原节点记录）：

```bash
xnc register
```

Dev 频道安装器在下载页选择，或 `curl -LO "https://xnc.app/installer?channel=dev"`。

## 快速上手

```bash
xnc login                            # 连接 server
xnc register                         # 本机注册为节点（未装 agent 时提示先装安装器）
xnc status                           # 本机安装/注册/在线状态 + 当前会话
xnc node list                        # 查看节点
xnc exec <node> "hostname"           # 执行命令（自动选 shell）
xnc exec <node> --shell bash "ls"    # bash（引号最简）
xnc exec <node> --file deploy.ps1    # 脚本文件
xnc shell <node>                     # 交互终端（~. 断开）
xnc put <node> <local> <remote>      # 上传（sha256 校验）
xnc get <node> <remote> <local>      # 下载
xnc screen <node> --snap out.jpg     # 屏幕截图
xnc screen <node> --open             # 实时画面（浏览器）
xnc rdp <node>                       # 远程桌面
xnc upgrade [--channel dev]          # 手动触发本机升级
xnc logout                           # 退出用户会话（节点不受影响）
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

双频道：`stable`（正式）/ `dev`（开发测试）。Agent 收到推送后经安装器静默自更新（下载 installer → sha256 校验 → 静默安装 → 看门狗回滚保护），零手工干预；`xnc upgrade` 可随时手动触发。

## 架构

```
Browser ──── Web UI / WebCodecs / 下载页
    │
Server (Go, xnc.app) ─── REST + WS 信令 + release store + 安装器分发
    │ (agent 主动出站 443；媒体经 TURN 中继)
Agent (Windows 服务, SYSTEM)
    ├─ exec engine (bash/pwsh/powershell/cmd)
    ├─ shell (ConPTY)
    ├─ screen (DXGI Desktop Duplication → H.264, 经 XNCCore)
    ├─ agentctl 管道（register/deregister/status/upgrade 本地控制面）
    └─ self-updater (installer-orchestrated + rollback)
```

## 开发与交付

> Agent/工程师操作规范（硬性契约、部署、发布、签名）见 [AGENTS.md](AGENTS.md)；CI/版本/发布/部署设计见 [docs/ci-release-and-deploy.md](docs/ci-release-and-deploy.md)。

- **测试门禁**：PR/push main → `ci.yml` 五矩阵（Linux Go + PG、Windows agent/shellhost、web、native、安装器试构建）。
- **安装器发版**：打 tag `vX.Y.Z[-dev]` → `release.yml` 签名构建 → GitHub Release（发布终点）；生产 server 由 installersync 定时拉取，在线节点自动升级。
- **server 发版**：手动触发 `build-server` workflow 输入版本号 → GHCR `v<版本>+latest` → Watchtower 轮询 `:latest` 自动换版（5–10 分钟）。
- **本地开发栈**：

```bash
cd deploy && docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d --build
(cd cli && go build -o ../bin/xnc.exe .)
(cd agent && go build -o ../bin/xnc-agent.exe ./cmd/xnc-agent)
```

- **版本号**：`MAJOR.MINOR.PATCH[-dev]`，段 ≤4 位，git tag 为唯一发版动作（版本单一来源：tag → 构建注入 == installer.json == /api/health）。

## 测试设备凭据

`deploy/.env`（gitignored）：`cp .env.example .env` 后填入。server 只部署在 xnc.app（阿里云 SRV，镜像化交付），本机禁止运行 server 栈。

## 负载 smoke

```bash
make load N=1000
# xnc node list --json | grep -o '"status":"online"' | wc -l
```
