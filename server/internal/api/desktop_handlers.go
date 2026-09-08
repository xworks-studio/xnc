package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"xnc/proto"
	"xnc/server/internal/session"
)

// desktopBodyMaxBytes：desktop 请求体上限 4KB——wtsSession 一个字段远小于
// 此，先于解码生效，杜绝无限缓冲。
const desktopBodyMaxBytes = 4 * 1024

// desktopReq 是客户端可影响的**白名单全集**：wtsSession。
// StreamEndpoint/HostToken 等安全敏感字段绝不从客户端接受——endpoint 只
// 来自 server config（XNC_RTV_ENDPOINT），HostToken 只来自 relay Hub 的
// per-node 签发（经 SESSION_OPEN→agent→core→stdin 下发到 xnc-host）。
// 未知字段经 json.Decode 默认忽略，等效剥离。
type desktopReq struct {
	WTSSession uint32 `json:"wtsSession"`
}

// desktopStart 处理 POST /api/nodes/{id}/desktop（RTV，2026-09-08 重构）：
// ① RTV endpoint 检查——XNC_RTV_ENDPOINT 未配置时 503 RTV_UNCONFIGURED
// （拒绝开会话优于开一个必死的会话）；
// ② 4KB 体上限 + 白名单解码；
// ③ HostToken 签发/复用（relay Hub per-node 表——core StartCapture 幂等
// 复用运行中的 host，token 必须跨会话可重放）；
// ④ 服务端构造 DesktopParams（StreamEndpoint/HostToken/WTSSession）；
// ⑤ 委托 startSession（KindDesktop，审计 desktop.open/desktop.close，RBAC
// operator+ 由 startSession 统一判定，每节点并发上限由 manager 治理）；
// ⑥ 202 响应并入 viewer 腿地址（同源推导）与 lease 判定。
func (h *handlers) desktopStart(w http.ResponseWriter, r *http.Request) {
	if h.cfg.RTVStreamEndpoint == "" || h.rtv == nil {
		respondError(w, proto.Err(503, proto.CodeRtvUnconfigured,
			"desktop sessions require RTV; server has no XNC_RTV_ENDPOINT configuration"))
		return
	}
	if _, err := uuid.Parse(chi.URLParam(r, "id")); err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, desktopBodyMaxBytes)
	var req desktopReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}

	nodeID := chi.URLParam(r, "id")
	hostToken := h.rtv.Hub.HostTokenFor(nodeID)
	params, err := json.Marshal(proto.DesktopParams{
		StreamEndpoint: h.cfg.RTVStreamEndpoint, // server config 独占
		HostToken:      hostToken,               // relay Hub 签发；绝不回显给客户端
		WTSSession:     req.WTSSession,
		TLSInsecure:    h.cfg.RTVInsecureTLS, // dev 自签栈专用（server config 独占）
	})
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "encode params"))
		return
	}

	// viewer 腿地址按请求同源推导（生产 = xnc.app 经 caddy/UDP443；dev =
	// 本机直连）。HostToken/StreamEndpoint 的 endpoint 形态可不同（endpoint
	// 是 host 腿 QUIC 地址，可能经独立端口映射）。
	wtURL := "https://" + r.Host + "/wt"
	wsURL := "wss://" + r.Host + "/ws"

	h.startSession(w, r, proto.KindDesktop, params, "desktop.open", "desktop.close",
		map[string]any{"wtUrl": wtURL, "wsUrl": wsURL},
		nil,
		func(res *session.CreateResult) map[string]any {
			// lease 判定随 202 下发（RTV 后 lease 为 server 侧簿记，relay 的
			// input 门控按 desktopLeases 表判定；granted=false = view-only）。
			return map[string]any{"lease": map[string]any{
				"granted": res.LeaseGranted, "leaseId": res.LeaseID,
			}}
		})
}

// rtvStats 管理端观测面（原 MVP /statsz 收权版本：JWT admin 组内挂载）。
func (h *handlers) rtvStats(w http.ResponseWriter, _ *http.Request) {
	if h.rtv == nil {
		respondJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	respondJSON(w, http.StatusOK, h.rtv.Hub.Snapshot())
}
