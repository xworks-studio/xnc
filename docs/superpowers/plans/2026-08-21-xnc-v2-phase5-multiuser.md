# XNC v2 Phase 5（多用户/RBAC/审计查询）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付多用户能力：用户管理（admin-only）、Membership CRUD、全量 RBAC（viewer 403 on sessions）、Node disable/enable、Cluster delete、审计查询 API+CLI，E2E 覆盖 Scenario F。

**Architecture:** 全部数据表已存在（users/cluster_members/audit_logs）。新增 sqlc queries + REST 端点 + CLI 命令。RBAC 是在既有 membership 检查上加 role 维度——提取共享 `requireRole(w, r, nodeID/clusterID, minRole)` 助手，各端点调用。

**Tech Stack:** 沿用全栈，零新依赖。

**Spec:** `spec.md` §6（角色矩阵）/§7（数据模型）/§27（REST）/§38（Authorization）/§51（错误码）/§57（CLI 命令树）/§7.6（audit action 词汇）。

## Global Constraints

- 模块结构不变；本机无 make；Docker 运行中（testcontainers）；双 GOOS 编译门槛。
- **角色矩阵**（spec §6，绑定）：
  | Role | 查看节点 | Exec/Shell/File/Tunnel/Screen | Cluster 配置（成员/token/disable） |
  |---|---|---|---|
  | owner | Yes | Yes | Yes |
  | operator | Yes | Yes | No |
  | viewer | Yes | **No** | No |
- **RBAC 实施**（spec §38，绑定）：每次操作检查 Cluster Membership + Role + Node.cluster_id；绝不允许 nodeId 单独授权。
- **用户管理**：仅 admin（bootstrap 用户）或 cluster owner 可创建用户；`POST /api/users {email, display_name, password}` 无自注册。
- **审计查询**：`GET /api/audit?nodeId=&userId=&action=&since=`；仅 admin 可查全量（后续按 cluster 过滤）。
- Node disable/enable：仅 owner；disabled 节点拒绝一切会话创建（403 NODE_DISABLED）。
- Cluster delete：仅 owner；有节点时 409（先 disable+清理节点）；软删除（name 加后缀 `_deleted_<ts>` 保留审计链）。
- 审计 action 新增：`user.create`、`cluster.delete`、`cluster.member.add`、`cluster.member.remove`、`node.disable`、`node.enable`。
- CLI 契约（cli.md §57）：`xnc cluster member add/remove/list`、`xnc node disable/enable`、`xnc audit list`。**用户创建不做 CLI**（REST 端点留给 Phase 7 Web UI）。
- 退出码：FORBIDDEN→241、NODE_DISABLED→242（或 250——spec §51 无专用码，用 403 HTTP + FORBIDDEN 错误码→241）。
- 每任务一 commit。

**不含**：OIDC/SAML（商业化）、用户自注册、密码修改/重置（后续）、audit 日志保留策略执行（后台清理属运维）。

---

## 文件结构总览

```text
server/internal/db/queries/
├── users_admin.sql             T1  CreateUserByEmail/ListUsers/CountUsersByEmail
├── membership.sql              T2  ListMembers/AddMember/RemoveMember/GetMemberRole
├── nodes_admin.sql             T4  SetNodeEnabled（复用 SetNodeStatus 但区分 disabled）
├── clusters_admin.sql          T4  SoftDeleteCluster/RenameCluster
├── audit_query.sql             T5  QueryAuditLog（带过滤+分页）
server/internal/api/
├── user_handlers.go            T1  POST/GET /api/users
├── member_handlers.go          T2  GET/POST/DELETE /api/clusters/{id}/members
├── rbac.go                     T3  requireRole / requireMinRole 助手
├── admin_handlers.go           T4  node disable/enable + cluster delete
├── audit_handlers.go           T5  GET /api/audit
├── rbac_test.go                T3/T4  Scenario F 全量矩阵
cli/
├── cmd_user.go                 T6  user create/list
├── cmd_member.go               T6  cluster member add/remove/list
├── cmd_admin.go                T6  node disable/enable、cluster delete
├── cmd_audit.go                T6  audit list
scripts/e2e_phase5.sh           T7  Scenario F
```

---

### Task 1: server — 用户管理端点

**Files:**
- Create: `server/internal/db/queries/users_admin.sql`、`server/internal/api/user_handlers.go`
- Modify: `server/internal/api/router.go`
- Test: `server/internal/api/user_handlers_test.go`

**Interfaces:**
- Consumes: `auth.UserFrom(ctx)`、bcrypt 既有 `auth.HashPassword`、`startSession` 的审计模式
- Produces:

```text
POST /api/users  {"email","display_name","password"}  → 201 {id,email,display_name}
  仅 admin（当前实现：用户是任一 cluster 的 owner 即视为 admin——简化判定，
  后续商业化加 isAdmin 字段）
  409 已存在（UNIQUE email）
GET  /api/users  → 200 [{id,email,display_name}]（仅 admin）
sqlc: CreateUserByEmail / ListUsers / CountUsersByEmail
audit: user.create {email}
```

- [ ] **Step 1-5:** TDD 循环（测试→红→实现→绿→commit `feat(server): user management endpoints`）。测试覆盖：admin 创建/列表/非-admin 403/重复 email 409/audit 行。

---

### Task 2: server — Membership 管理端点

**Files:**
- Create: `server/internal/db/queries/membership.sql`、`server/internal/api/member_handlers.go`
- Modify: `server/internal/api/router.go`
- Test: `server/internal/api/member_handlers_test.go`

**Interfaces:**
- Consumes: T1 的用户查询
- Produces:

```text
GET    /api/clusters/{id}/members          → 200 [{user_id,email,display_name,role}]
POST   /api/clusters/{id}/members          {"user_id","role"} → 201（仅 owner）
DELETE /api/clusters/{id}/members/{userId}  → 204（仅 owner；不能移除自己如果是唯一 owner）
sqlc: ListMembers / AddMember / RemoveMember / GetMemberRole
audit: cluster.member.add {user_id, role} / cluster.member.remove {user_id}
role ∈ {"owner","operator","viewer"}；非法 role → 400
```

- [ ] **Step 1-5:** TDD 循环。测试覆盖：列表/owner 添加 operator+viewer/非-owner 403/移除/移除唯一 owner 400/audit。

---

### Task 3: server — RBAC 全量执行

**Files:**
- Create: `server/internal/api/rbac.go`
- Modify: `server/internal/api/exec_handlers.go`、`shell_handlers.go`、`file_handlers.go`、`tunnel_handlers.go`（各端点加 role 检查）
- Test: `server/internal/api/rbac_test.go`

**Interfaces:**
- Consumes: T2 的 `GetMemberRole`
- Produces:

```go
// rbac.go
func (h *handlers) requireMinRole(w http.ResponseWriter, r *http.Request, nodeID uuid.UUID, minRole string) (*sqlc.Node, bool)
// 检查 membership + role >= minRole（owner > operator > viewer）
// minRole = "viewer" → 仅需 membership（等同现有 GetNodeForUser）
// minRole = "operator" → role ∈ {operator, owner}
// minRole = "owner" → role == owner
// 失败：404 NODE_NOT_FOUND（非成员）/ 403 FORBIDDEN（角色不足）
// 成功：返回 Node 行（含 cluster_id）

var roleRank = map[string]int{"viewer": 0, "operator": 1, "owner": 2}
```

各端点改为调用：exec/shell/file/tunnel → `requireMinRole(w, r, nodeID, "operator")`。

- [ ] **Step 1-5:** TDD 循环（commit `feat(server): full rbac on all session endpoints`）。测试覆盖 **Scenario F**：
  - viewer 对 exec/shell/upload/tunnel 全 403
  - operator 对 exec/shell/upload/tunnel 全 202
  - operator 对 node disable 403（T4 前disable 不存在，只测 role）
  - 非 member 对 node list 404（现有行为不变）

---

### Task 4: server — Node disable/enable + Cluster delete

**Files:**
- Create: `server/internal/db/queries/nodes_admin.sql`、`server/internal/db/queries/clusters_admin.sql`、`server/internal/api/admin_handlers.go`
- Modify: `server/internal/api/rbac.go`（requireMinRole 加 disabled 检查）、`server/internal/api/router.go`
- Test: `server/internal/api/admin_handlers_test.go`

**Interfaces:**
- Consumes: T3 `requireMinRole`
- Produces:

```text
POST /api/nodes/{id}/disable  → 204（owner only；audit node.disable）
POST /api/nodes/{id}/enable   → 204（owner only；audit node.enable）
DELETE /api/clusters/{id}     → 204（owner only；有节点→409 CLUSTER_NOT_EMPTY）
  软删除：UPDATE clusters SET name = name || '_deleted_' || extract(epoch from now())::bigint
sqlc: SetNodeStatus 已有（复用 status='disabled'）
      SoftDeleteCluster / CountNodesInCluster
rbac.go 增强：requireMinRole 在 disabled 节点返回 403 NODE_DISABLED
```

- [ ] **Step 1-5:** TDD 循环（commit `feat(server): node disable/enable and cluster delete`）。

---

### Task 5: server — 审计查询端点

**Files:**
- Create: `server/internal/db/queries/audit_query.sql`、`server/internal/api/audit_handlers.go`
- Modify: `server/internal/api/router.go`
- Test: `server/internal/api/audit_handlers_test.go`

**Interfaces:**
- Consumes: `sqlc` 生成
- Produces:

```text
GET /api/audit?nodeId=&userId=&action=&since=&limit=50&offset=0
  → 200 [{id,user_id,cluster_id,node_id,action,session_id,metadata,created_at}]
  仅 admin（复用 T1 判定）
  since: ISO 8601 duration（如 7d/24h）或绝对时间戳
sqlc: QueryAuditLog（动态 WHERE + LIMIT/OFFSET）
```

- [ ] **Step 1-5:** TDD 循环（commit `feat(server): audit query endpoint`）。

---

### Task 6: CLI — 全部新命令

**Files:**
- Create: `cli/cmd_member.go`、`cli/cmd_admin.go`、`cli/cmd_audit.go`
- Modify: `cli/main.go`（注册）
- Test: `cli/cmd_admin_test.go`

**Interfaces:**
- Consumes: T1-T5 的全部端点
- Produces:

```text
xnc cluster member list <cluster>              # table: EMAIL ROLE
xnc cluster member add <cluster> <user-id> --role <r>
xnc cluster member remove <cluster> <user-id>
xnc node disable <node> / xnc node enable <node>
xnc cluster delete <cluster>
xnc audit list [--node n] [--user u] [--action a] [--since 7d] [--limit 50]
```

**注意**：用户创建/列表**不做 CLI**——REST 端点（T1）保留给 Phase 7 Web UI 消费。admin 在 Web UI 上线前用 curl 或直接调 API 创建用户。

- [ ] **Step 1-5:** TDD 循环（commit `feat(cli): member, admin, and audit commands`）。测试：envelope golden + 退出码（241 on 403）。

---

### Task 7: E2E Scenario F

**Files:**
- Create: `scripts/e2e_phase5.sh`
- Modify: `Makefile`

**Interfaces:**
- Consumes: 全部
- Produces: `bash scripts/e2e_phase5.sh` 验收

脚本流程：起栈 → admin 登录 → 创建 operator+viewer 用户 → 加入 cluster → mockagent 上线 → **viewer 对 exec/shell/upload 全 403（exit 241）** → operator 对 exec 202 → node disable → exec 403 NODE_DISABLED → enable → 恢复 → audit list 有记录 → 清理。

- [ ] **Step 1-3:** 写脚本→跑→commit（`test(e2e): phase 5 scenario F rbac matrix`）。

---

## 完成定义

```text
全模块测试绿 + 双 GOOS 编译
bash scripts/e2e_phase5.sh 全绿（Scenario F 完整矩阵）
viewer 在所有会话端点（exec/shell/upload/download/tunnel）收到 403 + CLI exit 241
operator 正常执行所有会话操作
node disable → 所有会话 403 NODE_DISABLED；enable → 恢复
audit list 返回带过滤的分页结果
admin 创建的用户能登录并按角色获得正确权限
```
