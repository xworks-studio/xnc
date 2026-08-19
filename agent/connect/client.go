// Package connect 实现到控制面的长连接客户端：
// 挑战-应答认证（Ed25519）→ HELLO → 心跳，断线指数退避重连。
package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/coder/websocket"

	"xnc/agent/identity"
	"xnc/agent/machineinfo"
	"xnc/proto"
)

// errConnDead 表示 drain 检测到连接死亡（读超时/对端关闭/二进制帧）。
var errConnDead = errors.New("control connection dead")

type Client struct {
	ServerURL string
	Key       *identity.Key
	Info      machineinfo.Info
	Beat      time.Duration // 心跳间隔，NewClient 默认 30s
	// BackoffReset：连接存活超过该时长后退避计数归零——长连接证明健康，
	// 下一次抖动应快速重试。NewClient 默认 1min；测试可缩短。
	BackoffReset time.Duration
	Log          *slog.Logger
}

func NewClient(serverURL string, k *identity.Key, info machineinfo.Info) *Client {
	return &Client{ServerURL: serverURL, Key: k, Info: info,
		Beat: 30 * time.Second, BackoffReset: time.Minute, Log: slog.Default()}
}

// Run 维持控制连接：断开后按 1,2,5,10,30s（上限 30s）退避重连；ctx 取消即返回。
func (c *Client) Run(ctx context.Context) error {
	backoff := []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second}
	reset := c.BackoffReset
	if reset <= 0 {
		reset = time.Minute
	}
	n := 0
	for {
		start := time.Now() // 拨号前记录连接起点
		if err := c.once(ctx); err != nil && !errors.Is(err, context.Canceled) {
			c.Log.Warn("control connection lost", "err", err)
		}
		if time.Since(start) > reset {
			n = 0 // 长连接证明健康，退避计数归零
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff[min(n, len(backoff)-1)]):
			n++
		}
	}
}

// once 完成一次完整连接生命周期：dial → CHALLENGE → CHALLENGE_RESPONSE → HELLO
// → HELLO_ACK → 心跳循环，出错返回（由 Run 重连）。
func (c *Client) once(ctx context.Context) error {
	url := wsURL(c.ServerURL) + "/api/agent/connect"
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return err
	}
	defer ws.CloseNow()

	// CHALLENGE
	m, err := readMsg(ctx, ws, 30*time.Second)
	if err != nil {
		return fmt.Errorf("read challenge: %w", err)
	}
	if m.Type != proto.TypeChallenge {
		return fmt.Errorf("expected CHALLENGE, got %q", m.Type)
	}
	var ch proto.Challenge
	if err := m.Decode(&ch); err != nil {
		return err
	}
	if err := writeMsg(ctx, ws, proto.TypeChallengeResponse, proto.ChallengeResponse{
		NodeID: c.Key.NodeID, Signature: c.Key.Sign([]byte(ch.Nonce))}); err != nil {
		return err
	}
	if err := writeMsg(ctx, ws, proto.TypeHello, proto.Hello{
		NodeID: c.Key.NodeID, Hostname: c.Info.Hostname,
		AgentVersion: c.Info.AgentVersion, ShellType: c.Info.ShellType}); err != nil {
		return err
	}
	m, err = readMsg(ctx, ws, 30*time.Second)
	if err != nil {
		return fmt.Errorf("read HELLO_ACK: %w", err)
	}
	if m.Type != proto.TypeHelloAck {
		return fmt.Errorf("auth rejected: expected HELLO_ACK, got %q", m.Type)
	}
	c.Log.Info("control connection ready", "node", c.Key.NodeID)

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
			if err := writeMsg(ctx, ws, proto.TypeHeartbeat, struct{}{}); err != nil {
				return err
			}
		}
	}
}

// drain 持续消费入站帧直至连接判定死亡：HEARTBEAT_ACK 丢弃；ERROR 帧记
// Warn（含错误码）不致命；其他帧忽略（前向兼容）。每次读携带 max(3×Beat,1s)
// 的存活超时——读错误（超时=对端沉默、连接关闭、二进制帧）即调用 dead（恰
// 好一次）并返回。pctx 取消属正常拆除，静默退出。
func (c *Client) drain(pctx context.Context, ws *websocket.Conn, dead func()) {
	for {
		m, err := readMsg(pctx, ws, c.readDeadline())
		if err == nil {
			switch m.Type {
			case proto.TypeHeartbeatAck:
				// 心跳回执，丢弃。
			case proto.TypeError:
				var e proto.ErrorPayload
				_ = m.Decode(&e)
				c.Log.Warn("server ERROR frame", "code", e.Code, "message", e.Message)
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
