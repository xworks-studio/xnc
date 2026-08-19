// Agent 编排节点生命周期：确保已注册（身份加载/生成/enroll），然后维持控制连接。
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"xnc/agent/connect"
	"xnc/agent/enroll"
	"xnc/agent/identity"
	"xnc/agent/machineinfo"
)

type Agent struct {
	ServerURL string
	Token     string
	StateDir  string
}

func (a *Agent) identityPath() string { return filepath.Join(a.StateDir, "identity.json") }

// EnsureEnrolled 加载既有身份；不存在则生成密钥并用 Token 注册。
func (a *Agent) EnsureEnrolled(ctx context.Context) (*identity.Key, machineinfo.Info, error) {
	info := machineinfo.Collect()
	k, err := identity.Load(a.identityPath())
	if err == nil && k.NodeID != "" {
		return k, info, nil
	}
	if a.Token == "" {
		return nil, info, fmt.Errorf("no identity and no enrollment token")
	}
	k = identity.Generate()
	if err := enroll.Enroll(ctx, a.ServerURL, a.Token, k, info); err != nil {
		return nil, info, err
	}
	if err := identity.Save(k, a.identityPath()); err != nil {
		return nil, info, err
	}
	slog.Info("enrolled", "node", k.NodeID)
	return k, info, nil
}

func (a *Agent) Run(ctx context.Context) error {
	k, info, err := a.EnsureEnrolled(ctx)
	if err != nil {
		return err
	}
	return connect.NewClient(a.ServerURL, k, info).Run(ctx)
}
