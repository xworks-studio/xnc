# XNC — Windows 节点控制

XNC 是一个 Windows 节点控制系统：服务端（Go）管理节点，Agent（Rust）运行在被控 Windows 节点上，通过 WebSocket 长连接接收指令；CLI（Go）提供命令行操作入口。

## 目录结构

| 目录 | 说明 |
| --- | --- |
| `xnc-server/` | 服务端（Go）：REST API、Agent 网关、PostgreSQL |
| `xnc-agent/` | Agent（Rust workspace）：`xnc-proto` 协议库 + `xnc-agent` 主程序 |
| `xnc-cli/` | 命令行客户端（Go） |
| `web/` | Web 前端 |
| `proto/` | 协议消息契约文档 |
| `skills/` | 仓库工程规范与技能文档 |
| `deploy/` | 部署配置 |
| `scripts/` | 辅助脚本 |

## 快速开始

```bash
make db-up    # 启动 PostgreSQL 16 (localhost:5432, xnc/xnc/xnc)
make server   # 启动服务端
make cli      # 启动 CLI
make test     # 运行全部测试 (Go + Rust)
```

依赖：Go 1.26+、Docker、（Agent 开发）Rust、（Web 开发）Node.js。凭据放 `config.env`（已 gitignore，严禁入库）。
