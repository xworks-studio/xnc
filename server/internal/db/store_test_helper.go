package db

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"xnc/server/internal/config"
)

// OpenTestStore 为测试启动一次性 PostgreSQL 容器并完成迁移。
// 说明：该助手需被 bootstrap/api 等其他包的测试引用，Go 的 _test.go 无法跨包导入，
// 因此放在普通文件中（仅测试代码可达，生产路径不应调用）。
func OpenTestStore(t *testing.T) *Store {
	t.Helper()
	pg, err := postgres.Run(t.Context(), "postgres:16-alpine",
		postgres.WithDatabase("xnc"), postgres.WithUsername("xnc"), postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second)),
	)
	require.NoError(t, err)
	// 注意：t.Context() 在 Cleanup 运行前已被取消，Terminate 需用独立 context。
	t.Cleanup(func() { _ = pg.Terminate(context.Background()) })
	u, err := pg.ConnectionString(t.Context(), "sslmode=disable")
	require.NoError(t, err)
	st, err := OpenStore(t.Context(), config.Config{DatabaseURL: u})
	require.NoError(t, err)
	t.Cleanup(st.Close)
	return st
}
