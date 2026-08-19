package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
