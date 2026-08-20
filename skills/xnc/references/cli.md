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
--output   json | table（默认 table）；数据命令另有 --json 简写，等价于 --output json
--timeout  exec/run 的命令/脚本超时（默认 300s，范围 1-86400），仅这两条命令有
--yes      破坏性命令确认
```

## exec / run 细节

- `--timeout N`：命令/脚本超时秒数，默认 300，范围 1-86400；到点 agent kill 远端整个进程树并返回 `timedOut:true`（CLI 退出码 243）
- `exec` 独有 `--cwd PATH`：远端工作目录
- `--` 之后的所有 token 都是远端命令参数（含 `-Verbose` 等 flag），不再被 CLI 解析；命令带参数时必须用 `--`，否则被 CLI 当作自己的 flag 报用法错误（退出码 2）
- `run` 的脚本来源：`--file <path>` 或 `-`（读 stdin；位置参数形式 `xnc run n1 -` 等价）；脚本 ≤ 256 KB，超出 CLI 预检直接拒绝（退出码 2），upload+exec 大脚本路径后续版本提供
- `--json` 时 stdout/stderr 实时流先输出，envelope 固定为最后一行——按行（jsonl）解析时取尾行即可

## JSON envelope

```json
{"ok": true, "data": {}, "error": null}
{"ok": false, "data": null, "error": {"code": "NODE_OFFLINE", "message": "..."}}
```

exec / run 的 data 字段：`node`、`exitCode`、`stdout`、`stderr`、`durationMs`、`timedOut`。
超时被 kill 时 `exitCode: null` 且 `timedOut: true`。
`--json` 下 stdout/stderr 实时流先于 envelope 输出，envelope 是最后一行。

## 退出码

| 码 | 含义 |
| --: | ---- |
| 0 | 成功 |
| 2 | 用法错误 |
| 240 | 认证失败（重新 login） |
| 241 | 权限不足 |
| 242 | 节点离线 |
| 243 | exec/run 超时或未执行完（`timedOut:true`） |
| 244 | 资源不存在 |
| 245 | 网络错误（含 exec/run 会话在收到 EXEC_RESULT 前断开：`NETWORK`, "session ended without result"） |
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
