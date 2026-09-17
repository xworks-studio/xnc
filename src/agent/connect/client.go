// Package connect 实现到控制面的长连接客户端：
// 挑战-应答认证（Ed25519）→ HELLO → 心跳，断线指数退避重连。
package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"xnc/agent/identity"
	"xnc/agent/machineinfo"
	"xnc/proto"
)

// errConnDead 表示 drain 检测到连接死亡（读超时/对端关闭/二进制帧）。
var errConnDead = errors.New("control connection dead")

// Handler 处理 server → agent 的控制消息。nil 安全：Client.Handler 为 nil 时
// 维持 Phase 1 行为（ack 丢弃、ERROR 记日志）。
type Handler interface {
	HandleSessionOpen(ctx context.Context, so proto.SessionOpen)
	HandleSessionClose(ctx context.Context, sc proto.SessionClose)
}

type Client struct {
	ServerURL string
	Key       *identity.Key
	Info      machineinfo.Info
	Beat      time.Duration // 心跳间隔，NewClient 默认 30s
	// BackoffReset：连接存活超过该时长后退避计数归零——长连接证明健康，
	// 下一次抖动应快速重试。NewClient 默认 1min；测试可缩短。
	BackoffReset time.Duration
	Log          *slog.Logger
	Handler      Handler // nil 安全：会话消息分发；nil 时仅丢弃
	// OnReady 每次连接就绪（HELLO_ACK 之后、读循环启动之前）各调用恰好一次，
	// 交出该条连接的控制写闭包。重连后再次触发：旧闭包绑定已死连接，接收方
	// （如会话引擎）必须在回调内整体重建自身，不得跨连接复用旧闭包。nil 跳过。
	OnReady func(sendControl func(m proto.Message) error)
	// TargetVersionFunc：快速版本检查回调（HELLO_ACK/心跳 ACK 目标版本）。
	TargetVersionFunc func(target string)
	// UpdateAvailableFunc：UPDATE_AVAILABLE 到达（独立 goroutine 分发，
	// spec §9.1 触发①）。推送载荷在推送路径（updater.HandlePush）下即
	// 目标清单（URL+SHA256，已认证通道）；接收方也可选择忽略载荷、仅把
	// 它当作"立即检查一轮"的信号（轮询路径拉 setup.json）。nil 时静默忽略。
	UpdateAvailableFunc func(ctx context.Context, push proto.UpdateAvailable)
	// SasRequestFunc：SAS_REQUEST 到达（独立 goroutine 分发；web 工具栏
	// "发送 Ctrl+Alt+Del"，2026-09-17 安全桌面交互）。接收方完成 core
	// 0x0110 往返后经 CurrentSend 回 SAS_RESULT（凭 reqId 关联）。nil 时
	// 静默忽略（前向兼容同 UPDATE_AVAILABLE）。
	SasRequestFunc func(ctx context.Context, sr proto.SasRequest)

	sendMu      sync.Mutex
	currentSend func(m proto.Message) error
	// connReady 反映当前控制连接是否就绪（握手完成 true，连接终止 false）。
	// agentctl status 的 "online" 判据。
	connReady atomic.Bool
}

func NewClient(serverURL string, k *identity.Key, info machineinfo.Info) *Client {
	return &Client{ServerURL: serverURL, Key: k, Info: info,
		Beat: 30 * time.Second, BackoffReset: time.Minute, Log: slog.Default()}
}

// Run 维持控制连接：指数退避 + 随机抖动重连。
//
// 退避公式：delay = min(2s × 2^n + rand(0, 2s), 5min)，n = 连续失败次数。
// 连接存活 > BackoffReset（默认 1min）→ n 归零。
// 抖动防止多节点同时断网后的雷群效应（同批机器不会同时重连）。
// 长断网场景（笔记本合盖/网线断）5min 封顶比 30s 温和得多。
func (c *Client) Run(ctx context.Context) error {
	const base = 2 * time.Second
	const maxBackoff = 5 * time.Minute
	reset := c.BackoffReset
	if reset <= 0 {
		reset = time.Minute
	}
	n := 0
	for {
		start := time.Now() // 拨号前记录连接起点
		if err := c.once(ctx); err != nil && !errors.Is(err, context.Canceled) {
			c.Log.Warn("control connection lost", "attempt", n, "err", err)
		}
		if time.Since(start) > reset {
			n = 0 // 长连接证明健康，退避计数归零
		}
		// 指数退避 + 抖动：2s, 4s, 8s, 16s, 32s, 64s, ... → 5min 封顶
		delay := base << uint(min(n, 18)) // 2^n × 2s，防位移溢出
		if delay > maxBackoff {
			delay = maxBackoff
		}
		jitter := time.Duration(rand.Int63n(int64(base)))
		delay += jitter
		if n > 0 {
			c.Log.Info("reconnect backoff", "attempt", n, "delay", delay.String())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
			n++
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// RunOnce 完成一次 dial → 认证 → HELLO_ACK，随即以正常关闭码收线并返回 nil。
// 供 E2E/load（mockagent --once）验证注册+认证+连接就绪后即刻退出；
// 不维持心跳，offline 判定由 E2E kill 进程实现。
func (c *Client) RunOnce(ctx context.Context) error {
	ws, err := c.handshake(ctx)
	if err != nil {
		return err
	}
	// 正常关闭握手；对端即时 CloseNow 亦算成功（认证已达成本次目标），
	// 关闭帧交互的残余错误不作为失败上报，CloseNow 兜底回收。
	_ = ws.Close(websocket.StatusNormalClosure, "once")
	ws.CloseNow()
	return nil
}

// Connected 报告当前控制连接是否就绪（HELLO_ACK 后 true；连接终止/重连间隙
// false）。agentctl status 的 "online" 判据；不触发拨号。
func (c *Client) Connected() bool { return c.connReady.Load() }

// nodeDeleteAckTimeout 是 DeleteNode 发送后等待服务端关闭（删除确认）的
// 时限；包级变量供测试缩短。
var nodeDeleteAckTimeout = 30 * time.Second

// DeleteNode 机器自注销（spec §7 deregister）：一次性控制连接——挑战认证
// （机器身份即凭据）→ 发送 NODE_DELETE → 等待服务端关闭（关闭即删除确认；
// ERROR 帧先到 = 删除失败，返回带错误码的 error）。不进入心跳循环，不影响
// 可能并存的常驻连接（服务端注销后会自行逐出后者）。
//
// 确认判据（严格）：只有对端关闭——close 帧（websocket.CloseStatus 命中）
// 或 TCP 层 EOF——才算删除确认；读取超时/链路错误/坏帧一律报错。绝不能把
// 未知状态当成功，否则 agent 会误删 binding 而服务端节点残留（孤儿节点，
// 且后续 deregister 只会得到 not_registered，无自愈路径）。
func (c *Client) DeleteNode(ctx context.Context) error {
	ws, err := c.handshake(ctx)
	if err != nil {
		return err
	}
	defer ws.CloseNow()
	if err := writeMsg(ctx, ws, proto.TypeNodeDelete, struct{}{}); err != nil {
		return fmt.Errorf("send node_delete: %w", err)
	}
	for {
		m, err := readMsg(ctx, ws, nodeDeleteAckTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err() // 调用方取消/超时
			}
			// 对端关闭 = 删除确认：close 帧（任意状态码，含 1006 异常关闭
			// 帧）或裸 TCP EOF（无 close 帧的硬关闭）。
			if websocket.CloseStatus(err) != -1 ||
				errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			// 读取超时（readMsg 内部派生 ctx 的 DeadlineExceeded）/链路
			// 错误/坏帧：状态未知，按失败上报。
			return fmt.Errorf("node_delete: no close confirmation: %w", err)
		}
		if m.Type == proto.TypeError {
			var e proto.ErrorPayload
			_ = m.Decode(&e)
			// 服务端错误直通 "<code>: <message>"（agentctl 管道应答同一格式）。
			return fmt.Errorf("%s: %s", e.Code, e.Message)
		}
		// 其他帧（迟到的心跳 ACK 等）：忽略，继续等关闭。
	}
}

// handshake 拨号并完成认证：dial → CHALLENGE → CHALLENGE_RESPONSE → HELLO →
// HELLO_ACK。成功返回就绪连接（调用方负责关闭）；任何失败路径连接已关闭。
func (c *Client) handshake(ctx context.Context) (*websocket.Conn, error) {
	ws, _, err := websocket.Dial(ctx, wsURL(c.ServerURL)+"/api/agent/connect", nil)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			ws.CloseNow()
		}
	}()

	// CHALLENGE
	m, err := readMsg(ctx, ws, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("read challenge: %w", err)
	}
	if m.Type != proto.TypeChallenge {
		return nil, fmt.Errorf("expected CHALLENGE, got %q", m.Type)
	}
	var ch proto.Challenge
	if err := m.Decode(&ch); err != nil {
		return nil, err
	}
	if err := writeMsg(ctx, ws, proto.TypeChallengeResponse, proto.ChallengeResponse{
		NodeID: c.Key.NodeID, Signature: c.Key.Sign([]byte(ch.Nonce))}); err != nil {
		return nil, err
	}
	if err := writeMsg(ctx, ws, proto.TypeHello, proto.Hello{
		NodeID: c.Key.NodeID, Hostname: c.Info.Hostname,
		AgentVersion: c.Info.AgentVersion, ShellType: c.Info.ShellType}); err != nil {
		return nil, err
	}
	m, err = readMsg(ctx, ws, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("read HELLO_ACK: %w", err)
	}
	if m.Type != proto.TypeHelloAck {
		return nil, fmt.Errorf("auth rejected: expected HELLO_ACK, got %q", m.Type)
	}
	var hAck proto.HelloAck
	_ = m.Decode(&hAck)
	if hAck.TargetVersion != "" {
		c.OnTargetVersion(hAck.TargetVersion) // 快速版本检查 ①：握手回执
	}
	c.Log.Info("control connection ready", "node", c.Key.NodeID)
	ok = true
	return ws, nil
}

// once 完成一次完整连接生命周期：dial → CHALLENGE → CHALLENGE_RESPONSE → HELLO
// → HELLO_ACK → OnReady → 心跳循环，出错返回（由 Run 重连）。
func (c *Client) once(ctx context.Context) error {
	ws, err := c.handshake(ctx)
	if err != nil {
		return err
	}
	c.connReady.Store(true)
	defer c.connReady.Store(false)
	defer ws.CloseNow()

	// 连接就绪（HELLO_ACK 已收到、读循环未启动）：把本条连接的控制写闭包经
	// OnReady 交出。每次连接各调用一次；闭包捕获外层 ctx（随 Run 取消，不随
	// 连接拆除）与本条 ws——重连后旧闭包指向死连接，接收方须在回调内重建。
	if c.OnReady != nil {
		c.OnReady(func(m proto.Message) error { return writeControl(ctx, ws, m) })
		c.sendMu.Lock()
		c.currentSend = func(m proto.Message) error { return writeControl(ctx, ws, m) }
		c.sendMu.Unlock()
	}

	// 泄读循环：消费 HEARTBEAT_ACK 等入站帧。不读的话 ACK 积压（~39B/30s）
	// 会撑满接收窗口（~64KB ≈ 14h），服务器写超时掐线 → 节点周期性 offline
	// 抖动。同时以 3×Beat 读超时做死端检测，不再依赖 TCP 重传超时。
	// 注意不能用 ws.CloseRead：它对任何数据帧（含 HEARTBEAT_ACK）都会以
	// StatusPolicyViolation 中止连接。
	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		c.drain(pctx, ws, pcancel)
	}()
	defer func() { pcancel(); <-drainDone }() // 保证 drain 收敛，不跨重连泄漏

	tick := time.NewTicker(c.Beat)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = ws.Close(websocket.StatusNormalClosure, "shutdown")
			return ctx.Err()
		case <-pctx.Done():
			return errConnDead
		case <-tick.C:
			if err := writeMsg(ctx, ws, proto.TypeHeartbeat, proto.Heartbeat{Version: c.Info.AgentVersion}); err != nil {
				return err
			}
		}
	}
}

// drain 持续消费入站帧直至连接判定死亡：HEARTBEAT_ACK 丢弃；ERROR 帧记
// Warn（含错误码）不致命；SESSION_OPEN/SESSION_CLOSE 解码后交 Handler 在
// 独立 goroutine 分发（会话处理不得阻塞心跳/读取，Handler 为 nil 时跳过）；
// 其他帧忽略（前向兼容）。每次读携带 max(3×Beat,1s) 的存活超时——读错误
// （超时=对端沉默、连接关闭、二进制帧）即调用 dead（恰好一次）并返回。
// pctx 取消属正常拆除，静默退出；分发 goroutine 同以 pctx 为生命周期。
func (c *Client) drain(pctx context.Context, ws *websocket.Conn, dead func()) {
	for {
		m, err := readMsg(pctx, ws, c.readDeadline())
		if err == nil {
			switch m.Type {
			case proto.TypeHeartbeatAck:
				var hAck proto.HeartbeatAck
				_ = m.Decode(&hAck)
				if hAck.TargetVersion != "" {
					c.OnTargetVersion(hAck.TargetVersion) // 快速版本检查 ②：心跳搭车
				}
				// 心跳回执，丢弃。
			case proto.TypeError:
				var e proto.ErrorPayload
				_ = m.Decode(&e)
				c.Log.Warn("server ERROR frame", "code", e.Code, "message", e.Message)
			case proto.TypeSessionOpen:
				var so proto.SessionOpen
				if err := m.Decode(&so); err == nil && c.Handler != nil {
					h := c.Handler
					go h.HandleSessionOpen(pctx, so) // 会话处理不得阻塞心跳/读取
				}
			case proto.TypeUpdateAvailable:
				var push proto.UpdateAvailable
				if err := m.Decode(&push); err == nil {
					go c.HandleUpdateAvailable(pctx, push) // 检查/下载不得阻塞心跳读取
				}
			case proto.TypeSasRequest:
				var sr proto.SasRequest
				if err := m.Decode(&sr); err == nil && c.SasRequestFunc != nil {
					f := c.SasRequestFunc
					go f(pctx, sr) // core 往返（秒级）不得阻塞心跳读取
				}
			case proto.TypeSessionClose:
				var sc proto.SessionClose
				if err := m.Decode(&sc); err == nil && c.Handler != nil {
					h := c.Handler
					go h.HandleSessionClose(pctx, sc)
				}
			}
			continue
		}
		if pctx.Err() != nil {
			return // 正常拆除（外层取消 / once 退出），非连接故障
		}
		c.Log.Warn("drain: connection dead", "err", err)
		dead()
		return
	}
}

// readDeadline 是 drain 每次读的存活超时：3×Beat，下限 1s 防零值退化。
func (c *Client) readDeadline() time.Duration {
	if d := 3 * c.Beat; d >= time.Second {
		return d
	}
	return time.Second
}

// wsURL 把 HTTP(S) 服务地址转换为 WS(S)。
func wsURL(u string) string {
	return strings.Replace(strings.Replace(u, "https://", "wss://", 1), "http://", "ws://", 1)
}

func writeMsg(ctx context.Context, ws *websocket.Conn, typ string, payload any) error {
	m, err := proto.NewMsg(typ, payload)
	if err != nil {
		return err
	}
	return writeControl(ctx, ws, m)
}

// writeControl 把已构造的完整控制帧写入控制连接。OnReady 交出的 sendControl
// 闭包即绑定本函数（捕获 once 的外层 ctx 与本条 ws）：ctx 随 Run 取消而非
// 随连接拆除，连接存续期内闭包始终有效；连接死亡后写入报错，由下一次
// OnReady 重建依赖。coder/websocket 保证并发 Write 安全（与心跳写并存无竞态）。
func writeControl(ctx context.Context, ws *websocket.Conn, m proto.Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return ws.Write(wctx, websocket.MessageText, b)
}

func readMsg(ctx context.Context, ws *websocket.Conn, timeout time.Duration) (proto.Message, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	typ, data, err := ws.Read(rctx)
	if err != nil {
		return proto.Message{}, err
	}
	// 协议绑定（design §2.1）：控制连接只接受文本帧，二进制帧按错误处理。
	if typ != websocket.MessageText {
		return proto.Message{}, errors.New("binary frame rejected")
	}
	var m proto.Message
	return m, json.Unmarshal(data, &m)
}

// ---- 自更新接入（三入口回调）----

// OnTargetVersion 快速版本检查回调：HELLO_ACK 回执与心跳 ACK 携带的目标
// 版本到达时调用（nil 跳过）。与推送路径收敛到同一处理方（updater
// 幂等去重），三入口（握手/心跳/强制）无需区分来源。
func (c *Client) OnTargetVersion(target string) {
	if c.TargetVersionFunc != nil {
		c.TargetVersionFunc(target)
	}
}

// HandleUpdateAvailable drain 收到 UPDATE_AVAILABLE 时分发（独立
// goroutine，与会话消息同等待遇——检查/下载不得阻塞心跳读取）。
func (c *Client) HandleUpdateAvailable(pctx context.Context, push proto.UpdateAvailable) {
	if c.UpdateAvailableFunc != nil {
		c.UpdateAvailableFunc(pctx, push)
	}
}

// CurrentSend 返回当前连接的控制写闭包（无连接时返回 nil-safe 占位）。
// updater 经此上报 UPDATE_STATUS；重连后闭包自然指向新连接。
func (c *Client) CurrentSend() func(m proto.Message) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.currentSend == nil {
		return func(proto.Message) error { return errNoConn }
	}
	return c.currentSend
}

var errNoConn = fmt.Errorf("control connection not ready")
