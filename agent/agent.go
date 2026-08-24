// Agent 编排节点生命周期：确保已注册（身份加载/生成/enroll），然后维持控制连接。
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"xnc/agent/connect"
	"xnc/agent/desktop"
	"xnc/agent/enroll"
	"xnc/agent/identity"
	"xnc/agent/machineinfo"
	"xnc/agent/session"
	"xnc/agent/updater"
	"xnc/proto"
)

type Agent struct {
	ServerURL string
	Token     string
	StateDir  string
	// HostnameOverride / MachineIDOverride：仅 run-dev-console 使用的一次性
	// 身份覆盖；空值 = 生产行为完全不变。
	HostnameOverride  string
	MachineIDOverride string
}

func (a *Agent) identityPath() string { return filepath.Join(a.StateDir, "identity.json") }

// info 采集机器信息并应用 dev 覆盖（空覆盖 = 原样返回，生产行为）。
func (a *Agent) info() machineinfo.Info {
	i := machineinfo.Collect()
	if a.HostnameOverride != "" {
		i.Hostname = a.HostnameOverride
	}
	if a.MachineIDOverride != "" {
		i.MachineID = a.MachineIDOverride
	}
	return i
}

// EnsureEnrolled 加载既有身份；不存在则生成密钥并用 Token 注册。
func (a *Agent) EnsureEnrolled(ctx context.Context) (*identity.Key, machineinfo.Info, error) {
	info := a.info()
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

	// 自更新：清扫上次失败的 staging 残留；三入口（握手回执/心跳搭车/
	// 强制 OFFER）收敛到同一 Updater（幂等去重）。
	updater.CleanupStale(a.StateDir)
	upd := &updater.Updater{
		ServerURL: a.ServerURL, StateDir: a.StateDir,
		Version: info.AgentVersion, Log: slog.Default(),
	}

	c := connect.NewClient(a.ServerURL, k, info)
	// 每次连接就绪（含重连）重建 engine：旧 engine 的 sendControl 绑定旧连接，
	// 其 active 会话已随断连作废，重建即正确语义。
	c.OnReady = func(sendControl func(m proto.Message) error) {
		engine := session.NewEngine(slog.Default(), sendControl)
		// exec/shell:凭据与 desktop 同源(env → XNCCore 服务缺省)。
		shellHost := session.ShellHostFromStateDir(a.StateDir, slog.Default())
		engine.Register(proto.KindExec, &session.Exec{Log: slog.Default(), Host: shellHost})
		engine.Register(proto.KindShell, &session.Shell{Log: slog.Default(), Host: shellHost})
		engine.Register(proto.KindFile, session.NewFile(slog.Default()))
		engine.Register(proto.KindTunnel, session.NewTunnel(slog.Default()))
		engine.Register(proto.KindScreen, session.NewScreenHandler())
		// desktop:凭据 = env(dev)→ 生产缺省(XNCCore 服务的
		// \\.\pipe\xnc-core + <StateDir>\core-secret.hex)。缺省零行为
		// 变化(refused 路径不变)。
		if dh := desktop.NewHandler(a.StateDir, slog.Default()); dh != nil {
			engine.Register(proto.KindDesktop, dh)
		}
		c.Handler = engine

		// 控制连接就绪 = 新版存活的证明：写 apply-update 子进程等的
		// connected 标记（apply 验证/回滚的判据），并上报 UPDATE_STATUS
		// done（若本进程是更新产物）。
		_ = os.WriteFile(updater.ConnectedMarkerPath(a.StateDir), []byte("ok"), 0o644)

		// 目标版本信号（快速检查①②）需要 send 上报 STATUS——每条连接
		// 重新注入。
		upd.Report = sendControl
	}
	// 快速版本检查：targetVersion ≠ 当前版本时向 server 主动询问式触发
	// 依赖 server 的 OFFER（服务端在 ACK 后自查）——agent 侧把 target
	// 视作 offer 的本地等价信号（URL 由 server 在 OFFER 给出；此路径
	// 仅当日标版本信号先于 OFFER 到达时用于日志感知，不自行构造 URL）。
	c.TargetVersionFunc = func(target string) {
		slog.Info("update: target version signal", "target", target, "current", info.AgentVersion)
	}
	c.UpdateOfferFunc = func(ctx context.Context, offer proto.UpdateOffer) {
		upd.Handle(ctx, offer, c.CurrentSend())
	}
	return c.Run(ctx)
}
