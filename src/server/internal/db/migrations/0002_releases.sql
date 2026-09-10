-- 0002_releases.sql — 统一自更新：release 制品与节点 pin。
--
-- releases/release_artifacts：bundle（agent+helper）与 CLI 制品入库存
-- bytea（部署为单实例 + 既有 pgdata 卷，≤10 节点规模下 ~15MB 制品直接进
-- 库比挂磁盘卷简单且随备份走）。
-- nodes.target_release：admin 可 pin 节点到指定版本；NULL = 跟随最新。
-- agent 的目标版本 = COALESCE(nodes.target_release, 最新 release)。

CREATE TABLE releases (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    version    TEXT NOT NULL UNIQUE,
    notes      TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE release_artifacts (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    release_id UUID NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,               -- 'bundle.tar.gz' | 'xnc-windows-amd64.exe'
    sha256     TEXT NOT NULL,
    size       BIGINT NOT NULL,
    data       BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (release_id, name)
);

ALTER TABLE nodes ADD COLUMN target_release TEXT;

CREATE INDEX idx_releases_created ON releases (created_at DESC);
