package api

import (
	"encoding/json"
	"io"
	"net/http"

	"xnc/proto"
)

// desktopBodyMaxBytes：desktop 请求体上限 4KB——signaling/wtsSession 两个
// 字段远小于此，先于解码生效，杜绝无限缓冲。
const desktopBodyMaxBytes = 4 * 1024

// desktopSignaling 是 M1-Slice2 唯一的 desktop 信令形态（空 = 缺省）。
const desktopSignaling = "webrtc"

// desktopReq 是客户端可影响的**白名单全集**：signaling + wtsSession。
// turn / iceTransportPolicy 等安全敏感字段绝不从客户端接受——TURN 配置
// 只来自 server config（SESSION_OPEN params 与 REST 响应同源），
// iceTransportPolicy 从不下发（agent 缺省 = 强制 relay；DesktopIceAll
// 仅回环单测）。未知字段经 json.Decode 默认忽略，等效剥离。
type desktopReq struct {
	Signaling  string `json:"signaling"`
	WTSSession uint32 `json:"wtsSession"`
}

// turnConfig 从 server config 构造 desktop TURN 配置；不完整（URLs/username/
// credential 任一缺失）→ nil = 未配置。
func (h *handlers) turnConfig() *proto.DesktopTurnConfig {
	t := &proto.DesktopTurnConfig{
		URLs: h.cfg.TurnURLs, Username: h.cfg.TurnUsername, Credential: h.cfg.TurnCredential,
	}
	if !t.Configured() {
		return nil
	}
	return t
}

// desktopStart 处理 POST /api/nodes/{id}/desktop（M1-Slice2）：
// ① TURN 配置检查——desktop relay-only，server 无 TURN 时 503
// TURN_UNCONFIGURED（拒绝开会话优于开一个必死的会话）；
// ② 4KB 体上限 + 白名单解码（空体 = 缺省 webrtc）+ signaling 校验；
// ③ 服务端构造 DesktopParams（turn 来自 config，客户端字段仅白名单两项）；
// ④ 委托 startSession（KindDesktop，审计 desktop.open/desktop.close，
// RBAC operator+ 由 startSession 统一判定，每节点单会话由 manager 治理）；
// ⑤ 202 响应并入 turn 配置（viewer 据此建 relay-only PeerConnection）。
func (h *handlers) desktopStart(w http.ResponseWriter, r *http.Request) {
	turn := h.turnConfig()
	if turn == nil {
		respondError(w, proto.Err(503, proto.CodeTurnUnconfigured,
			"desktop sessions require TURN; server has no TURN configuration"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, desktopBodyMaxBytes)
	var req desktopReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}
	if req.Signaling == "" {
		req.Signaling = desktopSignaling
	}
	if req.Signaling != desktopSignaling {
		respondError(w, proto.Err(400, proto.CodeInternal,
			"signaling must be \"webrtc\""))
		return
	}
	params, err := json.Marshal(proto.DesktopParams{
		Signaling:  req.Signaling,
		Turn:       turn, // server config 独占；客户端提交的 turn 已被白名单剥离
		WTSSession: req.WTSSession,
		// IceTransportPolicy 故意不设：缺省 = agent 侧强制 relay。
	})
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "encode params"))
		return
	}
	h.startSession(w, r, proto.KindDesktop, params, "desktop.open", "desktop.close",
		map[string]any{"turn": turn})
}
