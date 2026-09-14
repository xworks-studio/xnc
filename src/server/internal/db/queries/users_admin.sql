-- 用户管理（admin-only REST）专用查询。CreateUserByEmail 与既有 CreateUser 的
-- 区别：不经调用方生成 id（DB 默认 gen_random_uuid），供 handler 直接消费。

-- name: CreateUserByEmail :one
INSERT INTO users (email, display_name, password_hash)
VALUES ($1, $2, $3) RETURNING *;

-- name: ListUsers :many
SELECT * FROM users ORDER BY email;

-- name: CountUsersByEmail :one
SELECT count(*) FROM users WHERE email = $1;

-- name: UpdateUserIsAdmin :execrows
-- admin 授/撤（PATCH /api/users/{id}）。撤（$2=false）带最后 admin 保护：
-- 系统内须仍有其他 is_admin 用户。返回 0 行 = 目标不存在或触发保护。
UPDATE users SET is_admin = $2
WHERE users.id = $1
  AND ($2 OR (SELECT count(*) FROM users sub WHERE sub.is_admin) > 1);

-- name: CountNodesInOwnedClusters :one
-- 删除用户前置检查：用户名下（owner_id 归属）集群仍挂节点则 409 拒绝——
-- 节点是资产，删除用户不得连带吞掉机器注册记录。
SELECT count(*) FROM nodes n JOIN clusters c ON c.id = n.cluster_id
WHERE c.owner_id = $1;

-- name: DeleteOwnedClusters :execrows
-- 删除用户时清掉其名下（此时必然已无节点）的集群行：membership 级联，
-- enrollment token 经 0006 的 ON DELETE CASCADE 级联。
DELETE FROM clusters WHERE owner_id = $1;

-- name: DeleteUser :execrows
-- 用户行硬删除：membership 级联（cluster_members user FK ON DELETE CASCADE），
-- 审计行保留、user_id 置空（0006 FK 语义）。
DELETE FROM users WHERE id = $1;
