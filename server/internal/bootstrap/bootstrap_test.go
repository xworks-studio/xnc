package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/server/internal/config"
	"xnc/server/internal/db"
)

func TestEnsureAdminIdempotent(t *testing.T) {
	st := db.OpenTestStore(t) // 见 Step 3 测试助手
	cfg := config.Config{AdminEmail: "admin@t.local", AdminPassword: "pw-123456"}

	require.NoError(t, EnsureAdmin(t.Context(), st, cfg))
	require.NoError(t, EnsureAdmin(t.Context(), st, cfg)) // 幂等

	n, err := st.Q().CountUsers(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	u, err := st.Q().GetUserByEmail(t.Context(), "admin@t.local")
	require.NoError(t, err)
	assert.NotEmpty(t, u.PasswordHash)
	assert.NotEqual(t, "pw-123456", u.PasswordHash)

	var role string
	require.NoError(t, st.Pool().QueryRow(t.Context(),
		`SELECT role FROM cluster_members cm JOIN clusters c ON c.id=cm.cluster_id
		 WHERE c.name='default' AND cm.user_id=$1`, u.ID).Scan(&role))
	assert.Equal(t, "owner", role)
}

// TestEnsureAdminPartialFailureRecovery 验证 bootstrap 三条写入同事务：
// 中间语句失败时整体回滚（users 不留半成品），故障恢复后重试可完整创建。
func TestEnsureAdminPartialFailureRecovery(t *testing.T) {
	st := db.OpenTestStore(t)
	cfg := config.Config{AdminEmail: "admin@t.local", AdminPassword: "pw-123456"}

	// 模拟中间语句失败：临时重命名 clusters 表，使 clusters INSERT 报错。
	_, err := st.Pool().Exec(t.Context(), `ALTER TABLE clusters RENAME TO clusters_bak`)
	require.NoError(t, err)

	err = EnsureAdmin(t.Context(), st, cfg)
	require.Error(t, err)

	// 恢复表名（重命名是可逆的元数据操作，FK 依赖自动跟随）。
	_, err = st.Pool().Exec(t.Context(), `ALTER TABLE clusters_bak RENAME TO clusters`)
	require.NoError(t, err)

	// 失败后不得留下半成品：users 必须为空（事务回滚，而非仅返回错误）。
	n, err := st.Q().CountUsers(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	// 故障恢复后重试：应完整创建 admin + default cluster + owner membership。
	require.NoError(t, EnsureAdmin(t.Context(), st, cfg))

	n, err = st.Q().CountUsers(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	u, err := st.Q().GetUserByEmail(t.Context(), "admin@t.local")
	require.NoError(t, err)
	var role string
	require.NoError(t, st.Pool().QueryRow(t.Context(),
		`SELECT role FROM cluster_members cm JOIN clusters c ON c.id=cm.cluster_id
		 WHERE c.name='default' AND cm.user_id=$1`, u.ID).Scan(&role))
	assert.Equal(t, "owner", role)
}
