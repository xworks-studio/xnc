package registry

// Registry 维护 NodeID → 活跃控制连接。Task 9 完成完整实现。
type Registry struct{}

func New() *Registry { return &Registry{} }
