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

// TestDesktopICEPolicyEnv（M4 Task 7）：XNC_DESKTOP_ICE_POLICY 缺省 =
// "relay"（向后兼容：不配置与旧版 relay-only 行为一致）；"all" = 放开 LAN
// 直连；未知值 fail closed 回 "relay"。
func TestDesktopICEPolicyEnv(t *testing.T) {
	setRequiredEnv(t)

	// 缺省：未配置 → relay（不配置 = 行为不变）。
	c, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "relay", c.DesktopICEPolicy)

	// 显式 all → 放开直连。
	t.Setenv("XNC_DESKTOP_ICE_POLICY", "all")
	c, err = Load()
	require.NoError(t, err)
	assert.Equal(t, "all", c.DesktopICEPolicy)

	// 未知值（拼错/恶意）→ fail closed 回 relay。
	t.Setenv("XNC_DESKTOP_ICE_POLICY", "ALL")
	c, err = Load()
	require.NoError(t, err)
	assert.Equal(t, "relay", c.DesktopICEPolicy)

	t.Setenv("XNC_DESKTOP_ICE_POLICY", "direct")
	c, err = Load()
	require.NoError(t, err)
	assert.Equal(t, "relay", c.DesktopICEPolicy)
}
