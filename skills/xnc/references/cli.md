# xnc CLI 参考

## 命令树

| 命令 | 说明 |
| ---- | ---- |
| `xnc login [--server URL]` | 登录，凭证写入配置文件 |
| `xnc whoami` | 当前用户与 Server 信息 |
| `xnc cluster list / show / create / delete` | Cluster 管理（owner） |
| `xnc cluster member list / add / remove` | 成员与角色管理（owner） |
| `xnc token create <cluster> [--ttl 30m] [--max-uses 1]` | 生成节点注册 Token，明文只显示一次 |
| `xnc token revoke <id>` | 吊销 Token |
| `xnc node list [--cluster c] [--status online]` | 节点列表 |
| `xnc node show <node>` | 节点详情（含 shell_type、agent_version、last_seen） |
| `xnc node disable / enable <node>` | 停用/启用节点 |
| `xnc exec <node> [--timeout N] [--cwd PATH] -- <command...>` | 一次性命令 |
| `xnc run <node> (--file x.ps1 \| -) [--timeout N]` | 脚本执行，`-` 表示 stdin |
| `xnc shell <node> [--cols N] [--rows N]` | 交互式 PowerShell（需 TTY） |
| `xnc rdp <node> [--local-port N]` | 反向隧道 + mstsc |
| `xnc upload <node> <local> <remote>` | 上传（sha256 校验） |
| `xnc download <node> <remote> <local>` | 下载（sha256 校验） |
| `xnc screen <node> [--snapshot out.jpg \| --open] [--fps N]` | 桌面快照/只读预览（operator+） |
| `xnc audit list [--node/--user/--action/--since]` | 审计日志查询 |
| `xnc version` / `xnc status` | 版本 / Server 连通性 |

## Node 选择器

`web-01`（唯一名）· `production/web-01`（cluster/name）· `<node-id>`（UUID）· `--cluster` 组合 flag。名称歧义时报错并列出候选。

## 全局 flag

```text
--server   默认 $XNC_SERVER
--token    默认 $XNC_TOKEN，其次配置文件
--output   json | table（默认 table）
--timeout  请求超时（默认 30s；exec/run 默认 300s）
--yes      破坏性命令确认
```

## JSON envelope

```json
{"ok": true, "data": {}, "error": null}
{"ok": false, "data": null, "error": {"code": "NODE_OFFLINE", "message": "..."}}
```

exec / run 的 data 字段：`node`、`exitCode`、`stdout`、`stderr`、`durationMs`。
超时被 kill 时 `exitCode: null` 且 `timedOut: true`。

## 退出码

| 码 | 含义 |
| --: | ---- |
| 0 | 成功 |
| 2 | 用法错误 |
| 240 | 认证失败（重新 login） |
| 241 | 权限不足 |
| 242 | 节点离线 |
| 243 | 超时 |
| 244 | 资源不存在 |
| 245 | 网络错误 |
| 246 | 会话/配额超限 |
| 250 | 内部错误 |

远端命令已执行时，其 exitCode 原样透传，优先于上表。

## Server 错误码（error.code）

`UNAUTHORIZED` `FORBIDDEN` `CLUSTER_NOT_FOUND` `NODE_NOT_FOUND` `NODE_OFFLINE`
`ENROLLMENT_TOKEN_INVALID` `ENROLLMENT_TOKEN_EXPIRED` `SESSION_NOT_FOUND`
`SESSION_EXPIRED` `SHELL_START_FAILED` `TUNNEL_START_FAILED` `RDP_NOT_AVAILABLE`
`FILE_NOT_FOUND` `FILE_TOO_LARGE` `HASH_MISMATCH` `AGENT_VERSION_UNSUPPORTED`

## 典型工作流

```bash
# 注册新节点
xnc token create production --ttl 30m
# → 在目标节点执行：xnc-agent.exe install --server https://... --token <token>

# 批量巡检
xnc node list --status online --json
xnc exec web-01 -- Get-Service WinRM
xnc exec web-01 -- Get-PSDrive C | Select-Object Used,Free

# 部署脚本并执行
xnc upload web-01 ./health.ps1 C:\Temp\health.ps1
xnc exec web-01 -- pwsh -File C:\Temp\health.ps1

# 取回日志
xnc download web-01 C:\logs\app.log ./web-01-app.log
```
