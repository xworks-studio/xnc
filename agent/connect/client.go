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

type Client struct {
	ServerURL string
	Key       *identity.Key
	Info      machineinfo.Info
	Beat      time.Duration // 心跳间隔，NewClient 默认 30s
	Log       *slog.Logger
}

func NewClient(serverURL string, k *identity.Key, info machineinfo.Info) *Client {
	return &Client{ServerURL: serverURL, Key: k, Info: info,
		Beat: 30 * time.Second, Log: slog.Default()}
}

// Run 维持控制连接：断开后按 1,2,5,10,30s（上限 30s）退避重连；ctx 取消即返回。
func (c *Client) Run(ctx context.Context) error {
	backoff := []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second}
	n := 0
	for {
		if err := c.once(ctx); err != nil && !errors.Is(err, context.Canceled) {
			c.Log.Warn("control connection lost", "err", err)
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
	if err != nil || m.Type != proto.TypeChallenge {
		return fmt.Errorf("expect challenge: %w", err)
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
	if err != nil || m.Type != proto.TypeHelloAck {
		return fmt.Errorf("auth rejected")
	}
	c.Log.Info("control connection ready", "node", c.Key.NodeID)

	tick := time.NewTicker(c.Beat)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = ws.Close(websocket.StatusNormalClosure, "shutdown")
			return ctx.Err()
		case <-tick.C:
			if err := writeMsg(ctx, ws, proto.TypeHeartbeat, struct{}{}); err != nil {
				return err
			}
		}
	}
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
	_, data, err := ws.Read(rctx)
	if err != nil {
		return proto.Message{}, err
	}
	var m proto.Message
	return m, json.Unmarshal(data, &m)
}
