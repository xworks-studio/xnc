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

// wsReadLimit 会话 WS 单帧读上限：file kind 以 64KiB chunk 收发（cli 上传/
// agent 下载），coder/websocket 默认 32768 会在首帧即杀会话（pump 的 Reader
// 按 conn 读限执行）。上限统一取 proto.MaxSessionFrameBytes（agent/CLI 同一
// 常量，防各处漂移断流）。
const wsReadLimit = proto.MaxSessionFrameBytes

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
// 连接移交 manager/pump。
//
// 所有权语义（fix：handler goroutine 泄漏）：websocket.Accept 会 hijack 底层
// 连接，此后 net/http 永不 cancel 该请求的 ctx——曾实现的 <-r.Context().Done()
// 挂起永不返回，每个会话泄漏 2 个永久驻留的 handler goroutine。修复：attach
// 成功即置 attached 并直接 return；hijacked conn 的生命周期独立于 handler，
// 由 manager（s.agentWS/s.clientWS 持引用）与 pump 接管，close 时统一 CloseNow。
// CloseNow 兜底仅在连接未成功移交时触发（attach 拒绝路径；Accept 失败时
// 连接从未建立，无需关闭）。
func (h *handlers) sessionWS(w http.ResponseWriter, r *http.Request, id, token string,
	attach func(string, string, *websocket.Conn) *proto.APIError) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	// 读限须在 attach（pump 开始读帧）前生效：file 64KiB chunk 超默认 32768。
	c.SetReadLimit(wsReadLimit)
	attached := false
	defer func() {
		if !attached {
			_ = c.CloseNow() // attach 拒绝：WS 已 101 升级无响应体可写，关闭即全部语义
		}
	}()
	if apiErr := attach(id, token, c); apiErr != nil {
		slog.Warn("session attach rejected", "code", apiErr.Code)
		return
	}
	attached = true
	// 连接所有权自此归 manager/pump：handler 直接返回。此路径绝不能 CloseNow
	// （会与 pump 竞争关闭在用连接），也绝不能读帧（会消费 pump 的数据）。
	return
}
