-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: CreateUser :one
INSERT INTO users (id, email, display_name, password_hash)
VALUES ($1, $2, $3, $4) RETURNING *;

-- name: UpdateUserDisplayName :execrows
UPDATE users SET display_name = $2 WHERE id = $1;

-- name: UpdateUserPassword :execrows
UPDATE users SET password_hash = $2 WHERE id = $1;
