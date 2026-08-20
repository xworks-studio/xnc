# Skill: xnc
# XNC Windows 节点运维

用 xnc CLI 操作 XNC 管理的 Windows 节点：查状态、执行命令与脚本、传输文件。面向 AI Agent 设计，所有命令支持 --json。

## 前置检查

1. 环境变量：`XNC_SERVER`（如 https://control.example.com）、`XNC_TOKEN`；缺失时先执行 `xnc login`
2. 连通性：`xnc whoami --json`

## 节点发现

```bash
xnc node list --json
xnc node show production/web-01 --json
```

节点离线时错误码为 NODE_OFFLINE（CLI 退出码 242），先确认状态再操作。

## 执行一次性命令（优先使用）

```bash
xnc exec web-01 -- hostname
xnc exec web-01 --timeout 60 -- Get-Service WinRM
```

- stdout/stderr 流式返回；`--json` 时 data 含 exitCode / stdout / stderr / durationMs / timedOut（envelope 固定在流之后最后一行）
- CLI 退出码 = 远程退出码透传；240+ 表示命令未执行（242 离线 / 243 超时或未执行完 / 245 会话在收到结果前断开 / 240 认证失效）
- 超时会 kill 远端进程树，使用前提醒用户

## 执行脚本

```bash
xnc run web-01 --file ./fix.ps1
cat fix.ps1 | xnc run web-01 -
```

Agent 端落地临时文件执行后自动删除（失败路径同样清理）；脚本上限 256 KB，超出 CLI 预检拒绝（退出码 2），upload+exec 大脚本路径后续版本提供。

## 文件传输

```bash
xnc upload web-01 ./app.zip C:\app\app.zip
xnc download web-01 C:\logs\app.log ./app.log
```

sha256 自动校验，不匹配返回 HASH_MISMATCH。单文件上限 256 MB。

## 桌面预览（只读）

```bash
xnc screen web-01 --snapshot desktop.jpg --json
```

- 用于无扰动判断桌面状态：弹窗是否卡住、安装器是否等输入、谁登录着
- 只读、无键鼠注入、低频 JPEG（默认 1 fps）；实时流用 Web 预览面板
- 不要用 RDP 去"看一眼"——RDP 会锁定 console 或新建会话，扰动被观察状态
- 状态字段：capturing / locked / no_session

## 交互式 Shell / 远程桌面（仅人类交互场景）

```bash
xnc shell web-01      # 需要真 TTY；Agent 场景改用 exec；行首 ~. 断开；提示符带 [机器名] 前缀
xnc rdp web-01        # 启动 mstsc 经反向隧道，交给人类操作
```

## 安全注意

- Shell / Exec 以 LocalSystem 运行 == 管理员权限：删除文件、重启服务、改注册表等破坏性命令，先向用户确认再执行
- viewer 角色只读（403 / 退出码 241）；240 表示凭证失效，提示用户重新 login
- 长命令显式 `--timeout`（默认 300s），避免默认超时截断长任务
- 完整命令树、退出码与错误码表见 references/cli.md
