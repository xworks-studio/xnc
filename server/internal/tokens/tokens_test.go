package tokens

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerate(t *testing.T) {
	pt, hash, err := Generate()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(pt, "xnc_enroll_"))
	assert.Len(t, pt, len("xnc_enroll_")+43)

	sum := sha256.Sum256([]byte(pt))
	assert.Equal(t, hex.EncodeToString(sum[:]), hash)

	pt2, _, _ := Generate()
	assert.NotEqual(t, pt, pt2)
}
