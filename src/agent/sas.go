//go:build windows

// sas.go — SAS_REQUEST 的 agent 侧处理（2026-09-17 安全桌面交互）。
//
// 链路：web 工具栏"发送 Ctrl+Alt+Del" → server REST(RBAC+审计) → 控制连接
// SAS_REQUEST → 本文件 → XNCCore 0x0110（服务态门常开；core 幂等开启
// SoftwareSASGeneration 后调 sas.dll!SendSAS）→ SAS_RESULT 回执（reqId 关联）。
// host 侧的采集/输入桌面跟随让安全桌面（登录/锁屏/UAC）在流内可见可控，
// 本文件不涉及——见 src/host 的 desktop 跟随实现。
package agent

import (
	"context"
	"errors"
	"log/slog"

	"xnc/agent/coreclient"
	"xnc/proto"
)

// handleSasRequest 处理一条 SAS_REQUEST：解析 core 凭据（与 desktop 同源：
// env → 生产缺省 \\.\pipe\xnc-core + <StateDir>\core-secret.hex）→ 拨号 →
// SendSAS(reason) → 结果经当前控制连接回 SAS_RESULT。
// 回执语义：OK=true 仅表示 core 未拒绝且 SendSAS 未抛异常；hr=0 是
// "调用正常返回"而非"SAS 已送达"的证明（T6 验收以安全桌面出现为准）。
func (a *Agent) handleSasRequest(ctx context.Context, sr proto.SasRequest) {
	log := slog.Default().With("reqId", sr.ReqID)
	res := proto.SasResult{ReqID: sr.ReqID}
	send := func() { a.sendSasResult(ctx, res, log) }

	pipe, secret, err := coreclient.ResolveCoreEndpoint(a.StateDir)
	if err != nil {
		res.Err = "resolve core endpoint: " + err.Error()
		send()
		return
	}
	// Dial 内部握手超时（handshakeTimeout）+ RPC 超时（rpcTimeout）均有界；
	// 外层不再叠超时——慢失败交给 server 侧 8s HTTP 等待上限收敛。
	c, err := coreclient.Dial(pipe, secret)
	if err != nil {
		res.Err = "core dial: " + err.Error()
		send()
		return
	}
	hr, err := c.SendSAS(sr.Reason)
	_ = c.Close()
	if err != nil {
		var rej *coreclient.RejectedError
		if errors.As(err, &rej) {
			res.Code = rej.Code
		} else {
			res.Err = err.Error()
		}
		send()
		return
	}
	res.OK = true
	res.HR = hr
	send()
}

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
