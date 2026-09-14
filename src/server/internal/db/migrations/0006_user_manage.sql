-- 0006: 用户管理面板支撑（上次登录时间 / 管理员重置密码 / 删除用户）。
-- 设计延续 docs/superpowers/specs/2026-09-14-user-cluster-self-service-design.md。

-- 1) 上次登录时间（login 成功后写 now()；NULL = 从未登录）——Users 页独立列。
ALTER TABLE users ADD COLUMN last_login_at timestamptz;

-- 2) 删除用户的可落地前提：
--    a) 审计行永久保留（audit 是不可变历史），actor 被删时置空而非级联删除；
--    b) enrollment token 是集群的从属凭据，随集群硬删除级联清掉（此前无
--       ON DELETE，用户持有的空集群会因 token 残留而删不掉）。
--    （节点不在处理范围：用户名下集群仍含节点时端点层 409 拒绝删除，
--    出路 = 先 move/删除节点——与集群删除的 CLUSTER_NOT_EMPTY 同语义。）
ALTER TABLE audit_logs DROP CONSTRAINT audit_logs_user_id_fkey;
ALTER TABLE audit_logs ADD CONSTRAINT audit_logs_user_id_fkey
  FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE enrollment_tokens DROP CONSTRAINT enrollment_tokens_cluster_id_fkey;
ALTER TABLE enrollment_tokens ADD CONSTRAINT enrollment_tokens_cluster_id_fkey
  FOREIGN KEY (cluster_id) REFERENCES clusters(id) ON DELETE CASCADE;
