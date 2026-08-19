package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"xnc/server/internal/config"
)

func startPG(t *testing.T) *postgres.PostgresContainer {
	t.Helper()
	pg, err := postgres.Run(t.Context(), "postgres:16-alpine",
		postgres.WithDatabase("xnc"),
		postgres.WithUsername("xnc"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp").
				WithStartupTimeout(60*time.Second)),
	)
	require.NoError(t, err)
	return pg
}

func TestMigrateIdempotent(t *testing.T) {
	t.Parallel()
	pg := startPG(t)
	u, err := pg.ConnectionString(t.Context(), "sslmode=disable")
	require.NoError(t, err)

	st, err := OpenStore(t.Context(), config.Config{DatabaseURL: u})
	require.NoError(t, err)
	defer st.Pool().Close()

	// 二次迁移不报错（幂等）。
	require.NoError(t, Migrate(t.Context(), st.Pool()))

	var n int
	require.NoError(t, st.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_name IN ('users','clusters','cluster_members','nodes',
		 'enrollment_tokens','audit_logs','schema_migrations')`).Scan(&n))
	assert.Equal(t, 7, n)
}
