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
