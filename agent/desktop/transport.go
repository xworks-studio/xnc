// transport.go — Pion publisher(M1-Slice2 Task 4)。
//
// API 选择(与 plan「写 track」约定):TrackLocalStaticSample + WriteSample
// (pion/webrtc v4.2.x 的惯用发送 API),不手写 RTP 打包——pion 内置
// H264Payloader 消费 Annex-B AU:SPS/PPS 经 STAP-A 合并、大 NALU FU-A 分片
// (packetization-mode=1),viewer 侧 pion H264 depacketizer 还原 4 字节起始码。
// RTP 时戳 = WriteSample 按 sample.Duration 累计(duration*90kHz,带小数
// 余量):我们用相邻帧 mono_us 差作为 Duration,故时戳序列 ≡ mono_us@90kHz
// (模随机起始偏移与亚 tick 舍入)。mono 差非法(≤0 或 >500ms)时回退
// DefaultDuration(由 HOST_HELLO fps 推得)。
package desktop

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
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
	// DefaultDuration 是 mono 差不可用时(首帧/时钟回退)的帧时长。
	DefaultDuration time.Duration
	Log             *slog.Logger
}

// Publisher 承载一个 viewer 会话的发送侧 PeerConnection + H264 视频轨。
type Publisher struct {
	log       *slog.Logger
	pc        *webrtc.PeerConnection
	sender    *webrtc.RTPSender
	track     *webrtc.TrackLocalStaticSample
	defDur    time.Duration
	stats     pubStats
	wg        sync.WaitGroup
	closeO    sync.Once
	connReady atomic.Bool // ICE+DTLS 已通(到达即向 host 请求 fresh IDR)
	keyFn     func(reason string)
	iceFn     func(webrtc.ICECandidateInit)
	stateMu   sync.Mutex
	lastMono  uint64
	started   bool // 连接后首个 key 帧已写出(viewer 可解码起点)
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
	defDur := cfg.DefaultDuration
	if defDur <= 0 {
		defDur = 33 * time.Millisecond
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
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
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
		log: log, pc: pc, sender: sender, track: track, defDur: defDur,
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
	// 首帧语义(关键):ICE+DTLS 就绪前 RTPSender 尚未启动,WriteSample
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

// WriteFrame 将一帧 Annex-B AU 写入视频轨:Duration 取 mono_us 差
// (非法则回退默认),由 pion 计入 RTP 时戳(见文件头)。
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
	p.stateMu.Lock()
	started := p.started
	p.stateMu.Unlock()
	if !started && !f.Key {
		p.stats.preKeyDropped.Add(1)
		return nil
	}
	d := p.frameDuration(f.MonoUs)
	ws := time.Now()
	if err := p.track.WriteSample(media.Sample{Data: f.AU, Duration: d}); err != nil {
		return fmt.Errorf("desktop: write sample: %w", err)
	}
	if ms := time.Since(ws).Milliseconds(); ms > 20 {
		p.log.Info("desktop write_sample slow", "ms", ms, "auBytes", len(f.AU))
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
