# 用户自助 Cluster 管理设计（默认 cluster / 自助 CRUD / 成员管理 / 机器单归属）

> 2026-09-14。需求（用户原话拆解）：
> 1. 每个用户注册后自动拥有一个属于自己的**默认 cluster**；
> 2. 用户可自助**添加 / 修改 / 删除** cluster；
> 3. 用户可自助**添加 / 修改 / 删除** cluster 成员；
> 4. **一台机器同一时间只能属于一个 cluster**。
>
> 本文是实施规格（spec.md §6 的增量），实现以本文 + 现有代码模式为准。

## 1. 现状与差距

代码基线：`src/server/internal/db/migrations/0001_init.sql`（clusters /
cluster_members / nodes 三表结构至今未动）。

| 能力 | 现状 | 差距 |
|---|---|---|
| 默认 cluster | 仅 bootstrap admin 首启建 `default` cluster（`bootstrap.go:44`）；admin 建新用户后其 cluster 数为 0，`xnc register` 直接报 CLUSTER_NOT_FOUND | 建用户事务内自动建个人默认 cluster + 存量用户回填 |
| cluster 增 | `POST /api/clusters` 任意 JWT 用户可建并成为 owner（`cluster_handlers.go:30`）——已满足"自助" | 加每用户数量限额（防滥用） |
| cluster 改 | 无改名 API（name 是唯一可变属性） | 新增 `PATCH /api/clusters/{id}` |
| cluster 删 | `DELETE /api/clusters/{id}` owner-only + 非空 409（`admin_handlers.go:65`）——已满足 | 无 |
| 成员增 | `POST .../members` owner-only，但只收 `user_id`，而 `GET /api/users` 是 admin-only —— 普通 owner 拿不到 user_id，实际加不了人 | 请求体支持 `email` |
| 成员改 | 无改 role API | 新增 `PATCH .../members/{userId}` |
| 成员删 | API/CLI 已有；Web 无 UI | Web 补 |
| 机器单归属 | JWT 注册路径查 `GetMachineIDConflictCluster` 跨 cluster 409；**token 路径（/api/agent/enroll）没查**；DB 约束是 `UNIQUE(cluster_id, machine_id)`，跨 cluster 并发注册存在竞态窗口 | DB 层 machine_id 全局唯一兜底 |
| 换 cluster 出路 | 只能平台 admin `DELETE /api/nodes/{id}` 后重注册：nodeId 变、审计断链、须管理员终端 | 新增 owner 自助 **node move** API（nodeId 不变） |

### 关键雷区：admin 谓词是派生的

`isAdminUser`（`user_handlers.go:21`）= **"是任一 cluster 的 owner"**（无独立
admin 字段）。本改动落地后人人都有默认 cluster → 人人是 owner → **人人自动
成为平台 admin**（列全部用户、删任意节点、传 release、管 relay 池）。这是
本设计必须先行处理的依赖项：引入显式 `users.is_admin` 列（§3.1）。

### 顺带修复的存量缺陷

软删除 = 仅改名 `name_deleted_<unix秒>`（`admin_handlers.go:79`），无标记列，
导致 `ListClustersForUser`（`clusters.sql:1`）**仍会列出已删除的 cluster**。
本设计加 `clusters.deleted_at` 并在所有读取路径过滤。

## 2. 目标语义（不变式）

1. **每个用户至少有一个（未删除的）cluster 且角色为 owner**——由系统在建号
   事务内保证（个人默认 cluster，`personal=true` 标记）；用户可删除它，但删
   光后 register 会报 CLUSTER_NOT_FOUND，由 UI/CLI 引导新建（不做硬保护）。
2. cluster 的自助管理边界 = **owner 角色成员**（rename/delete/members/tokens
   均 owner-only，沿用现状）；创建 cluster = 任意 JWT 用户（沿用现状）。
3. **machine_id 全局唯一**：一台机器任一时刻恰好出现在 0 或 1 个 cluster。
   跨 cluster 的重复注册一律 409 MACHINE_ID_CONFLICT；唯一合法的迁移路径 =
   **双边 owner 的 move**（或平台 admin 删节点）。
4. 存量客户端零改动：register / binding.json / enrollment token / agent 协议
   全部不动，新能力全部是服务端增量。

## 3. 数据库改动（migration `0005_user_default_clusters.sql`）

单文件四段，任一段失败整体回滚（现有迁移机制为顺序执行、无 down）。

### 3.1 显式 admin 位

```sql
ALTER TABLE users ADD COLUMN is_admin boolean NOT NULL DEFAULT false;
-- 回填 = 迁移瞬间的现行谓词快照：现网已是 owner 的用户保持 admin（零权限回归）
UPDATE users SET is_admin = true
WHERE EXISTS (SELECT 1 FROM cluster_members cm
              WHERE cm.user_id = users.id AND cm.role = 'owner');
```

迁移后**应尽快由管理员收敛**（`PATCH /api/users/{id}` 撤掉不该保留的
is_admin——现网"人人 owner 即人人 admin"是既成事实，回填只是如实快照）。

**顺序是正确性依赖，不可调换**：本节快照必须在 §3.3 默认 cluster 回填之前
执行——顺序颠倒会把回填产生的 owner 全部快照成 admin。§10 的"Task 1-4
同 PR"约束防的是同一窗口的运行态版本。

### 3.2 cluster 软删除标记 + 个人默认标记

```sql
ALTER TABLE clusters ADD COLUMN deleted_at timestamptz;              -- NULL=存活
ALTER TABLE clusters ADD COLUMN personal boolean NOT NULL DEFAULT false;
-- 存量回填：主用审计精确定位（每次软删除都写 cluster.delete 审计行，
-- admin_handlers.go:93），regex 仅兜底覆盖"改名成功但审计写入失败"的窗口
-- （历史删除行已被改名，原名不可恢复，保留现状）
UPDATE clusters SET deleted_at = now()
WHERE id IN (SELECT DISTINCT cluster_id FROM audit_logs
             WHERE action = 'cluster.delete' AND cluster_id IS NOT NULL);
UPDATE clusters SET deleted_at = to_timestamp(
  (regexp_match(name, '_deleted_([0-9]+)$'))[1]::bigint)
WHERE name ~ '_deleted_[0-9]+$' AND deleted_at IS NULL;

-- name 唯一性改部分索引（存活行内唯一）：已删行保留原名 → 同名可重建，
-- 删除不再需要改名释放名字
ALTER TABLE clusters DROP CONSTRAINT clusters_name_key;
CREATE UNIQUE INDEX clusters_name_live_key ON clusters (name)
WHERE deleted_at IS NULL;
```

`SoftDeleteCluster` 简化为 `UPDATE clusters SET deleted_at = now() WHERE
id = $1`（不再改名；恢复 = 清 deleted_at，比改名兜底更直接；同秒同名重建
再删的 23505 重试分支随之退役）。所有 cluster 读取路径加 `deleted_at IS
NULL` 过滤：`GetClusterByID/GetClusterByName/ListClustersForUser/GetMachineIDConflictCluster`
（修复"已删 cluster 仍出现在列表"的存量缺陷）。

### 3.3 存量用户回填个人默认 cluster

```sql
DO $$
DECLARE u record; cname text; n int := 2;
BEGIN
  FOR u IN SELECT id, email FROM users
           WHERE NOT EXISTS (SELECT 1 FROM cluster_members cm
                               JOIN clusters c ON c.id = cm.cluster_id
                              WHERE cm.user_id = users.id
                                AND c.deleted_at IS NULL)
  LOOP
    cname := left(lower(regexp_replace(split_part(u.email,'@',1),
                 '[^a-z0-9._-]','','g')), 40) || '-default';
    cname := CASE WHEN cname = '-default' THEN 'user-default' ELSE cname END;
    WHILE EXISTS (SELECT 1 FROM clusters WHERE name = cname
                  AND deleted_at IS NULL) LOOP
      cname := left(cname, 40) || '-' || n; n := n + 1;
    END LOOP;
    INSERT INTO clusters (id, owner_id, name, personal)
    VALUES (gen_random_uuid(), u.id, cname, true);
    INSERT INTO cluster_members (cluster_id, user_id, role)
    SELECT id, u.id, 'owner' FROM clusters WHERE name = cname AND owner_id = u.id;
  END LOOP;
END $$;
```

命名规则：`<email 本地部分清洗>-default`，重名追加 `-2/-3…`（name 全局
UNIQUE）。迁移属一次性数据变更，不写审计行（变更本身可从 git 历史追溯）。

### 3.4 machine_id 全局唯一（机器单归属的 DB 层不变式）

```sql
-- 前置断言：存量不得有跨 cluster 重复（JWT 路径始终 409，理论无重复；
-- 有重复则中止迁移人工清理——保留 last_seen 最新一条，删其余）
DO $$
DECLARE dup text;
BEGIN
  SELECT string_agg(machine_id, ', ') INTO dup
  FROM (SELECT machine_id FROM nodes GROUP BY machine_id HAVING count(*) > 1) t;
  IF dup IS NOT NULL THEN
    RAISE EXCEPTION 'duplicate machine_id across clusters, manual cleanup required: %', dup;
  END IF;
END $$;

ALTER TABLE nodes DROP CONSTRAINT nodes_cluster_id_machine_id_key;
CREATE UNIQUE INDEX nodes_machine_id_key ON nodes (machine_id);
```

全局唯一索引同时封死三处漏洞：JWT 注册的查-插竞态、token 注册路径
（`agentEnroll` 从未查跨 cluster 冲突）、以及 move 的并发兜底。处理逻辑上
`CreateNode` 撞 23505 时映射 409 MACHINE_ID_CONFLICT（当前一律 500）。
注：非 Windows dev agent 的 machine_id 是 `"nonwindows-"+hostname`，全局唯一
化是一处 dev-only 行为收窄：**不同主机同 hostname 跨 cluster 注册将从"可
共存"变为 409**。产品节点是 Windows（MachineGuid 全局唯一）不受影响；
mockagent 的 machine_id 生成方式须先过 V4 验证再上线。

## 4. API 改动

| 方法与路径 | 权限 | 语义 | 状态 |
|---|---|---|---|
| `POST /api/users` | admin | 同事务建用户 + 个人默认 cluster + owner 成员 | 改 |
| `PATCH /api/users/{id}` | admin | `{is_admin?, display_name?}`——admin 授/撤收敛入口；**最后 admin 保护**（唯一 is_admin 用户不可被撤/自撤 → 400） | 新（Task 3 必做：§3.1 收敛路径依赖它） |
| `GET /api/clusters` | JWT | 响应扩为 `{id, name, personal, role}`；过滤已删 | 改 |
| `POST /api/clusters` | JWT | 每用户存活自有 cluster ≤ `XNC_MAX_CLUSTERS_PER_USER`（默认 20）→ 400 | 改（限额） |
| `GET /api/clusters/{id}` | 任一成员 | `{id, name, personal, memberCount, nodeCount}`（spec.md:1295 已列未实现） | 新 |
| `PATCH /api/clusters/{id}` | owner | `{name}` 改名；撞名 409/400；审计 `cluster.rename` | 新 |
| `DELETE /api/clusters/{id}` | owner | 软删除加写 `deleted_at`（行为不变：非空 409） | 改 |
| `POST /api/clusters/{id}/members` | owner | 请求体 `{email?|user_id?, role}`——**email 优先**，二者给一；`404 USER_NOT_FOUND` | 改 |
| `PATCH /api/clusters/{id}/members/{userId}` | owner | `{role}`；最后 owner 降级/移出保护；审计 `cluster.member.role` | 新 |
| `DELETE /api/clusters/{id}/members/{userId}` | owner | 不变（Web 补 UI） | — |
| `POST /api/nodes/{id}/move` | **源 ∧ 目标 cluster 双 owner** | `{target: <clusterId或name>}`；nodeId 不变 | 新 |

proto 新增错误码：`CodeUserNotFound`（404；现 add member 的 "user not found"
误用 400+CodeInternal）、`CodeLastAdmin`（400；最后 admin 保护）。
`CodeMachineIDConflict` 已有。

### 4.1 成员 role 变更细则

- 与 removeMember 共用**最后 owner 保护**：目标成员当前 role=owner、新
  role≠owner（或 remove）、且 `CountClusterOwners ≤ 1` → 400。保护必须
  **原子化**——现有 removeMember 的查-删分离在并发双删时可清光 owner（存量
  缺陷）：用条件 UPDATE（带"其余 owner 数 > 1"子查询）或事务内
  `SELECT ... FOR UPDATE`，新端点做对并顺手修 removeMember。
- 允许提升普通成员为 owner（多 owner 合法）；允许 owner 自降（有其他 owner
  时）。
- role ∈ {owner, operator, viewer} 沿用 `validRoles`。

### 4.2 node move 细节

```
POST /api/nodes/{id}/move  {"target": "<clusterIdOrName>"}
1. GetNodeByID → 404 NODE_NOT_FOUND（disabled 节点亦可 move，状态保留）
2. resolveCluster(target)（含 deleted 过滤）→ 404 CLUSTER_NOT_FOUND
3. target == node.cluster_id → 400
4. 发起者对源 cluster 与目标 cluster 均须 role='owner'（两次 GetMemberRole；
   源侧非 owner → 403；目标侧非 owner/非成员 → 403）——把机器移出旧组、放入
   新组都需要资产方同意；单人多 cluster 场景即自己对自己
5. 前置友好检查 GetMachineIDConflictCluster → 409（message 带占用 cluster 名）
6. 事务：
   a. 目标内 name 唯一化（重名自动 `<name>-2` 递增，enroll 同款策略）；检查
      与 UPDATE 之间的并发窗口由 `(cluster_id, name)` 唯一约束兜底，撞 23505
      按 §5.3 约束名分流后递增重试
   b. UPDATE nodes SET cluster_id=target, name=唯一化名
      WHERE id=$1 AND cluster_id=<读到的源>   -- 乐观并发：0 行 → 409 请重试
   c. 唯一索引兜底：23505 → 409 MACHINE_ID_CONFLICT
   d. 审计 node.move {from_cluster, to_cluster, old_name, new_name}
7. 200 {nodeId, clusterId, name}
```

- **在线节点 move 不逐出控制连接**：连接身份 = 公钥挑战-应答（nodeId 维
  度），cluster_id 只是元数据；节点列表按 `ListNodesForUser` 的成员 JOIN
  即时换组；会话/RTV 票据均按 nodeId 路由，不受影响。实现时需 grep
  registry/session 确认无按 clusterId 缓存的路径（验证项 V1）。
- agent 本地 binding.json 的 clusterId 会过期显示（仅 `xnc status` 展示用，
  注册 API 不读它），下次 register/deregister 自然刷新；可选后续：复用 WS
  控制推送通道下发 clusterId 修正（不阻塞本期）。
- 对比旧出路（admin 删节点 + 重注册）：move 保留 nodeId 与审计连续性、不
  需要管理员终端、秒级生效。旧路径保留为 admin 兜底（孤儿节点清理）。

## 5. Server 实现要点

1. **isAdminUser 改读 `users.is_admin`**：auth middleware 本就全行查库
   （`middleware.go`），`sqlc.User` 自带新列后直接取字段，省一次 EXISTS 查询。
   fail-closed 语义不变。
2. **createUser 事务化**（现无事务）：参照 `bootstrap.EnsureAdmin` 模式——
   tx{ CreateUserByEmail → EnsurePersonalCluster(user) → 审计
   user.create（metadata 带默认 cluster id） }。`EnsurePersonalCluster`
   抽成独立函数（name 清洗/唯一化 + CreateCluster(personal=true) +
   AddMembership owner），供未来自注册复用；命名规则与 §3.3 迁移一致。
   并发同 localpart 建号（john@a.com / john@b.com）会在名字检查上竞态——
   CreateCluster 撞 23505 时在事务内递增后缀重试（≤5 次），不得让建号失败。
3. **enroll 错误映射**：`enrollNode` 的 `CreateNode` 分支识别 23505 并按
   `pgErr.ConstraintName` 分流——`nodes_machine_id_key` → 409
   MACHINE_ID_CONFLICT（token 路径由此获得跨 cluster 拒绝）；
   `nodes_cluster_id_name_key` → 名字唯一化竞态，递增后缀重试。两条注册
   路径共用。
4. **enrollNode 增加 cluster 存活校验（修存量 bug）**：token 路径
   （agentEnroll）从不校验 cluster 状态，已删 cluster 的未过期 enrollment
   token 仍可把节点注册进已删 cluster；加了列表过滤后这种节点会从所有
   列表消失（比现状更糟）。enrollNode 事务内校验 cluster 存活（JWT 路径
   的 resolveCluster 已过滤，此校验主要覆盖 token 路径）：遇已删 cluster
   → 410（token 视作随 cluster 失效）。
5. **软删除查询过滤**：§3.2 列出的 sqlc 查询统一加 `deleted_at IS NULL`，
   `resolveCluster`/`authorizeClusterOwner` 复用过滤后的查询即自动生效。
6. **EnsureAdmin 更新**：bootstrap 显式写 `is_admin=true`（0005 只回填存量
   库；全新库首启用户不经迁移，漏设则首个 admin 不是 admin——审查发现的
   必修项）。
7. 审计动作新增：`cluster.rename`、`cluster.member.role`、`node.move`、
   `user.admin.toggle`；`user.create` metadata 增补默认 cluster id。

## 6. CLI 改动（`src/cli`）

```
xnc cluster create <name>                          # 新
xnc cluster rename <cluster> <new-name>            # 新
xnc cluster delete <cluster>                       # 已有
xnc cluster member add <cluster> <email> [--role operator]   # 改：按 email
xnc cluster member role <cluster> <email> <role>   # 新
xnc cluster member remove <cluster> <email>        # 改：按 email（UUID 兼容保留）
xnc node move <node> --cluster <target>            # 新；node 解析沿用现有 node
                                                   # 子命令惯例（UUID，或 name +
                                                   # 所在 --cluster——name 仅
                                                   # cluster 内唯一）
```

- `cluster` 参数沿用现有"UUID 或 name"解析；`--cluster` 目标同款。
- `xnc register` 无改动：默认 cluster 保证列表非空，多 cluster 交互选择如常。
- move 成功输出新 `{clusterId, name}`（提示 name 可能因冲突自动递增）。

## 7. Web 改动（`src/web`）

- **Clusters 页**（`pages/Clusters.tsx`，现只读列表 → 完整管理）：
  - 表格加列：角色（我的 role）、`personal` 徽标（"个人"）、成员数/节点数；
  - 新建 cluster 对话框；行内改名 / 删除（非空时服务端 409 → 行内提示
    "先移出或删除节点"，附跳转）；
  - **成员管理面板**（选中 cluster 展开）：成员表（email/角色/加入时间）+
    按 email 添加（role 下拉）+ 改角色 + 移除（最后 owner 保护由服务端 400
    兜底提示）。
- **Nodes 页**（`pages/Nodes.tsx`）：行操作"移动到…"——仅当用户是节点当前
  cluster 的 owner 时展示；目标下拉 = 我任 owner 的其他存活 cluster；调
  `POST /api/nodes/{id}/move`。
- **Users 页**（`pages/Users.tsx`）：移除 Cluster membership 表单（成员管理
  已迁至 Clusters 页，不再依赖 admin 列全量用户）；保留用户列表/创建；新增
  is_admin 开关（调 PATCH，admin-only 渲染）。
- `types.ts`：`ClusterDTO` 扩 `{personal, role}`。
- 顶栏无需"当前 cluster"全局概念——cluster 仍是过滤器语义，不引入切换态。

## 8. 安全考量

- **email 探测面**：add member 的 404 暴露"该邮箱是否已注册"。与登录接口
  （统一 invalid credentials）不同，但成员邀请场景普遍如此且操作已限
  owner——接受，标注即可。
- **admin 收敛路径**：迁移回填 = 现状快照（零回归），上线后管理员经
  `PATCH /api/users/{id}` 收敛 is_admin=false；该端点自身 admin-only 并审计
  `user.admin.toggle`。**最后 admin 保护**：系统内最后一个 is_admin=true
  用户不可被撤销或自撤（400 LAST_ADMIN）——EnsureAdmin 只在零用户时自愈，
  清光 admin 后没有 API 出路，只剩手工 SQL。
- **move 的信任边界**：双边 owner 门槛保证跨信任域迁移必有双方同意；
  adopt 语义不变（仍限同 cluster——跨 cluster 换 key 必须先 move 再
  register，机器身份链在审计上可追）。
- **限额**：`XNC_MAX_CLUSTERS_PER_USER`（默认 20）只限"我任 owner 的存活
  cluster 数"，防刷。

## 9. 兼容性

- 存量 CLI/agent 零改动：register、binding.json、`/api/agent/enroll`、WS
  协议全部不变；新端点纯增量，旧客户端无感。
- 老 CLI 收到 MACHINE_ID_CONFLICT 的提示语义不变（message 仍含冲突 cluster
  名）；新增的自助出路（move）只在文档/新 CLI 里引导。
- spec.md 更新点：API 表（:1293-1320）加 PATCH/GET/move/role 行；CLI 表
  （:2675-2684）加新命令；§6 角色矩阵加"个人默认 cluster"段。
- **AGENTS.md 硬性契约第 6 条需同步修订**：machineId 跨 cluster=409 的出路
  从"管理端删除旧记录后重注册"扩为"双边 owner 的 node move（nodeId 不变）
  或管理端删除"。

## 10. 任务拆分（feat 分支，逐任务独立提交）

分支 `feat/user-default-clusters`，PR 过 ci 六矩阵后并回 main。

| # | 任务 | 范围 |
|---|---|---|
| 1 | migration 0005 + sqlc 重生成 + deleted_at 过滤 | §3 全部、§5.5 |
| 2 | admin 位落地 + 默认 cluster 事务 | §3.1 对应 handler（isAdminUser 改字段、**EnsureAdmin 置 is_admin**、createUser 事务、EnsurePersonalCluster、限额、listClusters DTO） |
| 3 | cluster GET/PATCH + 成员 email/role + 用户 PATCH | §4 对应行 + proto CodeUserNotFound / CodeLastAdmin |
| 4 | node move + enroll 23505 映射 + cluster 存活校验 | §4.2、§5.3、§5.4 |
| 5 | CLI 子命令 | §6 |
| 6 | Web UI | §7 |
| 7 | 文档 | spec.md、AGENTS.md §2.6 修订 |

**Task 1-4 必须同一 PR、单次部署合入**：0005 回填默认 cluster 之后、新版
isAdminUser 上线之前，若旧版 isAdminUser（派生谓词）服务仍在运行，原本无
cluster 的普通用户会瞬间成为平台 admin——拆开发布存在权限窗口，禁止。

P2（本期不做）：`DELETE /api/users`（用户注销）超范围不做。

## 11. 测试计划

server（testcontainers 真 PG）：

- 迁移：0005 幂等重跑安全；回填后每个旧用户恰有一个 personal owner
  cluster；audit 带 cluster.delete 的行与历史改名行均正确打 deleted_at、
  用户故意创建的同形名（无删除审计）不误标；已删 cluster 同名重建成功且
  互不干扰；跨 cluster 重复 machine_id 时迁移中止。
- 建用户：admin 建 → 断言默认 cluster 存在、owner 成员、personal=true、
  name 唯一化；事务失败回滚不留半成品；并发同 localpart 不同域建号均成功。
- bootstrap：全新库首启断言 admin 用户 is_admin=true（防 EnsureAdmin 漏设
  回归——审查发现的必修项）。
- admin：is_admin=false 用户建 cluster 后 isAdminUser 仍 false（防回归——
  这是本设计最大的行为变化）；回填用户保持 admin；PATCH 授/撤 is_admin 落
  审计；最后一个 admin 被撤销/自撤 → 400。
- rename：重名 409；已删 cluster 不可 resolve（404）；列表不出现已删
  cluster（存量缺陷回归测试）。
- 成员：email 添加成功/未注册 404；role 变更；最后 owner 降级与移除均
  400，且并发双降级最后两个 owner 恰一个成功（保护原子性）；viewer 读成员
  表 200、写 403。
- move：单边 owner 403；target 不存在 404；同名自动递增；nodeId/公钥/
  status 不变；machine 冲突 409；并发双 register 不同 cluster 同 machine
  恰一个成功（唯一索引竞态回归）。
- token 注册路径跨 cluster 同 machineId → 409（修复回归）；已删 cluster
  的未过期 token 注册 → 410。
- 软删除后同名 cluster 可重建且互不干扰。

CLI：新子命令 smoke（对接测试 server）；Web：组件手工冒烟 + 现有页面回归。

## 12. 开放验证项（实现时确认）

- **V1**：grep registry/session/rtvpool 确认无按 clusterId 缓存或路由的路径
  （move 在线不断连的前提）。
- **V2**：`agentConnect`（WS 控制连接握手）确认鉴权不依赖 clusterId（探索
  显示为公钥挑战-应答，需实现时复核）。
- **V3**：现网预检 `SELECT machine_id, count(*) FROM nodes GROUP BY 1
  HAVING count(*)>1` 为空后才可上线 0005（迁移断言会硬失败兜底）。
- **V4**：mockagent 负载测试的 machine_id 生成方式确认与全局唯一索引兼容
  （load 测试单 cluster 内本就受 cluster 级唯一约束，理论无影响）。
