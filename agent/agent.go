// Agent 编排节点生命周期：确保已注册（身份加载/生成/enroll），然后维持控制连接。
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"xnc/agent/agentctl"
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
	// CtlPipeName 控制管道名（spec §6.3；空 = agentctl.PipeName，测试覆盖）。
	CtlPipeName string

	// rebind 唤醒通道（缓冲 1，非阻塞通知）：register 写入 binding / deregister
	// 拆线时唤醒空转轮询或打断既有连接周期，使注册即时生效（<1s，而非等
	// 5s 轮询）。懒初始化（Agent 以零值结构体构造），rebindMu 守护。
	rebind   chan struct{}
	rebindMu sync.Mutex

	connMu sync.Mutex
	conn   *connect.Client // 当前连接周期的控制客户端（status online 判据）
}

// rebindCh 返回（必要时创建）唤醒通道。
func (a *Agent) rebindCh() chan struct{} {
	a.rebindMu.Lock()
	defer a.rebindMu.Unlock()
	if a.rebind == nil {
		a.rebind = make(chan struct{}, 1)
	}
	return a.rebind
}

// notifyRebind 非阻塞通知：无消费者时信号留驻缓冲，下一个进入等待的周期
// 立即消费（空唤醒无害——重查 binding 后继续原状态）。
func (a *Agent) notifyRebind() {
	select {
	case a.rebindCh() <- struct{}{}:
	default:
	}
}

func (a *Agent) setConn(c *connect.Client) {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	a.conn = c
}

func (a *Agent) currentConn() *connect.Client {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	return a.conn
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
// 记 awaiting registration。控制管道注册（Task 4）经 rebind 唤醒即时接续
// （<1s，5s 轮询仅兜底）。ctx 取消（服务停止/Ctrl+C）即返回 ctx.Err()。
func (a *Agent) idleAwaitRegistration(ctx context.Context) (*binding.Binding, error) {
	slog.Info("awaiting registration", "stateDir", a.StateDir)
	t := time.NewTicker(awaitBindingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-a.rebindCh():
			if b, ok := a.currentBinding(); ok {
				slog.Info("binding detected (woken); proceeding", "server", b.Server, "node", b.NodeID)
				return b, nil
			}
			// 空唤醒（如 deregister 后的残留信号）：继续空转。
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
	// 控制管道（spec §6.3）：空转期等待注册指令 / 运行期处理 deregister 与
	// status。失败仅记日志（agent 主功能不因此阻断）。
	a.startCtlPipe(ctx)
	for {
		// 绑定是外联与自更新的前提：空转期间不触碰 updater/connect。
		b, err := a.awaitBinding(ctx)
		if err != nil {
			return err
		}
		if err := a.runConnected(ctx, b); err != nil {
			return err
		}
		// runConnected 返回 nil = rebind 打断（register 覆盖绑定 / deregister
		// 拆线）→ 回到循环头重新评估绑定（新绑定 → 新周期；无绑定 → 空转）。
	}
}

// startCtlPipe 启动 agentctl 控制管道服务端（随 ctx 关闭）。
func (a *Agent) startCtlPipe(ctx context.Context) {
	s := &agentctl.Server{Deps: a, Log: slog.Default(), Name: a.CtlPipeName}
	go func() {
		if err := s.Listen(ctx); err != nil && ctx.Err() == nil {
			slog.Error("agentctl pipe server exited", "err", err)
		}
	}()
}

// runConnected 一个连接周期：EnsureEnrolled → updater/connect 装配 →
// c.Run 维持控制连接。rebind 信号打断周期（返回 nil，外层重评估绑定）；
// 外层 ctx 取消按停机向上传递。
func (a *Agent) runConnected(ctx context.Context, b *binding.Binding) error {
	cycleCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-a.rebindCh():
			cancel() // register/deregister：打断本周期
		case <-cycleCtx.Done():
		}
	}()
	defer func() { cancel(); <-watcherDone }()

	// server 以机器级绑定为准（spec §5.2/§5.3 同一原则）：--server 仅在
	// 绑定未提供时兜底；两者都有且不一致时以绑定为准并记警告。
	server := a.ServerURL
	if b.Server != "" {
		if a.ServerURL != "" && a.ServerURL != b.Server {
			slog.Warn("server mismatch: binding.json wins", "flag", a.ServerURL, "binding", b.Server)
		}
		server = b.Server
	}

	k, info, err := a.EnsureEnrolled(cycleCtx)
	if err != nil {
		if cycleCtx.Err() != nil && ctx.Err() == nil {
			return nil // rebind 打断（新周期重走）
		}
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
	a.setConn(c)
	defer a.setConn(nil)
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
	if err := c.Run(cycleCtx); err != nil && ctx.Err() == nil && cycleCtx.Err() == nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err() // 服务停止
	}
	return nil // rebind 打断（cycleCtx 取消）→ 外层重评估
}

// ---- agentctl 控制管道操作（spec §6.2/§6.3/§7；agentctl.Deps 实现）----

// loadOrCreateIdentity 加载本机身份，不存在则生成（NodeID 空，注册成功后
// 回写）。身份生成必须在 agent 进程内（SYSTEM 上下文的 DPAPI 保护；CLI 永不
// 持有私钥）。损坏/不可解密的 identity 直接报错——静默重建会孤儿化既有注册。
func (a *Agent) loadOrCreateIdentity() (*identity.Key, bool, error) {
	k, err := identity.Load(a.identityPath())
	if err == nil {
		return k, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, err
	}
	return identity.Generate(), true, nil
}

// registerNode 调 Task 2 端点（spec §6.4）：
// POST {server}/api/clusters/{id}/nodes/register，Authorization: 用户 JWT
// （仅内存经手：进请求头、出函数体即弃，不落盘不写日志）；body 为 enroll 减
// token（hostname/machineId/osVersion/agentVersion/publicKey）。成功返回
// nodeId；失败返回 proto 错误码形态 "<code>: <message>"（管道应答直通）。
func registerNode(ctx context.Context, server, clusterID, jwt, publicKeyB64 string, info machineinfo.Info) (string, error) {
	body, err := json.Marshal(map[string]string{
		"hostname": info.Hostname, "machineId": info.MachineID,
		"osVersion": info.OSVersion, "agentVersion": info.AgentVersion,
		"publicKey": publicKeyB64,
	})
	if err != nil {
		return "", fmt.Errorf("internal: %v", err)
	}
	u := strings.TrimRight(server, "/") + "/api/clusters/" + url.PathEscape(clusterID) + "/nodes/register"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("internal: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("internal: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		var out struct {
			NodeID string `json:"nodeId"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", fmt.Errorf("internal: %v", err)
		}
		if out.NodeID == "" {
			return "", fmt.Errorf("internal: missing nodeId in response")
		}
		return out.NodeID, nil
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&e)
	if e.Error.Code != "" {
		return "", fmt.Errorf("%s: %s", e.Error.Code, e.Error.Message)
	}
	return "", fmt.Errorf("internal: status %d", resp.StatusCode)
}

// Register 实现 agentctl.Deps（spec §6.2 第 3 步）：load-or-create 身份 →
// 用户 JWT 调注册端点（幂等：同 cluster+machineId+公钥由服务端复用 201）→
// NodeID 回写 identity + 原子写 binding → rebind 唤醒连接路径（<1s 上线）。
func (a *Agent) Register(ctx context.Context, server, clusterID, jwt string) (string, error) {
	k, created, err := a.loadOrCreateIdentity()
	if err != nil {
		return "", fmt.Errorf("internal: load identity: %v", err)
	}
	if created {
		// 生成即落盘（公钥发出前私钥必须已持久化——注册成功而身份丢失
		// 会产生无法认证的孤儿节点）。
		if err := identity.Save(k, a.identityPath()); err != nil {
			return "", fmt.Errorf("internal: save identity: %v", err)
		}
	}
	nodeID, err := registerNode(ctx, server, clusterID, jwt, k.PublicKeyB64(), a.info())
	if err != nil {
		return "", err
	}
	k.NodeID = nodeID
	if err := identity.Save(k, a.identityPath()); err != nil {
		return "", fmt.Errorf("internal: save identity: %v", err)
	}
	if err := binding.Save(a.StateDir, &binding.Binding{
		Server: server, ClusterID: clusterID, NodeID: nodeID,
		RegisteredAt: time.Now().UTC(),
	}); err != nil {
		return "", fmt.Errorf("internal: save binding: %v", err)
	}
	a.notifyRebind()
	slog.Info("registered via agentctl", "node", nodeID, "server", server, "cluster", clusterID)
	return nodeID, nil
}

// Deregister 实现 agentctl.Deps（spec §7）：一次性控制连接挑战认证（机器身份
// 即凭据，无 JWT）→ NODE_DELETE → rebind 拆既有连接周期 → 删 binding.json
// （保留 identity.json 供重注册复用）。服务端拒绝（ERROR 帧）时不删 binding，
// 错误以 "<code>: <message>" 直通。
func (a *Agent) Deregister(ctx context.Context) error {
	b, ok := a.currentBinding()
	if !ok {
		return errors.New("not_registered: no binding")
	}
	k, err := identity.Load(a.identityPath())
	if err != nil {
		return fmt.Errorf("internal: load identity: %v", err)
	}
	if k.NodeID == "" {
		return errors.New("internal: identity has no nodeId; cannot authenticate deregister")
	}
	c := connect.NewClient(b.Server, k, a.info())
	if err := c.DeleteNode(ctx); err != nil {
		return err
	}
	// 注销成功：先删 binding 再 rebind——若先发信号，run 循环可能抢在删除
	// 落盘前消费它、重读到仍在的 binding，为已注销节点进入无信号的连接周期
	// （无限退避重试）。顺序保证：周期重评估时 binding 必已消失 → 回空转。
	if err := os.Remove(binding.Path(a.StateDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("internal: remove binding: %v", err)
	}
	a.notifyRebind()
	slog.Info("deregistered via agentctl", "node", b.NodeID, "server", b.Server)
	return nil
}

// Status 实现 agentctl.Deps（spec §6.3 status op；xnc status 本机数据源）：
// 无有效绑定 = unregistered；有绑定 = registered；控制连接就绪 = online。
func (a *Agent) Status() agentctl.Status {
	st := agentctl.Status{State: agentctl.StateUnregistered, Version: machineinfo.Version}
	b, ok := a.currentBinding()
	if !ok {
		return st
	}
	st.State = agentctl.StateRegistered
	st.NodeID, st.Server, st.ClusterID = b.NodeID, b.Server, b.ClusterID
	if c := a.currentConn(); c != nil && c.Connected() {
		st.State = agentctl.StateOnline
	}
	return st
}
