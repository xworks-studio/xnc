package api

import (
	"encoding/json"
	"net/http"

	"xnc/proto"
)

const (
	// screenBodyMaxBytes：screen 请求体上限 4KB——fps/quality/maxWidth 三个
	// 数字字段远小于此，先于解码生效，杜绝无限缓冲。
	screenBodyMaxBytes = 4 * 1024
	// 缺省值（请求未给出或给 0 时写入 Params）。
	screenDefaultFps      = 15
	screenDefaultQuality  = 60
	screenDefaultMaxWidth = 1920
	// 合法上限；0 视为缺省，缺省后落入区间，负数与超限均 400。
	screenFpsMax      = 30
	screenQualityMax  = 100
	screenMaxWidthCap = 1920
)

// screenStart 处理 POST /api/nodes/{id}/screen：4KB 体上限 + fps/quality/
// maxWidth 校验（0 → 缺省 15/60/1920）→ 委托 startSession（KindScreen，
// 审计 screen.open/screen.close，RBAC operator+ 由 startSession 统一判定）。
func (h *handlers) screenStart(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, screenBodyMaxBytes)
	var req struct {
		Fps      int  `json:"fps"`
		Quality  int  `json:"quality"`
		MaxWidth int  `json:"maxWidth"`
		Snapshot bool `json:"snapshot"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}
	if req.Fps == 0 {
		req.Fps = screenDefaultFps
	}
	if req.Quality == 0 {
		req.Quality = screenDefaultQuality
	}
	if req.MaxWidth == 0 {
		req.MaxWidth = screenDefaultMaxWidth
	}
	switch {
	case req.Fps < 1 || req.Fps > screenFpsMax:
		respondError(w, proto.Err(400, proto.CodeInternal, "fps must be 1-30"))
		return
	case req.Quality < 1 || req.Quality > screenQualityMax:
		respondError(w, proto.Err(400, proto.CodeInternal, "quality must be 1-100"))
		return
	case req.MaxWidth < 1 || req.MaxWidth > screenMaxWidthCap:
		respondError(w, proto.Err(400, proto.CodeInternal, "maxWidth must be 1-1920"))
		return
	}
	params, err := json.Marshal(proto.ScreenParams{
		Fps: req.Fps, Quality: req.Quality, MaxWidth: req.MaxWidth,
		Snapshot: req.Snapshot,
	})
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "encode params"))
		return
	}
	h.startSession(w, r, proto.KindScreen, params, "screen.open", "screen.close", nil, nil, nil, nil)
}
