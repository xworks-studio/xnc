-- 0005: 用户自助 cluster 基线（默认 cluster / 显式 admin 位 / 软删除标记 /
-- machine_id 全局唯一）。设计见
-- docs/superpowers/specs/2026-09-14-user-cluster-self-service-design.md。

-- 1) 显式 admin 位：取代"任一 cluster owner 即 admin"的派生谓词（该谓词在
--    人人拥有默认 cluster 后会让所有用户变成平台 admin）。回填 = 迁移瞬间
--    现行谓词快照，现网 owner 保持 admin（零权限回归）。
--    顺序约束：本段必须在第 3 段默认 cluster 回填之前执行，否则回填产生的
--    owner 会被快照成 admin。
ALTER TABLE users ADD COLUMN is_admin boolean NOT NULL DEFAULT false;
UPDATE users SET is_admin = true
WHERE EXISTS (SELECT 1 FROM cluster_members cm
              WHERE cm.user_id = users.id AND cm.role = 'owner');

-- 2) cluster 软删除标记 + 个人默认标记。存量 deleted_at 回填主用审计精确
--    定位（每次软删除都写 cluster.delete 审计行），regex 兜底覆盖"改名成功
--    但审计写入失败"的窗口（历史删除行已被改名 name_deleted_<unix秒>，
--    原名不可恢复，保留现状）。
ALTER TABLE clusters ADD COLUMN deleted_at timestamptz;
ALTER TABLE clusters ADD COLUMN personal boolean NOT NULL DEFAULT false;
UPDATE clusters SET deleted_at = now()
WHERE id IN (SELECT DISTINCT cluster_id FROM audit_logs
             WHERE action = 'cluster.delete' AND cluster_id IS NOT NULL);
UPDATE clusters SET deleted_at = to_timestamp(
  (regexp_match(name, '_deleted_([0-9]+)$'))[1]::bigint)
WHERE name ~ '_deleted_[0-9]+$' AND deleted_at IS NULL;

-- name 唯一性改部分索引（存活行内唯一）：已删行保留原名 → 同名可重建，
-- 软删除不再需要改名释放名字。
ALTER TABLE clusters DROP CONSTRAINT clusters_name_key;
CREATE UNIQUE INDEX clusters_name_live_key ON clusters (name)
WHERE deleted_at IS NULL;

-- 3) 存量回填个人默认 cluster：没有任何存活 cluster 成员关系的用户各建一
--    个（命名 <email 本地部分清洗>-default，重名递增 -2/-3…）。
DO $$
DECLARE u record; cname text; n int;
BEGIN
  FOR u IN SELECT id, email FROM users
           WHERE NOT EXISTS (SELECT 1 FROM cluster_members cm
                               JOIN clusters c ON c.id = cm.cluster_id
                              WHERE cm.user_id = users.id
                                AND c.deleted_at IS NULL)
  LOOP
    cname := left(lower(regexp_replace(split_part(u.email, '@', 1),
                 '[^a-z0-9._-]', '', 'g')), 40) || '-default';
    IF cname = '-default' THEN
      cname := 'user-default';
    END IF;
    n := 2;
    WHILE EXISTS (SELECT 1 FROM clusters WHERE name = cname
                  AND deleted_at IS NULL) LOOP
      cname := left(cname, 40) || '-' || n;
      n := n + 1;
    END LOOP;
    INSERT INTO clusters (id, owner_id, name, personal)
    VALUES (gen_random_uuid(), u.id, cname, true);
    INSERT INTO cluster_members (cluster_id, user_id, role)
    SELECT id, u.id, 'owner' FROM clusters
    WHERE name = cname AND owner_id = u.id AND deleted_at IS NULL;
  END LOOP;
END $$;

-- 4) machine_id 全局唯一（一台机器同一时间只能属于一个 cluster 的 DB 层
--    不变式；封死跨 cluster 并发注册竞态与 token 注册路径漏检）。前置断言：
--    存量不得有跨 cluster 重复（JWT 路径始终 409；有则中止人工清理——保留
--    last_seen 最新一条，删其余）。
DO $$
DECLARE dup text;
BEGIN
  SELECT string_agg(machine_id, ', ') INTO dup
  FROM (SELECT machine_id FROM nodes GROUP BY machine_id HAVING count(*) > 1) t;
  IF dup IS NOT NULL THEN
    RAISE EXCEPTION 'duplicate machine_id across clusters, manual cleanup required: %', dup;
  END IF;
END $$;
ALTER TABLE nodes DROP CONSTRAINT nodes_cluster_id_machine_id_key;
CREATE UNIQUE INDEX nodes_machine_id_key ON nodes (machine_id);
