package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"xnc/proto"
	"xnc/server/internal/db/sqlc"
	"xnc/server/internal/session"
)

// desktopBodyMaxBytes：desktop 请求体上限 4KB——signaling/wtsSession 两个
// 字段远小于此，先于解码生效，杜绝无限缓冲。
const desktopBodyMaxBytes = 4 * 1024

// desktopSignaling 是 M1-Slice2 唯一的 desktop 信令形态（空 = 缺省）。
const desktopSignaling = "webrtc"

// desktopReq 是客户端可影响的**白名单全集**：signaling + wtsSession。
// turn / iceTransportPolicy / mediaProtocol 等安全敏感字段绝不从客户端接受
// ——TURN 配置只来自 server config（SESSION_OPEN params 与 REST 响应同源），
// mediaProtocol 只来自 server 侧 canary 选择（M4 Task 4），iceTransportPolicy
// 从不下发（agent 缺省 = 强制 relay；DesktopIceAll 仅回环单测）。未知字段经
// json.Decode 默认忽略，等效剥离。
type desktopReq struct {
	Signaling  string `json:"signaling"`
	WTSSession uint32 `json:"wtsSession"`
}

// turnConfig 构造 desktop TURN 配置。池（XNC_TURN_POOL）配置且至少一台
// healthy → 返回池内分配的单台 TURN（udp+tcp 两个 URL，凭据共用）；分配失败
// （池空/全不健康/凭据缺失）→ 回落 XNC_TURN_URLS 全列表（现状）。两者皆不
// 完整（URLs/username/credential 任一缺失）→ nil = 未配置。
func (h *handlers) turnConfig() *proto.DesktopTurnConfig {
	if h.turnPool != nil {
		if t, ok := h.turnPool.Allocate(); ok && t.Configured() {
			return &t
		}
		// 池分配失败（全不健康/空）→ 回落旧路径（保持现状语义）
	}
	t := &proto.DesktopTurnConfig{
		URLs: h.cfg.TurnURLs, Username: h.cfg.TurnUsername, Credential: h.cfg.TurnCredential,
	}
	if !t.Configured() {
		return nil
	}
	return t
}

// desktopMediaRollout 把 server config 的 v2 rollout 字段折叠成选择输入
//（M4 Task 4；纯函数见 desktop_media_select.go）。
func (h *handlers) desktopMediaRollout() MediaRollout {
	return MediaRollout{
		Percent:   h.cfg.DesktopMediaV2Percent,
		Allowlist: h.cfg.DesktopMediaV2Allowlist,
		Rollback:  h.cfg.DesktopMediaV2Rollback,
	}
}

// desktopStart 处理 POST /api/nodes/{id}/desktop（M1-Slice2）：
// ① TURN 配置检查——desktop relay-only，server 无 TURN 时 503
// TURN_UNCONFIGURED（拒绝开会话优于开一个必死的会话）；
// ② mediaProtocol 选择（M4 Task 4）——节点 allowlist/百分比/回滚开关
//（server config）在 startSession 之前（= Host/Publisher 启动之前）一次性
// 选定并随 params 快照；live 会话绝不重选，客户端提交值被白名单剥离；
// ③ 4KB 体上限 + 白名单解码（空体 = 缺省 webrtc）+ signaling 校验；
// ④ 服务端构造 DesktopParams（turn/mediaProtocol 来自 config 与 server
// 选择，客户端字段仅白名单两项）；
// ⑤ 委托 startSession（KindDesktop，审计 desktop.open/desktop.close 携带
// 选定 mediaProtocol，RBAC operator+ 由 startSession 统一判定，每节点单
// 会话由 manager 治理）；
// ⑥ 202 响应并入 turn 配置与选定 mediaProtocol（viewer 据此建 relay-only
// PeerConnection）。
func (h *handlers) desktopStart(w http.ResponseWriter, r *http.Request) {
	turn := h.turnConfig()
	if turn == nil {
		respondError(w, proto.Err(503, proto.CodeTurnUnconfigured,
			"desktop sessions require TURN; server has no TURN configuration"))
		return
	}
	// ② mediaProtocol 选择先于一切会话副作用（绑定裁决 1：before
	// Host/Publisher start）；节点 UUID 大小写形态统一后入桶/allowlist 匹配。
	nodeID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return
	}
	media := selectMediaProtocol(h.desktopMediaRollout(), nodeID.String())

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
		// MediaProtocol 只来自 server 选择（canary 控制点）；客户端提交
		// 值已被 desktopReq 白名单剥离。
		MediaProtocol: media,
	})
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "encode params"))
		return
	}
	h.startSession(w, r, proto.KindDesktop, params, "desktop.open", "desktop.close",
		map[string]any{"turn": turn, "mediaProtocol": media},
		map[string]string{"mediaProtocol": media},
		func(res *session.CreateResult) map[string]any {
			// ① M2-Slice3 Task 4：lease 判定随 202 下发（授予时 leaseId 与
			// SESSION_OPEN params 同源；viewer 凭 granted 决定是否发
			// lease_request/启用输入 UI）。
			return map[string]any{"lease": map[string]any{
				"granted": res.LeaseGranted, "leaseId": res.LeaseID,
			}}
		})
}

// desktopCapabilities 按 RBAC 角色计算 capability 集（M2-Slice3 Task 4，
// spec §14）：viewer 只读；operator +鼠标/键盘；owner +SAS/system。
func desktopCapabilities(role string) []string {
	switch role {
	case "owner":
		return []string{proto.CapScreenView, proto.CapInputMouse, proto.CapInputKeyboard,
			proto.CapInputSecureAttn, proto.CapShellSystem}
	case "operator":
		return []string{proto.CapScreenView, proto.CapInputMouse, proto.CapInputKeyboard}
	default: // viewer（当前 RBAC 下到不了会话创建；保守只读）
		return []string{proto.CapScreenView}
	}
}

// withDesktopCapabilities 把角色 capability 集并入 desktop params（会话
// 创建时一次性定死；agent 侧据此强制 input.*/secure_attention）。
func withDesktopCapabilities(ctx context.Context, h *handlers,
	node *sqlc.Node, userID uuid.UUID, params json.RawMessage) json.RawMessage {
	role, err := h.st.Q().GetMemberRole(ctx, sqlc.GetMemberRoleParams{
		ClusterID: node.ClusterID, UserID: userID,
	})
	if err != nil {
		role = "viewer" // 查询失败：保守只读（requireMinRole 已验证成员）
	}
	var p proto.DesktopParams
	if json.Unmarshal(params, &p) != nil {
		return params // 非法形态：交给后续（agent bad_params）
	}
	p.Capabilities = desktopCapabilities(role)
	p.LeaseID = "" // 防御：客户端提交的 leaseId 绝不透传
	// 客户端提交的 leaseId/capabilities 绝不透传（白名单外字段）。
	if b, err := json.Marshal(p); err == nil {
		return b
	}
	return params
}
