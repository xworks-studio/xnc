package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustB64Decode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	require.NoError(t, err)
	return b
}

func TestKeySaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "identity.json")

	k := Generate()
	require.Empty(t, k.NodeID)
	k.NodeID = "11111111-1111-1111-1111-111111111111"
	require.NoError(t, Save(k, p))

	got, err := Load(p)
	require.NoError(t, err)
	assert.Equal(t, k.NodeID, got.NodeID)
	assert.Equal(t, k.PublicKeyB64(), got.PublicKeyB64())

	// 签名可验
	pubBytes := mustB64Decode(t, got.PublicKeyB64())
	assert.True(t, ed25519.Verify(ed25519.PublicKey(pubBytes), []byte("hi"),
		got.Sign([]byte("hi"))))
}
