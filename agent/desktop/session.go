// session.go — kind=desktop 会话 handler(M1-Slice2 Task 4)。
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
//	end-of-candidates)忽略——pion 自行完成收集。
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
//
// 生命周期:SESSION_OPEN params 见 proto.DesktopParams;会话关闭(ctx 取消 /
// WS 断开)→ Publisher.Close + Source.Close + Starter.Stop(引用计数,最后
// 一个会话才真正 StopCapture)——全部 best-effort,顺序即此。
package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"

	"xnc/proto"
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
	// M2-Slice3 Task 2:意图自愈通知(经既有 state 帧形态;未知 code 的
	// 旧 viewer 按未知 state 忽略,向后兼容)。
	vocabStateReattached  = "reattached"
	vocabStateCaptureLost = "capture_lost"
)

const (
	// sigReadLimit 信令 text 帧读上限(SDP < 64KiB,取 1MiB 防御性余量,
	// 远低于 proto.MaxSessionFrameBytes)。
	sigReadLimit = 1 << 20
	// sigWriteTimeout 单条信令写时限。
	sigWriteTimeout = 10 * time.Second
)

// Handler 实现 session.Handler(kind=desktop)。Starter 决定帧源;nil 时
// Handle 直接报错(注册侧应保证非 nil)。serverLease 登记表惰性建立:每
// Handler 一份 = 一个 Starter = 一个采集实例(M2-Slice3 Task 4:仲裁在
// server,agent 只登记/比对 params 携带的 server leaseId;测试可预置
// h.leases 注入)。
type Handler struct {
	Log     *slog.Logger
	Starter Starter

	leaseOnce sync.Once
	leases    *serverLease
}

func (h *Handler) serverLeases() *serverLease {
	h.leaseOnce.Do(func() {
		if h.leases == nil {
			h.leases = newServerLease()
		}
	})
	return h.leases
}

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

// Handle 承载一个 viewer 的 desktop 会话直至 ctx 取消或 WS 断开。
func (h *Handler) Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage) {
	log := h.Log
	if log == nil {
		log = slog.Default()
	}
	if h.Starter == nil {
		h.failFast(ctx, ws, "start_failed", "desktop starter unavailable")
		return
	}
	log = log.With("session", sessionID)
	w := &wsWriter{ws: ws}

	var p proto.DesktopParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			h.failFast(ctx, ws, "bad_params", "params: "+err.Error())
			return
		}
	}
	if p.Signaling != "" && p.Signaling != "webrtc" {
		h.failFast(ctx, ws, "bad_params", "unsupported signaling: "+p.Signaling)
		return
	}

	// ① 启动采集 + ATTACH(pipe host 就绪才有 HOST_HELLO 维度信息)。
	// 采集意图(M2-Slice3 Task 2):源死亡(logoff/pipe 断/core 掉线)→
	// 退避 re-StartCapture,新源透明替换 —— close 时结清最终 Start/Stop
	// 配对(旧源由 reportDead 即时结清)。
	it := newCaptureIntent(ctx, h.Starter, p.WTSSession, log)
	src, err := h.Starter.Start(ctx, p.WTSSession)
	if err != nil {
		log.Warn("desktop start failed", "err", err)
		h.failFast(ctx, ws, "start_failed", err.Error())
		return
	}
	it.adopt(src)
	defer it.close() // 成功 ATTACH 之后才登记(失败路径无配对可结清)
	dyn := it.source()

	// ①b 输入/lease 控制器(本会话一条;close 合成卡键释放并释放本地
	// lease 登记,defer LIFO:在 pub.Close 之后、src/closeAll 之前运行
	// ——键释放需要 pipe 仍开)。源 = 意图动态源:重挂后输入/光标自动走
	// 新 sub。leaseId/capabilities 均来自 server 签发的 params(M2-Slice3
	// Task 4:未携带 leaseId = view-only;input.* 缺失拒对应输入)。
	ictl := newInputController(dyn, h.serverLeases(), sessionID, p.LeaseID,
		p.Capabilities, log)
	defer ictl.close()

	// ② ready(HOST_HELLO 维度 + fps→默认帧时长)。
	hello := src.Hello()
	defDur := 33 * time.Millisecond
	if hello != nil && hello.Fps > 0 {
		if d := time.Second / time.Duration(hello.Fps); d > 0 {
			defDur = d
		}
	}
	if hello != nil {
		w.write(ctx, readyFrame{Type: vocabReady,
			Width: hello.W, Height: hello.H, Fps: hello.Fps, Gen: hello.Gen,
			Displays: displaysOf(hello)})
	} else {
		w.write(ctx, readyFrame{Type: vocabReady})
	}

	// ①c 意图自愈的 viewer 通知(M2-Slice3 Task 2):重挂成功 → state
	// 帧 code=reattached(几何有变则补 display_changed reason=reattach,
	// viewer 重映射输入坐标);放弃 → state 帧 code=capture_lost。PC 不
	// 重建,帧流经同一 track 续传(viewer 无需重新协商)。
	it.onPublish = func(newSrc Source) {
		w.write(ctx, stateFrame{Type: vocabState, Code: vocabStateReattached, Recoverable: true})
		nh := newSrc.Hello()
		if nh != nil && hello != nil && (nh.W != hello.W || nh.H != hello.H) {
			w.write(ctx, displayChangedFrame{Type: vocabDisplayChanged,
				Generation: nh.Gen, W: nh.W, H: nh.H, Reason: "reattach"})
		}
	}
	it.onGiveUp = func() {
		w.write(ctx, stateFrame{Type: vocabState, Code: vocabStateCaptureLost, Recoverable: false})
	}

	// ③ 状态事件泵:STATE → {"type":"state"};0x010A →
	// {"type":"display_changed"}(M2-Slice1 Task 2;几何/代际变化)。
	go func() {
		for {
			ev, ok := dyn.RecvState(ctx)
			if !ok {
				return
			}
			w.write(ctx, stateFrame{Type: vocabState, Code: ev.Code, Recoverable: ev.Recoverable})
		}
	}()
	go func() {
		for {
			ev, ok := dyn.RecvDisplay(ctx)
			if !ok {
				return
			}
			w.write(ctx, displayChangedFrame{
				Type: vocabDisplayChanged, Generation: ev.Gen, W: ev.W, H: ev.H, Reason: ev.Reason})
		}
	}()

	// ④ 信令主循环:offer 建联(恰好一次),ice 喂候选。
	var (
		pub         *Publisher
		pubOnce     sync.Once
		sasInFlight atomic.Bool // 同会话并发 SAS ≤1(M2-Slice2 Task 1)
	)
	defer func() {
		if pub != nil {
			pubOnce.Do(func() { _ = pub.Close() })
		}
	}()
	for {
		mt, r, err := ws.Reader(ctx)
		if err != nil {
			// 会话终因可见性(T6 调试加):ctx 取消 / viewer 断开 / server 关泵。
			log.Info("desktop session ended", "err", err.Error())
			return // ctx 取消 / viewer 断开 / server 关泵
		}
		if mt != websocket.MessageText {
			continue
		}
		// 必须先把整帧读尽再解析:json.Decoder.Decode 在值结束处即停,
		// 大 SDP 经真实网络分段到达时会留下未读字节,下一次 Reader() 即
		// "previous message not read to completion" 断流(回环单分段测不出,
		// T6 XIAOXIN 实测暴露)。ReadAll 后 Unmarshal 与 e2eviewer 同款。
		b, err := io.ReadAll(io.LimitReader(r, sigReadLimit))
		if err != nil {
			continue
		}
		var f compactFrame
		if json.Unmarshal(b, &f) != nil {
			continue // 非 JSON:忽略,信令流自愈
		}
		switch f.Type {
		case vocabOffer:
			if pub != nil {
				continue // 重复 offer:忽略(已应答)
			}
			pub, err = h.setupPublisher(ctx, w, dyn, &p, f.SDP, defDur, ictl, log)
			if err != nil {
				log.Warn("desktop webrtc setup failed", "err", err)
				w.write(ctx, errorFrame{Type: vocabError, Code: "webrtc_failed", Message: err.Error()})
				return
			}
		case vocabICE:
			if pub == nil || f.Candidate == nil {
				continue
			}
			if err := pub.AddCandidate(*f.Candidate); err != nil {
				log.Debug("add ice candidate failed", "err", err)
			}
		case vocabKeyframeReq:
			// 浏览器 PLI 按钮(T6):无法从 JS 发 RTCP PLI,信令帧走同一
			// RequestKeyframe 路径(reason="viewer-pli" 进 host 记账)。
			if err := src.RequestKeyframe("viewer-pli"); err != nil {
				log.Debug("viewer keyframe request failed", "err", err)
			}
		case vocabLeaseRequest:
			// M2-Slice3 Task 4:仲裁在 server——本会话 params 带 server
			// 签发 leaseId 且仍是当前活约 → granted(回显该 leaseId);
			// 否则 denied{held}(view-only 会话与非持有者同答)。词汇
			// 保留供旧 viewer;agent 不再本地授予/撤销。
			if ictl.holdsLease() {
				w.write(ctx, leaseGrantedFrame{Type: vocabLeaseGranted, LeaseID: ictl.serverLeaseID()})
			} else {
				w.write(ctx, leaseDeniedFrame{Type: vocabLeaseDenied, Reason: "held"})
			}
		case vocabSecureAttention:
			// M2-Slice1 Task 5:viewer SAS 按钮 → Starter(SasCaller)→
			// core 0x0110(门控 = core 侧 --allow-sas,票据 = Slice3)。
			// M2-Slice3 Task 4:server capability 把门——params 未下发
			// input.secure_attention(owner 专属)立即回 capability_denied,
			// 不触达 Starter/core(RBAC 拒绝在 server 侧已定,agent 执行)。
			if !ictl.capAllows(proto.CapInputSecureAttn) {
				w.write(ctx, secureAttentionResultFrame{
					Type: vocabSecureAttentionResult, OK: false, HR: 0, Code: "capability_denied"})
				continue
			}
			// 异步执行:core RPC 上限 15s,不阻塞信令循环(晚到的 offer/
			// ice 不受牵连);回复帧经 wsWriter 串行写出,时序无约束。
			// M2-Slice2 Task 1:同会话 in-flight 去重——同时至多一条 SAS
			// RPC;并发请求立即回 {ok:false,code:"busy"}。
			if !sasInFlight.CompareAndSwap(false, true) {
				w.write(ctx, secureAttentionResultFrame{
					Type: vocabSecureAttentionResult, OK: false, HR: 0, Code: "busy"})
				continue
			}
			go func() {
				defer sasInFlight.Store(false)
				res := SasResult{OK: false, Code: "unsupported"}
				if sc, ok := h.Starter.(SasCaller); ok {
					res = sc.SendSAS("viewer")
				}
				log.Info("secure attention", "ok", res.OK, "hr", fmt.Sprintf("%#x", res.HR), "code", res.Code)
				w.write(ctx, secureAttentionResultFrame{
					Type: vocabSecureAttentionResult, OK: res.OK, HR: res.HR, Code: res.Code})
			}()
		case vocabSwitchDisplay:
			// M2-Slice3 Task 5:viewer 显示器下拉 → 0x0128。门控 = server
			// capability input.*(与其它输入同路;view-only 会话拒绝)。
			// 非法 idx / 旧 host 由下行帧表达(host STATE{invalid_display}
			// / DISPLAY_CHANGED reason=switch / state invalid_display)。
			var sf switchDisplayFrame
			if json.Unmarshal(b, &sf) != nil || sf.Index < 0 {
				continue
			}
			if !ictl.capAllows(proto.CapInputMouse) && !ictl.capAllows(proto.CapInputKeyboard) {
				w.write(ctx, stateFrame{Type: vocabState, Code: "switch_denied", Recoverable: false})
				continue
			}
			sw, ok := dyn.(DisplaySwitcher)
			if !ok {
				w.write(ctx, stateFrame{Type: vocabState, Code: "switch_unsupported", Recoverable: false})
				continue
			}
			if err := sw.SwitchDisplay(uint32(sf.Index)); err != nil {
				log.Debug("switch_display failed", "index", sf.Index, "err", err)
			}
		default:
			// 未知 type:忽略(向后兼容词汇演进)。
		}
	}
}

// setupPublisher 建 PeerConnection、接好回调和帧泵,并返回 answer。
// ictl 在 offer 应答前 attach 三条输入/光标 DataChannel(必须先于
// HandleOffer 建立),并在 answer 后启动 cursor 泵(0x0109 → cursor 通道)。
func (h *Handler) setupPublisher(ctx context.Context, w *wsWriter, src Source,
	p *proto.DesktopParams, offerSDP string, defDur time.Duration,
	ictl *inputController, log *slog.Logger) (*Publisher, error) {
	relay := p.IceTransportPolicy != proto.DesktopIceAll
	var ice []webrtc.ICEServer
	if p.Turn != nil {
		ice = append(ice, webrtc.ICEServer{
			URLs:       p.Turn.URLs,
			Username:   p.Turn.Username,
			Credential: p.Turn.Credential,
		})
	}
	if relay && len(ice) == 0 {
		return nil, fmt.Errorf("relay-only desktop session requires TURN servers (check server config)")
	}
	pub, err := NewPublisher(PublisherConfig{
		ICEServers:      ice,
		RelayOnly:       relay,
		DefaultDuration: defDur,
		Log:             log,
	})
	if err != nil {
		return nil, err
	}
	// PLI/FIR → host 按需 IDR(reason 进 host 记账)。
	pub.OnKeyRequest(func(reason string) {
		if err := src.RequestKeyframe(reason); err != nil {
			log.Debug("request keyframe failed", "reason", reason, "err", err)
		}
	})
	// 本地候选 → viewer(trickle;含收集完成哨兵)。
	pub.OnICECandidate(func(c webrtc.ICECandidateInit) {
		w.write(ctx, iceFrame{Type: vocabICE, Candidate: &c})
	})
	// 输入/光标通道(input.go):input 可靠、mouse/cursor 不可靠;必须在
	// HandleOffer 之前创建(answer 需含 SCTP;通道本体经 in-band 协商)。
	if err := ictl.attach(pub); err != nil {
		_ = pub.Close()
		return nil, fmt.Errorf("input channels: %w", err)
	}
	answerSDP, err := pub.HandleOffer(offerSDP)
	if err != nil {
		_ = pub.Close()
		return nil, err
	}
	w.write(ctx, answerFrame{Type: vocabAnswer, SDP: answerSDP})

	// 帧泵:pipe → RTP。源终结(RecvFrame false)或写失败(PC 已死)即
	// 退出——会话由信令主循环的 WS 错误路径统一收线。
	go func() {
		for {
			f, ok := src.RecvFrame(ctx)
			if !ok {
				return
			}
			if err := pub.WriteFrame(f); err != nil {
				log.Info("desktop frame pump stopped", "err", err)
				return
			}
		}
	}()

	// cursor 泵:pipe 0x0109 → cursor DataChannel(不可靠,最新即准)。
	go func() {
		for {
			ev, ok := src.RecvCursor(ctx)
			if !ok {
				return
			}
			ictl.forwardCursor(ev)
		}
	}()
	return pub, nil
}

// failFast 发一条 error 帧即收线(会话早夭:参数错/启动失败)。
func (h *Handler) failFast(ctx context.Context, ws *websocket.Conn, code, msg string) {
	w := &wsWriter{ws: ws}
	w.write(ctx, errorFrame{Type: vocabError, Code: code, Message: msg})
}

// ---- 出站信令帧形态(T5/T6 的 JSON 契约载体)----

type readyFrame struct {
	Type     string     `json:"type"`
	Width    uint32     `json:"width,omitempty"`
	Height   uint32     `json:"height,omitempty"`
	Fps      uint32     `json:"fps,omitempty"`
	Gen      uint32     `json:"gen,omitempty"`
	Displays []displayJSON `json:"displays,omitempty"`
}

// switchDisplayFrame 是 {"type":"switch_display","index":N} 上行帧。
type switchDisplayFrame struct {
	Type  string `json:"type"`
	Index int64  `json:"index"`
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
