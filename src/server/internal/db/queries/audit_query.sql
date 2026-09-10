-- 审计查询（GET /api/audit）：动态 WHERE + 分页 + 人类可读名称。
-- sqlc.narg 生成可空参数（pgtype.*）——零值（Valid=false）即 NULL，
-- 对应过滤跳过；action/userId/nodeId/since 全部可选组合。

-- name: QueryAuditLog :many
SELECT a.id, a.user_id, a.cluster_id, a.node_id, a.action, a.session_id, a.metadata, a.created_at,
       u.email AS user_email,
       n.name AS node_name
FROM audit_logs a
LEFT JOIN users u ON u.id = a.user_id
LEFT JOIN nodes n ON n.id = a.node_id
WHERE (sqlc.narg('node_id')::uuid IS NULL OR a.node_id = sqlc.narg('node_id'))
  AND (sqlc.narg('user_id')::uuid IS NULL OR a.user_id = sqlc.narg('user_id'))
  AND (sqlc.narg('action')::text IS NULL OR a.action = sqlc.narg('action'))
  AND (sqlc.narg('since')::timestamptz IS NULL OR a.created_at >= sqlc.narg('since'))
ORDER BY a.created_at DESC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');
