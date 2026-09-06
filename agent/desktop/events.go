// events.go — desktop 会话的 WS 信令分发(handleSignalingFrame 的 switch
// 各 case 抽为独立方法;帧形态与词汇见 signaling.go,生命周期在 session.go)。
package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"xnc/proto"
)

// sessionEvents 持有信令分发所需的本会话状态(Handle 组装;各 case 处理
// 函数都是它的方法)。pub 在建联成功后由 onOffer 写入,Handle 的 defer 依
// 此结清。qos 是 Handler 级共享决策点(可为 nil:无 HOST_HELLO 的测试
// 拓扑);sessionID 是本 viewer 的路由键(M3 Task 3)。
type sessionEvents struct {
	h         *Handler
	ctx       context.Context
	log       *slog.Logger
	w         *wsWriter
	dyn       Source // 意图动态源:重挂后输入/光标/状态自动走新 sub
	src       Source // 当前实源(keyframe 请求直达;重挂后更新)
	params    *proto.DesktopParams
	defDur    time.Duration // fps 推导的默认帧时长(offer 建联用)
	ictl      *inputController
	sessionID string     // 本 viewer 会话 id(QoS 路由)
	qos       *streamQoS // 共享 QoS(可 nil)

	pub         *Publisher
	sasInFlight atomic.Bool // 同会话并发 SAS ≤1(M2-Slice2 Task 1)

	// lastSenderStats 是上一拍(1s 反馈节拍)的本会话发送器统计快照
	// (Fix 2:窗口差分 DeadlineDroppedRate 用;nil = 首拍只采样)。信令
	// 循环单线程写,无锁。
	lastSenderStats *ViewerStats
}

// senderCongestionSnapshot 采集本会话发送侧拥塞证据(Fix 2):在 web 的
// 1s 反馈节拍上对 pub.vs.Stats() 差分 —— DeadlineDroppedRate = 窗口内
// 入队门拒收占尝试比;OverflowFlushes/桶债务为记录量。发送器在本机测
// 量排队,先于浏览器 jitter 代理看到真拥塞。无 pub(建联前)/首拍返回
// 零值(证据缺席)。
func (e *sessionEvents) senderCongestionSnapshot() SenderCongestion {
	if e.pub == nil {
		return SenderCongestion{}
	}
	st := e.pub.vs.Stats()
	defer func() { e.lastSenderStats = &st }()
	if e.lastSenderStats == nil {
		return SenderCongestion{BucketDebtMs: st.DebtMs}
	}
	prev := e.lastSenderStats
	sc := SenderCongestion{
		OverflowFlushes: st.OverflowFlushes - prev.OverflowFlushes,
		BucketDebtMs:    st.DebtMs,
	}
	attempted := (st.Admitted - prev.Admitted) +
		(st.DeadlineDropped - prev.DeadlineDropped) +
		(st.OverflowFlushes - prev.OverflowFlushes)
	if attempted > 0 {
		sc.DeadlineDroppedRate = float64(st.DeadlineDropped-prev.DeadlineDropped) / float64(attempted)
	}
	return sc
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
	case vocabViewerFeedback:
		e.onViewerFeedback(raw)
	default:
		// 未知 type:忽略(向后兼容词汇演进)。
	}
	return true
}

// onOffer 建 PC + answer(恰一次;重复 offer 忽略)。建联成功后向共享 QoS
// 注册本会话端点(其发送器预算/暂停由此驱动,M3 Task 3)。
func (e *sessionEvents) onOffer(f compactFrame) bool {
	if e.pub != nil {
		return true // 重复 offer:忽略(已应答)
	}
	pub, err := e.h.setupPublisher(e.ctx, e.w, e.dyn, e.params, f.SDP, e.defDur, e.ictl, e.sessionID, e.qos, e.log)
	if err != nil {
		e.log.Warn("desktop webrtc setup failed", "err", err)
		e.w.write(e.ctx, errorFrame{Type: vocabError, Code: "webrtc_failed", Message: err.Error()})
		return false
	}
	e.pub = pub
	if e.qos != nil {
		e.qos.attach(e.sessionID, e.ctx, e.w, pub)
	}
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
// 合并关键帧路径(reason="viewer-pli 进 host 记账)。M3 Task 3 carry(裁决
// 1):经 KeyframeCoordinator(Publisher 的合并 seam;connect 之外的
// reason 常规冷却,在途请求不复制),不再直打 Source——与 RTCP PLI/FIR、
// overflow/pacer/resume 同一条 250ms 合并面。建联前的请求(无 pub)保持
// 直打 Source(协调器尚未存在,首帧语义不受冷却影响)。
func (e *sessionEvents) onKeyframeReq() {
	if e.pub != nil {
		e.pub.RequestKeyframe("viewer-pli")
		return
	}
	if err := e.src.RequestKeyframe("viewer-pli"); err != nil {
		e.log.Debug("viewer keyframe request failed", "err", err)
	}
}

// onViewerFeedback M3 Task 3:viewer 网络观测(getStats 汇总)→ 共享
// QoSController.Observe → Action 应用(SET_VIDEO_CONFIG 0x0129 下发 host
// + 全体发送器 pacing 预算;旁观者带宽不足 → spectator_network_paused
// + 暂停)。qos 为 nil(无 HOST_HELLO 的测试拓扑)时安全忽略。
// M4 交付指标:随反馈节拍(web 的 1s 节奏)记一条 sender_stats INFO
//(发送侧拥塞面的可观测性:入队门拒收/年龄冲刷/令牌债务/准入通过率),
// 并把 viewer 呈现指标经 0x012A 转发 host 的 diag 面(best-effort)。
func (e *sessionEvents) onViewerFeedback(raw []byte) {
	if e.qos == nil {
		return
	}
	var f viewerFeedbackFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		// final-fixwave minor 3:畸形反馈留一条观测线(忽略不变,信令流
		// 自愈——但静默吞掉会让协议漂移不可见)。
		e.log.Debug("desktop viewer_feedback malformed; ignored", "err", err)
		return
	}
	sc := e.senderCongestionSnapshot()
	if e.pub != nil {
		if st := e.pub.vs.Stats(); st.FramesSent > 0 {
			attempted := st.Admitted + st.DeadlineDropped + st.OverflowFlushes
			pass := 100.0
			if attempted > 0 {
				pass = float64(st.Admitted) / float64(attempted) * 100
			}
			e.log.Info("sender_stats",
				"deadline_dropped", st.DeadlineDropped,
				"overflow_flushes", st.OverflowFlushes,
				"bucket_debt_ms", st.DebtMs,
				"admission_pass_rate", strconv.FormatFloat(pass, 'f', 1, 64)+"%")
		}
		// viewer 指标 → host diag 面(可选能力;未实现/未广告 = 静默)。
		if rep, ok := e.dyn.(ViewerMetricsReporter); ok {
			if err := rep.ReportViewerMetrics(f.PresentedFps, f.E2EP95Ms); err != nil {
				e.log.Debug("viewer metrics forward failed", "err", err)
			}
		}
	}
	acts := e.qos.observe(ViewerFeedback{
		SessionID:    e.sessionID,
		Visible:      f.Visible,
		EstimatedBps: f.EstimatedBps,
		QueueMs:      f.QueueMs,
		DecodeQueue:  f.DecodeQueue,
		RTTMs:        f.RTTMs,
		PresentedFps: f.PresentedFps,
		Sender:       sc,
	})
	if len(acts) > 0 {
		e.qos.apply(acts, e.dyn)
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
