-- 审计查询（GET /api/audit）：动态 WHERE + 分页。
-- sqlc.narg 生成可空参数（pgtype.*）——零值（Valid=false）即 NULL，
-- 对应过滤跳过；action/userId/nodeId/since 全部可选组合。

-- name: QueryAuditLog :many
SELECT id, user_id, cluster_id, node_id, action, session_id, metadata, created_at
FROM audit_logs
WHERE (sqlc.narg('node_id')::uuid IS NULL OR node_id = sqlc.narg('node_id'))
  AND (sqlc.narg('user_id')::uuid IS NULL OR user_id = sqlc.narg('user_id'))
  AND (sqlc.narg('action')::text IS NULL OR action = sqlc.narg('action'))
  AND (sqlc.narg('since')::timestamptz IS NULL OR created_at >= sqlc.narg('since'))
ORDER BY created_at DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');
