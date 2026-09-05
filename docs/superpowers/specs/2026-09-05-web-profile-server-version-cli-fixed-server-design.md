# Web 个人信息 / 版本展示 / CLI 固定 server — 设计

日期：2026-09-05
状态：已与用户对齐（四项决策均取推荐项）
关联：`docs/superpowers/specs/2026-09-03-innosetup-installer-unified-auth-design.md`（账号体系基线）

## 1. 背景与目标

四个独立小改动，共同指向"产品化收口"：

1. 已登录用户的 console 侧栏没有下载页入口（`/download` 是 LoginGuard
   外的独立页，登录态无路可达）。
2. web 上看不到当前 server 版本（`/api/health` 已返回 version，无消费方）。
3. 无个人信息页：用户无法自助修改 display_name 与密码（现状只能靠
   管理员删号重建）。
4. CLI register/login 会交互询问 Server URL——对唯一生产环境 xnc.app
   的产品而言是无意义的手工步骤，也是误配置来源。

## 2. 范围与非目标

**范围**：上述四项；涉及 web（Sidebar/Download/新 Profile 页/auth 上下文）、
server（两个新端点 + 两条 sqlc 查询）、cli（server 解析链固定默认值 +
去 prompt）、文档（AGENTS.md §6、README）。

**非目标**（明确不做，防扩散）：

- email 修改（牵动登录身份/唯一性/审计链，v1 只读）。
- 改密后吊销存量 JWT（无状态 token 保持有效，已决策；token_version
  机制列为远期）。
- 构建元数据注入（commit sha/构建时间）——版本字符串即构建标识
  （tag → 镜像 → /api/health 单一来源），不新增注入链。
- 密码策略统一：改密端点执行 ≥8 字符新规则；`POST /api/users` 创建
  端点维持现状（仅非空校验），策略统一为独立后续项。
- Users 管理页（admin 面向）不动。

## 3. 设计

### 3.1 Download 入口（已登录）

- `web/src/components/Sidebar.tsx` nav 增加 `Download` 项（Users 之后），
  指向既有公开路由 `/download`。路由结构不变（独立布局，无侧栏）。
- `web/src/pages/Download.tsx` 页头链接自适应：未登录显示
  "Sign in to console"（现状，指向 /login）；已登录（`useAuth().user != null`）
  显示 "Back to console"，指向 `/nodes`（LoginGuard 自然放行）。

### 3.2 server 版本小字

- Sidebar `side-foot` 顶部渲染 `server v{version}`（`dim` 小字）。
- 数据源 `GET /api/health`（公开、无 JWT；不走 `api()` 封装避免 401
  重定向语义），Sidebar 挂载时 fetch 一次。
- 失败（网络/非 200）静默不渲染该行——版本展示是锦上添花，不产生
  噪声；无轮询、无缓存失效问题（页面刷新即重取）。

### 3.3 个人信息页

**路由与入口**：`/profile` 挂 App shell 内（LoginGuard 保护，main.tsx
App children 增加 route）。入口 = side-foot 的 email 文本改为可点击
（`Link to="/profile"`，样式加 cursor/hover；不加 nav 项，避免与
Users 混淆）。未登录时 side-foot 不渲染 email，行为不变。

**页面**（`web/src/pages/Profile.tsx`）两卡片：

- 账号卡：email 只读展示；display_name 输入框 + Save。成功后调
  `updateUser`（AuthProvider 新方法：更新 state + localStorage
  `xnc_user`），侧栏 email 与页面即时反映。
- 改密卡：current/new/confirm 三输入 + Submit。new !== confirm 前端
  拦截；成功显示提示（不强制登出——JWT 保持有效，已决策）。

**API**（挂 `/api/auth` 现有 JWT 中间件组，`server/internal/api/auth_handlers.go`）：

- `PATCH /api/auth/me`，body `{"display_name": string}` → `200 {"user": userDTO}`。
  校验：TrimSpace 后 ≤64 字符（超长 400）；空串合法（清除显示名）。
  sqlc：`UpdateUserDisplayName(ctx, {id, name})`。
- `POST /api/auth/password`，body `{"current_password", "new_password"}`
  → `204`。校验顺序：非空（400）→ current 经 `auth.VerifyPassword`
  比对 `GetUserByID` 的 hash（不符 401 invalid credentials，与 login
  同文案防枚举）→ new 长度 ≥8（400 weak password）→ bcrypt 落库。
  sqlc：`UpdateUserPassword(ctx, {id, hash})`。
  审计：`user.password_change`（沿用 user.create 的 background-ctx
  写法与 `域.动作` 命名；display_name 修改无安全语义，不审计）。
- 两条查询落 `server/internal/db/queries/`（users 相关文件）+ `sqlc generate`。

**会话语义**：改密不吊销 JWT（无状态 24h）；web/CLI 已持有 token 的
会话继续有效，下次登录用新密码。

### 3.4 CLI 固定 server

- `cli/main.go`：新增 `const defaultServerURL = "https://xnc.app"`；
  `resolveServer` 链变为 flag > env（经 config 合并）> config file >
  **default**——返回值不再可能为空。
- root `--server` flag `MarkHidden("server")`（帮助文本消失、功能
  保留——开发/测试继续 `--server` 指向本地假服务器，存量测试零改动）；
  使用说明（main.go usage 示例、doc.go）删除 `--server` 行。
- 去交互 prompt：
  - `cli/termux.go`：`loginDeps` 删除 `promptServer` 字段；
    `runLoginFlow` 删除 server 补齐分支（server 恒非空）。
  - `cli/cmd_auth.go` login、`cli/cmd_register.go` registerLogin：
    删除 `promptServer` 注入；非交互分支删除
    "--server, XNC_SERVER, or config file required" usage 报错（不可能
    触发）。
  - `cli/main.go` `dial()` 删除 server 空判分支（同上）。
- 语义：普通用户 `xnc register` / `xnc login` 直接对 xnc.app 认证，
  不再被询问 server；`XNC_SERVER` env 与 `--server` flag 保留为
  未文档化的开发通道。config 文件继续保存实际使用的 server（兼容
  测试与历史安装）。

## 4. 测试计划

- **server**（api 包，真 PG testcontainers）：
  - PATCH /api/auth/me：200 更新生效 + 返回 userDTO；>64 字符 400；
    TrimSpace 生效；无 JWT 401。
  - POST /api/auth/password：204 + 新密码可登录、旧密码失效；current
    错 401；new <8 字符 400；缺字段 400；无 JWT 401；审计行
    （user.password_change）存在。
- **web**（vitest，照 Download.test.tsx 模式 mock fetch）：
  - Sidebar：渲染 Download 链接；health 返回后渲染版本小字、失败不渲染；
    email 链接指向 /profile。
  - Download：已登录渲染 "Back to console"，未登录渲染 "Sign in"。
  - Profile：display_name 保存调 PATCH 且 updateUser 生效；改密三态
    （成功/mismatch 前端拦截/服务端错误显示）。
- **cli**（既有模式）：
  - resolveServer：无 flag/env/config 时返回 https://xnc.app（单测）。
  - cmd_ux_test.go 等 4 处 `promptServer` 注入随 loginDeps 字段删除
    一并更新。
  - register/login 非交互缺 server 不再 usage 报错（用 --server 指向
    假服务器维持其余行为断言）。

## 5. 文档影响

- AGENTS.md §6：register/login 描述去掉 server 选择（固定 xnc.app）。
- README：`xnc register --server ...` 示例改为 `xnc register`。
- 本设计文档为唯一新增文档；不改动 spec.md。

## 6. 验收清单

1. 登录态侧栏可见 Download 入口；点击到达下载页，页头为
   "Back to console"，返回直达 /nodes。
2. 侧栏底部显示 `server vX.Y.Z`，与 `/api/health` 一致。
3. /profile：改 display_name 后侧栏 email 区与重登录后均保持；改密后
   旧密码 login 401、新密码成功；当前会话不中断。
4. 全新机器 `xnc register`：无任何 server 询问，直接 email/密码 →
   cluster 选择 → 上线。
5. ci.yml 五矩阵全绿（web vitest、server 真 PG、cli go test）。
