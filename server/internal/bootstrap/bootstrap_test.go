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
