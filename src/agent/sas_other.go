//go:build !windows

// sas_other.go — 非 Windows:SAS(安全注意力序列)是 Windows 概念
// (XNCCore 0x0110 → sas.dll SendSAS),立即回"不支持"的 SAS_RESULT,
// 不让 server 侧等待方挂到超时。
package agent

import (
	"context"
	"log/slog"

	"xnc/proto"
)

func (a *Agent) handleSasRequest(ctx context.Context, sr proto.SasRequest) {
	res := proto.SasResult{ReqID: sr.ReqID, Err: "sas: unsupported on this platform"}
	a.sendSasResult(ctx, res, slog.Default().With("reqId", sr.ReqID))
}
