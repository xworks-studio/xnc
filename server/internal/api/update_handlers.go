// update_handlers.go — 自更新 REST 面：admin 上传/灰度 + CLI latest/下载
// （用户 JWT）。
//
// 上传方是部署流水线（构建机 curl，admin JWT）——CLI 面向使用者不提供
// 上传命令。制品经 multipart/form-data：version、notes、setup（Inno Setup
// 安装器，固定制品名 setup.exe，设计 §4/§12）、cli（可选
// xnc-windows-amd64.exe）。服务端校验后入库（bytea，随 pgdata 备份走）。
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/db/sqlc"
)

const maxUploadBytes = 128 << 20 // setup+cli 合计上限

// adminUploadRelease — POST /api/admin/releases（multipart）。
// 终态形态（未上线直采，设计 §12/§14）：setup（必填，Inno Setup 安装器，
// 固定制品名 setup.exe，MZ 魔数兜底校验）+ cli（可选
// xnc-windows-amd64.exe）。bundle 部件已随遗留更新通道退役——仍传入即
// 400 明确报错（bundle channel retired），避免流水线静默降级到坏形态。
func (h *handlers) adminUploadRelease(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !isAdminUser(r.Context(), h.st, u.ID) {
		respondError(w, proto.Err(403, proto.CodeForbidden, "admin required"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "invalid multipart: "+err.Error()))
		return
	}
	version := r.FormValue("version")
	if version == "" {
		respondError(w, proto.Err(400, proto.CodeInternal, "version required"))
		return
	}
	notes := r.FormValue("notes")

	// bundle 部件已退役：明确拒绝而非忽略。
	if len(r.MultipartForm.File["bundle"]) > 0 {
		respondError(w, proto.Err(400, proto.CodeInternal, "bundle channel retired; upload setup.exe (installer) only"))
		return
	}

	// setup（必填）：读入并校验先于 CreateRelease——非法输入不留半截 release。
	sf, _, err := r.FormFile("setup")
	if err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "setup file required"))
		return
	}
	defer sf.Close()
	setupBytes, err := io.ReadAll(sf)
	if err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "read setup: "+err.Error()))
		return
	}
	if len(setupBytes) < 2 || setupBytes[0] != 'M' || setupBytes[1] != 'Z' {
		respondError(w, proto.Err(400, proto.CodeInternal, "setup is not a Windows executable (MZ)"))
		return
	}

	ctx := r.Context()
	channel := r.FormValue("channel")
	if channel == "" {
		channel = "stable"
	}
	if channel != "stable" && channel != "dev" {
		respondError(w, proto.Err(400, proto.CodeInternal, "channel must be stable or dev"))
		return
	}
	rel, err := h.st.Q().CreateRelease(ctx, sqlc.CreateReleaseParams{Version: version, Notes: notes, Channel: channel})
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "create release: "+err.Error()))
		return
	}

	// setup.exe 是安装/更新的唯一制品面：maybeOfferUpdate 仅按它推送
	// UPDATE_AVAILABLE，/setup.exe 与 /setup.json 由 setup_handlers 动态
	// 服务该制品。
	ssum := sha256.Sum256(setupBytes)
	if err := h.st.Q().PutArtifact(ctx, sqlc.PutArtifactParams{
		ReleaseID: rel.ID, Name: setupArtifactName,
		Sha256: hex.EncodeToString(ssum[:]), Size: int64(len(setupBytes)), Data: setupBytes,
	}); err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "store setup: "+err.Error()))
		return
	}

	if cf, _, cerr := r.FormFile("cli"); cerr == nil {
		defer cf.Close()
		cliBytes, rerr := io.ReadAll(cf)
		if rerr != nil {
			respondError(w, proto.Err(400, proto.CodeInternal, "read cli: "+rerr.Error()))
			return
		}
		csum := sha256.Sum256(cliBytes)
		if err := h.st.Q().PutArtifact(ctx, sqlc.PutArtifactParams{
			ReleaseID: rel.ID, Name: cliArtifactName,
			Sha256: hex.EncodeToString(csum[:]), Size: int64(len(cliBytes)), Data: cliBytes,
		}); err != nil {
			respondError(w, proto.Err(500, proto.CodeInternal, "store cli: "+err.Error()))
			return
		}
	}

	respondJSON(w, http.StatusCreated, map[string]any{
		"releaseId": rel.ID, "version": version,
	})
}

// adminListReleases — GET /api/admin/releases。
func (h *handlers) adminListReleases(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !isAdminUser(r.Context(), h.st, u.ID) {
		respondError(w, proto.Err(403, proto.CodeForbidden, "admin required"))
		return
	}
	rels, err := h.st.Q().ListReleases(r.Context())
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, err.Error()))
		return
	}
	out := make([]map[string]any, 0, len(rels))
	for _, rel := range rels {
		out = append(out, map[string]any{
			"id": rel.ID, "version": rel.Version, "notes": rel.Notes,
			"channel": rel.Channel, "createdAt": rel.CreatedAt,
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// adminDeleteRelease — DELETE /api/admin/releases/{id}。
// 删除 release 及其制品（release_artifacts 外键 ON DELETE CASCADE），latest
// 由 GetLatestReleaseByChannel 按 created_at 自动回落到剩余的最新版本。
// 删除无强约束：节点 pin（target_release 按版本字符串）指向被删版本时，
// targetReleaseFor 的 GetReleaseByVersion 查询失败即自然回退频道最新——允许
// 删、latest 回落是刻意语义（清理坏版本/历史版本用）。
func (h *handlers) adminDeleteRelease(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !isAdminUser(r.Context(), h.st, u.ID) {
		respondError(w, proto.Err(403, proto.CodeForbidden, "admin required"))
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, proto.Err(404, "NOT_FOUND", "release not found"))
		return
	}
	n, err := h.st.Q().DeleteRelease(r.Context(), id)
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, err.Error()))
		return
	}
	if n == 0 {
		respondError(w, proto.Err(404, "NOT_FOUND", "release not found"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// adminRollout — POST /api/admin/rollout：
//   {version, nodeId}                    pin 节点到版本 + 强制推送
//   {nodeId, channel: "dev"}             切节点频道 + 强制推送（新频道的最新）
//   {unpin: true}                        清除所有 pin（跟随频道最新）
func (h *handlers) adminRollout(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !isAdminUser(r.Context(), h.st, u.ID) {
		respondError(w, proto.Err(403, proto.CodeForbidden, "admin required"))
		return
	}
	var req struct {
		Version string `json:"version"`
		NodeID  string `json:"nodeId"`
		Channel string `json:"channel"`
		Unpin   bool   `json:"unpin"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "invalid body"))
		return
	}
	ctx := r.Context()

	if req.Unpin {
		if err := h.clearAllPins(ctx); err != nil {
			respondError(w, proto.Err(500, proto.CodeInternal, err.Error()))
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"unpinned": true})
		return
	}

	if req.NodeID == "" {
		respondError(w, proto.Err(400, proto.CodeInternal, "nodeId required"))
		return
	}
	nodeID, err := uuid.Parse(req.NodeID)
	if err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "invalid nodeId"))
		return
	}

	// 频道切换（可选，与版本 pin 可同时使用）。
	if req.Channel != "" {
		if req.Channel != "stable" && req.Channel != "dev" {
			respondError(w, proto.Err(400, proto.CodeInternal, "channel must be stable or dev"))
			return
		}
		if err := h.st.Q().SetNodeChannel(ctx, sqlc.SetNodeChannelParams{
			ID: nodeID, Channel: req.Channel,
		}); err != nil {
			respondError(w, proto.Err(500, proto.CodeInternal, err.Error()))
			return
		}
	}

	if req.Version == "" {
		// 仅切频道：不 pin，让节点跟随新频道的最新。
		offered := false
		if nc := h.reg.Get(nodeID.String()); nc != nil {
			node, err := h.st.Q().GetNodeByID(ctx, nodeID)
			if err == nil {
				h.maybeOfferUpdate(ctx, nodeID, node.AgentVersion, nc.Send)
				offered = true
			}
		}
		respondJSON(w, http.StatusOK, map[string]any{"channel": req.Channel, "nodeId": req.NodeID, "offeredNow": offered})
		return
	}

	if _, err := h.st.Q().GetReleaseByVersion(ctx, req.Version); err != nil {
		respondError(w, proto.Err(404, "NOT_FOUND", "release not found"))
		return
	}
	if err := h.st.Q().SetNodeTargetRelease(ctx, sqlc.SetNodeTargetReleaseParams{
		ID: nodeID, TargetRelease: pgText(req.Version),
	}); err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, err.Error()))
		return
	}

	// 强制即时推送：节点在线则立刻下发。
	offered := false
	if nc := h.reg.Get(nodeID.String()); nc != nil {
		node, err := h.st.Q().GetNodeByID(ctx, nodeID)
		if err == nil {
			h.maybeOfferUpdate(ctx, nodeID, node.AgentVersion, nc.Send)
			offered = true
		}
	}
	respondJSON(w, http.StatusOK, map[string]any{"pinned": req.Version, "nodeId": req.NodeID, "offeredNow": offered})
}

// clearAllPins — v1 的 unpin：清除全部节点 target_release（跟随最新）。
func (h *handlers) clearAllPins(ctx context.Context) error {
	_, err := h.st.Pool().Exec(ctx, `UPDATE nodes SET target_release = NULL`)
	return err
}

// pgText — string → pgtype.Text。
func pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

// cliLatest — GET /api/cli/latest?channel=（用户 JWT）：CLI 自更新元信息。
func (h *handlers) cliLatest(w http.ResponseWriter, r *http.Request) {
	channel := r.URL.Query().Get("channel")
	if channel == "" {
		channel = "stable"
	}
	rel, err := h.st.Q().GetLatestReleaseByChannel(r.Context(), channel)
	if err != nil {
		respondError(w, proto.Err(404, "NOT_FOUND", "no releases"))
		return
	}
	art, err := h.st.Q().GetArtifact(r.Context(), sqlc.GetArtifactParams{ReleaseID: rel.ID, Name: cliArtifactName})
	if err != nil {
		respondError(w, proto.Err(404, "NOT_FOUND", "no cli artifact in latest release"))
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"version": rel.Version, "sha256": art.Sha256, "size": art.Size, "channel": channel,
	})
}

// cliDownload — GET /api/cli/download?channel=（用户 JWT）：CLI 二进制。
func (h *handlers) cliDownload(w http.ResponseWriter, r *http.Request) {
	channel := r.URL.Query().Get("channel")
	if channel == "" {
		channel = "stable"
	}
	rel, err := h.st.Q().GetLatestReleaseByChannel(r.Context(), channel)
	if err != nil {
		respondError(w, proto.Err(404, "NOT_FOUND", "no releases"))
		return
	}
	art, err := h.st.Q().GetArtifact(r.Context(), sqlc.GetArtifactParams{ReleaseID: rel.ID, Name: cliArtifactName})
	if err != nil {
		respondError(w, proto.Err(404, "NOT_FOUND", "no cli artifact"))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Xnc-Sha256", art.Sha256)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(art.Data)
}
