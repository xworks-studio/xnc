-- name: CreateNode :one
INSERT INTO nodes (id, cluster_id, name, machine_id, hostname, os_version,
                   agent_version, shell_type, public_key)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING *;

-- name: GetNodeByIdentity :one
SELECT * FROM nodes WHERE cluster_id = $1 AND machine_id = $2;

-- name: GetNodeByNameInCluster :one
SELECT * FROM nodes WHERE cluster_id = $1 AND name = $2;

-- name: GetNodeByID :one
SELECT * FROM nodes WHERE id = $1;

-- name: GetMachineIDConflictCluster :one
-- 跨 cluster machineId 冲突检查（用户 JWT 注册 409，spec §6.4）：machineId 已
-- 注册于其他 cluster 时返回该 cluster 名（供 CLI 提示 --force 或管理端处理）。
SELECT c.name FROM nodes n JOIN clusters c ON c.id = n.cluster_id
WHERE n.machine_id = $1 AND n.cluster_id <> $2 LIMIT 1;

-- name: DeleteNode :execrows
-- 机器自注销（spec §7：控制连接上 NODE_DELETE，机器身份即凭据）。返回删除
-- 行数：0 = 节点已不存在（重复注销按幂等成功处理）。
DELETE FROM nodes WHERE id = $1;
