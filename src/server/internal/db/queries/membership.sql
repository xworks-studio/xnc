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

-- name: RemoveMemberGuarded :execrows
-- 原子化的最后 owner 保护：目标非 owner 直接删；是 owner 则须还有其他
-- owner（子查询在同一语句内判定，封死查-删分离窗口的并发双删清光 owner）。
-- 返回 0 行 = 未删（非成员 或 最后一个 owner），由调用方区分。
DELETE FROM cluster_members AS m
WHERE m.cluster_id = $1 AND m.user_id = $2
  AND (m.role <> 'owner'
       OR (SELECT count(*) FROM cluster_members sub
           WHERE sub.cluster_id = $1 AND sub.role = 'owner') > 1);

-- name: UpdateMemberRole :execrows
-- 改 role 的同款保护：owner 降级为非 owner 须还有其他 owner。返回 0 行 =
-- 未改（非成员 或 最后一个 owner 降级），由调用方区分。
UPDATE cluster_members AS m SET role = $3
WHERE m.cluster_id = $1 AND m.user_id = $2
  AND (m.role <> 'owner' OR $3 = 'owner'
       OR (SELECT count(*) FROM cluster_members sub
           WHERE sub.cluster_id = $1 AND sub.role = 'owner') > 1);

-- name: GetMemberRole :one
SELECT role FROM cluster_members WHERE cluster_id = $1 AND user_id = $2;

-- name: CountClusterOwners :one
SELECT count(*) FROM cluster_members WHERE cluster_id = $1 AND role = 'owner';
