// session.go — kind=desktop 会话 handler(M1-Slice2 Task 4)。
//
// == 信令 JSON 词汇(desktop 会话 WS 的全部 text 帧;server pump 原样中转,
// T5 server / T6 web 与 e2eviewer 按此消费)==
//
// agent → viewer:
//
//	{"type":"ready","width":1920,"height":1080,"fps":30,"gen":1}
//	    StartCapture + pipe ATTACH 完成(HOST_HELLO 内容);viewer 收到后才
//	    发 offer(时序契约;更早到达的 offer 也会被处理,但正常流如此)。
//	{"type":"answer","sdp":"v=0..."}            对 viewer offer 的 SDP answer。
//	{"type":"ice","candidate":{...}}            trickle 候选(浏览器
//	    RTCIceCandidateInit 形态:{candidate,sdpMid,sdpMLineIndex,
//	    usernameFragment});candidate 为空串 "" 表示候选收集完成。
//	{"type":"state","code":"capture_rebuilt","recoverable":true}
//	    desktop pipe STATE 事件透传。
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
//	法从 JS 发 RTCP PLI,实验页 PLI 按钮走此帧;映射到与真 PLI 同一条
//	RequestKeyframe 路径,reason="viewer-pli")。
//	{"type":"lease_request"}                    输入控制权请求(M1-Slice3;
//	                                            见 input.go 头注释)。
//
// agent → viewer(Slice3 追加 lease 三帧,词汇见 input.go):
//
//	{"type":"lease_granted","leaseId":"<16 hex>"}
//	{"type":"lease_denied","reason":"held"}
//	{"type":"lease_revoked","reason":"idle"|"disconnect"}
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
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"

	"xnc/proto"
)

// 信令帧 type 词汇(与文件头注释一一对应;T5/T6 契约)。
const (
	vocabReady        = "ready"
	vocabOffer        = "offer"
	vocabAnswer       = "answer"
	vocabICE          = "ice"
	vocabState        = "state"
	vocabError        = "error"
	vocabKeyframeReq  = "keyframe-req"
	vocabLeaseRequest = "lease_request"
	vocabLeaseGranted = "lease_granted"
	vocabLeaseDenied  = "lease_denied"
	vocabLeaseRevoked = "lease_revoked"
)

const (
	// sigReadLimit 信令 text 帧读上限(SDP < 64KiB,取 1MiB 防御性余量,
	// 远低于 proto.MaxSessionFrameBytes)。
	sigReadLimit = 1 << 20
	// sigWriteTimeout 单条信令写时限。
	sigWriteTimeout = 10 * time.Second
)

// Handler 实现 session.Handler(kind=desktop)。Starter 决定帧源;nil 时
// Handle 直接报错(注册侧应保证非 nil)。lease 表惰性建立:每 Handler
// 一份 = 一个 Starter = 一个采集实例(多 viewer 会话共享仲裁;测试可
// 预置 h.leases 注入短 idle)。
type Handler struct {
	Log     *slog.Logger
	Starter Starter

	leaseOnce sync.Once
	leases    *leaseTable
}

func (h *Handler) leaseTable() *leaseTable {
	h.leaseOnce.Do(func() {
		if h.leases == nil {
			h.leases = newLeaseTable()
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
	src, err := h.Starter.Start(ctx, p.WTSSession)
	if err != nil {
		log.Warn("desktop start failed", "err", err)
		h.failFast(ctx, ws, "start_failed", err.Error())
		return
	}
	var closeOnce sync.Once
	closeAll := func() {
		closeOnce.Do(func() {
			_ = src.Close()
			if err := h.Starter.Stop(); err != nil {
				log.Warn("desktop stop capture failed", "err", err)
			}
		})
	}
	defer closeAll()

	// ①b 输入/lease 控制器(本会话一条;close 合成卡键释放并释放 lease,
	// defer LIFO:在 pub.Close 之后、src/closeAll 之前运行——键释放需要
	// pipe 仍开)。
	ictl := newInputController(src, h.leaseTable(), log)
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
			Width: hello.W, Height: hello.H, Fps: hello.Fps, Gen: hello.Gen})
	} else {
		w.write(ctx, readyFrame{Type: vocabReady})
	}

	// ③ 状态事件泵:STATE → {"type":"state"}。
	go func() {
		for {
			ev, ok := src.RecvState(ctx)
			if !ok {
				return
			}
			w.write(ctx, stateFrame{Type: vocabState, Code: ev.Code, Recoverable: ev.Recoverable})
		}
	}()

	// ④ 信令主循环:offer 建联(恰好一次),ice 喂候选。
	var (
		pub     *Publisher
		pubOnce sync.Once
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
			pub, err = h.setupPublisher(ctx, w, src, &p, f.SDP, defDur, ictl, log)
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
			// M1 简化仲裁(input.go):首请求者得;撤销时经 notify 回
			// lease_revoked 帧;非持有者输入丢弃+计数(不断连)。
			if id, ok := ictl.grantLease(func(reason string) {
				w.write(ctx, leaseRevokedFrame{Type: vocabLeaseRevoked, Reason: reason})
			}); ok {
				w.write(ctx, leaseGrantedFrame{Type: vocabLeaseGranted, LeaseID: id})
			} else {
				w.write(ctx, leaseDeniedFrame{Type: vocabLeaseDenied, Reason: "held"})
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
	Type   string `json:"type"`
	Width  uint32 `json:"width,omitempty"`
	Height uint32 `json:"height,omitempty"`
	Fps    uint32 `json:"fps,omitempty"`
	Gen    uint32 `json:"gen,omitempty"`
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
