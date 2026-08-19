-- name: CreateEnrollmentToken :one
INSERT INTO enrollment_tokens (id, cluster_id, token_hash, expires_at, max_uses, created_by)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: GetEnrollmentTokenByHash :one
SELECT * FROM enrollment_tokens WHERE token_hash = $1;

-- name: ConsumeEnrollmentToken :one
UPDATE enrollment_tokens SET used_count = used_count + 1
WHERE id = $1 AND used_count < max_uses
RETURNING used_count;
