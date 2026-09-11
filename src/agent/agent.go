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
	"xnc/agent/display"
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

	// upgrading：手动触发（agentctl upgrade op）的后台 ForceCheck 进行中
	// 标记（触发 → goroutine 返回的窗口）；与 pending 文件共同构成 update
	// op 的在途判据（§9.3 串行化）。
	upgradeMu sync.Mutex
	upgrading bool

	// display：虚拟显示器管理器（懒初始化；display op / desktop 会话钩子
	// 共用；agent 退出时 Close 拆除设备——Handle 生命周期兜底）。
	displayMu sync.Mutex
	display   *display.Manager
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
	// 虚拟显示器管理器（懒初始化；Close 收线时拆除设备——Handle 生命周期
	// 兜底保证 agent 崩溃也不残留虚拟屏）。
	defer a.closeDisplay()
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

	// 自更新编排（spec §9，安装器即更新器）：清扫 bundle 期遗留；构造
	// 编排器（频道来自绑定）。启动巡检（过期 pending 兜底/版本一致性）
	// 与 6h 轮询随周期启动；WS 推送与目标版本信号经回调触发。
	updater.CleanupStale(a.StateDir)
	upd := updater.New(server, a.StateDir, info.AgentVersion, channelOf(b), slog.Default())
	go upd.StartupPendingCheck(cycleCtx)
	go func() {
		t := time.NewTicker(updater.PollInterval)
		defer t.Stop()
		for {
			select {
			case <-cycleCtx.Done():
				return
			case <-t.C:
				_ = upd.CheckNow(cycleCtx)
			}
		}
	}()

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
		// desktop(RTV thin):凭据 = env(dev)→ 生产缺省(XNCCore 服务的
		// \\.\pipe\xnc-core + <StateDir>\core-secret.hex)；nodeId 取注册
		// 身份(host 向 relay 注册用)。缺省零行为变化(refused 路径不变)。
		// display 管理器供会话钩子触发/移除虚拟显示器（IDD）。
		if dh := desktop.NewHandler(a.StateDir, k.NodeID, slog.Default(), a.displayMgr()); dh != nil {
			engine.Register(proto.KindDesktop, dh)
		}
		c.Handler = engine

		// 控制连接就绪（spec §9.4 自检的"WS 可达"判据）。先补报看门狗/
		// 离线回滚留下的延迟审计（一次性），再做更新自检收尾（仅
		// pending.to == 自报版本时有动作：成功删 pending+看门狗+写缓存+
		// audit update_ok；失败主动回滚）。
		upd.Report = sendControl
		if ev, ok := updater.ConsumeAuditMarker(a.StateDir); ok {
			slog.Warn("update: deferred rollback audit", "event", ev.Event,
				"from", ev.From, "to", ev.To, "reason", ev.Reason)
			if m, err := proto.NewMsg(proto.TypeUpdateAudit, ev); err == nil {
				_ = sendControl(m)
			}
		}
		go upd.SelfCheck(cycleCtx)
	}
	// 快速版本检查（HELLO_ACK/心跳 ACK 目标版本 ≠ 自报）：立即拉 setup.json
	// 检查并应用（spec §9.1）。
	c.TargetVersionFunc = func(target string) {
		if target != "" && target != info.AgentVersion {
			slog.Info("update: target version signal", "target", target, "current", info.AgentVersion)
			go func() { _ = upd.CheckNow(cycleCtx) }()
		}
	}
	// WS 推送 UPDATE_AVAILABLE（spec §9.1 触发①）：推送即完整目标清单
	// （已认证通道），直接编排。
	c.UpdateAvailableFunc = func(ctx context.Context, push proto.UpdateAvailable) {
		_ = upd.HandlePush(ctx, push)
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

// displayMgr 懒初始化虚拟显示器管理器（进程级单例；display op 与 desktop
// 会话钩子共用同一状态机）。
func (a *Agent) displayMgr() *display.Manager {
	a.displayMu.Lock()
	defer a.displayMu.Unlock()
	if a.display == nil {
		a.display = display.NewManager(slog.Default())
	}
	return a.display
}

// closeDisplay agent 退出收线：拆除设备（Handle 生命周期兜底由 OS 保证，
// 此处显式收线让退出路径干净可测）。
func (a *Agent) closeDisplay() {
	a.displayMu.Lock()
	defer a.displayMu.Unlock()
	if a.display != nil {
		a.display.Close()
	}
}

// Display 实现 agentctl.Deps（虚拟显示器本地控制）：on = 手动开并保持；
// off = 移除；status = 只读快照。驱动未装（idd 组件未装/装失败）时 on
// 返回 not_installed（status 恒成功——诊断入口不受影响）。
// defer/recover：display 路径含底层 syscall（LazyProc 缺 DLL 会 panic），
// 任何 panic 收敛为错误应答——绝不让一条本地显示操作杀死 agent 进程
// （2026-09-10 XIAOXIN 实测：swdevice.dll 硬路径加载 panic 曾致服务崩）。
func (a *Agent) Display(_ context.Context, action string) (info *agentctl.DisplayInfo, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("display: panic recovered", "action", action, "panic", r)
			info, err = nil, fmt.Errorf("internal: display operation panicked: %v", r)
		}
	}()
	m := a.displayMgr()
	switch action {
	case agentctl.DisplayActionOn:
		if err := m.SetOn(); err != nil {
			return nil, err
		}
	case agentctl.DisplayActionOff:
		m.SetOff()
	case agentctl.DisplayActionStatus:
	default:
		return nil, fmt.Errorf("bad_request: action must be on, off or status")
	}
	st := m.Snapshot()
	return &agentctl.DisplayInfo{
		DriverInstalled: st.DriverInstalled,
		DevicePresent:   st.DevicePresent,
		VirtualActive:   st.VirtualActive,
		PhysicalActive:  st.PhysicalActive,
		LidClosed:       st.LidClosed,
		LidKnown:        st.LidKnown,
		ForceLid:        st.ForceLid,
		AutoActive:      st.AutoActive,
		ManualActive:    st.ManualActive,
		Enabled:         st.Enabled,
	}, nil
}

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
// Channel 报告生效频道（空绑定归一为 stable）；Update 在更新在途时给出
// 进度信号（applying = pending 存在，安装器执行/新 agent 自检中；checking
// = 手动触发的检查进行中）——xnc upgrade 轮询用，尽力而为。
func (a *Agent) Status() agentctl.Status {
	st := agentctl.Status{State: agentctl.StateUnregistered, Version: machineinfo.Version}
	b, ok := a.currentBinding()
	if !ok {
		return st
	}
	st.State = agentctl.StateRegistered
	st.NodeID, st.Server, st.ClusterID = b.NodeID, b.Server, b.ClusterID
	st.Channel = channelOf(b)
	if c := a.currentConn(); c != nil && c.Connected() {
		st.State = agentctl.StateOnline
	}
	if p, ok := updater.LoadPending(a.StateDir); ok {
		st.Update = &agentctl.UpdateInfo{
			Phase: agentctl.UpdatePhaseApplying, From: p.From, To: p.To}
	} else if a.upgradeInFlight() {
		st.Update = &agentctl.UpdateInfo{Phase: agentctl.UpdatePhaseChecking}
	}
	return st
}

// channelOf 绑定频道（spec §9.1；空 = stable）。
func channelOf(b *binding.Binding) string {
	if b.Channel == "" {
		return "stable"
	}
	return b.Channel
}

func (a *Agent) beginUpgradeInFlight() bool {
	a.upgradeMu.Lock()
	defer a.upgradeMu.Unlock()
	if a.upgrading {
		return false
	}
	a.upgrading = true
	return true
}

func (a *Agent) endUpgradeInFlight() {
	a.upgradeMu.Lock()
	defer a.upgradeMu.Unlock()
	a.upgrading = false
}

func (a *Agent) upgradeInFlight() bool {
	a.upgradeMu.Lock()
	defer a.upgradeMu.Unlock()
	return a.upgrading
}

// Upgrade 实现 agentctl.Deps（spec §9.1 手动触发 + §9.5 跨频道切换），
// 快速返回：绑定校验与频道切换同步完成，检查本身在后台 goroutine 执行
// （管道一问一答不等下载；且请求 ctx 随连接关闭失效，后台检查用独立
// 生命周期）。串行化（§9.3）三层：pending 存在、本触发标记（原子
// test-and-set，一次手动触发未收线 → triggered=false 非错误，dispatch 配
// note 应答）、updater 包进程级编排互斥（与连接周期的轮询/推送编排跨
// 实例互斥）。无绑定 = 无更新源，同步报错。
//
// 跨频道切换：原子改 binding（其余字段保持），audit 记维护操作
// （update_channel {from,to}，log-only——T7 最小口径，无 DB/WS 通道），
// 并 rebind 唤醒连接周期，运行中的 6h 轮询编排器按新频道重建；本次手动
// 检查不依赖重建（自带新频道的独立 updater），rebind 只为周期轮询换代。
// 手动 updater 不带 Report 闭包——成功后的 update_ok audit 落
// update-audit.json，由安装完成重启后的新连接周期补报（与离线回滚同一
// 延迟审计机制）。
func (a *Agent) Upgrade(_ context.Context, channel string) (bool, error) {
	b, ok := a.currentBinding()
	if !ok {
		return false, errors.New("not_registered: no binding")
	}
	if _, pending := updater.LoadPending(a.StateDir); pending {
		return false, nil
	}
	if !a.beginUpgradeInFlight() {
		return false, nil
	}

	effective := channelOf(b)
	if channel != "" && channel != effective {
		nb := *b
		nb.Channel = channel
		if err := binding.Save(a.StateDir, &nb); err != nil {
			a.endUpgradeInFlight()
			return false, fmt.Errorf("internal: save binding: %v", err)
		}
		slog.Info("audit: update_channel", "from", effective, "to", channel)
		a.notifyRebind()
		effective = channel
	}

	// 后台立即检查并静默应用（退避跳过 = 人工决策；黑名单/降级/pending
	// 保护照常）。结果不回传（管道应答已返回）——CLI 经 status 轮询感知：
	// version 变化或 update 进度字段；失败仅记日志。
	server, stateDir := b.Server, a.StateDir
	go func() {
		defer a.endUpgradeInFlight()
		u := updater.New(server, stateDir, machineinfo.Version, effective, slog.Default())
		if err := u.ForceCheck(context.Background()); err != nil {
			slog.Warn("update: manual trigger failed", "channel", effective, "err", err)
		}
	}()
	return true, nil
}
