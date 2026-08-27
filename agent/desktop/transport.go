// transport.go — Pion publisher(M1-Slice2 Task 4)。
//
// API 选择:TrackLocalStaticRTP + Pion H264Payloader。Annex-B AU 由 Pion
// 完成 STAP-A/FU-A 分包，每帧的所有 RTP 包都直接使用该 AU 的
// PresentMonoUs@90kHz 时戳；不再用 sample.Duration 隐式推进时间轴。
package desktop

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
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
	Log             *slog.Logger
}

// Publisher 承载一个 viewer 会话的发送侧 PeerConnection + H264 视频轨。
type Publisher struct {
	log        *slog.Logger
	pc         *webrtc.PeerConnection
	sender     *webrtc.RTPSender
	track      *webrtc.TrackLocalStaticRTP
	packetizer rtp.Packetizer
	rtpClock   *rtpClock
	writeMu    sync.Mutex
	// defDur/lastMono 只保留给旧 frameDuration 单测，不在发送热路径上。
	defDur        time.Duration
	stats         pubStats
	wg            sync.WaitGroup
	closeO        sync.Once
	connReady     atomic.Bool // ICE+DTLS 已通(到达即向 host 请求 fresh IDR)
	keyFn         func(reason string)
	iceFn         func(webrtc.ICECandidateInit)
	stateMu       sync.Mutex
	lastMono      uint64
	started       bool // 连接后首个 key 帧已写出(viewer 可解码起点)
	intervalStats frameIntervalStats
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
	api, err := newDesktopAPI()
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
		log:    log,
		pc:     pc,
		sender: sender,
		track:  track,
		// The packetizer's payload type (102) and SSRC (0) literals are inert:
		// both are per-binding values overwritten by TrackLocalStaticRTP.writeRTP
		// for each binding before the packet hits the wire.
		packetizer: rtp.NewPacketizer(1200, 102, 0, &codecs.H264Payloader{}, rtp.NewRandomSequencer(), 90000),
		rtpClock:   newRTPClock(rand.Uint32()),
	}
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

// WriteFrame 将一帧 Annex-B AU 分包后写入视频轨。同一 AU 的所有
// 包使用同一个 PresentMonoUs@90kHz 时戳，仅最后一包置 Marker。
//
// 首帧语义(双保险,见 NewPublisher 的 Connected 钩子):连接就绪前的帧
// 一律丢弃(RTPSender 未启动,写了也静默蒸发);连接后丢弃 delta 直到
// 首个 key 帧成功写出——viewer 收到的第一帧永远是 IDR,静止桌面/慢启动
// 场景无画面黑洞。
func (p *Publisher) WriteFrame(f Frame) error {
	if len(f.AU) == 0 {
		return nil
	}
	if !p.connReady.Load() {
		p.stats.preConnDropped.Add(1)
		return nil
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.stateMu.Lock()
	started := p.started
	p.stateMu.Unlock()
	if !started && !f.Key {
		p.stats.preKeyDropped.Add(1)
		return nil
	}
	hadPreviousTimestamp := p.rtpClock.started
	previousMonoUs := p.rtpClock.last
	timestamp, err := p.rtpClock.Timestamp(f.PresentMonoUs)
	if err != nil {
		return err
	}
	packets := p.packetizer.Packetize(f.AU, 0)
	if len(packets) == 0 {
		return errors.New("desktop: H264 payloader produced no RTP packets")
	}
	for i, packet := range packets {
		packet.Timestamp = timestamp
		packet.Marker = i == len(packets)-1
	}
	ws := time.Now()
	for i, packet := range packets {
		if err := p.track.WriteRTP(packet); err != nil {
			return fmt.Errorf("desktop: write RTP packet %d/%d: %w", i+1, len(packets), err)
		}
	}
	if ms := time.Since(ws).Milliseconds(); ms > 20 {
		p.log.Info("desktop write_rtp slow", "ms", ms, "auBytes", len(f.AU), "rtpPackets", len(packets))
	}
	if hadPreviousTimestamp {
		if deltaUs := f.PresentMonoUs - previousMonoUs; deltaUs <= 1_000_000 {
			p.intervalStats.add(time.Duration(deltaUs)*time.Microsecond, p.log)
		}
	}
	if !started {
		p.stateMu.Lock()
		p.started = true
		p.stateMu.Unlock()
	}
	p.stats.frames.Add(1)
	p.stats.bytes.Add(uint64(len(f.AU)))
	return nil
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

// Stats 返回透传统计快照(计数器均为累计值)。
func (p *Publisher) Stats() PubStats {
	return PubStats{
		FramesWritten:  p.stats.frames.Load(),
		BytesWritten:   p.stats.bytes.Load(),
		PreConnDropped: p.stats.preConnDropped.Load(),
		PreKeyDropped:  p.stats.preKeyDropped.Load(),
		PLI:            p.stats.pli.Load(),
		FIR:            p.stats.fir.Load(),
		NACK:           p.stats.nack.Load(),
		TWCC:           p.stats.twcc.Load(),
	}
}

// Close 关闭 PeerConnection 并等 RTCP 泵退出;幂等。关闭时打一条统计摘要
// 日志(不含任何凭据)。
func (p *Publisher) Close() error {
	var err error
	p.closeO.Do(func() {
		err = p.pc.Close()
		p.wg.Wait()
		st := p.Stats()
		p.log.Info("desktop publisher closed",
			"frames", st.FramesWritten, "bytes", st.BytesWritten,
			"preConnDropped", st.PreConnDropped, "preKeyDropped", st.PreKeyDropped,
			"pli", st.PLI, "fir", st.FIR, "nack", st.NACK, "twcc", st.TWCC)
	})
	return err
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
