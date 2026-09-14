-- Cluster 删除（DELETE /api/clusters/{id}）查询。
-- 软删除策略：有节点时端点层拒绝（409）；0005 起删除 = 打 deleted_at（name
-- 唯一性是存活行部分索引，原名自动可复用，不再需要改名把戏）。

-- name: CountNodesInCluster :one
SELECT count(*) FROM nodes WHERE cluster_id = $1;

-- name: SoftDeleteCluster :execrows
UPDATE clusters SET deleted_at = now()
WHERE id = $1 AND deleted_at IS NULL;
