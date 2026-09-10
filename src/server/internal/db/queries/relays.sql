-- name: UpsertRelay :one
-- 注册/再注册：按 pubkey 幂等（relay 重启换 relayId 不换身份）。再注册
-- 只刷新画像（endpoints/capacity/version/region）与 last_seen，不触碰
-- status——审批语义只归管理端。
INSERT INTO relays (id, pubkey, region, endpoints, max_sessions, max_mbps_out, version, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (pubkey) DO UPDATE SET
  region = EXCLUDED.region,
  endpoints = EXCLUDED.endpoints,
  max_sessions = EXCLUDED.max_sessions,
  max_mbps_out = EXCLUDED.max_mbps_out,
  version = EXCLUDED.version,
  last_seen_at = now()
RETURNING *;

-- name: GetRelayByPubkey :one
SELECT * FROM relays WHERE pubkey = $1;

-- name: GetRelay :one
SELECT * FROM relays WHERE id = $1;

-- name: ListRelays :many
SELECT * FROM relays ORDER BY created_at;

-- name: ListActiveRelays :many
SELECT * FROM relays WHERE status = 'active' ORDER BY created_at;

-- name: SetRelayStatus :execrows
UPDATE relays SET status = $2 WHERE id = $1;

-- name: TouchRelay :exec
UPDATE relays SET last_seen_at = now() WHERE id = $1;
