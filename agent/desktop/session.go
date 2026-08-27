// session.go — kind=desktop 会话 handler(M1-Slice2 Task 4)。
//
// 职责划分(feat/arch-clean 拆分,行为零变化):
//   - signaling.go:信令 JSON 词汇 / 常量 / 帧形态(原文件头词汇注释随之
//     迁移);事件分发在 events.go(sessionEvents + 各 case 方法)。
//   - frames.go:帧泵 / 光标泵 / 状态事件泵 goroutine 主体。
//   - 本文件:Handler 结构、生命周期(启动采集/输入控制器/ready/自愈
//     通知/收线)与组装(信令循环 + setupPublisher)。
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

	// ③ 状态/显示器事件泵(帧形态见 signaling.go,泵体见 frames.go)。
	go pumpStateEvents(ctx, w, dyn)
	go pumpDisplayEvents(ctx, w, dyn)

	// ④ 信令主循环:offer 建联(恰好一次),ice 喂候选;分发在 events.go
	// (sessionEvents.handle,各 case 独立方法)。
	ev := &sessionEvents{
		h: h, ctx: ctx, log: log, w: w,
		dyn: dyn, src: src,
		params: &p, defDur: defDur, ictl: ictl,
	}
	pubOnce := sync.Once{}
	defer func() {
		if ev.pub != nil {
			pubOnce.Do(func() { _ = ev.pub.Close() })
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
		if !ev.handle(f, b) {
			return // offer 建联失败已发 error 帧,agent 收线
		}
	}
}

// setupPublisher 建 PeerConnection、接好回调和帧泵,并返回 answer。
// ictl 在 offer 应答前 attach 三条输入/光标 DataChannel(必须先于
// HandleOffer 建立),并在 answer 后启动 cursor 泵(0x0109 → cursor 通道)。
// M3 Task 2:关键帧请求协调器在此组建——本会话全部请求源(RTCP PLI/FIR、
// 连接就绪、viewer 发送器 overflow/pacer/resume)经 OnKeyRequest seam 汇入
// (connect=新订阅 urgent),帧泵的 IDR 观测经 idrObservingSource 喂
// OnIDR;生命周期随 ctx(与帧/状态泵同一收线模型)。
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
	// 关键帧请求协调器(M3 Task 2):PLI/FIR/connect/overflow/pacer/resume
	// 全部经协调器合并(connect=新订阅 urgent 绕过 250ms 冷却,其余常规);
	// host 侧按需产 IDR。宽限内无 IDR → OnState 下发 encoder_idr_timeout
	// (与 pumpStateEvents 同一 state 帧形态)。sink=src(动态源):重挂后
	// 请求自动走新 sub。watcher 随 ctx 收线。
	coord := newKeyframeCoordinator(KeyframeCoordinatorConfig{
		Sink: src,
		Ctx:  ctx,
		OnState: func(code string, recoverable bool) {
			w.write(ctx, stateFrame{Type: vocabState, Code: code, Recoverable: recoverable})
		},
		FramePeriod: defDur,
		Log:         log,
	})
	coord.start()
	// PLI/FIR/connect/overflow/pacer/resume → 协调器(见上)。
	pub.OnKeyRequest(func(reason string) {
		coord.Request(reason, keyRequestIsUrgent(reason))
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

	// 帧泵 + 光标泵(泵体见 frames.go;源终结/PC 死即各自退出,会话由
	// 信令主循环的 WS 错误路径统一收线)。帧泵的源经 idrObservingSource
	// 包裹:key 帧身份(CodecEpoch/EncodeSeq,Task 1 透传)在汇出点喂
	// coord.OnIDR——在途关键帧请求只被「匹配或更新」的 IDR 清除。
	go pumpFrames(ctx, log, idrObservingSource{Source: src, onIDR: coord.OnIDR}, pub)
	go pumpCursor(ctx, src, ictl)
	return pub, nil
}

// failFast 发一条 error 帧即收线(会话早夭:参数错/启动失败)。
func (h *Handler) failFast(ctx context.Context, ws *websocket.Conn, code, msg string) {
	w := &wsWriter{ws: ws}
	w.write(ctx, errorFrame{Type: vocabError, Code: code, Message: msg})
}
