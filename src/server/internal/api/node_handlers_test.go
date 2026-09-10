package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNodeListFiltersAndAuth(t *testing.T) {
	env := NewTestEnv(t)
	env.EnrollNode(t, "WEB-A", "mid-a")
	env.EnrollNode(t, "WEB-B", "mid-b")

	// 未认证 401
	nodes, status := env.ListNodesStatus(t, "")
	assert.Equal(t, 401, status)
	assert.Nil(t, nodes)

	nodes = env.ListNodes(t)
	require.Len(t, nodes, 2)
	assert.Equal(t, "offline", nodes[0]["status"]) // registry 无连接
}
