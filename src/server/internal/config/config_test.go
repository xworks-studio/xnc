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

// TestRTVEnv（RTV 重构）：XNC_RTV_ENDPOINT 缺省空；配置后透传；
// 腿地址缺省 :4433/:443。
func TestRTVEnv(t *testing.T) {
	setRequiredEnv(t)
	c, err := Load()
	require.NoError(t, err)
	assert.Empty(t, c.RTVStreamEndpoint)
	assert.Equal(t, ":4433", c.RTVHostAddr)
	assert.Equal(t, ":443", c.RTVWTAddr)

	t.Setenv("XNC_RTV_ENDPOINT", "xnc.app:4433")
	t.Setenv("XNC_RTV_HOST_ADDR", ":9443")
	c, err = Load()
	require.NoError(t, err)
	assert.Equal(t, "xnc.app:4433", c.RTVStreamEndpoint)
	assert.Equal(t, ":9443", c.RTVHostAddr)
}
