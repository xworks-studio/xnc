// tunnel.go — 会话 kind=tunnel 的 TCP 端口隧道：SESSION_OPEN params 为服务端
// 白名单展开的 {"target","host","port"}（agent 只信 host/port，不接受自选
// 目标），拨号 host:port（10s 超时）成功后做双向泵（ws binary ↔ tcp 裸流）；
// 目标不可达回 ERROR{RDP_NOT_AVAILABLE} 后关线。
package session

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"xnc/proto"
)

const tunnelDialTimeout = 10 * time.Second

// Tunnel 端口隧道处理器。Log 为 nil 时用 slog.Default()。
type Tunnel struct{ Log *slog.Logger }

func NewTunnel(log *slog.Logger) *Tunnel { return &Tunnel{Log: log} }

func (t *Tunnel) logger() *slog.Logger {
	if t.Log != nil {
		return t.Log
	}
	return slog.Default()
}

// Handle 拨号目标后双向泵：任一方向断开（tcp EOF / ws 断开 / SESSION_CLOSE
// 的 ctx 取消）即收线。返回即会话结束（引擎随后 CloseNow）。
func (t *Tunnel) Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage) {
	// params 为 {target,host,port}：host/port 由服务端白名单解析，agent 侧
	// 内联解码（proto.TunnelParams 只有 target，此处形状不同）。
	var p struct {
		Target string `json:"target"`
		Host   string `json:"host"`
		Port   int    `json:"port"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Host == "" || p.Port <= 0 {
		t.tunnelError(ctx, ws)
		return
	}
	d := net.Dialer{Timeout: tunnelDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(p.Host, strconv.Itoa(p.Port)))
	if err != nil {
		t.logger().Warn("tunnel target unreachable", "session", sessionID, "err", err)
		t.tunnelError(ctx, ws)
		return
	}
	defer func() { _ = conn.Close() }()

	wsDone := make(chan struct{})
	go func() { // ws → tcp；退出时关 tcp，解除下方 conn.Read 阻塞
		defer close(wsDone)
		defer func() { _ = conn.Close() }()
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				return // 对端断开 / ctx 取消
			}
			if typ != websocket.MessageBinary {
				continue // 忽略 text（不应出现）
			}
			if _, err := conn.Write(data); err != nil {
				return
			}
		}
	}()
	// tcp → ws
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
			we := ws.Write(wctx, websocket.MessageBinary, buf[:n])
			cancel()
			if we != nil {
				<-wsDone // 对端已断：等读侧 goroutine 随之退出
				return
			}
		}
		if err != nil { // tcp EOF（目标关流）或读侧已关 conn
			_ = ws.Close(websocket.StatusNormalClosure, "")
			<-wsDone
			return
		}
	}
}

// tunnelError 不可达终态：ERROR{RDP_NOT_AVAILABLE} text 帧后以 BadGateway 关线。
func (t *Tunnel) tunnelError(ctx context.Context, ws *websocket.Conn) {
	m, _ := proto.NewMsg(proto.TypeError, proto.ErrorPayload{
		Code:    proto.CodeRdpNotAvailable,
		Message: "tunnel target unreachable",
	})
	b, _ := json.Marshal(m)
	wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
	defer cancel()
	_ = ws.Write(wctx, websocket.MessageText, b)
	_ = ws.Close(websocket.StatusBadGateway, "tunnel target unreachable")
}
