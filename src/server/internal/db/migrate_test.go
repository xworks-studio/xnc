package db

import (
	"context"
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestMigrateIdempotent(t *testing.T) {
	t.Parallel()
	st := OpenTestStore(t)

	// 二次迁移不报错（幂等）。
	require.NoError(t, Migrate(t.Context(), st.Pool()))

	var n int
	require.NoError(t, st.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_name IN ('users','clusters','cluster_members','nodes',
		 'enrollment_tokens','audit_logs','schema_migrations')`).Scan(&n))
	assert.Equal(t, 7, n)
}

// openStoreAt0004 启动裸 PG 容器并只应用 0001-0004，供 0005 回填语义测试
// 构造存量状态（OpenTestStore 会直通全部迁移，不适用）。
func openStoreAt0004(t *testing.T) *Store {
	t.Helper()
	pg, err := postgres.Run(t.Context(), "postgres:16-alpine",
		postgres.WithDatabase("xnc"), postgres.WithUsername("xnc"), postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(3*time.Minute)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Terminate(context.Background()) })
	u, err := pg.ConnectionString(t.Context(), "sslmode=disable")
	require.NoError(t, err)
	pool, err := pgxpool.New(t.Context(), u)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	// embed FS 文件名带后缀，直接按序号 glob 精确取 0001-0004（并登记
	// schema_migrations，Migrate 按版本记录判已应用）。
	_, err = pool.Exec(t.Context(),
		`CREATE TABLE IF NOT EXISTS schema_migrations(
		 version int PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`)
	require.NoError(t, err)
	for i := 1; i <= 4; i++ {
		matches, err := fs.Glob(migrationsFS, fmt.Sprintf("migrations/%04d_*.sql", i))
		require.NoError(t, err)
		require.Len(t, matches, 1)
		sql, err := migrationsFS.ReadFile(matches[0])
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(), string(sql))
		require.NoError(t, err)
		_, err = pool.Exec(t.Context(),
			`INSERT INTO schema_migrations(version) VALUES($1)`, i)
		require.NoError(t, err)
	}
	return &Store{pool: pool}
}

func TestMigrate0005Backfill(t *testing.T) {
	t.Parallel()
	st := openStoreAt0004(t)
	ctx := t.Context()
	pool := st.Pool()

	// 存量状态构造：
	//   owner@x  —— 存活 cluster "live" 的 owner（迁移后 is_admin=true）
	//   viewer@x —— "live" 的 viewer（is_admin=false，已有存活 cluster → 不回填默认）
	//   bare@x   —— 无任何 cluster（is_admin=false + 回填个人默认 cluster）
	//   deleted@x—— 仅是已软删（改名形态）cluster 的 owner（is_admin=true——
	//               快照谓词不看存活；无存活 cluster → 回填个人默认）
	//   cluster "audit-gone" 带 cluster.delete 审计行（audit 回填路径）
	//   cluster "regex-gone_deleted_1700000000" 无审计（regex 兜底路径）
	seed := `
INSERT INTO users(id, email, display_name, password_hash) VALUES
 ('11111111-1111-1111-1111-111111111111', 'owner@x.local',  'o', 'x'),
 ('22222222-2222-2222-2222-222222222222', 'viewer@x.local', 'v', 'x'),
 ('33333333-3333-3333-3333-333333333333', 'bare@x.local',   'b', 'x'),
 ('44444444-4444-4444-4444-444444444444', 'deleted@x.local','d', 'x');
INSERT INTO clusters(id, name, owner_id) VALUES
 ('aaaaaaaa-0000-0000-0000-000000000001', 'live',    '11111111-1111-1111-1111-111111111111'),
 ('aaaaaaaa-0000-0000-0000-000000000002', 'audit-gone', '11111111-1111-1111-1111-111111111111'),
 ('aaaaaaaa-0000-0000-0000-000000000003', 'regex-gone_deleted_1700000000', '44444444-4444-4444-4444-444444444444');
INSERT INTO cluster_members(cluster_id, user_id, role) VALUES
 ('aaaaaaaa-0000-0000-0000-000000000001', '11111111-1111-1111-1111-111111111111', 'owner'),
 ('aaaaaaaa-0000-0000-0000-000000000001', '22222222-2222-2222-2222-222222222222', 'viewer'),
 ('aaaaaaaa-0000-0000-0000-000000000003', '44444444-4444-4444-4444-444444444444', 'owner');
INSERT INTO audit_logs(user_id, cluster_id, action, metadata)
VALUES ('11111111-1111-1111-1111-111111111111',
        'aaaaaaaa-0000-0000-0000-000000000002', 'cluster.delete', '{}');
`
	_, err := pool.Exec(ctx, seed)
	require.NoError(t, err)

	require.NoError(t, Migrate(ctx, pool))

	one := func(q string, args ...any) string {
		var v string
		require.NoError(t, pool.QueryRow(ctx, q, args...).Scan(&v))
		return v
	}

	// admin 快照：owner/deleted 为 true，viewer/bare 为 false。
	assert.Equal(t, "true", one(`SELECT is_admin::text FROM users WHERE email='owner@x.local'`))
	assert.Equal(t, "false", one(`SELECT is_admin::text FROM users WHERE email='viewer@x.local'`))
	assert.Equal(t, "false", one(`SELECT is_admin::text FROM users WHERE email='bare@x.local'`))
	assert.Equal(t, "true", one(`SELECT is_admin::text FROM users WHERE email='deleted@x.local'`))

	// 软删除回填：audit 行与 regex 兜底各打一个标（两条路径各覆盖一个）。
	assert.Equal(t, "2", one(`SELECT count(*)::text FROM clusters WHERE deleted_at IS NOT NULL`))
	assert.NotNil(t, one(`SELECT deleted_at::text FROM clusters WHERE name='audit-gone'`))
	assert.Equal(t, "1", one(
		`SELECT count(*)::text FROM clusters WHERE name='regex-gone_deleted_1700000000' AND deleted_at IS NOT NULL`))

	// 默认 cluster 回填：bare 与 deleted 各得一个 personal owner cluster；
	// owner/viewer 已有存活 cluster 不回填。回填的 email 本地部分清洗正确。
	assert.Equal(t, "2", one(`
SELECT count(*)::text FROM clusters WHERE personal`))
	assert.Equal(t, "1", one(`
SELECT count(*)::text FROM clusters c
JOIN cluster_members m ON m.cluster_id=c.id AND m.user_id='33333333-3333-3333-3333-333333333333' AND m.role='owner'
WHERE c.personal AND c.name='bare-default' AND c.deleted_at IS NULL`))

	// name 部分唯一索引：已删 cluster 的名字可被新建复用。
	_, err = pool.Exec(ctx,
		`INSERT INTO clusters(id, name, owner_id) VALUES
		 ('cccccccc-0000-0000-0000-000000000001', 'audit-gone',
		  '11111111-1111-1111-1111-111111111111')`)
	require.NoError(t, err)

	// 同名存活冲突仍被拦（部分索引只覆盖存活行）。
	_, err = pool.Exec(ctx,
		`INSERT INTO clusters(id, name, owner_id) VALUES
		 ('cccccccc-0000-0000-0000-000000000002', 'live',
		  '11111111-1111-1111-1111-111111111111')`)
	require.Error(t, err)

	// machine_id 全局唯一：跨 cluster 重复插入被拒。
	_, err = pool.Exec(ctx, `
INSERT INTO nodes(id, cluster_id, name, machine_id, hostname, os_version,
                  agent_version, public_key) VALUES
 ('dddddddd-0000-0000-0000-000000000001', 'aaaaaaaa-0000-0000-0000-000000000001',
  'n1', 'MID-1', 'h1', 'w', '1', 'k1'),
 ('dddddddd-0000-0000-0000-000000000002', 'aaaaaaaa-0000-0000-0000-000000000002',
  'n2', 'MID-1', 'h2', 'w', '1', 'k2')`)
	require.Error(t, err) // 第二行撞全局唯一（且 audit-gone 已删——换 live 集群再验）
	_, err = pool.Exec(ctx, `
INSERT INTO nodes(id, cluster_id, name, machine_id, hostname, os_version,
                  agent_version, public_key) VALUES
 ('dddddddd-0000-0000-0000-000000000003', 'aaaaaaaa-0000-0000-0000-000000000001',
  'n3', 'MID-2', 'h3', 'w', '1', 'k3')`)
	require.NoError(t, err)
}

func TestMigrate0005RejectsDuplicateMachine(t *testing.T) {
	t.Parallel()
	st := openStoreAt0004(t)
	ctx := t.Context()
	pool := st.Pool()

	// 两 cluster 各挂同 machineId 的节点——0005 必须中止（人工清理出口）。
	_, err := pool.Exec(ctx, `
INSERT INTO users(id, email, display_name, password_hash) VALUES
 ('11111111-1111-1111-1111-111111111111', 'o@x.local', 'o', 'x');
INSERT INTO clusters(id, name, owner_id) VALUES
 ('aaaaaaaa-0000-0000-0000-000000000001', 'c1', '11111111-1111-1111-1111-111111111111'),
 ('aaaaaaaa-0000-0000-0000-000000000002', 'c2', '11111111-1111-1111-1111-111111111111');
INSERT INTO nodes(id, cluster_id, name, machine_id, hostname, os_version,
                  agent_version, public_key) VALUES
 ('dddddddd-0000-0000-0000-000000000001', 'aaaaaaaa-0000-0000-0000-000000000001',
  'n1', 'MID-DUP', 'h1', 'w', '1', 'k1'),
 ('dddddddd-0000-0000-0000-000000000002', 'aaaaaaaa-0000-0000-0000-000000000002',
  'n2', 'MID-DUP', 'h2', 'w', '1', 'k2')`)
	require.NoError(t, err)

	err = Migrate(ctx, pool)
	require.Error(t, err)

	// 失败整体回滚：0005 未记录、is_admin 列未加。
	var applied bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=5)`).Scan(&applied))
	assert.False(t, applied)
	var hasCol bool
	require.NoError(t, pool.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM information_schema.columns
 WHERE table_name='users' AND column_name='is_admin')`).Scan(&hasCol))
	assert.False(t, hasCol)
}
