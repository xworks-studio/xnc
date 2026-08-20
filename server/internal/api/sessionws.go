package api

import (
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"xnc/proto"
)

// wsBaseURL 推导绝对 WS 基址：X-Forwarded-Proto 优先（caddy 反代注入 https），
// 其次 r.TLS（原生 TLS），默认 ws（本地 dev 明文）。SESSION_OPEN 的 WsURL 由此构造。
func wsBaseURL(r *http.Request) string {
	scheme := "ws"
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" {
		scheme = "wss"
	} else if r.TLS != nil {
		scheme = "wss"
	}
	return scheme + "://" + r.Host
}

// clientSessionWS 处理 GET /api/session/{id}?token=（client 侧）。
// token 即凭证，不走 JWT：会话 token 单用途且 60s TTL，比长期 JWT 更收紧。
func (h *handlers) clientSessionWS(w http.ResponseWriter, r *http.Request) {
	h.sessionWS(w, r, chi.URLParam(r, "id"), r.URL.Query().Get("token"), h.sess.AttachClient)
}

// agentSessionWS 处理 GET /api/agent/session?token=（agent 侧；
// 会话 ID 已隐含在 token 背后的会话里，无需路径参数）。
func (h *handlers) agentSessionWS(w http.ResponseWriter, r *http.Request) {
	h.sessionWS(w, r, "", r.URL.Query().Get("token"), h.sess.AttachAgent)
}

// sessionWS 两侧共用的 attach 流程：Accept → attach（token 校验/单用途消耗）→
// 连接移交 manager。attach 失败时 defer CloseNow 即可（WS 已完成 101 升级，
// 无 REST 响应体可写，关闭连接就是全部语义）。
func (h *handlers) sessionWS(w http.ResponseWriter, r *http.Request, id, token string,
	attach func(string, string, *websocket.Conn) *proto.APIError) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	if apiErr := attach(id, token, c); apiErr != nil {
		slog.Warn("session attach rejected", "code", apiErr.Code)
		return // attach 失败：defer 关闭连接即可
	}
	// 连接交由 manager/pump 接管。此处不能读帧（会消费 pump 的数据），
	// 挂起直到请求 ctx 结束；manager 关闭连接（CloseNow）时 handler 让出
	// 连接所有权，由 pump/NotifyClose 完成收尾。
	<-r.Context().Done()
}
