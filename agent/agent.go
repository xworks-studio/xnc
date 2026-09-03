// Agent 编排节点生命周期：确保已注册（身份加载/生成/enroll），然后维持控制连接。
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"xnc/agent/binding"
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

// awaitBindingInterval 未注册空转的重查周期（spec §6.1 为 5s；测试缩短）。
var awaitBindingInterval = 5 * time.Second

// currentBinding 读取有效绑定（Server 非空才算数——空 server 无法外联）。
// 损坏文件记警告并视同未绑定（空转等待覆盖修复，不崩溃）。
func (a *Agent) currentBinding() (*binding.Binding, bool) {
	b, ok, err := binding.Load(a.StateDir)
	if err != nil {
		slog.Warn("binding.json unreadable", "path", binding.Path(a.StateDir), "err", err)
		return nil, false
	}
	if !ok || b.Server == "" {
		return nil, false
	}
	return b, true
}

// awaitBinding 取得机器级绑定后才允许外联（spec §6.1）。顺序：
//  1. 已有 binding.json → 直接用；
//  2. --token 流（dev console / 一次性 token 安装）→ 现 enroll 路径，
//     成功后固化 binding（重启不再依赖 token）；
//  3. 保守合成：遗留已注册身份（identity.json 带 NodeID）+ 显式 --server
//     才合成，绝不猜 server；clusterId/注册时间未知，Task 7 迁移补全；
//  4. 都不满足 → 未注册空转：5s 周期重查 binding.json 并记 awaiting
//     registration 日志，外部写入（Task 4 控制管道注册）即接续，无需重启。
func (a *Agent) awaitBinding(ctx context.Context) (*binding.Binding, error) {
	if b, ok := a.currentBinding(); ok {
		return b, nil
	}
	if a.Token != "" {
		return a.enrollAndBind(ctx)
	}
	if b, ok := a.synthesizeLegacyBinding(); ok {
		return b, nil
	}
	return a.idleAwaitRegistration(ctx)
}

// enrollAndBind 遗留 --token 注册：EnsureEnrolled（已有身份则不外呼），
// 成功即固化 binding.json。RegisteredAt 取绑定写入时刻（遗留身份的原始
// 注册时间不可考，为近似值）。
func (a *Agent) enrollAndBind(ctx context.Context) (*binding.Binding, error) {
	k, _, err := a.EnsureEnrolled(ctx)
	if err != nil {
		return nil, err
	}
	b := &binding.Binding{Server: a.ServerURL, NodeID: k.NodeID, RegisteredAt: time.Now().UTC()}
	if err := binding.Save(a.StateDir, b); err != nil {
		// 固化失败不阻断本次运行（内存绑定已可用），重启后重走 token 路径。
		slog.Warn("binding.json save failed", "err", err)
	}
	return b, nil
}

// synthesizeLegacyBinding 从遗留配置合成绑定（仅显式 --server，保守策略）。
func (a *Agent) synthesizeLegacyBinding() (*binding.Binding, bool) {
	if a.ServerURL == "" {
		return nil, false
	}
	k, err := identity.Load(a.identityPath())
	if err != nil || k.NodeID == "" {
		return nil, false
	}
	b := &binding.Binding{Server: a.ServerURL, NodeID: k.NodeID}
	if err := binding.Save(a.StateDir, b); err != nil {
		slog.Warn("binding.json save failed", "err", err)
	}
	slog.Info("binding synthesized from legacy identity", "node", k.NodeID, "server", a.ServerURL)
	return b, true
}

// idleAwaitRegistration 未注册空转态（spec §6.1）：不发起任何外联、不做
// 更新检查（更新源即 server，无绑定即无源）；仅周期重查 binding.json 并
// 记 awaiting registration。ctx 取消（服务停止/Ctrl+C）即返回 ctx.Err()。
func (a *Agent) idleAwaitRegistration(ctx context.Context) (*binding.Binding, error) {
	slog.Info("awaiting registration", "stateDir", a.StateDir)
	t := time.NewTicker(awaitBindingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
			if b, ok := a.currentBinding(); ok {
				slog.Info("binding detected; proceeding", "server", b.Server, "node", b.NodeID)
				return b, nil
			}
			slog.Info("awaiting registration")
		}
	}
}

func (a *Agent) Run(ctx context.Context) error {
	// 绑定是外联与自更新的前提：空转期间不触碰 updater/connect。
	b, err := a.awaitBinding(ctx)
	if err != nil {
		return err
	}
	// server 以机器级绑定为准（spec §5.2/§5.3 同一原则）：--server 仅在
	// 绑定未提供时兜底；两者都有且不一致时以绑定为准并记警告。
	server := a.ServerURL
	if b.Server != "" {
		if a.ServerURL != "" && a.ServerURL != b.Server {
			slog.Warn("server mismatch: binding.json wins", "flag", a.ServerURL, "binding", b.Server)
		}
		server = b.Server
	}

	k, info, err := a.EnsureEnrolled(ctx)
	if err != nil {
		return err
	}
	if b.NodeID != "" && k.NodeID != b.NodeID {
		// 身份与绑定不一致（手改/残留）：以身份为准（私钥不可换），仅告警。
		slog.Warn("binding/identity node mismatch", "binding", b.NodeID, "identity", k.NodeID)
	}

	// 自更新：清扫上次失败的 staging 残留；三入口（握手回执/心跳搭车/
	// 强制 OFFER）收敛到同一 Updater（幂等去重）。
	updater.CleanupStale(a.StateDir)
	upd := &updater.Updater{
		ServerURL: server, StateDir: a.StateDir,
		Version: info.AgentVersion, Log: slog.Default(),
	}

	c := connect.NewClient(server, k, info)
	// 每次连接就绪（含重连）重建 engine：旧 engine 的 sendControl 绑定旧连接，
	// 其 active 会话已随断连作废，重建即正确语义。
	c.OnReady = func(sendControl func(m proto.Message) error) {
		engine := session.NewEngine(slog.Default(), sendControl)
		// exec/shell:凭据与 desktop 同源(env → XNCCore 服务缺省)。
		shellHost := session.ShellHostFromStateDir(a.StateDir, slog.Default())
		engine.Register(proto.KindExec, &session.Exec{Log: slog.Default(), Host: shellHost, StateDir: a.StateDir})
		engine.Register(proto.KindShell, &session.Shell{Log: slog.Default(), Host: shellHost})
		engine.Register(proto.KindFile, session.NewFile(slog.Default()))
		engine.Register(proto.KindTunnel, session.NewTunnel(slog.Default()))
		engine.Register(proto.KindScreen, session.NewScreenHandler(a.StateDir))
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
