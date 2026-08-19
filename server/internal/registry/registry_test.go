package registry

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRegistryReplace(t *testing.T) {
	r := New()
	c1ctx, c1cancel := context.WithCancel(context.Background())
	c1 := &NodeConn{NodeID: "n1", Cancel: c1cancel, LastBeat: time.Now()}
	r.Add(c1)
	assert.True(t, r.Online("n1"))
	assert.Equal(t, 1, r.Count())

	// 同节点新连接替换旧连接：旧 ctx 被 cancel
	c2ctx, c2cancel := context.WithCancel(context.Background())
	c2 := &NodeConn{NodeID: "n1", Cancel: c2cancel, LastBeat: time.Now()}
	r.Add(c2)
	assert.Equal(t, 1, r.Count())
	select {
	case <-c1ctx.Done():
	default:
		t.Fatal("old conn should be cancelled")
	}
	_ = c2ctx

	r.Remove("n1")
	assert.False(t, r.Online("n1"))
	assert.Equal(t, 0, r.Count())

	// RemoveIf（identity-aware）：被顶替的旧连接的清理请求必须被拒绝，
	// 不得误删同节点新连接；当前连接的 RemoveIf 才真正移除。
	r.Add(c2)
	assert.False(t, r.RemoveIf("n1", c1), "replaced conn must not remove the new one")
	assert.True(t, r.Online("n1"))
	assert.Equal(t, 1, r.Count())
	assert.True(t, r.RemoveIf("n1", c2))
	assert.False(t, r.Online("n1"))
	assert.Equal(t, 0, r.Count())
}
