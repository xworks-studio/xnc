package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("XNC_DATABASE_URL", "postgres://xnc@localhost:5432/xnc")
	t.Setenv("XNC_JWT_SECRET", "0123456789abcdef0123456789abcdef")
}

// TestTurnPoolEnv：XNC_TURN_POOL 逗号分隔 ip[:port]，复用 envList 语义
// （去空白/跳空项）；未配置 → nil。
func TestTurnPoolEnv(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("XNC_TURN_POOL", "1.2.3.4, 5.6.7.8:3479 ,,9.9.9.9")
	c, err := Load()
	require.NoError(t, err)
	assert.Equal(t, []string{"1.2.3.4", "5.6.7.8:3479", "9.9.9.9"}, c.TurnPool)

	t.Setenv("XNC_TURN_POOL", "")
	c, err = Load()
	require.NoError(t, err)
	assert.Nil(t, c.TurnPool)
}
