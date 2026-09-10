-- name: InsertAuditLog :exec
INSERT INTO audit_logs (user_id, cluster_id, node_id, action, metadata)
VALUES ($1, $2, $3, $4, $5);
