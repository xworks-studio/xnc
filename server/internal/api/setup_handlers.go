// setup_handlers.go — 安装器分发端点：/setup.exe + /setup.json（设计 §4）。
//
// Inno Setup 安装器存于 release store（与 cli 制品同库同表，固定名
// setup.exe）。/setup.exe 直流最新安装器（200 流式 + X-Xnc-Sha256 完整性
// 头，交付风格与 cli 制品一致）；/setup.json 动态生成版本清单
// {version, url, sha256, size, releasedAt}（无需入库），供 xnc upgrade
// --check、CI 与编排工具消费。两端点均无认证——安装器是产品首次下载入口
// （官网/README 统一 https://xnc.app/setup.exe）。
package api

import (
	"context"
	"net/http"

	"xnc/proto"
	"xnc/server/internal/db/sqlc"
)

// setupArtifactName — release 内 Inno Setup 安装器制品的固定命名。
const setupArtifactName = "setup.exe"

// setupChannel 解析 channel 查询参数：缺省 stable；stable|dev 之外的取值
// 返回 false（该频道从未有 release，按 404 处理，与"无 release"语义一致）。
func setupChannel(r *http.Request) (string, bool) {
	ch := r.URL.Query().Get("channel")
	if ch == "" {
		ch = "stable"
	}
	if ch != "stable" && ch != "dev" {
		return "", false
	}
	return ch, true
}

// latestSetupArtifact 解析频道 → 最新 release → setup.exe 制品；任一环节
// 未命中（无 release / 无 setup 制品，如历史 bundle-only release）返回 404。
func (h *handlers) latestSetupArtifact(ctx context.Context, channel string) (sqlc.Release, sqlc.ReleaseArtifact, *proto.APIError) {
	rel, err := h.st.Q().GetLatestReleaseByChannel(ctx, channel)
	if err != nil {
		return rel, sqlc.ReleaseArtifact{}, proto.Err(404, "NOT_FOUND", "no releases")
	}
	art, err := h.st.Q().GetArtifact(ctx, sqlc.GetArtifactParams{ReleaseID: rel.ID, Name: setupArtifactName})
	if err != nil {
		return rel, sqlc.ReleaseArtifact{}, proto.Err(404, "NOT_FOUND", "no setup artifact")
	}
	return rel, art, nil
}

// setupDownload — GET /setup.exe?channel=stable|dev：频道最新安装器直流。
// URL 保持短稳定（agent 更新契约不变），正式文件名经 Content-Disposition
// 下发（XNC-Setup[-dev]-<version>.exe，浏览器另存为所见即所得）。
func (h *handlers) setupDownload(w http.ResponseWriter, r *http.Request) {
	channel, ok := setupChannel(r)
	if !ok {
		respondError(w, proto.Err(404, "NOT_FOUND", "unknown channel"))
		return
	}
	rel, art, aerr := h.latestSetupArtifact(r.Context(), channel)
	if aerr != nil {
		respondError(w, aerr)
		return
	}
	suffix := ""
	if channel == "dev" {
		suffix = "-dev"
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition",
		`attachment; filename="XNC-Installer`+suffix+`-`+rel.Version+`.exe"`)
	w.Header().Set("X-Xnc-Sha256", art.Sha256)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(art.Data)
}

// setupManifest — GET /setup.json?channel=：动态版本清单。url 为同源绝对
// 地址（scheme 推导与 wsBaseURL 同法：X-Forwarded-Proto > r.TLS > 明文），
// 指向 /setup.exe?channel=<ch>，curl -L / 浏览器 / 编排工具可直接消费。
func (h *handlers) setupManifest(w http.ResponseWriter, r *http.Request) {
	channel, ok := setupChannel(r)
	if !ok {
		respondError(w, proto.Err(404, "NOT_FOUND", "unknown channel"))
		return
	}
	rel, art, aerr := h.latestSetupArtifact(r.Context(), channel)
	if aerr != nil {
		respondError(w, aerr)
		return
	}
	scheme := "http"
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" {
		scheme = "https"
	} else if r.TLS != nil {
		scheme = "https"
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"version":    rel.Version,
		"url":        scheme + "://" + r.Host + "/installer?channel=" + channel,
		"sha256":     art.Sha256,
		"size":       art.Size,
		"releasedAt": rel.CreatedAt,
	})
}

// ---- server 镜像分发（设计文档 §4：CI 只发布文件，服务器脚本拉取）----

// serverImageArtifactName — release 内 server 镜像 tar.gz 制品的固定命名
//（channel=server 的 release 由 build-server.yml 上传）。
const serverImageArtifactName = "server-image.tar.gz"

// serverImageDownload — GET /server-image?channel=server：频道最新 server
// 镜像 tar.gz 直流。响应头 X-Xnc-Version（拉取脚本与本地运行版本比对用，
// 免解析 manifest）+ X-Xnc-Sha256。无认证——与 /installer 同级的产品分发面。
func (h *handlers) serverImageDownload(w http.ResponseWriter, r *http.Request) {
	ch := r.URL.Query().Get("channel")
	if ch == "" {
		ch = "server"
	}
	if ch != "server" {
		respondError(w, proto.Err(404, "NOT_FOUND", "unknown channel"))
		return
	}
	rel, err := h.st.Q().GetLatestReleaseByChannel(r.Context(), ch)
	if err != nil {
		respondError(w, proto.Err(404, "NOT_FOUND", "no releases"))
		return
	}
	art, err := h.st.Q().GetArtifact(r.Context(), sqlc.GetArtifactParams{ReleaseID: rel.ID, Name: serverImageArtifactName})
	if err != nil {
		respondError(w, proto.Err(404, "NOT_FOUND", "no server-image artifact"))
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("X-Xnc-Version", rel.Version)
	w.Header().Set("X-Xnc-Sha256", art.Sha256)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(art.Data)
}
