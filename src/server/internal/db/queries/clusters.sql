-- name: ListClustersForUser :many
SELECT c.id, c.name, c.owner_id, c.created_at, m.role
FROM clusters c JOIN cluster_members m ON m.cluster_id = c.id
WHERE m.user_id = $1 ORDER BY c.name;

-- name: CreateCluster :one
INSERT INTO clusters (id, name, owner_id) VALUES ($1, $2, $3) RETURNING *;

-- name: GetClusterByName :one
SELECT * FROM clusters WHERE name = $1;

-- name: GetClusterByID :one
SELECT * FROM clusters WHERE id = $1;

-- name: AddMembership :exec
INSERT INTO cluster_members (cluster_id, user_id, role) VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;
