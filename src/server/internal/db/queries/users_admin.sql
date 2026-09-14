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
