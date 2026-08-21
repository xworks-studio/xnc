-- Membership 管理（GET/POST/DELETE /api/clusters/{id}/members）查询。
-- role 受 cluster_members 的 CHECK 约束（owner/operator/viewer）。

-- name: ListMembers :many
SELECT m.user_id, u.email, u.display_name, m.role
FROM cluster_members m JOIN users u ON u.id = m.user_id
WHERE m.cluster_id = $1 ORDER BY u.email;

-- name: AddMember :exec
INSERT INTO cluster_members (cluster_id, user_id, role) VALUES ($1, $2, $3);

-- name: RemoveMember :exec
DELETE FROM cluster_members WHERE cluster_id = $1 AND user_id = $2;

-- name: GetMemberRole :one
SELECT role FROM cluster_members WHERE cluster_id = $1 AND user_id = $2;

-- name: CountClusterOwners :one
SELECT count(*) FROM cluster_members WHERE cluster_id = $1 AND role = 'owner';
