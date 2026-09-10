-- 0003_channels.sql — 发布频道（dev/stable 双通道）。
--
-- releases.channel：制品入库时标记所属频道。
-- nodes.channel：节点订阅的频道（默认 stable），目标版本 =
--   COALESCE(nodes.target_release, 最新 release WHERE channel = nodes.channel)。

ALTER TABLE releases ADD COLUMN IF NOT EXISTS channel TEXT NOT NULL DEFAULT 'stable'
    CHECK (channel IN ('stable', 'dev'));

ALTER TABLE nodes ADD COLUMN IF NOT EXISTS channel TEXT NOT NULL DEFAULT 'stable';

CREATE INDEX idx_releases_channel ON releases (channel, created_at DESC);
