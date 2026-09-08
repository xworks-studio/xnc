// legws.go — 浏览器腿（兜底）：WebSocket /ws（TCP，经 caddy 反代到
// xnc-server:8080 的主 mux）。文本帧 = 控制消息（JSON，与 WT 控制流同构、
// 无长度前缀）；二进制帧 = 媒体包下行（可靠有序——FEC 在此腿无丢包场景，
// 仅保持协议同构）。连接期经 AuthViewer 回调完成会话 token 鉴权。
package rtv

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// WSHandler 返回挂到主 HTTP mux 的 /ws 处理器（caddy → xnc-server:8080）。
func (s *Server) WSHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		binding, apiErr := s.AuthViewer(r.URL.Query().Get("token"))
		if apiErr != nil {
			http.Error(w, apiErr.Message, apiErr.Status)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: s.WSOrigins,
		})
		if err != nil {
			return
		}
		s.handleWSSession(binding, c)
	}
}

func (s *Server) handleWSSession(binding ViewerBinding, c *websocket.Conn) {
	v := &wsViewer{id: s.Hub.NextViewerID(), binding: binding, conn: c}
	slog.Info("rtv: ws viewer connected", "viewer", v.id, "node", binding.Node)

	cleanup := func() {
		s.Hub.RemoveViewer(v.Node(), v)
		c.Close(websocket.StatusNormalClosure, "bye")
	}
	defer cleanup()

	var m struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	for {
		typ, data, err := c.Read(context.Background())
		if err != nil {
			slog.Info("rtv: ws viewer read ended", "viewer", v.id, "err", err)
			return
		}
		if typ != websocket.MessageText {
			continue // 忽略二进制上行
		}
		if s.Touch != nil {
			s.Touch(v.Session())
		}
		if err := json.Unmarshal(data, &m); err != nil {
			slog.Warn("rtv: bad ws json", "err", err)
			continue
		}
		switch m.Type {
		case "hello":
			// 未绑定到当前 host 会话才重试 AddViewer（host 掉线/被替换后
			// 此前会话的绑定已解除）。曾经的"只处理首条 hello"门闩导致
			// host 离线期间到达的 hello 被吞，host 回来后 viewer 永远卡死。
			if m.Role == "viewer" && !s.Hub.IsViewerBound(v.Node(), v) {
				s.Hub.AddViewer(v.Node(), v) // host 不在时下发 hostOffline，等下一条 hello 再试
			}
		default:
			if h := s.Hub.Host(v.Node()); h != nil {
				h.forwardToHost(data, v)
			}
		}
	}
}

// wsViewer WebSocket 适配器（coder/websocket 单写者约束 → 互斥）。
type wsViewer struct {
	id      uint64
	binding ViewerBinding
	conn    *websocket.Conn
	mu      sync.Mutex
}

func (v *wsViewer) ID() uint64      { return v.id }
func (v *wsViewer) Kind() string    { return "ws" }
func (v *wsViewer) Node() string    { return v.binding.Node }
func (v *wsViewer) Session() string { return v.binding.Session }

func (v *wsViewer) SendDatagram(b []byte) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return v.conn.Write(ctx, websocket.MessageBinary, b)
}

func (v *wsViewer) SendControlJSON(b json.RawMessage) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return v.conn.Write(ctx, websocket.MessageText, b)
}

func (v *wsViewer) Close() {
	v.conn.Close(websocket.StatusNormalClosure, "bye")
}
