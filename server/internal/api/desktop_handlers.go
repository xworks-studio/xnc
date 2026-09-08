package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"xnc/proto"
	"xnc/rtv"
	"xnc/server/internal/auth"
	"xnc/server/internal/session"
)

// desktopBodyMaxBytes：desktop 请求体上限 4KB——wtsSession 一个字段远小于
// 此，先于解码生效，杜绝无限缓冲。
const desktopBodyMaxBytes = 4 * 1024

// viewerTicketTTL viewer 张票有效期上限（relay-plane spec §3.1：与会话
// 生命周期对齐；撤销靠墓碑，不靠短 exp）。
const viewerTicketTTL = 24 * time.Hour

// desktopReq 是客户端可影响的**白名单全集**：wtsSession。
// StreamEndpoint/HostToken 等安全敏感字段绝不从客户端接受——endpoint 只
// 来自 server config（XNC_RTV_ENDPOINT），HostToken 只来自签发器的
// per-(node,relay) 表（经 SESSION_OPEN→agent→core→stdin 下发到 xnc-host）。
// 未知字段经 json.Decode 默认忽略，等效剥离。
type desktopReq struct {
	WTSSession uint32 `json:"wtsSession"`
}

// desktopStart 处理 POST /api/nodes/{id}/desktop（RTV，relay-plane P1）：
// ① RTV endpoint 检查——XNC_RTV_ENDPOINT 未配置时 503 RTV_UNCONFIGURED；
// ② 4KB 体上限 + 白名单解码；
// ③ host 张票签发/复用（(node,relay) 粒度字节等值——core StartCapture
// 幂等复用运行中的 host，同节点第二个 viewer 不得触发换血）；
// ④ 服务端构造 DesktopParams（StreamEndpoint/HostToken/WTSSession）；
// ⑤ 委托 startSession（RBAC、并发上限、审计同现状）；
// ⑥ 202 响应：viewer 张票（会话粒度，覆盖 token 字段）+ viewer 腿地址 +
// lease 判定 + 被动首约；会话终局经 finishExtra 触发 relay 撤销（墓碑+
// 断连，spec §3.4）。
func (h *handlers) desktopStart(w http.ResponseWriter, r *http.Request) {
	if h.cfg.RTVStreamEndpoint == "" || h.rtv == nil || h.rtvSign == nil {
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

	// relay-plane 分配：node-sticky + 负载打分选外部中继；池空/全不合格
	// 回落 relay-0（内嵌，现行为）。rid 决定两张票的归属与 StreamEndpoint。
	rid := rtv.EmbeddedRelayID
	assignRelayID := rtv.EmbeddedRelayID
	assignRegion := "embedded"
	var candidates []proto.EndpointDesc
	if h.pool != nil {
		if a, ok := h.pool.Assign(nodeID); ok {
			rid = a.RelayID
			assignRelayID = a.RelayID
			assignRegion = a.Region
			candidates = a.Endpoints
		}
	}
	hostLeg := h.cfg.RTVStreamEndpoint
	if rid != rtv.EmbeddedRelayID {
		if hl := relayHostLeg(candidates); hl != "" {
			hostLeg = hl
		}
	}

	hostToken := h.rtvSign.HostTicketFor(nodeID, rid)
	params, err := json.Marshal(proto.DesktopParams{
		StreamEndpoint: hostLeg,   // relay-0 = server config；外部 = 归属 relay 的 host 腿
		HostToken:      hostToken, // 签发器 (node,relay) 表；绝不回显给客户端
		WTSSession:     req.WTSSession,
		// 纯 IP relay 自签证书的过渡态：P1 暂以 insecure 联调（T7 换
		// certSha256 钉扎后收紧）；relay-0 仍按 server config。
		TLSInsecure: h.cfg.RTVInsecureTLS || rid != rtv.EmbeddedRelayID,
	})
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "encode params"))
		return
	}

	// viewer 腿地址按请求同源推导（生产 = xnc.app 经 caddy/UDP443；dev =
	// 本机直连）。HostToken/StreamEndpoint 的 endpoint 形态可不同（endpoint
	// 是 host 腿 QUIC 地址，可能经独立端口映射）。
	wtHost := r.Host
	if p := h.cfg.RTVWTPublicPort; p != "" {
		// 过渡期非规范 WT 端口（UDP443 受限时）：替换 URL 的端口部分。
		if h := hostOnly(r.Host); h != "" {
			wtHost = h + ":" + p
		}
	}
	// 候选 = 同一 relay 的传输变体（relay-plane spec §3.2：跨 relay 盲试是
	// 死路——host 单宿主）。外部 relay 用注册端点；relay-0 按请求同源推导。
	// 兼容字段 wtUrl/wsUrl = 首个对应传输候选的派生 URL（旧 web 零改动）。
	var wtURL, wsURL string
	if rid != rtv.EmbeddedRelayID {
		ordered := orderCandidates(candidates)
		// 标注归属中继（观测信息；web 统计面板展示当前中继）。
		for i := range ordered {
			ordered[i].RelayID = assignRelayID
			ordered[i].Region = assignRegion
		}
		candidates = ordered
		wtHostPort := candidateHostPort(ordered, "wt")
		if wtHostPort != "" {
			wtURL = "https://" + wtHostPort + "/wt"
		}
		if wsHostPort := candidateHostPort(ordered, "ws"); wsHostPort != "" {
			wsURL = "wss://" + wsHostPort + "/ws"
		}
	} else {
		wtURL = "https://" + wtHost + "/wt"
		wsURL = "wss://" + r.Host + "/ws"
		host, _, _ := net.SplitHostPort(r.Host)
		if host == "" {
			host = r.Host
		}
		wtPort := 443
		if p := h.cfg.RTVWTPublicPort; p != "" {
			if n, err := strconv.Atoi(p); err == nil {
				wtPort = n
			}
		}
		candidates = []proto.EndpointDesc{
			{Transport: "wt", Host: host, Port: wtPort, Path: "/wt",
				RelayID: rtv.EmbeddedRelayID, Region: "embedded"},
			{Transport: "ws", Host: host, Port: 443, Path: "/ws",
				RelayID: rtv.EmbeddedRelayID, Region: "embedded"},
		}
	}

	name := h.viewerName(r)
	h.startSession(w, r, proto.KindDesktop, params, "desktop.open", "desktop.close",
		map[string]any{"wtUrl": wtURL, "wsUrl": wsURL},
		nil,
		func(res *session.CreateResult) map[string]any {
			out := map[string]any{"lease": map[string]any{
				// lease 判定随 202 下发（manager 侧簿记，granted=false =
				// view-only 提示；实际门控以 relay 仲裁机的 controlState 为准）。
				"granted": res.LeaseGranted, "leaseId": res.LeaseID,
			}}
			// viewer 张票（会话粒度）覆盖响应 token：web/rtvload 不感知变化，
			// 仍以 ?token= 连腿。P1 caps：operator 恒可控可输入（RBAC 门已由
			// startSession 把守），能力差异化留给后续策略演进。
			vtok, err := h.rtvSign.ViewerTicket(res.Session.ID, nodeID,
				rid, name, true, true, viewerTicketTTL)
			if err != nil {
				slog.Error("rtv: mint viewer ticket failed", "err", err)
				return out
			}
			out["token"] = vtok
			// 被动首约（manager 先到先得语义的 relay 侧等价物）：仅空闲时
			// 授予。外部 relay 的仲裁机在远端，P1 无下发通道——外部会话以
			// 显式 takeControl 为准（P2 经控制连接补 Grant 下发）。
			if rid == rtv.EmbeddedRelayID {
				h.rtv.Hub.Arbiter().Grant(nodeID, res.Session.ID, name)
			}
			out["candidates"] = candidates
			return out
		},
		func(sessionID, reason string) {
			// 会话终局 → 归属 relay 撤销：墓碑（防可重放票复活）+ 断连
			// （spec §3.4）。外部 relay 经控制连接下发；relay-0 进程内直调。
			if rid != rtv.EmbeddedRelayID {
				h.pool.SessionKill(rid, nodeID, sessionID, reason)
			} else {
				h.rtv.KillSession(nodeID, sessionID, reason)
			}
		})
}

// viewerName 解析当前用户的显示名（随 viewer 票走——relay 无 DB，
// controlState 广播的名字只能来自签发时）。回退 email。
func (h *handlers) viewerName(r *http.Request) string {
	u := auth.UserFrom(r.Context())
	name := u.Email
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if usr, err := h.st.Q().GetUserByID(ctx, u.ID); err == nil && usr.DisplayName != "" {
		name = usr.DisplayName
	}
	return name
}

// relayHostLeg 从候选里取 host 腿地址（transport=quic）。
func relayHostLeg(eps []proto.EndpointDesc) string {
	for _, e := range eps {
		if e.Transport == "quic" && e.Host != "" {
			return net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
		}
	}
	return ""
}

// orderCandidates 排序：wt 优先、ws 次之、quic（host 腿，viewer 不可用）
// 不进候选。
func orderCandidates(eps []proto.EndpointDesc) []proto.EndpointDesc {
	out := make([]proto.EndpointDesc, 0, len(eps))
	for _, e := range eps {
		if e.Transport == "wt" {
			out = append(out, e)
		}
	}
	for _, e := range eps {
		if e.Transport == "ws" {
			out = append(out, e)
		}
	}
	return out
}

// candidateHostPort 取指定传输候选的 host:port（无 = 空串）。
func candidateHostPort(eps []proto.EndpointDesc, transport string) string {
	for _, e := range eps {
		if e.Transport == transport && e.Host != "" && e.Port > 0 {
			return net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
		}
	}
	return ""
}

// hostOnly 剥离 Host 头的 :port 部分（空 = 形态异常，调用方保底原样）。
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// rtvStats 管理端观测面（原 MVP /statsz 收权版本：JWT admin 组内挂载）。
func (h *handlers) rtvStats(w http.ResponseWriter, _ *http.Request) {
	if h.rtv == nil {
		respondJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	snap := h.rtv.Hub.Snapshot()
	snap["relayId"] = rtv.EmbeddedRelayID
	if h.pool != nil {
		snap["relays"] = h.pool.Snapshot()
	}
	respondJSON(w, http.StatusOK, snap)
}
