# XNC v2 产品测试报告

> 测试环境：生产（https://control.xnc.app + LABS-TB16G7 agent）
> 测试日期：2026-08-21
> 测试方法：CLI 逐命令执行 + Web UI 浏览器自动化 + 混合场景

---

## 一、发现的 Bug（按严重度排序）

### P0 — 功能缺陷

| # | 位置 | 描述 | 复现步骤 |
|---|------|------|----------|
| 1 | CLI `audit list` | **USER 和 NODE 列显示原始 UUID**，不显示人类可读的 email/节点名 | `xnc audit list --since 1h` |
| 2 | CLI `exec --json` | **流式 stdout 先于 JSON envelope 输出**——脚本管道 `xnc exec --json -- cmd \| jq .data.stdout` 会失败（jq 收到混合流） | `xnc exec node --json -- hostname \| jq .` |
| 3 | Web UI NodeDetail | **"Last seen" 显示本地化格式** `2026/8/21 12:07:54`——与其他处 ISO 格式不一致，且无相对时间 | 打开节点详情页 |
| 4 | CLI `exec --json` 与流式输出 | **--json 模式下 stdout 仍然实时打印到终端**——"quiet" 模式缺失，Agent 消费者期望纯 JSON 输出 | `xnc exec node --json -- "Write-Output hi"` |

### P1 — 体验缺陷

| # | 位置 | 描述 |
|---|------|------|
| 5 | CLI `cluster member list` | 无 `--json` 时表格只有 EMAIL/ROLE，缺少 user_id（`--json` 可见）——添加成员时需要 ID |
| 6 | CLI `node list` | **无 `--watch` / `--refresh` 选项**——查看状态变化需反复执行 |
| 7 | CLI 整体 | **`xnc --version` 不存在**——只有 `xnc version`（子命令），习惯 `--version` flag 的用户会碰壁 |
| 8 | Web UI Nodes | **空列表无提示**——无节点时页面只有表头，无 "No nodes found" 或引导注册的信息 |
| 9 | Web UI NodeDetail | **Disable 按钮无确认**——点击立即禁用，无 "Are you sure?" 对话框 |
| 10 | Web UI Terminal | **断开后无重连按钮**——仅显示 disconnected 文本，需返回详情再点 Terminal |
| 11 | CLI `upload/download` | **无进度指示**——大文件传输期间无任何输出，用户不知道是否在传输 |
| 12 | Web UI 全局 | **浏览器 Console 有 chunk size 警告**（>500KB JS bundle）——不影响功能但影响专业感 |

### P2 — 打磨项

| # | 位置 | 描述 |
|---|------|------|
| 13 | CLI `login` | 成功输出 "Logged in to..." 但**未提示下一步操作**（如 "Try: xnc node list"） |
| 14 | CLI `token create` | **Token 全文输出在一行**——复制不便（无换行或仅显示 token） |
| 15 | Web UI Login | **无 "remember me"** / token 过期（24h）后无预警 |
| 16 | CLI 整体 | **`xnc status` 不检查 agent 版本兼容性**——仅返回 server 版本 |
| 17 | Web UI Terminal | **无字体大小调节** / 主题选择 |
| 18 | CLI `node show` | **last_seen_at 显示 ISO UTC 格式**——人类不友好（`2026-08-21T04:02:54Z` vs "2 min ago"） |

---

## 二、逐项测试结果

### A. CLI — 认证与基础

| 测试项 | 易用性 | 美观 | 性能 | 备注 |
|--------|--------|------|------|------|
| A1.1 login 交互提示 Server URL | ✅ | ✅ | ✅ | 带默认值[显示]，回车确认 |
| A1.2 密码掩码 | ✅ | ✅ | — | `Password: ******` |
| A1.3 密码错误重试提示 | ✅ | ✅ | — | `attempt 1 of 3` 清晰 |
| A1.4 三次失败错误信息 | ⚠️ | — | — | "too many failed attempts" 但无 cooldown 提示 |
| A1.5 登录成功输出 | ✅ | ✅ | ✅ | `Logged in to https://... as admin@xnc.app` |
| A1.6 Email 记忆 | ✅ | — | — | 二次登录有默认值 |
| A2.1 管道模式 --json | ✅ | — | ✅ | envelope 正确 |
| A3.1 whoami table | ⚠️ | ⚠️ | ✅ | KEY/VALUE 对齐但**显示原始 UUID** |
| A3.4 status | ✅ | ✅ | ✅ | 223ms 响应 |
| A3.3 未登录 whoami | ✅ | — | — | "invalid token" + exit 240 |
| A3.4 不可达 status | ✅ | — | — | NETWORK 错误 + exit 245 |

### B. CLI — 节点管理

| 测试项 | 易用性 | 美观 | 性能 | 备注 |
|--------|--------|------|------|------|
| B1.1 node list 对齐 | ✅ | ✅ | ✅ | 列宽一致，223ms |
| B1.5 --json | ✅ | — | — | envelope 正确 |
| B2.1 node show 按名称 | ✅ | ✅ | ✅ | 信息齐全 |
| B2.4 不存在节点 | ✅ | — | — | exit 244, "no node named" |
| B1.2 --cluster 过滤 | ✅ | — | — | 正确 |

### C. CLI — 远程执行

| 测试项 | 易用性 | 美观 | 性能 | 备注 |
|--------|--------|------|------|------|
| C1.1 exec hostname | ✅ | ✅ | ✅ | 实时输出，135ms |
| C1.2 stdout/stderr 区分 | ⚠️ | ⚠️ | — | **table 模式下流式输出无颜色区分**（stderr 无红色标记） |
| C1.3 退出码透传 | ✅ | — | ✅ | exit 7 → 7 |
| C1.4 超时 | ✅ | ✅ | ✅ | 3.1s 后 timedOut:true + exit 243 |
| C1.5 --json envelope | ⚠️ | — | — | **stdout 先打印再打 envelope**——管道消费不友好 |
| C1.6 长输出 100 行 | ✅ | ✅ | ✅ | 完整不截断 |
| C1.7 节点不存在 | ✅ | — | — | exit 244 |
| C1.8 --cwd | ✅ | — | ✅ | Get-Location 返回 C:\Windows |
| C2.1 run --file | ✅ | ✅ | ✅ | exitCode 透传 |
| C2.2 stdin 管道 | ✅ | — | — | `cat script.ps1 \| xnc run node -` 正常 |

### D. CLI — 文件传输

| 测试项 | 易用性 | 美观 | 性能 | 备注 |
|--------|--------|------|------|------|
| D1.1 upload 小文件 | ✅ | ✅ | ✅ | sha256 显示完整 |
| D2.1 download roundtrip | ✅ | ✅ | ✅ | diff identical |
| D2.2 不存在文件 | ✅ | — | — | FILE_NOT_FOUND + exit 244 |
| D1.3 大文件 10MB | ⚠️ | ⚠️ | ✅ | **无进度指示**——静默等待 |

### F. CLI — 管理功能

| 测试项 | 易用性 | 美观 | 性能 | 备注 |
|--------|--------|------|------|------|
| F1.1 cluster list | ✅ | ✅ | ✅ | NAME/ID 对齐 |
| F1.2 token create | ✅ | ⚠️ | — | **token 全文一行不便复制** |
| F2.1 member list | ✅ | ⚠️ | — | **缺 user_id 列**（--json 可见） |
| F3.1 audit list | ⚠️ | ❌ | ✅ | **USER/NODE 显示 UUID 非人类可读** |

### G. CLI — 整体体验

| 测试项 | 易用性 | 美观 | 性能 | 备注 |
|--------|--------|------|------|------|
| G.1 --help 命令树 | ✅ | ✅ | — | 17 个命令，描述清晰 |
| G.5 退出码一致性 | ✅ | — | — | 0/1/7/240/244/245 全验证通过 |
| G.6 响应速度 | — | — | ✅ | node list 223ms |
| G.7 网络超时 | ✅ | — | — | NETWORK + exit 245 |

### H. Web UI — 登录与导航

| 测试项 | 易用性 | 美观 | 性能 | 备注 |
|--------|--------|------|------|------|
| H1.1 页面加载 | — | — | ✅ | 首次加载快（<2s） |
| H1.2 表单布局 | ✅ | ✅ | — | 深色主题，Email/Password 清晰 |
| H1.3 错误密码提示 | ✅ | ✅ | — | `alert: invalid credentials` |
| H1.4 登录跳转 | ✅ | — | ✅ | 到 /nodes |
| H1.6 深色主题 | — | ✅ | — | 专业感好 |
| H2.1 侧栏导航 | ✅ | ✅ | — | Nodes/Clusters/Users 清晰 |
| H2.2 当前页高亮 | ✅ | ✅ | — | active 状态正确 |
| H2.4 未登录重定向 | ✅ | — | — | 到 /login |
| H2.6 SPA 路由保持 | ✅ | — | — | F5 刷新后路由保持 |

### I. Web UI — 节点管理

| 测试项 | 易用性 | 美观 | 性能 | 备注 |
|--------|--------|------|------|------|
| I1.1 列表加载 | — | — | ✅ | 快速 |
| I1.2 表格列 | ✅ | ✅ | — | NAME/CLUSTER/STATUS/AGENT/LAST SEEN |
| I1.3 状态徽章 | ✅ | ✅ | — | 绿色 online |
| I1.4 自动刷新 | ✅ | — | ✅ | "2s ago" 实时更新 |
| I1.5 Cluster 过滤 | ✅ | ✅ | — | 下拉框正常 |
| I1.7 空列表 | ❌ | — | — | **无 "No nodes found" 提示** |
| I2.1 信息卡片 | ✅ | ✅ | — | 字段齐全（Name/Cluster/Status/Hostname/OS/Agent/Shell/LastSeen/ID） |
| I2.2 Terminal 按钮 | ✅ | ✅ | — | 醒目链接 |
| I2.3 RDP 按钮 | ✅ | ✅ | — | 展开 `xnc rdp LABS-TB16G7` |
| I2.4 Disable 按钮 | ⚠️ | — | — | **无确认对话框** |

### J. Web UI — Terminal

| 测试项 | 易用性 | 美观 | 性能 | 备注 |
|--------|--------|------|------|------|
| J.1 连接速度 | — | — | ✅ | <5s 从点击到可用 |
| J.2 状态栏 | ✅ | ✅ | — | "LABS-TB16G7" + "connected · powershell" |
| J.3 键入响应 | — | — | ✅ | hostname 命令即时返回 |
| J.4 提示符前缀 | ✅ | ✅ | — | `[LABS-TB16G7] PS C:\Windows\system32>` |
| J.9 断开提示 | ⚠️ | — | — | **无重连按钮** |

### K. Web UI — 用户管理

| 测试项 | 易用性 | 美观 | 性能 | 备注 |
|--------|--------|------|------|------|
| K.2 用户列表 | ✅ | ✅ | — | EMAIL/DISPLAY NAME/ID 对齐 |
| K.3 创建表单 | ✅ | ✅ | — | 字段齐全+标签清晰 |
| K.6 密码掩码 | ✅ | — | — | password type |
| K.7 成员分配 | ✅ | ✅ | — | User/Cluster/Role 三下拉 |

---

## 三、修复优先级建议

### 立即修复（影响日常使用）

1. **audit list 人类可读化**——JOIN users 和 nodes 表，显示 email/name 而非 UUID
2. **exec --json 静默模式**——`--json` 时不实时打印 stdout，只在 envelope 中返回
3. **node list / node show 相对时间**——`last_seen_at` 显示 "2m ago" 而非 ISO/本地化格式

### 短期改进（提升体验）

4. upload/download 进度条（`\r` 覆写或百分比）
5. Web UI 空列表友好提示
6. Web UI Disable 确认对话框
7. Web UI Terminal 断开后重连按钮
8. `xnc --version` flag alias
9. exec table 模式 stderr 红色标记

### 中期打磨

10. CLI `--watch` 模式（node list 自动刷新）
11. Web UI code-splitting（减小 bundle）
12. CLI token create 输出格式优化
13. Web UI 字体/主题设置

---

## 四、整体评分

| 维度 | CLI | Web UI |
|------|-----|--------|
| 易用性 | 4/5 | 4/5 |
| 美观 | 3.5/5 | 4/5 |
| 性能 | 5/5 | 4.5/5 |
| **综合** | **4.2/5** | **4.2/5** |

**评语**：核心功能（exec/shell/file/terminal）全部正常，性能优秀（sub-second 响应），协议设计稳健。主要短板在细节打磨：UUID 显示、流式 JSON envelope 混合输出、进度反馈缺失、防御性 UI（确认对话框、空状态）。修复 P0 四项后可达到 4.5/5 一流产品水平线。
