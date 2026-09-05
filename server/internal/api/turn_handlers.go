package api

// turn_handlers.go — GET /api/turn/status（用户 JWT）：TURN 服务状态 +
// 聚合用量（设计 §3.1）。username/credential 与会话下发同源，仅登录可见
//（浏览器回环探测分配 relay 候选用）；绝不入日志。

import (
	"net/http"

	"xnc/proto"
)

// turnStatus — GET /api/turn/status。
func (h *handlers) turnStatus(w http.ResponseWriter, r *http.Request) {
	mode := "urls"
	var pool []map[string]any
	if h.turnPool != nil {
		for _, s := range h.turnPool.Status() {
			mode = "pool"
			pool = append(pool, map[string]any{
				"ip": s.IP, "port": s.Port, "urls": s.URLs, "healthy": s.Healthy,
			})
		}
	}
	if pool == nil {
		pool = []map[string]any{}
	}
	fallback := h.cfg.TurnURLs
	if fallback == nil {
		fallback = []string{}
	}
	if mode == "urls" && len(fallback) == 0 {
		mode = "unconfigured"
	}
	// ICE 策略归一化（与 config.icePolicy 同法 fail-closed）：TestEnv 等直构
	// config 不经 Load()，零值 "" 必须归到 "relay"，端点契约恒定。
	policy := h.cfg.DesktopICEPolicy
	if policy != "all" {
		policy = "relay"
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"mode":                  mode,
		"icePolicy":             policy,
		"pool":                  pool,
		"fallbackUrls":          fallback,
		"username":              h.cfg.TurnUsername,
		"credential":            h.cfg.TurnCredential,
		"activeDesktopSessions": h.sess.CountActive(proto.KindDesktop),
	})
}
