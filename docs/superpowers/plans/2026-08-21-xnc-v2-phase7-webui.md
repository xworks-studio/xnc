# XNC v2 Phase 7（Web UI）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付 React Web UI：登录、Cluster/Node 列表与详情、Web Terminal（xterm.js）、用户管理页面，静态资源嵌入 xnc-server 二进制，零协议改动。

**Architecture:** Vite + React + TypeScript SPA，纯消费既有 REST API + shell 会话 WS。JWT 存 localStorage，Authorization header 调 API；Terminal 经 `POST /shell` 获取 session token 后拨 WS（query param 认证，浏览器无法设 WS header 的既定设计）。构建产物 `dist/` 经 Go `embed.FS` 嵌入 server，`/` 路径 serve。

**Tech Stack:** React 19 + TypeScript 5 + Vite 6 + react-router-dom 7 + @xterm/xterm 5 + @xterm/addon-fit；服务端 Go embed。

**Spec:** `spec.md` §52（页面清单）/ `docs/superpowers/specs/2026-08-19-xnc-v2-unified-session-design.md` §6 Phase 7；Phase 6 已跳过→预览面板省略。

## Global Constraints

- **零协议改动**（设计硬约束）：只消费既有 REST + 会话 WS。
- 页面清单（spec §52）：Login / Clusters / Nodes / Node Details / Terminal / **User Management**（Phase 5 REST 消费，替代 CLI）。预览面板**省略**（Phase 6 跳过）。
- Node Details 页 `[Remote Desktop]` 按钮显示 CLI 命令 `xnc rdp <node>`（spec §52）。
- Terminal 消费 shell 会话 WS：binary frame = 原始 VT 字节双向（设计 §3.2）；浏览器 WebSocket 的 binaryType 设 `arraybuffer`。
- JWT 存 localStorage key `xnc_token`；API 调用 `Authorization: Bearer <token>`；401 → redirect to /login。
- **embed**：`server/web/dist/` 通过 `//go:embed` 嵌入；`GET /` serve index.html（SPA fallback `/*` → index.html）；`/api/*` 路由优先。
- 深色主题（运维工具惯例）；无 UI 框架（纯 CSS，最小依赖）。
- Vite dev proxy `/api → http://localhost:8080`（开发模式热更新）。
- 构建命令：`cd web && npm install && npm run build` → `web/dist/`。
- Phase 5 用户管理 REST：`POST /api/users {email, display_name, password}`、`GET /api/users`（admin-only）。
- RBAC：viewer 登录后可看节点但不能打开 Terminal（POST /shell 403 → UI 显示权限不足提示）。

---

## 文件结构总览

```text
web/                              新顶层目录
├── package.json
├── vite.config.ts
├── tsconfig.json
├── index.html
├── src/
│   ├── main.tsx                  入口 + Router
│   ├── App.tsx                   布局壳（侧栏 + 内容区）
│   ├── api.ts                    API 客户端（fetch + JWT + error 处理）
│   ├── auth.tsx                  AuthContext + useAuth hook + LoginGuard
│   ├── types.ts                  Node/Cluster/User TypeScript 类型
│   ├── styles.css                全局深色主题
│   ├── pages/
│   │   ├── Login.tsx             登录页
│   │   ├── Clusters.tsx          Cluster 列表
│   │   ├── Nodes.tsx             Node 列表（含 cluster 过滤）
│   │   ├── NodeDetail.tsx        Node 详情 + Terminal/RDP 按钮
│   │   ├── Terminal.tsx          xterm.js Web Terminal
│   │   └── Users.tsx             用户管理（admin-only）
│   └── components/
│       ├── StatusBadge.tsx       online/offline/disabled 徽章
│       └── Sidebar.tsx           侧栏导航
server/
├── web_embed.go                  //go:embed web/dist + SPA handler
└── web/dist/                     构建产物（.gitignore；Dockerfile 构建时生成）
```

---

### Task 1: Vite + React + TS 脚手架 + 路由 + 主题

**Files:**
- Create: `web/` 全部脚手架文件 + `src/main.tsx` + `src/App.tsx` + `src/styles.css`

**Interfaces:**
- Produces: 可 `npm run dev` 启动的空壳 SPA，路由 `/login`、`/clusters`、`/nodes`、`/nodes/:id`、`/terminal/:id`、`/users`

- [ ] **Step 1:** `npm create vite@latest web -- --template react-ts`；`cd web && npm install react-router-dom`
- [ ] **Step 2:** `src/main.tsx` — BrowserRouter + Routes；`src/App.tsx` — 侧栏 + Outlet；`src/styles.css` — 深色主题基础（`background: #1a1a2e; color: #eee`）
- [ ] **Step 3:** 验证 `npm run dev` 打开 `http://localhost:5173` 显示壳
- [ ] **Step 4:** Commit `feat(web): vite react ts scaffold with routing and dark theme`

---

### Task 2: API 客户端 + Auth + 登录页

**Files:**
- Create: `web/src/api.ts`、`web/src/auth.tsx`、`web/src/pages/Login.tsx`、`web/src/types.ts`

**Interfaces:**
- Consumes: `POST /api/auth/login {email,password}` → `{token,user}`
- Produces:

```typescript
// api.ts
export async function api<T>(path: string, opts?: RequestInit): Promise<T>
// fetch with Authorization header from localStorage("xnc_token")
// 401 → localStorage.removeItem + window.location = "/login"
// error → throw with {code, message} from response body

// auth.tsx
export function useAuth(): { user: UserDTO | null; login(email, password): Promise<void>; logout(): void }
export function LoginGuard({ children }): JSX.Element  // 无 token → <Navigate to="/login" />

// types.ts
export interface UserDTO { id: string; email: string; display_name: string }
export interface NodeDTO { id: string; name: string; cluster: string; hostname: string;
  os_version: string; agent_version: string; shell_type: string;
  status: "online" | "offline" | "disabled"; last_seen_at: string | null }
export interface ClusterDTO { id: string; name: string }
```

Login 页面：email + password 输入框 → POST /auth/login → 存 token → navigate("/nodes")。

- [ ] Commit: `feat(web): api client, auth context, login page`

---

### Task 3: Cluster/Node 列表页 + Node 详情

**Files:**
- Create: `web/src/pages/Clusters.tsx`、`web/src/pages/Nodes.tsx`、`web/src/pages/NodeDetail.tsx`、`web/src/components/StatusBadge.tsx`、`web/src/components/Sidebar.tsx`

**Interfaces:**
- Consumes: `GET /api/clusters`、`GET /api/nodes?clusterId=`、`GET /api/nodes/{id}`

Nodes 列表：table（NAME / CLUSTER / STATUS / AGENT / LAST SEEN），10s 自动刷新，cluster 下拉过滤。
Node 详情：信息卡片 + `[Terminal]` 按钮（→ `/terminal/{nodeId}`）+ `[Remote Desktop]` 按钮显示 `xnc rdp <node>` 命令 + `[Disable]`/`[Enable]` 按钮（owner 可见，调 POST disable/enable）。

- [ ] Commit: `feat(web): cluster and node list pages, node detail`

---

### Task 4: Web Terminal（xterm.js）

**Files:**
- Create: `web/src/pages/Terminal.tsx`

**Interfaces:**
- Consumes: `POST /api/nodes/{id}/shell {cols, rows}` → 202 `{sessionId, token, websocketUrl}`；shell 会话 WS binary frame

```typescript
// Terminal.tsx 核心逻辑
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";

// 1. POST /shell → get {websocketUrl}
// 2. new WebSocket(websocketUrl); ws.binaryType = "arraybuffer"
// 3. new Terminal({theme: {background: "#1a1a2e"}}); fitAddon.fit()
// 4. ws.onmessage: binary → term.write(new Uint8Array(data))
//                    text(JSON) → SHELL_BEGIN → 显示 "connected: <shell>"
// 5. term.onData(data => ws.send(new TextEncoder().encode(data)))  // string→binary
// 6. resize: fitAddon.onResize → ws.send(JSON.stringify({type:"SHELL_RESIZE",payload:{cols,rows}}))
// 7. ws.onclose → term.write("\r\n[xnc] disconnected\r\n")
// 8. 组件卸载 → ws.close() + term.dispose()
```

- [ ] Commit: `feat(web): web terminal with xterm.js`

---

### Task 5: 用户管理页面

**Files:**
- Create: `web/src/pages/Users.tsx`

**Interfaces:**
- Consumes: `GET /api/users`、`POST /api/users`、`GET/POST/DELETE /api/clusters/{id}/members`

Users 页面：用户列表 table + 创建表单（email + display_name + password）+ 分配到 cluster（选择 cluster + role）。

非 admin（非任何 cluster 的 owner）访问 → API 返回 403 → 显示 "Admin access required"。

- [ ] Commit: `feat(web): user management page`

---

### Task 6: Go embed + SPA 路由

**Files:**
- Create: `server/web_embed.go`
- Modify: `server/internal/api/router.go`（挂载静态文件服务）

**Interfaces:**
- Consumes: `web/dist/` 构建产物
- Produces:

```go
// server/web_embed.go
//go:embed web/dist
var webFS embed.FS

// spaHandler serve 静态文件 + SPA fallback（非 /api/* 路径 → index.html）
func spaHandler() http.Handler
```

Router 顺序：`/api/*` 路由优先 → `/*` 走 spaHandler。Dockerfile 构建时先 `npm run build` 再 `go build`。

- [ ] Commit: `feat(server): embed web ui dist in binary with spa fallback`

---

### Task 7: E2E + 构建链

**Files:**
- Create: `scripts/e2e_phase7.sh`
- Modify: `deploy/Dockerfile`（加 Node.js 构建阶段）、`Makefile`

E2E：起栈 → curl `/` 返回 index.html → curl `/api/health` 正常 → 构建产物大小检查。

Dockerfile 多阶段：`node:22-alpine` build web → `golang:1.26-alpine` build server（copy web/dist）→ distroless。

- [ ] Commit: `test(e2e): phase 7 web ui build and serve`

---

## 完成定义

```text
cd web && npm run build → dist/ 生成
cd server && go build → 二进制内嵌 UI
bash scripts/e2e_phase7.sh 全绿
浏览器打开 https://control.xnc.app → 登录 → 看节点 → 打开 Terminal → 交互正常
用户管理页面可创建用户（替代 CLI）
```
