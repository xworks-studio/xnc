-- name: SetNodeStatus :exec
UPDATE nodes SET status = $2 WHERE id = $1;

-- name: TouchNode :exec
UPDATE nodes SET last_seen_at = now() WHERE id = $1;

-- name: UpdateNodeMeta :exec
UPDATE nodes SET hostname = $1, shell_type = $2, agent_version = $3
WHERE id = $4;

-- name: ListNodesForUser :many
SELECT n.id, n.cluster_id, n.name, n.hostname, n.os_version, n.agent_version,
       n.shell_type, n.status, n.last_seen_at, c.name AS cluster_name
FROM nodes n
JOIN cluster_members m ON m.cluster_id = n.cluster_id AND m.user_id = $1
JOIN clusters c ON c.id = n.cluster_id
ORDER BY c.name, n.name;

-- name: GetNodeForUser :one
SELECT n.id, n.cluster_id, n.name, n.hostname, n.os_version, n.agent_version,
       n.shell_type, n.status, n.last_seen_at, c.name AS cluster_name
FROM nodes n
JOIN cluster_members m ON m.cluster_id = n.cluster_id AND m.user_id = $1
JOIN clusters c ON c.id = n.cluster_id
WHERE n.id = $2;
