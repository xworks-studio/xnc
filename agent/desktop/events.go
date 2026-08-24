// events.go — desktop 会话的 WS 信令分发(handleSignalingFrame 的 switch
// 各 case 抽为独立方法;帧形态与词汇见 signaling.go,生命周期在 session.go)。
package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"xnc/proto"
)

// sessionEvents 持有信令分发所需的本会话状态(Handle 组装;各 case 处理
// 函数都是它的方法)。pub 在建联成功后由 onOffer 写入,Handle 的 defer 依
// 此结清。
type sessionEvents struct {
	h      *Handler
	ctx    context.Context
	log    *slog.Logger
	w      *wsWriter
	dyn    Source // 意图动态源:重挂后输入/光标/状态自动走新 sub
	src    Source // 当前实源(keyframe 请求直达;重挂后更新)
	params *proto.DesktopParams
	defDur time.Duration // fps 推导的默认帧时长(offer 建联用)
	ictl   *inputController

	pub         *Publisher
	sasInFlight atomic.Bool // 同会话并发 SAS ≤1(M2-Slice2 Task 1)
}

// handle 分发一条入站信令帧;返回 false = 会话终结(offer 建联失败,agent
// 收线)。未知 type 忽略(向后兼容词汇演进)。
func (e *sessionEvents) handle(f compactFrame, raw []byte) bool {
	switch f.Type {
	case vocabOffer:
		return e.onOffer(f)
	case vocabICE:
		e.onICE(f)
	case vocabKeyframeReq:
		e.onKeyframeReq()
	case vocabLeaseRequest:
		e.onLeaseRequest()
	case vocabSecureAttention:
		e.onSecureAttention()
	case vocabSwitchDisplay:
		e.onSwitchDisplay(raw)
	default:
		// 未知 type:忽略(向后兼容词汇演进)。
	}
	return true
}

// onOffer 建 PC + answer(恰一次;重复 offer 忽略)。
func (e *sessionEvents) onOffer(f compactFrame) bool {
	if e.pub != nil {
		return true // 重复 offer:忽略(已应答)
	}
	pub, err := e.h.setupPublisher(e.ctx, e.w, e.dyn, e.params, f.SDP, e.defDur, e.ictl, e.log)
	if err != nil {
		e.log.Warn("desktop webrtc setup failed", "err", err)
		e.w.write(e.ctx, errorFrame{Type: vocabError, Code: "webrtc_failed", Message: err.Error()})
		return false
	}
	e.pub = pub
	return true
}

// onICE 喂候选到已建联的 PC(建联前/空候选忽略;null 由 pion 自行收集)。
func (e *sessionEvents) onICE(f compactFrame) {
	if e.pub == nil || f.Candidate == nil {
		return
	}
	if err := e.pub.AddCandidate(*f.Candidate); err != nil {
		e.log.Debug("add ice candidate failed", "err", err)
	}
}

// onKeyframeReq 浏览器 PLI 按钮(T6):无法从 JS 发 RTCP PLI,信令帧走同一
// RequestKeyframe 路径(reason="viewer-pli" 进 host 记账)。
func (e *sessionEvents) onKeyframeReq() {
	if err := e.src.RequestKeyframe("viewer-pli"); err != nil {
		e.log.Debug("viewer keyframe request failed", "err", err)
	}
}

// onLeaseRequest M2-Slice3 Task 4:仲裁在 server——本会话 params 带 server
// 签发 leaseId 且仍是当前活约 → granted(回显该 leaseId);否则 denied{held}
// (view-only 会话与非持有者同答)。词汇保留供旧 viewer;agent 不再本地
// 授予/撤销。
func (e *sessionEvents) onLeaseRequest() {
	if e.ictl.holdsLease() {
		e.w.write(e.ctx, leaseGrantedFrame{Type: vocabLeaseGranted, LeaseID: e.ictl.serverLeaseID()})
	} else {
		e.w.write(e.ctx, leaseDeniedFrame{Type: vocabLeaseDenied, Reason: "held"})
	}
}

// onSecureAttention M2-Slice1 Task 5:viewer SAS 按钮 → Starter(SasCaller)
// → core 0x0110(门控 = core 侧 --allow-sas,票据 = Slice3)。M2-Slice3
// Task 4:server capability 把门——params 未下发 input.secure_attention
// (owner 专属)立即回 capability_denied,不触达 Starter/core(RBAC 拒绝在
// server 侧已定,agent 执行)。
func (e *sessionEvents) onSecureAttention() {
	if !e.ictl.capAllows(proto.CapInputSecureAttn) {
		e.w.write(e.ctx, secureAttentionResultFrame{
			Type: vocabSecureAttentionResult, OK: false, HR: 0, Code: "capability_denied"})
		return
	}
	// 异步执行:core RPC 上限 15s,不阻塞信令循环(晚到的 offer/ice 不受
	// 牵连);回复帧经 wsWriter 串行写出,时序无约束。M2-Slice2 Task 1:
	// 同会话 in-flight 去重——同时至多一条 SAS RPC;并发请求立即回
	// {ok:false,code:"busy"}。
	if !e.sasInFlight.CompareAndSwap(false, true) {
		e.w.write(e.ctx, secureAttentionResultFrame{
			Type: vocabSecureAttentionResult, OK: false, HR: 0, Code: "busy"})
		return
	}
	go func() {
		defer e.sasInFlight.Store(false)
		res := SasResult{OK: false, Code: "unsupported"}
		if sc, ok := e.h.Starter.(SasCaller); ok {
			res = sc.SendSAS("viewer")
		}
		e.log.Info("secure attention", "ok", res.OK, "hr", fmt.Sprintf("%#x", res.HR), "code", res.Code)
		e.w.write(e.ctx, secureAttentionResultFrame{
			Type: vocabSecureAttentionResult, OK: res.OK, HR: res.HR, Code: res.Code})
	}()
}

// onSwitchDisplay M2-Slice3 Task 5:viewer 显示器下拉 → 0x0128。门控 =
// server capability input.*(与其它输入同路;view-only 会话拒绝)。非法
// idx / 旧 host 由下行帧表达(host STATE{invalid_display} /
// DISPLAY_CHANGED reason=switch / state invalid_display)。
func (e *sessionEvents) onSwitchDisplay(raw []byte) {
	var sf switchDisplayFrame
	if json.Unmarshal(raw, &sf) != nil || sf.Index < 0 {
		return
	}
	if !e.ictl.capAllows(proto.CapInputMouse) && !e.ictl.capAllows(proto.CapInputKeyboard) {
		e.w.write(e.ctx, stateFrame{Type: vocabState, Code: "switch_denied", Recoverable: false})
		return
	}
	sw, ok := e.dyn.(DisplaySwitcher)
	if !ok {
		e.w.write(e.ctx, stateFrame{Type: vocabState, Code: "switch_unsupported", Recoverable: false})
		return
	}
	if err := sw.SwitchDisplay(uint32(sf.Index)); err != nil {
		e.log.Debug("switch_display failed", "index", sf.Index, "err", err)
	}
}
