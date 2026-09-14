-- name: ListClustersForUser :many
-- deleted_at IS NULL：软删除的 cluster 不再出现在用户列表（0005 存量缺陷修复）。
-- member/node 计数随行带出（子查询聚合，Web 列表页一次拉齐免 N+1）。
SELECT c.id, c.name, c.owner_id, c.created_at, c.personal, m.role,
       (SELECT count(*) FROM cluster_members cm
         WHERE cm.cluster_id = c.id) AS member_count,
       (SELECT count(*) FROM nodes n WHERE n.cluster_id = c.id) AS node_count
FROM clusters c JOIN cluster_members m ON m.cluster_id = c.id
WHERE m.user_id = $1 AND c.deleted_at IS NULL ORDER BY c.name;

-- name: CreateCluster :one
INSERT INTO clusters (id, name, owner_id, personal) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: GetClusterByName :one
SELECT * FROM clusters WHERE name = $1 AND deleted_at IS NULL;

-- name: GetClusterByID :one
SELECT * FROM clusters WHERE id = $1 AND deleted_at IS NULL;

-- name: RenameCluster :one
UPDATE clusters SET name = $2 WHERE id = $1 AND deleted_at IS NULL RETURNING *;

-- name: AddMembership :exec
INSERT INTO cluster_members (cluster_id, user_id, role) VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: CountOwnedLiveClusters :one
-- 自助建 cluster 的每用户限额判定（只数存活且任 owner 的 cluster）。
SELECT count(*) FROM clusters c
JOIN cluster_members m ON m.cluster_id = c.id
WHERE m.user_id = $1 AND m.role = 'owner' AND c.deleted_at IS NULL;
