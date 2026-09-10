-- 用户管理（admin-only REST）专用查询。CreateUserByEmail 与既有 CreateUser 的
-- 区别：不经调用方生成 id（DB 默认 gen_random_uuid），供 handler 直接消费。

-- name: CreateUserByEmail :one
INSERT INTO users (email, display_name, password_hash)
VALUES ($1, $2, $3) RETURNING *;

-- name: ListUsers :many
SELECT * FROM users ORDER BY email;

-- name: CountUsersByEmail :one
SELECT count(*) FROM users WHERE email = $1;
