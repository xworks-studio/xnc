-- Cluster 删除（DELETE /api/clusters/{id}）查询。
-- 软删除策略：有节点时端点层拒绝（409），删除即重命名保留审计链。

-- name: CountNodesInCluster :one
SELECT count(*) FROM nodes WHERE cluster_id = $1;

-- name: SoftDeleteCluster :exec
UPDATE clusters SET name = $2 WHERE id = $1;
