-- 0004_relay_plane.sql — RTV 媒体中继池（relay-plane spec §2.3）。
--
-- relays：外部中继注册表（经控制连接动态注册 + 公钥准入；relay-0 内嵌
-- 中继不入表——它与 server 同生共死，无准入问题）。
-- status 生命周期：pending（注册待审批）→ active（参与分配）→ draining
-- （摘除中，P2 的 redirect 迁移期）→ retired（永久下线）。
-- 准入两路（spec §0）：XNC_RTV_RELAY_ALLOWLIST 公钥清单命中即 active；
-- 未命中落 pending，管理端审批激活。挑战-应答只证明持钥，准入由
-- 清单/审批决定——防任何人注册 relay 入池投毒。

CREATE TABLE relays (
    id           TEXT PRIMARY KEY,             -- rl-<8hex>，server 分配
    pubkey       TEXT NOT NULL UNIQUE,         -- ed25519 hex（64 字节）
    region       TEXT NOT NULL DEFAULT '',
    endpoints    JSONB NOT NULL DEFAULT '[]',  -- proto.EndpointDesc 数组
    max_sessions INT NOT NULL DEFAULT 0,       -- 自愿声明，分配打分用（0=未声明）
    max_mbps_out INT NOT NULL DEFAULT 0,
    version      TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','active','draining','retired')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
