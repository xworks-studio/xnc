// signaling.go — desktop 会话的信令词汇与帧形态(纯定义;分发在 events.go,
// 生命周期在 session.go)。
//
// == 信令 JSON 词汇(desktop 会话 WS 的全部 text 帧;server pump 原样中转,
// T5 server / T6 web 与 e2eviewer 按此消费)==
//
// agent → viewer:
//
//	{"type":"ready","width":1920,"height":1080,"fps":30,"gen":1,
//	 "displays":[{"index":0,"originX":0,"originY":0,"w":1920,"h":1080,
//	 "primary":true}]}
//	    StartCapture + pipe ATTACH 完成(HOST_HELLO 内容);viewer 收到后才
//	    发 offer(时序契约;更早到达的 offer 也会被处理,但正常流如此)。
//	    displays 为 M2-S3 Task 5 的多显示器表(旧 agent 省略)。
//	{"type":"answer","sdp":"v=0..."}            对 viewer offer 的 SDP answer。
//	{"type":"ice","candidate":{...}}            trickle 候选(浏览器
//	    RTCIceCandidateInit 形态:{candidate,sdpMid,sdpMLineIndex,
//	    usernameFragment});candidate 为空串 "" 表示候选收集完成。
//	{"type":"state","code":"capture_rebuilt","recoverable":true}
//	    desktop pipe STATE 事件透传(稳定码 + 可恢复性;M2-Slice1 Task 2
//	    追加 recovering / capture_rebuilt / backend_changed,同一形态)。
//	{"type":"display_changed","generation":3,"w":1920,"h":1080,
//	 "reason":"resolution"}
//	    desktop pipe 0x010A DISPLAY_CHANGED 透传(M2-Slice1 Task 2):统一
//	    CaptureReset 改变了流几何;viewer 用新 w/h 重映射输入坐标。
//	{"type":"error","code":"start_failed","message":"..."}
//	    会话早夭原因(start_failed / webrtc_failed / bad_params);此后
//	    agent 收线,viewer 应重开会话。
//
// viewer → agent:
//
//	{"type":"offer","sdp":"v=0..."}             恰一次;触发建 PC + answer。
//	{"type":"ice","candidate":{...}|null}       trickle 候选;null(浏览器
//	    end-of-candidates)忽略——pion 自行完成收集。
//	{"type":"keyframe-req"}                     显式关键帧请求(T6 加:浏览器无
//	                                            法从 JS 发 RTCP PLI,实验页 PLI 按钮走此帧;映射到与真 PLI 同一条
//	                                            RequestKeyframe 路径,reason="viewer-pli")。
//	{"type":"lease_request"}                    输入控制权查询(M2-Slice3 Task 4:
//	                                            仲裁在 server——持有 server 签
//	                                            发 leaseId 的会话 granted,其余
//	                                            denied{held};词汇见 input.go)。
//	{"type":"secure_attention"}                 SAS 触发(M2-Slice1 Task 5):
//	                                            → Starter(SasCaller) → core
//	                                            0x0110(reason="viewer";
//	                                            门控 = server capability
//	                                            (input.secure_attention,owner 专属)
//	                                            + core 侧 --allow-sas 双保险)。
//	{"type":"switch_display","index":1}         切换采集显示器(M2-Slice3
//	                                            Task 5;需 input.* capability)
//	                                            → Source(DisplaySwitcher)→
//	                                            0x0128;非法 idx 由 host 回
//	                                            STATE{invalid_display}。
//	{"type":"viewer_feedback","visible":true,   viewer 网络观测(M3 Task 3;
//	 "estimatedBps":4200000,                    getStats 汇总,~1s 一报)→
//	 "queueMs":12.5,"decodeQueue":2,            QoSController.Observe →
//	 "rttMs":38.2}                              Action(SET_VIDEO_CONFIG
//	                                            0x0129 / 旁观者暂停)。
//
// agent → viewer(SAS 结果帧,M2-Slice1 Task 5):
//
//	{"type":"secure_attention_result","ok":true,"hr":0}
//	    ok = 核心受理并调用 SendSAS;hr = 合成 HRESULT(0 = sas.dll 调用
//	    未抛异常,非「SAS 已送达」证明——验收以安全桌面出现为准,T6)。
//	{"type":"secure_attention_result","ok":false,"hr":0,"code":"SAS_DENIED"}
//	    稳定码:核心侧 SAS_DENIED(门控关)/ SAS_UNAVAILABLE(sas.dll
//	    不可载)/ BAD_PAYLOAD;agent 侧 unsupported(Starter 无能力)/
//	    core_unavailable / core_error / busy(同会话已有 SAS 在途,M2-Slice2
//	    Task 1 去重)。viewer 对 SAS_DENIED 禁用按钮。
//
// agent → viewer(Slice3 追加 lease 三帧,词汇见 input.go):
//
//	{"type":"lease_granted","leaseId":"<16 hex>"}
//	{"type":"lease_denied","reason":"held"}
//	{"type":"lease_revoked","reason":"idle"|"disconnect"}
//	    (server 仲裁下 agent 不再主动产生;词汇保留供旧 viewer)
//
// 二进制帧:本 kind 无(输入/光标走 DataChannel,见 input.go)。未知 type
// 一律忽略(向后兼容)。
package desktop

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

// 信令帧 type 词汇(与文件头注释一一对应;T5/T6 契约)。
const (
	vocabReady          = "ready"
	vocabOffer          = "offer"
	vocabAnswer         = "answer"
	vocabICE            = "ice"
	vocabState          = "state"
	vocabDisplayChanged = "display_changed"
	vocabError          = "error"
	vocabKeyframeReq    = "keyframe-req"
	vocabLeaseRequest   = "lease_request"
	vocabLeaseGranted   = "lease_granted"
	vocabLeaseDenied    = "lease_denied"
	vocabLeaseRevoked   = "lease_revoked"
	// M2-Slice1 Task 5:secure attention(viewer SAS 按钮 ↔ core 0x0110)。
	vocabSecureAttention       = "secure_attention"
	vocabSecureAttentionResult = "secure_attention_result"
	// M2-Slice3 Task 5:显示器切换(control 上行;host 侧回
	// STATE{invalid_display} / DISPLAY_CHANGED reason=switch 下行)。
	vocabSwitchDisplay = "switch_display"
	// M3 Task 3:viewer 网络观测上行(见文件头字段契约)。
	vocabViewerFeedback = "viewer_feedback"
	// M2-Slice3 Task 2:意图自愈通知(经既有 state 帧形态;未知 code 的
	// 旧 viewer 按未知 state 忽略,向后兼容)。
	vocabStateReattached  = "reattached"
	vocabStateCaptureLost = "capture_lost"
	// M3 Task 3:旁观者网络暂停的稳定态(PauseSpectator action 下发;
	// 经既有 state 帧形态,旧 viewer 按未知 code 忽略)。
	vocabStateSpectatorPaused = "spectator_network_paused"
)

const (
	// sigReadLimit 信令 text 帧读上限(SDP < 64KiB,取 1MiB 防御性余量,
	// 远低于 proto.MaxSessionFrameBytes)。
	sigReadLimit = 1 << 20
	// sigWriteTimeout 单条信令写时限。
	sigWriteTimeout = 10 * time.Second
)

// compactFrame 是全部入站信令帧的宽松解析形态。
type compactFrame struct {
	Type      string                   `json:"type"`
	SDP       string                   `json:"sdp,omitempty"`
	Candidate *webrtc.ICECandidateInit `json:"candidate,omitempty"`
}

// wsWriter 串行化并发写(信令主循环、ICE 回调、状态泵三方共写)。
type wsWriter struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func (w *wsWriter) write(ctx context.Context, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, sigWriteTimeout)
	defer cancel()
	_ = w.ws.Write(wctx, websocket.MessageText, b)
}

// ---- 出站信令帧形态(T5/T6 的 JSON 契约载体)----

type readyFrame struct {
	Type     string        `json:"type"`
	Width    uint32        `json:"width,omitempty"`
	Height   uint32        `json:"height,omitempty"`
	Fps      uint32        `json:"fps,omitempty"`
	Gen      uint32        `json:"gen,omitempty"`
	Displays []displayJSON `json:"displays,omitempty"`
}

// switchDisplayFrame 是 {"type":"switch_display","index":N} 上行帧。
type switchDisplayFrame struct {
	Type  string `json:"type"`
	Index int64  `json:"index"`
}

// viewerFeedbackFrame 是 {"type":"viewer_feedback",...} 上行帧(M3 Task 3;
// 字段契约见文件头。queueMs/rttMs 是毫秒;decodeQueue 是帧数)。
// presentedFps(M4):viewer 实际呈现帧率(rvfc/getStats 汇总);0 = 旧 web
// 未上报 —— QoS 节奏门据此区分「稀疏流的 jitter 代理基线」与真拥塞。
type viewerFeedbackFrame struct {
	Type         string  `json:"type"`
	Visible      bool    `json:"visible"`
	EstimatedBps uint64  `json:"estimatedBps"`
	QueueMs      float64 `json:"queueMs"`
	DecodeQueue  float64 `json:"decodeQueue"`
	RTTMs        float64 `json:"rttMs"`
	PresentedFps float64 `json:"presentedFps"`
}

// displayJSON 是 displays[] 的 viewer 契约形态(与 Display 镜像)。
type displayJSON struct {
	Index   uint32 `json:"index"`
	OriginX int32  `json:"originX"`
	OriginY int32  `json:"originY"`
	W       uint32 `json:"w"`
	H       uint32 `json:"h"`
	Primary bool   `json:"primary"`
}

// displaysOf 把 HelloInfo.Displays 转 viewer 形态(nil 透传省略字段)。
func displaysOf(h *HelloInfo) []displayJSON {
	if h == nil || len(h.Displays) == 0 {
		return nil
	}
	out := make([]displayJSON, 0, len(h.Displays))
	for _, d := range h.Displays {
		out = append(out, displayJSON(d))
	}
	return out
}

type answerFrame struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

type iceFrame struct {
	Type      string                   `json:"type"`
	Candidate *webrtc.ICECandidateInit `json:"candidate"`
}

type stateFrame struct {
	Type        string `json:"type"`
	Code        string `json:"code"`
	Recoverable bool   `json:"recoverable"`
}

// displayChangedFrame 是 0x010A 的信令形态(M2-Slice1 Task 2 契约:
// {generation,w,h,reason})。
type displayChangedFrame struct {
	Type       string `json:"type"`
	Generation uint32 `json:"generation"`
	W          uint32 `json:"w"`
	H          uint32 `json:"h"`
	Reason     string `json:"reason"`
}

type errorFrame struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// lease 三帧(M1-Slice3;形态契约见 input.go 头注释,T4/T5 消费)。
type leaseGrantedFrame struct {
	Type    string `json:"type"`
	LeaseID string `json:"leaseId"`
}

type leaseDeniedFrame struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

type leaseRevokedFrame struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

// secureAttentionResultFrame 是 SAS 请求的回执(M2-Slice1 Task 5 契约:
// {ok,hr[,code]};hr 恒在——0 语义见文件头,code 仅 ok=false 时非空)。
type secureAttentionResultFrame struct {
	Type string `json:"type"`
	OK   bool   `json:"ok"`
	HR   uint32 `json:"hr"`
	Code string `json:"code,omitempty"`
}
