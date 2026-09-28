-- 0007_node_platform.sql — 节点平台维度（2026-09-28 Linux 节点支持）：
-- nodes.platform 记录 agent 上报的 GOOS（windows|linux；历史行回填
-- windows）。发布分发与 UPDATE_AVAILABLE 推送按平台选制品（setup.exe vs
-- xnc-linux-amd64.tar.gz），web 按 platform 条件渲染 RDP/桌面面板。
ALTER TABLE nodes
    ADD COLUMN IF NOT EXISTS platform TEXT NOT NULL DEFAULT 'windows';
