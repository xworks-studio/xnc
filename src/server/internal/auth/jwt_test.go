package auth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenRoundtrip(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	tok, err := MakeToken(secret, "user-1", time.Hour)
	require.NoError(t, err)
	uid, err := ParseToken(secret, tok)
	require.NoError(t, err)
	assert.Equal(t, "user-1", uid)

	_, err = ParseToken([]byte("another-secret-another-secret-xx"), tok)
	assert.Error(t, err)

	expired, err := MakeToken(secret, "user-1", -time.Minute)
	require.NoError(t, err)
	_, err = ParseToken(secret, expired)
	assert.Error(t, err)
}
