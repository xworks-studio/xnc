-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: CreateUser :one
-- is_admin 显式入参：bootstrap 首启 admin 需 true（0005 起 admin 是显式列，
-- 全新库不经迁移回填，漏设则首启 admin 不是 admin）。
INSERT INTO users (id, email, display_name, password_hash, is_admin)
VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- name: UpdateUserDisplayName :execrows
UPDATE users SET display_name = $2 WHERE id = $1;

-- name: UpdateUserPassword :execrows
UPDATE users SET password_hash = $2 WHERE id = $1;
