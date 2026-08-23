package api

import (
	"encoding/json"
	"net/http"
	"regexp"

	"xnc/proto"
)

var (
	// absPathRe：Windows 绝对路径——盘符（C:\...）或 UNC（\\server\...）前缀。
	absPathRe = regexp.MustCompile(`^[A-Za-z]:\\.+|^\\\\.+`)
	// sha256Re：64 位小写 hex。
	sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

const (
	// fileBodyMaxBytes：file 请求体上限 4KB——path+size+sha256 远小于此，
	// 先于解码生效，杜绝无限缓冲。
	fileBodyMaxBytes = 4 * 1024
	// fileMaxBytes：上传尺寸上限 256MB（0 < size ≤ 此值）。
	fileMaxBytes = 256 * 1024 * 1024
)

// fileReq upload/download 共用请求体：upload 必带 size+sha256，download 只带 path。
type fileReq struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Sha256 string `json:"sha256"`
}

// fileUpload 处理 POST /api/nodes/{id}/files/upload：4KB 体上限 + path 绝对路径 +
// size ∈ (0, 256MB] + sha256 64 hex 校验 → 委托 startSession（KindFile，Direction
// "upload"）。审计 action 按 v1 §7.6 落地：open/close 同名 "file.upload"（不采用
// exec.start/exec.finish 的 .start/.finish 变体——v1 词汇即同名，close 行以
// metadata.reason 区分终态）。
func (h *handlers) fileUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, fileBodyMaxBytes)
	var req fileReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}
	switch {
	case !absPathRe.MatchString(req.Path):
		respondError(w, proto.Err(400, proto.CodeInternal, "path must be absolute"))
		return
	case req.Size <= 0 || req.Size > fileMaxBytes:
		respondError(w, proto.Err(400, proto.CodeFileTooLarge, "size out of range (1B-256MB)"))
		return
	case !sha256Re.MatchString(req.Sha256):
		respondError(w, proto.Err(400, proto.CodeInternal, "sha256 must be 64 hex chars"))
		return
	}
	params, err := json.Marshal(proto.FileParams{
		Direction: "upload", Path: req.Path, Size: req.Size, Sha256: req.Sha256,
	})
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "encode params"))
		return
	}
	h.startSession(w, r, proto.KindFile, params, "file.upload", "file.upload", nil, nil)
}

// fileDownload 处理 POST /api/nodes/{id}/files/download：仅 path 绝对路径校验
// → startSession（KindFile，Direction "download"）。审计 open/close 同名
// "file.download"，同 fileUpload 的 v1 §7.6 策略。
func (h *handlers) fileDownload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, fileBodyMaxBytes)
	var req fileReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}
	if !absPathRe.MatchString(req.Path) {
		respondError(w, proto.Err(400, proto.CodeInternal, "path must be absolute"))
		return
	}
	params, err := json.Marshal(proto.FileParams{
		Direction: "download", Path: req.Path,
	})
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "encode params"))
		return
	}
	h.startSession(w, r, proto.KindFile, params, "file.download", "file.download", nil, nil)
}
