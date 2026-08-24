-- name: CreateRelease :one
INSERT INTO releases (version, notes, channel) VALUES ($1, $2, $3)
ON CONFLICT (version) DO UPDATE SET notes = EXCLUDED.notes, channel = EXCLUDED.channel
RETURNING *;

-- name: PutArtifact :exec
INSERT INTO release_artifacts (release_id, name, sha256, size, data)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (release_id, name) DO UPDATE
SET sha256 = EXCLUDED.sha256, size = EXCLUDED.size, data = EXCLUDED.data;

-- name: ListReleases :many
SELECT id, version, notes, channel, created_at FROM releases ORDER BY created_at DESC LIMIT 50;

-- name: GetReleaseByVersion :one
SELECT * FROM releases WHERE version = $1;

-- name: GetArtifact :one
SELECT * FROM release_artifacts WHERE release_id = $1 AND name = $2;

-- name: GetLatestReleaseByChannel :one
SELECT * FROM releases WHERE channel = $1 ORDER BY created_at DESC LIMIT 1;

-- name: DeleteRelease :execrows
DELETE FROM releases WHERE id = $1;

-- name: SetNodeTargetRelease :exec
UPDATE nodes SET target_release = $2 WHERE id = $1;

-- name: SetNodeChannel :exec
UPDATE nodes SET channel = $2 WHERE id = $1;

-- name: GetNodeTargetRelease :one
SELECT target_release, channel FROM nodes WHERE id = $1;
