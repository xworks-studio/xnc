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
