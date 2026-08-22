package api

import (
	"encoding/json"
	"net/http"

	"xnc/proto"
)

const (
	// shellBodyMaxBytes：shell 请求体上限 4KB——cols/rows/shell 三个字段
	// 远小于此，先于解码生效，杜绝无限缓冲。
	shellBodyMaxBytes = 4 * 1024
	// 服务端缺省终端尺寸（请求未给出 cols/rows 时写入 Params）。
	shellDefaultCols = 120
	shellDefaultRows = 30
	// cols/rows 合法区间；0 视为缺省，缺省后再落入此区间。
	shellDimMax = 1000
)

// shellStart 处理 POST /api/nodes/{id}/shell：4KB 体上限 + cols/rows/shell
// 校验 → 委托 startSession（KindShell，审计 shell.open/shell.close）。
// Params 经 proto.ShellParams 序列化（omitempty：非零 cols/rows 均写出，
// 0 已在入口替换为缺省 120/30；shell 空串省略，由 agent 探测决定）。
func (h *handlers) shellStart(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, shellBodyMaxBytes)
	var req struct {
		Cols  int    `json:"cols"`
		Rows  int    `json:"rows"`
		Shell string `json:"shell"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}
	if req.Cols == 0 {
		req.Cols = shellDefaultCols
	}
	if req.Rows == 0 {
		req.Rows = shellDefaultRows
	}
	switch {
	case req.Cols < 1 || req.Cols > shellDimMax, req.Rows < 1 || req.Rows > shellDimMax:
		respondError(w, proto.Err(400, proto.CodeInternal, "cols/rows must be 1-1000"))
		return
	case req.Shell != "" && req.Shell != "pwsh" && req.Shell != "powershell":
		respondError(w, proto.Err(400, proto.CodeInternal, "shell must be pwsh or powershell"))
		return
	}
	params, err := json.Marshal(proto.ShellParams{
		Cols: req.Cols, Rows: req.Rows, Shell: req.Shell,
	})
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "encode params"))
		return
	}
	h.startSession(w, r, proto.KindShell, params, "shell.open", "shell.close", nil)
}
