// transport.go — Pion publisher(M1-Slice2 Task 4;M3 Task 1 起拆分)。
//
// Publisher 只保留传输面:PeerConnection、视频轨、信令(offer/answer/
// trickle)、RTCP 泵与连接就绪门。发包状态(RTP 时钟、分包、单帧队列、
// 令牌桶 pacing、WAIT_IDR 状态机)归每 viewer 一个的 ViewerSender
// (viewer_sender.go);WriteFrame 委派 Enqueue,session.go 零改动。
//
// API 选择:TrackLocalStaticRTP + Pion H264Payloader。Annex-B AU 由 Pion
// 完成 STAP-A/FU-A 分包,每帧的所有 RTP 包都直接使用该 AU 的
// PresentMonoUs@90kHz 时戳;不再用 sample.Duration 隐式推进时间轴。
package desktop

import (
	"bytes"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/cc"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/webrtc/v4"
)

// h264FmtpLine 是本管线的 H264 协商参数:Mf 编码器输出不限于此声明档位,
// 浏览器以带内 SPS 配置解码,level-asymmetry-allowed 保证应答兼容。
const h264FmtpLine = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"

// PublisherConfig 控制 publisher PeerConnection 的建立参数。
type PublisherConfig struct {
	// ICEServers 来自 SESSION_OPEN params.turn(server 下发)。
	ICEServers []webrtc.ICEServer
	// RelayOnly = iceTransportPolicy:relay(全局约束;回环单测传 false 走
	// host 候选,因为 relay 需真 TURN)。
	RelayOnly bool
	// DefaultDuration 仅为旧调用方的源码兼容字段；RTP 时间轴不使用它。
	DefaultDuration time.Duration
	// PacingBudgetBps 是 viewer 发送器的 pacing 预算(bits/s;0 →
	// defaultPacingBudgetBps)。令牌桶按其 pacingBudgetFraction(M4 起
	// ×1.05)铺开帧内突发。
	PacingBudgetBps int
	// QoS + SessionID(缺陷 A):QoS 非空时本会话的 PC 走 per-会话 API
	// (newSessionAPI,发送侧 GCC 估计器);SessionID 是估计回调的路由键
	// (OnTargetBitrateChange 闭包捕获,估计汇入 QoS.AgentEstimate)。
	QoS       *streamQoS
	SessionID string
	Log       *slog.Logger
}

// Publisher 承载一个 viewer 会话的发送侧 PeerConnection + H264 视频轨,
// 并拥有该 viewer 的 ViewerSender(发包/队列/状态机)。frame-meta 遥测
// (M3 Task 4)经发送器的帧边界 seam 汇出:身份在入队时绑定、本帧最后
// 一包成功写出后回调(修正轮:任何写出顺序下归属都不错位;布局/键控
// hash 见 frame_meta.go,seam 见 viewer_sender.go 的 MakeFrameMeta/
// OnFrameSent)。
type Publisher struct {
	log    *slog.Logger
	pc     *webrtc.PeerConnection
	sender *webrtc.RTPSender
	track  *webrtc.TrackLocalStaticRTP
	vs     *ViewerSender
	// frame-meta 面(attachFrameMeta 建立;key 每会话随机,绝不落盘/入日志)。
	metaKey   uint64
	metaDC    *webrtc.DataChannel
	metaRelay *frameMetaRelay
	// defDur/lastMono 只保留给旧 frameDuration 单测，不在发送热路径上。
	defDur    time.Duration
	stats     pubStats
	wg        sync.WaitGroup
	closeO    sync.Once
	connReady atomic.Bool // ICE+DTLS 已通(到达即向 host 请求 fresh IDR)
	keyFn     func(reason string)
	iceFn     func(webrtc.ICECandidateInit)
	stateMu   sync.Mutex
	lastMono  uint64
}

// cryptoSessionKey 抽取 frame-meta 会话键(final-fixwave minor 4:
// 键控 hash 的 key 必须不可预测 —— math/rand/v2 的会话键理论上可被旁观
// 重构;crypto/rand 之下碰撞/预测均不可行)。crypto/rand.Read 在受支持
// 平台不会失败;不可达的错误路径回退到时间抖动值,绝不因遥测键丢会话。
// (RTP 时钟基点/序列器种子仍用 math/rand —— 可预测性无关紧要。)
func cryptoSessionKey() uint64 {
	var b [8]byte
	if _, err := crand.Read(b[:]); err == nil {
		return binary.LittleEndian.Uint64(b[:])
	}
	ns := uint64(time.Now().UnixNano())
	return ns*0x9E3779B97F4A7C15 ^ ns>>29 // best-effort:仅遥测关联键
}

// RegisterDesktopCodecs 在 MediaEngine 上注册管线唯一的视频编解码
// (H264 90kHz)。反馈面:transport-cc(带宽测量)、ccm fir、nack、nack pli
// ——RegisterDefaultInterceptors 会再按引擎级补齐(幂等去重)。
func RegisterDesktopCodecs(m *webrtc.MediaEngine) error {
	return m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: h264FmtpLine,
			RTCPFeedback: []webrtc.RTCPFeedback{
				{Type: webrtc.TypeRTCPFBTransportCC},
				{Type: webrtc.TypeRTCPFBCCM, Parameter: "fir"},
				{Type: webrtc.TypeRTCPFBNACK},
				{Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"},
			},
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo)
}

// newDesktopAPI 组装共享的 webrtc.API:编解码注册 + 默认拦截器(NACK
// 重发、RTCP 报告、统计)+ TWCC 出向头扩展(让浏览器能测量我们的发送
// 带宽——qos_min「TWCC 开」)。
func newDesktopAPI() (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := RegisterDesktopCodecs(m); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, err
	}
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(m, ir); err != nil {
		return nil, err
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir)), nil
}

// newSessionAPI 组装 per-会话 webrtc.API(缺陷 A,设计 §2.1):在
// newDesktopAPI 的默认件之上加装 pion 发送侧 GCC 估计器 —— 浏览器对我们
// 的 pion→Chrome 流不暴露 availableIncomingBitrate(2026-09-06 生产实
// 测),web 上报的 est 是自指 goodput,QoS 控制器会被拖到 500k 地板。
// GCC 由浏览器回传的 TWCC 反馈驱动(ConfigureTWCCHeaderExtensionSender
// 已开出发向头扩展),OnTargetBitrateChange 把目标码率按 sessionID 汇入
// 共享 streamQoS。
//
// per-会话 Registry + API:估计回调闭包捕获 sessionID —— 共享 registry
// 的 OnNewPeerConnection id 与我们会话 id 无对应关系,多 viewer 时估计
// 无法归位;故对每个 viewer 会话复刻 newDesktopAPI 的默认件(NACK/
// SenderReport/TWCC 头扩展/统计)。装配顺序照官方示例
// (webrtc/examples/bandwidth-estimation-from-disk):cc 拦截器 Add 在
// ConfigureTWCCHeaderExtensionSender 与 RegisterDefaultInterceptors 之前。
// 链序语义(终审 Minor#2a:修前注释把 cc 说成「最外层」是反的):链按
// Add 顺序层层包裹 —— 后 Add 的在外层(出向 RTP 上先见包),先 Add 的
// 在内层、最贴近 wire(interceptor.Chain 的 BindLocalStream 依序
// writer = next.BindLocalStream(writer),最终 writer = 最后 Add 者)。
// cc 先 Add = 出向路径最内层(出向上最后一个见包):TWCC 头扩展拦截器
//(后 Add,外层)先给出向包的序列号盖好章,cc 的 OnSent 才能读到带
// TWCC 扩展的序列号;入向 RTCP(TWCC 反馈)上 cc 同样是最贴近 wire 的
// 一层(反序则 cc 在出向上最先见包、扩展未盖章 → GCC 对每个包报
// missing extension 而丢弃)。InitialBitrate 取流当前码率(未建回退
// bitrateForWidth(1920)),MaxBitrate 取 qosMaxBitrateBps。
func newSessionAPI(qos *streamQoS, sessionID string) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := RegisterDesktopCodecs(m); err != nil {
		return nil, err
	}
	initial := int(bitrateForWidth(1920))
	if cur := int(qos.Current().Bitrate); cur > 0 {
		initial = cur
	}
	ir := &interceptor.Registry{}
	ccInterceptor, err := cc.NewInterceptor(func() (cc.BandwidthEstimator, error) {
		est, err := gcc.NewSendSideBWE(
			gcc.SendSideBWEInitialBitrate(initial),
			gcc.SendSideBWEMaxBitrate(qosMaxBitrateBps),
		)
		if err != nil {
			return nil, err
		}
		// 回调闭包捕获 sessionID:估计直接路由到本会话的 viewer 表项
		//(pion 在 goroutine 里触发,streamQoS.AgentEstimate 自带串行)。
		est.OnTargetBitrateChange(func(bps int) { qos.AgentEstimate(sessionID, bps) })
		return est, nil
	})
	if err != nil {
		return nil, err
	}
	ir.Add(ccInterceptor)
	if err := webrtc.ConfigureTWCCHeaderExtensionSender(m, ir); err != nil {
		return nil, err
	}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, err
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir)), nil
}

// NewPublisher 建立 PeerConnection 与视频轨,并启动 RTCP 泵(qos_min.go)。
// RelayOnly 时必须给出至少一个 ICE(TURN)server,否则直接报错。
func NewPublisher(cfg PublisherConfig) (*Publisher, error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	policy := webrtc.ICETransportPolicyAll
	if cfg.RelayOnly {
		policy = webrtc.ICETransportPolicyRelay
		if len(cfg.ICEServers) == 0 {
			return nil, errors.New("desktop: relay-only publisher requires TURN ICE servers")
		}
	}
	// 缺陷 A:有共享 QoS 的会话走 per-会话 API(发送侧 GCC 估计器,
	// newSessionAPI);无 QoS(无 HOST_HELLO 的测试拓扑)保持原装配。
	var api *webrtc.API
	var err error
	if cfg.QoS != nil {
		api, err = newSessionAPI(cfg.QoS, cfg.SessionID)
	} else {
		api, err = newDesktopAPI()
	}
	if err != nil {
		return nil, fmt.Errorf("desktop: api: %w", err)
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers:         cfg.ICEServers,
		ICETransportPolicy: policy,
	})
	if err != nil {
		return nil, fmt.Errorf("desktop: peer connection: %w", err)
	}
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeH264,
		ClockRate:   90000,
		SDPFmtpLine: h264FmtpLine,
	}, "desktop-video", "xnc-desktop")
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("desktop: track: %w", err)
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("desktop: add track: %w", err)
	}
	p := &Publisher{
		log:     log,
		pc:      pc,
		sender:  sender,
		track:   track,
		metaKey: cryptoSessionKey(), // 会话键控 hash 的 64 位键(裁决 1;仅内存)
	}
	// 每 viewer 一个发送器:发包/单帧队列/令牌桶 pacing/WAIT_IDR 状态机
	// 全部私有(与其它 viewer 完全隔离)。合并关键帧回调与 Publisher 的
	// OnKeyRequest 同一 seam(connect/pli/fir 也经它汇出)。frame-meta:
	// MakeFrameMeta 在帧 admitted 入队时绑定身份+per-viewer 时戳,
	// OnFrameSent 在本帧最后一包成功写出后恰一次汇出(viewer_sender.go)。
	vs, err := newViewerSender(ViewerSenderConfig{
		WritePacket:   track.WriteRTP,
		KeyRequest:    p.fireKeyRequest,
		BudgetBps:     cfg.PacingBudgetBps,
		Log:           log,
		MakeFrameMeta: p.makeFrameMeta,
		OnFrameSent:   p.onFrameSent,
	})
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("desktop: viewer sender: %w", err)
	}
	p.vs = vs
	p.vs.start()
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		fn := p.iceFn
		if fn == nil {
			return
		}
		if c == nil {
			fn(webrtc.ICECandidateInit{Candidate: ""}) // 收集完成(end-of-candidates)
			return
		}
		fn(c.ToJSON())
	})
	// 首帧语义(关键):ICE+DTLS 就绪前 RTPSender 尚未启动,WriteRTP
	// 会被静默丢弃——ATTACH 时的 IDR 恰好落在这个窗口。因此:到达
	// Connected 即向 host 请求一次 fresh IDR(reason="connect",host 侧
	// ≥500ms 间隔记账),它必然在 sender 启动后产出;WriteFrame 侧配合
	//「连接后丢 delta 直到首个 key」双保险(见 WriteFrame)。
	pc.OnConnectionStateChange(func(cs webrtc.PeerConnectionState) {
		if cs == webrtc.PeerConnectionStateConnected && p.connReady.CompareAndSwap(false, true) {
			p.fireKeyRequest("connect")
		}
	})
	p.wg.Add(1)
	go p.rtcpLoop()
	return p, nil
}

// OnICECandidate 注册 trickle 回调(候选或 end-of-candidates 空串)。须在
// HandleOffer 前调用。
func (p *Publisher) OnICECandidate(fn func(webrtc.ICECandidateInit)) { p.iceFn = fn }

// OnKeyRequest 注册关键帧请求回调(PLI/FIR 到达时触发,reason="pli"/"fir")。
func (p *Publisher) OnKeyRequest(fn func(reason string)) { p.keyFn = fn }

// RequestKeyframe 从会话侧发起一条合并关键帧请求(M3 Task 3 carry:
// viewer-pli 等会话级请求经此汇入 coordinator 接管的同一 seam,而非直打
// Source)。urgent 判定沿用 keyRequestIsUrgent(reason)。
func (p *Publisher) RequestKeyframe(reason string) { p.fireKeyRequest(reason) }

// SetPacingBudget 更新本 viewer 发送器的 pacing 预算(M3 Task 3 裁决 2:
// 预算来源 = QoS controller 的当前码率)。
func (p *Publisher) SetPacingBudget(bps int) { p.vs.SetBudget(bps) }

// Pause 暂停本 viewer 的发送(M3 Task 3:PauseSpectator action)。
func (p *Publisher) Pause() { p.vs.Pause() }

// HandleOffer 消费 viewer 的 offer SDP 并返回本地 answer SDP(含 SetLocal)。
func (p *Publisher) HandleOffer(offerSDP string) (string, error) {
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: offerSDP,
	}); err != nil {
		return "", fmt.Errorf("desktop: set remote offer: %w", err)
	}
	ans, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("desktop: create answer: %w", err)
	}
	if err := p.pc.SetLocalDescription(ans); err != nil {
		return "", fmt.Errorf("desktop: set local answer: %w", err)
	}
	return ans.SDP, nil
}

// AddCandidate 注入 viewer trickle 候选(浏览器 RTCIceCandidateInit 形态)。
func (p *Publisher) AddCandidate(c webrtc.ICECandidateInit) error {
	return p.pc.AddICECandidate(c)
}

// WriteFrame 将一帧 Annex-B AU 交给本 viewer 的发送器(分包/队列/pacing/
// 状态机见 viewer_sender.go)。同一 AU 的所有包使用同一个
// PresentMonoUs@90kHz 时戳,仅最后一包置 Marker。frame-meta 由发送器的
// 帧边界 seam 汇出(入队绑定身份;未被送出的帧——抑制/丢弃——绝不发)。
//
// 首帧语义(双保险,见 NewPublisher 的 Connected 钩子):连接就绪前的帧
// 一律丢弃(RTPSender 未启动,写了也静默蒸发);连接后由发送器的
// WAIT_IDR 状态机丢弃 delta 直到首个 key 帧——viewer 收到的第一帧永远是
// IDR,静止桌面/慢启动场景无画面黑洞。
func (p *Publisher) WriteFrame(f Frame) error {
	if len(f.AU) == 0 {
		return nil
	}
	if !p.connReady.Load() {
		p.stats.preConnDropped.Add(1)
		return nil
	}
	return p.vs.Enqueue(f)
}

// frameIntervalStats 帧间隔分布诊断:播放节奏 = RTP 时间戳间隔 = mono 差,
// 间隔抖动直接表现为播放速度不均(果冻感)。滑动窗口最近 300 帧,每 10s
// 输出 min/avg/p50/p95/max——果冻感排查的确定性数据。
type frameIntervalStats struct {
	mu      sync.Mutex
	vals    []time.Duration
	lastLog time.Time
}

func (s *frameIntervalStats) add(d time.Duration, log *slog.Logger) {
	s.mu.Lock()
	s.vals = append(s.vals, d)
	if len(s.vals) > 300 {
		s.vals = s.vals[len(s.vals)-300:]
	}
	now := time.Now()
	if now.Sub(s.lastLog) >= 10*time.Second && len(s.vals) >= 30 {
		v := make([]time.Duration, len(s.vals))
		copy(v, s.vals)
		s.lastLog = now
		s.mu.Unlock()
		sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
		var sum time.Duration
		for _, x := range v {
			sum += x
		}
		log.Info("desktop frame interval",
			"n", len(v), "min", v[0].String(), "avg", (sum / time.Duration(len(v))).String(),
			"p50", v[len(v)/2].String(), "p95", v[len(v)*95/100].String(), "max", v[len(v)-1].String())
	} else {
		s.mu.Unlock()
	}
}

// frameDuration 推导本帧时长:mono 差直接采用,回退 DefaultDuration 仅在
// mono 不前进时(首帧/同帧 re-feed/时钟回跳)。无上限:Windows QPC 单调
// 不回退,合法间隙包括编码器强制 IDR 后的 lookahead 回填(~0.55s@30fps)
// 与长静止后恢复(用户离开桌面几十秒后操作,间隙 = 静止时长)——二者都
// 是真实时间,必须原样计入 RTP 时间轴。此前 2s 钳位让恢复首帧的时间戳
// 落后真实时间,Chrome 判其"迟到"丢弃,画面停在旧帧直到某帧被接受才
// 跳变——"回退帧"观感。host 重启/pipe 重连不会误入此路径:会话重建
// 时 Publisher 全新,lastMono 从 0 开始。
func (p *Publisher) frameDuration(monoUs uint64) time.Duration {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.lastMono != 0 && monoUs > p.lastMono {
		d := time.Duration(monoUs-p.lastMono) * time.Microsecond
		p.lastMono = monoUs
		return d
	}
	p.lastMono = monoUs
	return p.defDur
}

// Stats 返回透传统计快照(计数器均为累计值):帧/字节/抑制计数来自
// viewer 发送器,RTCP 计数来自本 Publisher 的 RTCP 泵。
func (p *Publisher) Stats() PubStats {
	vs := p.vs.Stats()
	return PubStats{
		FramesWritten:  vs.FramesSent,
		BytesWritten:   vs.BytesSent,
		PreConnDropped: p.stats.preConnDropped.Load(),
		PreKeyDropped:  vs.PreKeyDropped,
		PLI:            p.stats.pli.Load(),
		FIR:            p.stats.fir.Load(),
		NACK:           p.stats.nack.Load(),
		TWCC:           p.stats.twcc.Load(),
	}
}

// Close 关闭发送器(停 pace 泵)与 PeerConnection,等 RTCP 泵退出;幂等。
// 关闭时打一条统计摘要日志(不含任何凭据;frame-meta 的 56 字节记录与
// 会话键从不入日志)。顺序(修正轮):PC 先于 frame-meta 泵关闭——
// pc.Close 会中止 SCTP 关联、解阻塞可能卡死的 dc.Send;relay.close 自身
// 再有界等待兜底,遥测面绝不悬挂会话收线。
func (p *Publisher) Close() error {
	var err error
	p.closeO.Do(func() {
		p.vs.Close()
		err = p.pc.Close()
		if r := p.metaRelay; r != nil {
			r.close()
		}
		p.wg.Wait()
		st := p.Stats()
		p.log.Info("desktop publisher closed",
			"frames", st.FramesWritten, "bytes", st.BytesWritten,
			"preConnDropped", st.PreConnDropped, "preKeyDropped", st.PreKeyDropped,
			"pli", st.PLI, "fir", st.FIR, "nack", st.NACK, "twcc", st.TWCC,
			"metaDrops", p.TelemetryDrops())
	})
	return err
}

// ---- frame-meta 面(M3 Task 4;布局/键控 hash 见 frame_meta.go)----

// attachFrameMeta 创建 frame-meta 通道(unordered + 不重传)并启动非阻塞
// 中继泵。必须在 HandleOffer 之前调用(与输入通道同一约束:answer 需含
// SCTP);恰调用一次。发送器的帧边界 seam(NewPublisher 已接线)自此有
// 出口——连接前帧进不了发送器(WriteFrame 的 connReady 门),时序上
// attach 必先于任何 meta 产生。
func (p *Publisher) attachFrameMeta() error {
	dc, err := p.newDataChannel(dcLabelFrameMeta, true)
	if err != nil {
		return err
	}
	p.metaDC = dc
	p.metaRelay = newFrameMetaRelay(dc.Send)
	return nil
}

// TelemetryDrops 返回 frame-meta 遥测丢弃计数(中继缓冲满 + DC 发送
// 失败;遥测只丢不阻塞)。
func (p *Publisher) TelemetryDrops() uint64 {
	if r := p.metaRelay; r != nil {
		return r.drops.Load()
	}
	return 0
}

// makeFrameMeta 是发送器 seam 的组装侧(Enqueue 持锁调用):身份 + 该帧
// RTP 包实际将盖章的 per-viewer 时戳 → 会话键控记录。
func (p *Publisher) makeFrameMeta(f Frame, rtpTS uint32) *FrameMetaV1 {
	if p.metaRelay == nil {
		return nil // attach 前(防御;连接前帧本就进不来)
	}
	m := newFrameMetaV1(p.metaKey, f, rtpTS)
	return &m
}

// onFrameSent 是发送器 seam 的汇出侧(本帧最后一包成功写出后,持锁
// 调用):非阻塞入中继;缓冲满即丢弃计数,绝不影响发送面。
func (p *Publisher) onFrameSent(m *FrameMetaV1) {
	if r := p.metaRelay; r != nil {
		r.enqueue(*m)
	}
}

// frameMetaRelay 是发送路径 → frame-meta DataChannel 的非阻塞中继:
// enqueue 用 select/default(缓冲满即丢 + telemetryDrops 计数,绝不为
// 遥测阻塞媒体 pacing);泵 goroutine 独占调用 DC.Send——SCTP 拥塞时
// Send 可能长时间不返回,绝不能发生在 ViewerSender 的锁内。
type frameMetaRelay struct {
	send   func([]byte) error
	q      chan []byte
	done   chan struct{}
	drops  atomic.Uint64
	closeO sync.Once
	wg     sync.WaitGroup
}

// frameMetaQueueDepth 是中继缓冲容量(帧数;~1s @30fps。遥测可丢:
// 消费不动即丢弃,绝不反压)。
const frameMetaQueueDepth = 32

// frameMetaCloseGrace 是 close 等待泵退出的宽限(修正轮:Publisher.Close
// 先关 PC 解阻塞 dc.Send,正常路径远在宽限内退出;宽限是「pion 关联
// 关闭仍不解阻塞」形态的兜底——宁可放弃遥测泵,绝不悬挂会话收线)。
const frameMetaCloseGrace = 500 * time.Millisecond

func newFrameMetaRelay(send func([]byte) error) *frameMetaRelay {
	r := &frameMetaRelay{
		send: send,
		q:    make(chan []byte, frameMetaQueueDepth),
		done: make(chan struct{}),
	}
	r.wg.Add(1)
	go r.loop()
	return r
}

// enqueue 提交一条记录(编码即唯一分配:记录缓冲被 SCTP 出口持有,
// 不可复用;见 encodeFrameMetaV1)。永不阻塞。
func (r *frameMetaRelay) enqueue(m FrameMetaV1) {
	b := encodeFrameMetaV1(m)
	select {
	case r.q <- b:
	default:
		r.drops.Add(1) // 缓冲满:丢 meta,不丢媒体
	}
}

func (r *frameMetaRelay) loop() {
	defer r.wg.Done()
	for {
		select {
		case b := <-r.q:
			if r.send == nil || r.send(b) != nil {
				r.drops.Add(1)
			}
		case <-r.done:
			return
		}
	}
}

// close 停泵并等其退出(有界:宽限后放弃——最坏泄漏一个 goroutine,
// 由 Publisher.Close 先行的 pc.Close 解阻塞兜底);幂等。q 不 close:
// 关闭后残余 enqueue 只会堆积计丢弃,绝不 panic。
func (r *frameMetaRelay) close() {
	r.closeO.Do(func() { close(r.done) })
	waited := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(frameMetaCloseGrace):
		// 放弃:泵仍卡在 DC.Send(关联关闭未解阻塞)。会话收线优先。
	}
}

// ConnectionState 透传 PC 连接状态(会话侧观测用)。
func (p *Publisher) ConnectionState() webrtc.PeerConnectionState {
	return p.pc.ConnectionState()
}

// IsKeyframeAU 判定 Annex-B AU 是否含 IDR slice(NAL type 5)或 SPS(7)。
// 供 viewer 侧统计与 e2e 断言复用;容忍 3/4 字节起始码(4 字节码内嵌
// 00 00 01,同样命中)与无起始码裸 NAL。slice 载荷经 emulation prevention
// 不可能伪造起始码,故逐码扫描即安全。
func IsKeyframeAU(au []byte) bool {
	if len(au) == 0 {
		return false
	}
	startCode := []byte{0, 0, 1}
	sawCode := false
	for from := 0; from+3 <= len(au)-1; {
		j := bytes.Index(au[from:], startCode)
		if j < 0 {
			break
		}
		hdr := from + j + 3
		if hdr >= len(au) {
			break
		}
		sawCode = true
		if t := au[hdr] & 0x1F; t == 5 || t == 7 {
			return true
		}
		from = hdr
	}
	if !sawCode {
		t := au[0] & 0x1F
		return t == 5 || t == 7
	}
	return false
}
