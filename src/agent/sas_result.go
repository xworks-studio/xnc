// sas_result.go — SAS_RESULT 回执发送(平台无关):Windows 实现在 sas.go,
// 非 Windows 桩在 sas_other.go,共用本发送路径。
package agent

import (
	"context"
	"log/slog"

	"xnc/proto"
)

// sendSasResult 经当前控制连接回 SAS_RESULT；连接已死则只记日志
// （server 侧等待方按超时收场，不会悬挂——correlator 有 TTL）。
func (a *Agent) sendSasResult(ctx context.Context, res proto.SasResult, log *slog.Logger) {
	log.Info("sas: request handled", "ok", res.OK, "hr", res.HR, "code", res.Code, "err", res.Err)
	if ctx.Err() != nil {
		log.Warn("sas: context done before result could be sent")
		return
	}
	conn := a.currentConn()
	if conn == nil {
		log.Warn("sas: no live control connection for result")
		return
	}
	m, err := proto.NewMsg(proto.TypeSasResult, res)
	if err != nil {
		log.Error("sas: encode result", "err", err)
		return
	}
	if err := conn.CurrentSend()(m); err != nil {
		log.Warn("sas: send result failed", "err", err)
	}
}
