package registry

import (
	"context"
	"sync"
	"time"
)

// NodeConn 记录一条活跃控制连接；Cancel 用于顶替同节点旧连接，Beats 统计心跳次数。
type NodeConn struct {
	NodeID   string
	Cancel   context.CancelFunc
	LastBeat time.Time
	Beats    int64
}

type Registry struct {
	mu    sync.RWMutex
	conns map[string]*NodeConn
}

func New() *Registry { return &Registry{conns: map[string]*NodeConn{}} }

func (r *Registry) Add(c *NodeConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.conns[c.NodeID]; ok && old.Cancel != nil {
		old.Cancel()
	}
	r.conns[c.NodeID] = c
}

func (r *Registry) Remove(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.conns, nodeID)
}

// RemoveIf 仅当注册表中该节点的当前连接就是 c（指针相等）时才删除，返回是否真正移除。
// 被顶替的旧连接调用时返回 false：在线状态已由新连接接管，旧连接的清理不得误删新表项。
func (r *Registry) RemoveIf(nodeID string, c *NodeConn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.conns[nodeID]; ok && cur == c {
		delete(r.conns, nodeID)
		return true
	}
	return false
}

func (r *Registry) Online(nodeID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.conns[nodeID]
	return ok
}

func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.conns)
}
