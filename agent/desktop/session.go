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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
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
// h.leases 注入)。streamQoS 同理(每 Handler 一份 = 一条共享流的决策点;
// M3 Task 3)。
type Handler struct {
	Log     *slog.Logger
	Starter Starter

	leaseOnce sync.Once
	leases    *serverLease

	qosOnce sync.Once
	qos     *streamQoS
}

func (h *Handler) serverLeases() *serverLease {
	h.leaseOnce.Do(func() {
		if h.leases == nil {
			h.leases = newServerLease()
		}
	})
	return h.leases
}

// errVideoConfigUnsupported:host 未广告 SET_VIDEO_CONFIG 能力(v1 wire /
// 未接处理器的 v2 host;core_windows.go 把 desktoppipe 的同名错误映射到
// 此,session/qos 侧保持平台无关)。
var errVideoConfigUnsupported = errors.New("desktop: host does not support SET_VIDEO_CONFIG")

// qosMaxFPSEnv 是 agent 侧 fps 天花板的环境变量名(M4 模糊修正;节点
// 服务 env 设置 —— 生产金丝雀与 XNC_DESKTOP_FPS=30 一同开启,把控制器
// 的升档上限钉在 30:60fps 下编码器每帧预算 = 码率/fps 减半 → 运动期
// 粗量化 = 高帧率模糊)。
const qosMaxFPSEnv = "XNC_DESKTOP_QOS_MAX_FPS"

// qosParseMaxFPS 解析天花板原始值(纯函数,单测钉死)。空/0 = 无天花板
//(M3 行为);1..240 = 天花板(host --fps 合法域;>240 视为误配拒绝,
// 防一个 65535 把整个 fps 阶梯钳空)。
func qosParseMaxFPS(s string) (uint32, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("bad %s value %q", qosMaxFPSEnv, s)
	}
	if n > 240 {
		return 0, fmt.Errorf("%s value %d above the 240 host fps domain", qosMaxFPSEnv, n)
	}
	return uint32(n), nil
}

// qosManager 惰性建共享 QoS(种子 = 首个会话的 HOST_HELLO:初始码率按
// 流宽镜像 native 缺省、fps/max_w/aspect 取流几何)。hello 为 nil(测试
// fake 允许)→ 不建:viewer_feedback 解析后安全忽略,预算保持缺省。
func (h *Handler) qosManager(hello *HelloInfo) *streamQoS {
	h.qosOnce.Do(func() {
		if hello == nil || hello.W == 0 || hello.H == 0 {
			return
		}
		fps := hello.Fps
		if fps == 0 {
			fps = 30
		}
		log := h.Log
		if log == nil {
			log = slog.Default()
		}
		maxFPS, err := qosParseMaxFPS(os.Getenv(qosMaxFPSEnv))
		if err != nil {
			log.Warn("desktop qos: ignoring invalid fps ceiling", "err", err)
		} else if maxFPS > 0 {
			log.Info("desktop qos: fps ceiling armed", "max_fps", maxFPS)
		}
		h.qos = newStreamQoS(QoSControllerConfig{
			Initial: VideoConfig{Bitrate: bitrateForWidth(hello.W), FPS: fps, MaxW: hello.W},
			MaxFPS:  maxFPS,
			AspectW: hello.W,
			AspectH: hello.H,
		}, log)
	})
	return h.qos
}

// streamQoS 是一条共享流(一个 Starter/采集)的 QoS 汇点:纯决策器
// (qos_controller.go)+ 在场会话表。任何会话的 viewer_feedback 都汇入
// 同一决策器;动作路由回目标会话(SetVideoConfig → 全体 pacing 预算 +
// 观察会话的 Source 下发;PauseSpectator → 目标会话)。
type streamQoS struct {
	log  *slog.Logger
	ctrl *QoSController

	mu               sync.Mutex
	sessions         map[string]*qosSession          // sessionID → 应用端点(pub 建联后注册)
	keyframes        map[string]*KeyframeCoordinator // sessionID → 关键帧协调器(缺陷 B)
	configSend       bool                            // host 未拒能力前持续下发
	unsupportedNoted bool
}

func newStreamQoS(cfg QoSControllerConfig, log *slog.Logger) *streamQoS {
	return &streamQoS{log: log, ctrl: newQoSController(cfg),
		sessions:   make(map[string]*qosSession),
		keyframes:  make(map[string]*KeyframeCoordinator),
		configSend: true}
}

// observe 消费一条反馈并返回决策(控制器互斥;动作应用在锁外)。grace
// 挂起的拥塞剪码记一条 INFO(重置风暴诊断的核心观测线);节奏门抑制的
// 假拥塞拍记 DEBUG(M4:静态会话下每秒一条,INFO 会刷屏 —— 计数器
// CadenceHolds 提供聚合观测)。
func (q *streamQoS) observe(fb ViewerFeedback) []Action {
	q.mu.Lock()
	before := q.ctrl.HeldCuts()
	beforeHolds := q.ctrl.CadenceHolds()
	acts := q.ctrl.Observe(fb)
	held := q.ctrl.HeldCuts() - before
	holds := q.ctrl.CadenceHolds() - beforeHolds
	q.mu.Unlock()
	if held > 0 {
		q.log.Info("desktop qos: congestion cut held (encoder reset in flight)", "held_total", held)
	}
	if holds > 0 {
		q.log.Debug("desktop qos: browser queue congestion suppressed (sparse cadence)",
			"queue_ms", fb.QueueMs, "presented_fps", fb.PresentedFps, "holds_total", holds)
	}
	return acts
}

// frameObserved 通知决策器帧流仍在产出(session 帧泵逐帧喂入,携带该帧
// 的 codec epoch):host 重置后的「新代」帧确认重置完成 → 解除
// reset-recovery grace,拥塞控制恢复(旧代在飞帧不算确认 —— Fix 1)。
func (q *streamQoS) frameObserved(codecEpoch uint64) {
	q.mu.Lock()
	held := q.ctrl.FrameObserved(codecEpoch)
	q.mu.Unlock()
	if held {
		q.log.Info("desktop qos: encoder reset confirmed by new-generation frame; congestion control resumed",
			"codec_epoch", codecEpoch)
	}
}

// attach 注册一个会话的 QoS 应用端点(pub 已建联;Handle 收线时 detach)。
// I1(final-fixwave):注册即把当前生效配置套上 —— 迟到会话(controller
// 已决策之后才 offer 建联)的发送器预算立刻等于 cur.Bitrate×85%,不等
// 下一个 Action(修前迟到 viewer 挂 20Mbps 缺省预算狂奔到下一次决策)。
// 首个决策未发生时 Current() 即初始配置,同样成立(缺省预算只会更宽)。
func (q *streamQoS) attach(sessionID string, ctx context.Context, w *wsWriter, pub *Publisher) {
	q.mu.Lock()
	q.sessions[sessionID] = &qosSession{ctx: ctx, w: w, pub: pub}
	cur := q.ctrl.Current() // q.mu 即 ctrl 的串行锁(observe 同锁调用)
	q.mu.Unlock()
	pub.SetPacingBudget(int(cur.Bitrate))
}

// AgentEstimate 会话的 agent 侧 GCC 估计回调入口(transport 层
// OnTargetBitrateChange 直调;任意 goroutine 安全)。
func (q *streamQoS) AgentEstimate(sessionID string, bps int) {
	q.mu.Lock()
	q.ctrl.AgentEstimate(sessionID, bps)
	q.mu.Unlock()
}

// Current 返回共享流当前生效的编码参数(transport 层 per-会话 GCC 的
// InitialBitrate 来源,缺陷 A;q.mu 即 ctrl 的串行锁)。
func (q *streamQoS) Current() VideoConfig {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.ctrl.Current()
}

// detach 注销会话端点(幂等)。缺陷 C:同时从控制器 viewer 表删该表项
//(会话收线即删,不等 2 分钟 lastSeen 修剪 —— reload 冷启动的新 viewer
// 不再与陈旧 controller 表项比对而被误暂停;被删者是 controller 时触发
// 接任解暂停,见 QoSController.DetachViewer)。
func (q *streamQoS) detach(sessionID string) {
	q.mu.Lock()
	delete(q.sessions, sessionID)
	delete(q.keyframes, sessionID)
	q.ctrl.DetachViewer(sessionID)
	q.mu.Unlock()
}

// attachKeyframes 注册本会话的关键帧协调器(缺陷 B):setupPublisher 建好
// 协调器即注册,独立于 attach 的应用端点表 —— 后者要求 pub 已建成,而
// QoS 的 actionRequestKeyframe 在端点就位前就可能需要打 host。协调器的
// urgent 请求不经 Publisher 的 keyRequestIsUrgent seam(那里只认 connect,
// 见 qos_min.go),epoch 变更类请求由 QoS 动作直走 coordinator API。
func (q *streamQoS) attachKeyframes(sessionID string, coord *KeyframeCoordinator) {
	q.mu.Lock()
	q.keyframes[sessionID] = coord
	q.mu.Unlock()
}

// keyRequestReasonQoSReset 是逃逸阀 urgent 关键帧请求的 reason(≤31B,
// 与 pli/fir/connect/overflow/pacer/resume 同一 host 记账面)。
const keyRequestReasonQoSReset = "qos-reset"

// vocabStateSpectatorResumed(缺陷 C):接任/est 恢解暂停的稳定态
//(spectator_network_paused 的对偶;经既有 state 帧形态,旧 viewer 按
// 未知 code 忽略,向后兼容。词汇同伴都在 signaling.go,此条随缺陷 C 的
// 分派落点留在 session.go,避免本任务扩散改动面)。
const vocabStateSpectatorResumed = "spectator_network_resumed"

// apply 应用一批动作:SetVideoConfig → 全体在场会话的 pacing 预算(裁决
// 2:发送器预算来源 = controller 决策)+ 观察会话的 Source 下发(host 无
// 能力 → 记一次 unsupported 即停发,决策仍留在 agent 侧);PauseSpectator
// → 目标会话的 spectator_network_paused 稳定态 + ViewerSender.Pause();
// ResumeSpectator(缺陷 C)→ 目标会话的 spectator_network_resumed 稳定
// 态 + 发送器恢复(Pause 的对偶);RequestKeyframe → 各会话协调器的
// urgent 请求(缺陷 B 逃逸阀)。
func (q *streamQoS) apply(acts []Action, src Source) {
	for _, a := range acts {
		switch a.Kind {
		case actionSetVideoConfig:
			q.mu.Lock()
			sessions := make([]*qosSession, 0, len(q.sessions))
			for _, s := range q.sessions {
				sessions = append(sessions, s)
			}
			send := q.configSend
			q.mu.Unlock()
			for _, s := range sessions {
				s.apply(a) // 每会话自己的发送器预算
			}
			q.log.Info("desktop qos: set_video_config",
				"bitrate_bps", a.Config.Bitrate, "fps", a.Config.FPS, "max_w", a.Config.MaxW)
			if !send {
				// host 已判不支持:决策留在 agent 侧(预算仍接线)。配置
				// 未下发 → 不可能有编码器重置在途 → grace 立即解除。
				q.mu.Lock()
				q.ctrl.ResetConfirmed()
				q.mu.Unlock()
				continue
			}
			if err := src.SetVideoConfig(a.Config); err != nil {
				if errors.Is(err, errVideoConfigUnsupported) {
					q.mu.Lock()
					first := !q.unsupportedNoted
					q.configSend = false
					q.unsupportedNoted = true
					q.mu.Unlock()
					if first {
						q.log.Info("desktop qos: host lacks SET_VIDEO_CONFIG support; decisions stay agent-side")
					}
				} else {
					q.log.Debug("desktop qos: set_video_config failed", "err", err)
				}
			}
		case actionPauseSpectator:
			q.mu.Lock()
			s := q.sessions[a.ViewerID]
			q.mu.Unlock()
			if s != nil {
				s.apply(a)
			}
		case actionResumeSpectator:
			// 缺陷 C:与 PauseSpectator 同型(单目标会话;锁内快照,
			// 锁外应用)。目标已收线(如恢复动作在途时会话关闭)→ 无害
			// 跳过:发送器随会话关闭,无需恢复。
			q.mu.Lock()
			s := q.sessions[a.ViewerID]
			q.mu.Unlock()
			if s != nil {
				s.apply(a)
			}
		case actionRequestKeyframe:
			// 逃逸阀(缺陷 B):流级 urgent 关键帧请求直达各会话的协调器
			//(与 SetVideoConfig 分派同型:锁内快照,锁外触发)。多会话时
			// 每协调器各一条 —— sink 是同一 host 编码器,native 侧
			// kIdrMinIntervalMs 合并;阀本身 ≤1+2 次/3s,量级可忽略。
			q.mu.Lock()
			coords := make([]*KeyframeCoordinator, 0, len(q.keyframes))
			for _, kf := range q.keyframes {
				coords = append(coords, kf)
			}
			q.mu.Unlock()
			q.log.Info("desktop qos: reset confirmation timed out; requesting urgent keyframe",
				"retries_cap", qosResetConfirmRetries)
			for _, kf := range coords {
				kf.Request(keyRequestReasonQoSReset, true)
			}
		}
	}
}

// qosSession 是一个会话的 QoS 应用端点(锁外回调:WS 写 + 发送器)。
type qosSession struct {
	ctx context.Context
	w   *wsWriter
	pub *Publisher
}

func (s *qosSession) apply(a Action) {
	switch a.Kind {
	case actionSetVideoConfig:
		s.pub.SetPacingBudget(int(a.Config.Bitrate))
	case actionPauseSpectator:
		s.w.write(s.ctx, stateFrame{
			Type: vocabState, Code: vocabStateSpectatorPaused, Recoverable: true})
		s.pub.Pause()
	case actionResumeSpectator:
		// 缺陷 C:暂停的对偶 —— 通知 viewer 恢复 + 发送器 Resume(进入
		// waitIDR 重建参考链并恰一次合并关键帧请求,幂等)。直达
		// pub.vs:Publisher 只有 Pause 转发(events.go 的 Stats 直达同款),
		// 补一个 Resume 转发需动 transport.go —— 超出本任务的文件面。
		s.w.write(s.ctx, stateFrame{
			Type: vocabState, Code: vocabStateSpectatorResumed, Recoverable: true})
		s.pub.vs.Resume()
	}
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
	// M4 Task 4：会话级媒体版本钉子。server 在 SESSION_OPEN 前选定并快照进
	// params（canary：节点 allowlist/百分比/回滚）；agent 强制 HOST_HELLO 的
	// media_protocol 与钉子一致——不符即响亮收线，绝不混版续流。未知/缺席值
	// fail closed 到 v1（旧 server 继续可用）。
	wantMedia := mediaProtocolWireVersion(p.MediaProtocol)

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
	// ②a 媒体版本强制（M4 Task 4）：首个 HOST_HELLO 的 media_protocol 与
	// server 选定不符 → error 帧收线（Start/Stop 配对经已登记的 defer 走）。
	// 双向都拒：既不升版也不降版——会话版本在打开时定死。
	if hello != nil && hello.MediaProtocol != wantMedia {
		log.Error("desktop media protocol mismatch",
			"selected", p.MediaProtocol, "host_wire", hello.MediaProtocol, "want_wire", wantMedia)
		h.failFast(ctx, ws, mediaProtocolMismatchCode,
			fmt.Sprintf("server selected media protocol %q but host hello reports wire v%d",
				p.MediaProtocol, hello.MediaProtocol))
		return
	}
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
	// M4 Task 4:重挂换了版本的 host 同样拒绝——live 会话绝不换版(绝不在
	// 中途降版/升版续流),error 帧 + 关 WS 收线,清理走 Handle 的 defer。
	it.onPublish = func(newSrc Source) {
		if nh := newSrc.Hello(); nh != nil && nh.MediaProtocol != wantMedia {
			log.Error("desktop media protocol mismatch after reattach",
				"selected", p.MediaProtocol, "host_wire", nh.MediaProtocol, "want_wire", wantMedia)
			w.write(ctx, errorFrame{Type: vocabError, Code: mediaProtocolMismatchCode,
				Message: fmt.Sprintf("server selected media protocol %q but reattached host reports wire v%d",
					p.MediaProtocol, nh.MediaProtocol)})
			_ = ws.Close(websocket.StatusProtocolError, "media protocol mismatch")
			return
		}
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
	qos := h.qosManager(hello)
	ev := &sessionEvents{
		h: h, ctx: ctx, log: log, w: w,
		dyn: dyn, src: src,
		params: &p, defDur: defDur, ictl: ictl,
		sessionID: sessionID, qos: qos,
	}
	pubOnce := sync.Once{}
	defer func() {
		if ev.pub != nil {
			if qos != nil {
				qos.detach(sessionID) // 先注销 QoS 端点,再关发送器
			}
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

// setupPublisher 建 PeerConnection、接好回调和帧泵,并返回 answer。qos
// 非空时本会话的 PC 走 per-会话 API(发送侧 GCC 估计器,缺陷 A;估计经
// sessionID 路由回共享决策点)。
// ictl 在 offer 应答前 attach 三条输入/光标 DataChannel(必须先于
// HandleOffer 建立),frame-meta 遥测通道(M3 Task 4)随后同一约束建立,
// 并在 answer 后启动 cursor 泵(0x0109 → cursor 通道)。
// M3 Task 2:关键帧请求协调器在此组建——本会话全部请求源(RTCP PLI/FIR、
// 连接就绪、viewer 发送器 overflow/pacer/resume)经 OnKeyRequest seam 汇入
// (connect=新订阅 urgent),帧泵的 IDR 观测经 idrObservingSource 喂
// OnIDR;生命周期随 ctx(与帧/状态泵同一收线模型)。
// M4 修正:qos(共享决策点,可 nil)经 qosObservingSource 收到帧泵的逐
// 帧观测——reset-recovery grace 由新代帧流解除(见 qos_controller.go)。
func (h *Handler) setupPublisher(ctx context.Context, w *wsWriter, src Source,
	p *proto.DesktopParams, offerSDP string, defDur time.Duration,
	ictl *inputController, sessionID string, qos *streamQoS, log *slog.Logger) (*Publisher, error) {
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
		QoS:             qos,       // 缺陷 A:非空 → per-会话 GCC 估计装配
		SessionID:       sessionID, // 估计回调的路由键(OnTargetBitrateChange 闭包捕获)
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
	// frame-meta 通道(M3 Task 4):unordered + 不重传的轻量遥测,随输入
	// 通道同一约束在 HandleOffer 之前创建;发送点在本帧最后一包 RTP 写出
	// 之后(transport.go writePacketWithMeta),失败只计数不阻塞媒体。
	if err := pub.attachFrameMeta(); err != nil {
		_ = pub.Close()
		return nil, fmt.Errorf("frame meta channel: %w", err)
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
	// coord.OnIDR——在途关键帧请求只被「匹配或更新」的 IDR 清除。M4 修
	// 正:再包一层 qosObservingSource,逐帧喂共享 QoS(reset-recovery
	// grace 的解除信号,Fix 1 起只认新 codec epoch 的帧;连接就绪前的帧
	// 同样计入)。
	var pumpSrc Source = idrObservingSource{Source: src, onIDR: coord.OnIDR}
	if qos != nil {
		pumpSrc = qosObservingSource{Source: pumpSrc,
			onFrame: func(f Frame) { qos.frameObserved(f.CodecEpoch) }}
		// 缺陷 B:共享 QoS 的逃逸阀 urgent 关键帧动作直达本会话的协调器
		//(注册置于全部失败路径之后 —— setupPublisher 此后不再出错,端点
		// 生命周期由 Handle 收线时的 detach 结清)。
		qos.attachKeyframes(sessionID, coord)
	}
	go pumpFrames(ctx, log, pumpSrc, pub)
	go pumpCursor(ctx, src, ictl)
	return pub, nil
}

// failFast 发一条 error 帧即收线(会话早夭:参数错/启动失败)。
func (h *Handler) failFast(ctx context.Context, ws *websocket.Conn, code, msg string) {
	w := &wsWriter{ws: ws}
	w.write(ctx, errorFrame{Type: vocabError, Code: code, Message: msg})
}

// ---- M4 Task 4:会话级媒体协议钉子 ----

// mediaProtocolMismatchCode 是媒体版本不符的稳定错误码(error 帧 code 字段;
// 打开时与重挂后共用)。viewer 收到即知会话已收线,不应原参数重试。
const mediaProtocolMismatchCode = "media_protocol_mismatch"

// mediaProtocolWireVersion 把 server 选定的 mediaProtocol(proto 词汇
// "v1"/"v2")映射为 HOST_HELLO 尾随 u32 的 wire 值:0 = v1,2 = v2(镜像
// desktoppipe 的 mediaProtocolV2——本包跨平台,不导入 windows-only 的
// desktoppipe,常量就地复写)。空/"v1"/未知一律 0:fail closed 到 v1,
// 旧 server(未选择)与损坏 params 都退到安全缺省。
func mediaProtocolWireVersion(sel string) uint32 {
	if sel == proto.MediaProtocolV2 {
		return 2
	}
	return 0
}
