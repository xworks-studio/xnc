// update_handlers.go — 自更新 REST 面：admin 上传/灰度 + agent bundle
// 下载（令牌） + CLI latest/下载（用户 JWT）。
//
// 上传方是部署流水线（构建机 curl，admin JWT）——CLI 面向使用者不提供
// 上传命令。制品经 multipart/form-data：version、notes、bundle（tar.gz，
// 含 xnc-agent.exe + xnc-core/desktop/shell.exe + manifest.json）、setup
// （可选 Inno Setup 安装器，固定制品名 setup.exe，设计 §4/§12：一次发布
// 同时携带安装器与最后过渡 bundle，§14 迁移期形态）、cli（可选
// xnc-windows-amd64.exe）。服务端校验 bundle 内容与 manifest 哈希一致
// 后入库（bytea，随 pgdata 备份走）。
package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/db/sqlc"
)

const maxUploadBytes = 128 << 20 // bundle+cli 合计上限

type bundleManifest struct {
	Version string `json:"version"`
	Files   []struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	} `json:"files"`
}

// verifyBundle 解包校验：tar.gz 内 manifest.json 与必需 exe 齐全，逐文件
// 哈希与 manifest 一致，manifest 版本与声称版本一致。返回规范化后的
// 文件名集合（防路径穿越：仅接受扁平文件名）。
func verifyBundle(r io.Reader, version string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Name != filepath_Base(hdr.Name) {
			return proto.Err(400, proto.CodeInternal, "bundle contains non-flat entry: "+hdr.Name)
		}
		if hdr.Size > 64<<20 {
			return proto.Err(400, proto.CodeInternal, "bundle entry too large")
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		files[hdr.Name] = b
	}
	var mf bundleManifest
	if err := json.Unmarshal(files["manifest.json"], &mf); err != nil {
		return proto.Err(400, proto.CodeInternal, "bundle missing/invalid manifest.json")
	}
	if mf.Version != version {
		return proto.Err(400, proto.CodeInternal, "manifest version mismatch")
	}
	if _, ok := files["xnc-agent.exe"]; !ok {
		return proto.Err(400, proto.CodeInternal, "bundle missing xnc-agent.exe")
	}
	// 多余文件容忍（旧 bundle 含已退役的 xnc-screen-helper.exe 仍可通过）。
	for _, f := range mf.Files {
		b, ok := files[f.Name]
		if !ok {
			return proto.Err(400, proto.CodeInternal, "manifest references missing file: "+f.Name)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			return proto.Err(400, proto.CodeInternal, "sha256 mismatch: "+f.Name)
		}
	}
	return nil
}

func filepath_Base(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '/' || name[i] == '\\' {
			return name[i+1:]
		}
	}
	return name
}

// adminUploadRelease — POST /api/admin/releases（multipart）。
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

	bf, _, err := r.FormFile("bundle")
	if err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "bundle file required"))
		return
	}
	defer bf.Close()
	bundleBytes, err := io.ReadAll(bf)
	if err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "read bundle: "+err.Error()))
		return
	}
	if err := verifyBundle(bytes.NewReader(bundleBytes), version); err != nil {
		if ae, ok := err.(*proto.APIError); ok {
			respondError(w, ae)
		} else {
			respondError(w, proto.Err(400, proto.CodeInternal, err.Error()))
		}
		return
	}

	// setup（可选）：Inno Setup 安装器（PE 镜像，MZ 魔数兜底校验）。读入并
	// 校验先于 CreateRelease——非法输入不留半截 release。
	var setupBytes []byte
	if sf, _, serr := r.FormFile("setup"); serr == nil {
		defer sf.Close()
		setupBytes, serr = io.ReadAll(sf)
		if serr != nil {
			respondError(w, proto.Err(400, proto.CodeInternal, "read setup: "+serr.Error()))
			return
		}
		if len(setupBytes) < 2 || setupBytes[0] != 'M' || setupBytes[1] != 'Z' {
			respondError(w, proto.Err(400, proto.CodeInternal, "setup is not a Windows executable (MZ)"))
			return
		}
	} else if !errors.Is(serr, http.ErrMissingFile) {
		respondError(w, proto.Err(400, proto.CodeInternal, "read setup part: "+serr.Error()))
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
	sum := sha256.Sum256(bundleBytes)
	if err := h.st.Q().PutArtifact(ctx, sqlc.PutArtifactParams{
		ReleaseID: rel.ID, Name: bundleArtifactName,
		Sha256: hex.EncodeToString(sum[:]), Size: int64(len(bundleBytes)), Data: bundleBytes,
	}); err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "store bundle: "+err.Error()))
		return
	}

	// setup.exe 与 bundle 同 release 并存（§14 迁移期）：maybeOfferUpdate
	// 按制品偏好决策——release 带 setup.exe 则全员推 UPDATE_AVAILABLE
	// （安装器编排；存量 bundle agent 对其前向兼容忽略），仅 bundle-only
	// release 才走遗留 UPDATE_OFFER（存量节点的最后升级通道）。
	// /setup.exe 与 /setup.json 由 setup_handlers 动态服务该制品。
	if setupBytes != nil {
		ssum := sha256.Sum256(setupBytes)
		if err := h.st.Q().PutArtifact(ctx, sqlc.PutArtifactParams{
			ReleaseID: rel.ID, Name: setupArtifactName,
			Sha256: hex.EncodeToString(ssum[:]), Size: int64(len(setupBytes)), Data: setupBytes,
		}); err != nil {
			respondError(w, proto.Err(500, proto.CodeInternal, "store setup: "+err.Error()))
			return
		}
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
//   {version, nodeId}                    pin 节点到版本 + 强制 OFFER
//   {nodeId, channel: "dev"}             切节点频道 + 强制 OFFER（新频道的最新）
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

	// 强制即时 OFFER：节点在线则立刻下发。
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

// agentBundleDownload — GET /api/agent/bundle?token=&node=（OFFER 令牌）。
// node 参数为节点 ID（agent 知道自己的 NodeID），令牌与之绑定校验。
func (h *handlers) agentBundleDownload(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")
	nodeID, err := uuid.Parse(r.URL.Query().Get("node"))
	if err != nil {
		respondError(w, proto.Err(401, "UNAUTHORIZED", "invalid node"))
		return
	}
	releaseID, ok := consumeDownloadToken(tok, nodeID)
	if !ok {
		respondError(w, proto.Err(401, "UNAUTHORIZED", "invalid or expired token"))
		return
	}
	art, err := h.st.Q().GetArtifact(r.Context(), sqlc.GetArtifactParams{ReleaseID: releaseID, Name: bundleArtifactName})
	if err != nil {
		respondError(w, proto.Err(404, "NOT_FOUND", "bundle not found"))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Xnc-Sha256", art.Sha256)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(art.Data)
}

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
