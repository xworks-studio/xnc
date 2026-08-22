package api

import (
	"encoding/json"
	"net/http"

	"xnc/proto"
)

// tunnelBodyMaxBytes：tunnel 请求体上限 4KB（仅 target 一个短字段）。
const tunnelBodyMaxBytes = 4 * 1024

// tunnelTargets：服务端白名单——target 名解析为固定 host/port。agent 只信任
// SESSION_OPEN params 里的 host/port（不接受 agent 侧自选目标）。
var tunnelTargets = map[string]struct {
	Host string
	Port int
}{
	"rdp": {"127.0.0.1", 3389},
}

// tunnelStart 处理 POST /api/nodes/{id}/tunnel：target 白名单校验 → startSession
// （KindTunnel）。SESSION_OPEN params 不直接用 TunnelParams，而是展开为
// {"target","host","port"} 完整 map——agent 拨号需要 host/port。审计 open/close
// 同名 "rdp.open"（同 file.* 的 v1 §7.6 策略）。
func (h *handlers) tunnelStart(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, tunnelBodyMaxBytes)
	var req struct {
		Target string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}
	t, ok := tunnelTargets[req.Target]
	if !ok {
		respondError(w, proto.Err(400, proto.CodeInternal, "unknown tunnel target"))
		return
	}
	params, err := json.Marshal(map[string]any{
		"target": req.Target, "host": t.Host, "port": t.Port,
	})
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "encode params"))
		return
	}
	h.startSession(w, r, proto.KindTunnel, params, "rdp.open", "rdp.open", nil)
}
