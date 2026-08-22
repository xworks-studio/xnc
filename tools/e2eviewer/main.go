// e2eviewer - XNC M1-Slice2 桌面视频端到端 viewer(Go/Pion)。
//
// 两种运行模式:
//
//   - direct(T4 本机验证,不经 server):--direct-pipe + --direct-secret 直连
//     xnc-desktop --console-rt 的 rt pipe;同进程内跑 agent 侧 Publisher
//     (xnc/agent/desktop)+ 本 viewer PeerConnection,信令内存粘合。给
//     --turn 时两端 relay-only,媒体真走 coturn(T4 的网络级验证)。
//   - server(T6,经 dev server;REST 端点 = T5):--server/--node/--token
//     POST /api/nodes/{node}/desktop → {sessionId,token,websocketUrl,turn},
//     会话 WS 走 agent/desktop/session.go 的 JSON 信令词汇
//     (ready/offer/answer/ice/state/error)。
//
// 断言(--expect-*;0/1 退出码,e2 脚本消费):
//
//	首帧时延(run 开始→首个解码 AU)≤ --expect-first-frame-ms(默认 2000)
//	关键帧数 ≥ --expect-keyframes(默认 1)
//	帧数 ≤ --expect-frames-max(0=跳过;静止场景断言低产出,T6 门②)
//	首个 AU 为 IDR(--expect-first-key;静止期第二 viewer 的按需 IDR
//	承接断言,T6 门③)
//	若发过 PLI(--pli-interval 周期 或 --pli-at 单发):PLI→新 IDR ≤
//	--expect-pli-idr-ms(默认 2000;0=跳过,T6 门④)
//
// 输出:--out dump.h264 落盘 RTP 重组的 Annex-B 流(depacketizer 已带
// 4 字节起始码,直接追加);stdout 尾部一行 JSON 摘要(--json 仅打 JSON)。
//
// 凭据红线:TURN credential / pipe secret 绝不打印。
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"

	"xnc/agent/desktop"
	"xnc/agent/desktoppipe"
)

// config 是 flag 集合(两种模式共用)。
type config struct {
	server, node, token string
	turn                string   // user:pass@host:port(dev 快捷形态)
	turnURLs            []string // 显式 URL 覆盖(可重复;凭据仍取 --turn)
	directPipe          string
	directSecretHex     string
	out                 string
	duration            time.Duration
	expectFirstFrameMs  int
	expectKeyframes     int
	expectFramesMax     int // 0=跳过;帧数上限(静止场景低产出断言)
	expectFirstKey      bool
	expectPliIdrMs      int
	pliInterval         time.Duration
	pliAt               time.Duration // 0=off;run 内该时刻单发一次 PLI
	keyframeRetryAfter  time.Duration // 0=off;server 模式无帧到达超过此时长发 keyframe-req(丢包自救)
	jsonOnly            bool
}

func parseFlags() *config {
	c := &config{}
	flag.StringVar(&c.server, "server", "", "server base URL (server mode), e.g. http://192.168.1.12:18080")
	flag.StringVar(&c.node, "node", "", "node id (server mode)")
	flag.StringVar(&c.token, "token", "", "API bearer token (server mode)")
	flag.StringVar(&c.turn, "turn", "", "TURN creds user:pass@host:port (e.g. xncdev:xncdev-secret@192.168.1.12:3478)")
	flag.Var(&stringList{&c.turnURLs}, "turn-url", "explicit TURN URL (repeatable; overrides --turn host:port form)")
	flag.StringVar(&c.directPipe, "direct-pipe", "", "direct mode: xnc-desktop rt pipe name")
	flag.StringVar(&c.directSecretHex, "direct-secret", "", "direct mode: pipe secret (64 hex chars)")
	flag.StringVar(&c.out, "out", "", "dump received Annex-B stream to file")
	flag.DurationVar(&c.duration, "duration", 30*time.Second, "run duration")
	flag.IntVar(&c.expectFirstFrameMs, "expect-first-frame-ms", 2000, "fail if first frame later than this (0=skip)")
	flag.IntVar(&c.expectKeyframes, "expect-keyframes", 1, "fail if fewer keyframes (0=skip)")
	flag.IntVar(&c.expectFramesMax, "expect-frames-max", 0, "fail if MORE frames than this (0=skip; static-desktop gate)")
	flag.BoolVar(&c.expectFirstKey, "expect-first-key", false, "fail if the first decoded AU is not a keyframe")
	flag.IntVar(&c.expectPliIdrMs, "expect-pli-idr-ms", 2000, "fail if PLI->IDR slower than this (0=skip)")
	flag.DurationVar(&c.pliInterval, "pli-interval", 0, "send RTCP PLI every interval (0=off)")
	flag.DurationVar(&c.pliAt, "pli-at", 0, "send one RTCP PLI this far into the run (0=off)")
	flag.DurationVar(&c.keyframeRetryAfter, "keyframe-retry-after", 0,
		"server mode: if no AU has decoded yet for this long, send a {keyframe-req} signaling frame (0=off; lossy-link self-heal)")
	flag.BoolVar(&c.jsonOnly, "json", false, "print only the JSON summary")
	flag.Parse()
	return c
}

type stringList struct{ dst *[]string }

func (s *stringList) String() string     { return strings.Join(*s.dst, ",") }
func (s *stringList) Set(v string) error { *s.dst = append(*s.dst, v); return nil }

// parseTurn 解析 "user:pass@host:port" → ICE servers(tcp + udp 两个 URL,
// dev 全局约束:transport=tcp 优先,udp 兜底)。显式 --turn-url 优先
// (凭据仍取 --turn)。空 spec 且无显式 URL → nil(直连模式无 TURN)。
func parseTurn(spec string, explicit []string) ([]webrtc.ICEServer, error) {
	user, pass, host, ok := splitTurn(spec)
	switch {
	case len(explicit) > 0:
		if !ok {
			return nil, errors.New("--turn-url needs --turn credentials (user:pass@host:port)")
		}
		return []webrtc.ICEServer{{URLs: explicit, Username: user, Credential: pass}}, nil
	case spec == "":
		return nil, nil
	case !ok:
		return nil, errors.New("--turn must be user:pass@host:port")
	default:
		return []webrtc.ICEServer{{
			URLs:       []string{"turn:" + host + "?transport=tcp", "turn:" + host},
			Username:   user,
			Credential: pass,
		}}, nil
	}
}

func splitTurn(spec string) (user, pass, host string, ok bool) {
	up, at, found := strings.Cut(spec, "@")
	if !found {
		return "", "", "", false
	}
	user, pass, ok = strings.Cut(up, ":")
	if !ok || user == "" || pass == "" || at == "" {
		return "", "", "", false
	}
	return user, pass, at, true
}

// ---- viewer(两模式共用)----

// viewer 是接收侧:recvonly PC + 样本重组 + 统计/落盘/PLI。
type viewer struct {
	pc       *webrtc.PeerConnection
	ice      []webrtc.ICEServer
	relay    bool
	out      *os.File
	start    time.Time
	log      *slog.Logger
	trackCh  chan *webrtc.TrackRemote
	plisSent atomic.Uint64

	// keyframeReqFn(server 模式注入):发送 {"type":"keyframe-req"} 信令帧。
	// 丢包链路上首个 IDR 可能整包损坏,viewer 以此自救重请(T6 门③④)。
	keyframeReqFn func()
	keyframeReqs  atomic.Uint64
	lastKeyReqAt  atomic.Int64 // 最近一次 keyframe-req 发出(UnixNano)
	lastAUAt      atomic.Int64 // 最近一个解码 AU(UnixNano;0=尚无)

	rtpPkts   atomic.Uint64
	frames    atomic.Uint64
	keyframes atomic.Uint64
	bytes     atomic.Uint64
	firstAt   atomic.Int64 // UnixNano;0 = 未收帧
	firstKey  atomic.Bool  // 首个解码 AU 是否 IDR(门③承接断言)

	pliMu     atomic.Int64 // 最近一次 PLI 发出(UnixNano;0=无待验证)
	pliIDRMax atomic.Int64 // PLI→IDR 最大时延(ms;0=未发生)
}

func newViewer(c *config, ice []webrtc.ICEServer, relay bool, log *slog.Logger) (*viewer, error) {
	m := &webrtc.MediaEngine{}
	if err := desktop.RegisterDesktopCodecs(m); err != nil {
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		return nil, err
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir))
	policy := webrtc.ICETransportPolicyAll
	if relay {
		policy = webrtc.ICETransportPolicyRelay
		if len(ice) == 0 {
			return nil, errors.New("relay-only viewer requires TURN servers")
		}
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: ice, ICETransportPolicy: policy})
	if err != nil {
		return nil, err
	}
	v := &viewer{pc: pc, ice: ice, relay: relay, start: time.Now(), log: log, trackCh: make(chan *webrtc.TrackRemote, 1)}
	if c.out != "" {
		f, err := os.Create(c.out)
		if err != nil {
			_ = pc.Close()
			return nil, fmt.Errorf("out file: %w", err)
		}
		v.out = f
	}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		_ = pc.Close()
		return nil, err
	}
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		select {
		case v.trackCh <- tr:
		default:
		}
		sb := samplebuilder.New(1024, &codecs.H264Packet{}, 90000)
		for {
			pkt, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			v.rtpPkts.Add(1)
			sb.Push(pkt)
			for {
				smp := sb.Pop()
				if smp == nil {
					break
				}
				v.frames.Add(1)
				v.bytes.Add(uint64(len(smp.Data)))
				now := time.Now().UnixNano()
				v.lastAUAt.Store(now)
				if v.firstAt.CompareAndSwap(0, now) {
					v.firstKey.Store(desktop.IsKeyframeAU(smp.Data))
				}
				if desktop.IsKeyframeAU(smp.Data) {
					if t := v.pliMu.Load(); t != 0 {
						ms := time.Since(time.Unix(0, t)).Milliseconds()
						for {
							old := v.pliIDRMax.Load()
							if ms <= old || v.pliIDRMax.CompareAndSwap(old, ms) {
								break
							}
						}
						v.pliMu.Store(0)
					}
					v.keyframes.Add(1)
				}
				if v.out != nil {
					_, _ = v.out.Write(smp.Data)
				}
			}
		}
	})
	return v, nil
}

// waitConnected 轮询至 Connected(时限 d)。
func (v *viewer) waitConnected(d time.Duration) error {
	deadline := time.Now().Add(d)
	for v.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
		if time.Now().After(deadline) {
			return fmt.Errorf("viewer not connected after %v (state=%v)", d, v.pc.ConnectionState())
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil
}

// sendPLI 向首个 track 发 RTCP PLI(无 track 时为 no-op 返回 false)。
func (v *viewer) sendPLI(d time.Duration) bool {
	select {
	case tr := <-v.trackCh:
		v.trackCh <- tr // 放回供下次使用
		if err := v.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(tr.SSRC())}}); err != nil {
			v.log.Warn("send PLI failed", "err", err)
			return false
		}
		v.plisSent.Add(1)
		v.pliMu.Store(time.Now().UnixNano())
		return true
	case <-time.After(d):
		return false
	}
}

// summary 是断言输入与 --json 输出形态(T6 脚本契约)。
type summary struct {
	Mode             string   `json:"mode"`
	Connected        bool     `json:"connected"`
	FirstFrameMs     int64    `json:"firstFrameMs"`
	FirstKey         bool     `json:"firstKey"`
	RtpPackets       uint64   `json:"rtpPackets"`
	Frames           uint64   `json:"frames"`
	Keyframes        uint64   `json:"keyframes"`
	Bytes            uint64   `json:"bytes"`
	DumpFile         string   `json:"dumpFile,omitempty"`
	Relay            bool     `json:"relay"`
	PlisSent         uint64   `json:"plisSent"`
	KeyframeReqs     uint64   `json:"keyframeReqs"`
	PliToIdrMaxMs    int64    `json:"pliToIdrMaxMs"`
	DurationMs       int64    `json:"durationMs"`
	AssertionsPassed bool     `json:"assertionsPassed"`
	Failures         []string `json:"failures,omitempty"`
}

func (v *viewer) collect(mode, dump string, ran time.Duration) *summary {
	s := &summary{
		Mode: mode, Relay: v.relay, DumpFile: dump,
		RtpPackets: v.rtpPkts.Load(), Frames: v.frames.Load(),
		Keyframes: v.keyframes.Load(), Bytes: v.bytes.Load(),
		PlisSent: v.plisSent.Load(), KeyframeReqs: v.keyframeReqs.Load(), PliToIdrMaxMs: v.pliIDRMax.Load(),
		DurationMs: ran.Milliseconds(),
	}
	if t := v.firstAt.Load(); t != 0 {
		s.FirstFrameMs = time.Unix(0, t).Sub(v.start).Milliseconds()
		s.Connected = true
		s.FirstKey = v.firstKey.Load()
	}
	return s
}

// evaluate 施加 --expect-* 断言(就地填充 Failures/AssertionsPassed)。
func (s *summary) evaluate(c *config) {
	if c.expectFirstFrameMs > 0 {
		if s.FirstFrameMs == 0 {
			s.Failures = append(s.Failures, fmt.Sprintf("no first frame within %v", c.duration))
		} else if s.FirstFrameMs > int64(c.expectFirstFrameMs) {
			s.Failures = append(s.Failures, fmt.Sprintf("first frame %dms > %dms", s.FirstFrameMs, c.expectFirstFrameMs))
		}
	}
	if c.expectKeyframes > 0 && s.Keyframes < uint64(c.expectKeyframes) {
		s.Failures = append(s.Failures, fmt.Sprintf("keyframes %d < %d", s.Keyframes, c.expectKeyframes))
	}
	if c.expectFramesMax > 0 && s.Frames > uint64(c.expectFramesMax) {
		s.Failures = append(s.Failures, fmt.Sprintf("frames %d > %d (static desktop should stay near-silent)", s.Frames, c.expectFramesMax))
	}
	if c.expectFirstKey {
		if s.FirstFrameMs == 0 {
			s.Failures = append(s.Failures, "no frames at all; cannot verify first AU is IDR")
		} else if !s.FirstKey {
			s.Failures = append(s.Failures, "first decoded AU was not a keyframe (join must land on IDR)")
		}
	}
	if c.expectPliIdrMs > 0 && s.PlisSent > 0 {
		if s.PliToIdrMaxMs == 0 {
			s.Failures = append(s.Failures, fmt.Sprintf("%d PLI(s) sent but no IDR observed afterwards", s.PlisSent))
		} else if s.PliToIdrMaxMs > int64(c.expectPliIdrMs) {
			s.Failures = append(s.Failures, fmt.Sprintf("PLI->IDR %dms > %dms", s.PliToIdrMaxMs, c.expectPliIdrMs))
		}
	}
	s.AssertionsPassed = len(s.Failures) == 0
}

func (s *summary) report(c *config) {
	jb, _ := json.Marshal(s)
	if c.jsonOnly {
		fmt.Println(string(jb))
		return
	}
	fmt.Printf("--- e2eviewer summary ---%s\n", string(jb))
	for _, f := range s.Failures {
		fmt.Printf("FAIL: %s\n", f)
	}
}

// runFor 跑满 duration:PLI 心跳(可选)+ 单发 PLI(--pli-at)+ 中途进度一行。
func (v *viewer) runFor(ctx context.Context, c *config) {
	deadline := time.Now().Add(c.duration)
	var pliTick <-chan time.Time
	if c.pliInterval > 0 {
		t := time.NewTicker(c.pliInterval)
		defer t.Stop()
		pliTick = t.C
	}
	var pliOnce <-chan time.Time
	if c.pliAt > 0 {
		t := time.NewTimer(c.pliAt)
		defer t.Stop()
		pliOnce = t.C
	}
	for {
		if ctx.Err() != nil || time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-pliTick:
			v.sendPLI(2 * time.Second)
		case <-pliOnce:
			pliOnce = nil // 单发
			if ok := v.sendPLI(2 * time.Second); ok {
				v.log.Info("one-shot PLI sent (--pli-at)")
			}
		case <-time.After(200 * time.Millisecond):
			// 丢包自救:首帧迟迟未落地(首 IDR 被链路打散)→ 经信令重请
			// 关键帧,节流 = 冷却期不短于 --keyframe-retry-after 的一半。
			if c.keyframeRetryAfter > 0 && v.keyframeReqFn != nil && v.frames.Load() == 0 {
				ref := v.lastKeyReqAt.Load()
				if ref == 0 {
					ref = v.start.UnixNano()
				}
				if time.Since(time.Unix(0, ref)) >= c.keyframeRetryAfter {
					v.keyframeReqFn()
					v.lastKeyReqAt.Store(time.Now().UnixNano())
				}
			}
			v.log.Info("progress", "frames", v.frames.Load(), "keyframes", v.keyframes.Load())
		}
	}
}

// ---- direct 模式 ----

func runDirect(c *config) (*summary, error) {
	log := slog.Default()
	secret, err := hex.DecodeString(c.directSecretHex)
	if err != nil || len(secret) == 0 {
		return nil, errors.New("--direct-secret must be non-empty hex")
	}
	pipe := c.directPipe
	if !strings.Contains(pipe, `\`) {
		pipe = `\\.\pipe\` + pipe // 裸名归一化(shell 传完整路径常被吞反斜杠)
	}
	ice, err := parseTurn(c.turn, c.turnURLs)
	if err != nil {
		return nil, err
	}
	relay := len(ice) > 0
	log.Info("direct mode", "pipe", pipe, "relay", relay) // 无 secret

	subID, err := randSubID()
	if err != nil {
		return nil, err
	}
	sub, err := desktoppipe.Dial(pipe, string(secret), subID, desktoppipe.SubOpts{})
	if err != nil {
		return nil, fmt.Errorf("dial pipe: %w", err)
	}
	hello := sub.Hello()
	log.Info("attached to desktop host", "w", hello.W, "h", hello.H, "fps", hello.Fps, "gen", hello.Gen)

	pub, err := desktop.NewPublisher(desktop.PublisherConfig{
		ICEServers: ice, RelayOnly: relay, DefaultDuration: 33 * time.Millisecond, Log: log,
	})
	if err != nil {
		_ = sub.Close()
		return nil, err
	}
	pub.OnKeyRequest(func(reason string) { _ = sub.RequestKeyframe(reason) })

	v, err := newViewer(c, ice, relay, log)
	if err != nil {
		_ = pub.Close()
		_ = sub.Close()
		return nil, err
	}

	// 内存信令粘合:offer/answer + 双向 trickle。回调必须先于 SetLocal 注册
	// ——TURN 分配在 LAN 上毫秒级完成,晚注册会整批丢失候选事件(表现为
	// 双方永远 connecting:pion 的 OnICECandidate 不重放已发事件)。
	pub.OnICECandidate(func(ci webrtc.ICECandidateInit) {
		if ci.Candidate != "" {
			_ = v.pc.AddICECandidate(ci)
		}
	})
	v.pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand != nil {
			_ = pub.AddCandidate(cand.ToJSON())
		}
	})
	offer, err := v.pc.CreateOffer(nil)
	if err != nil {
		return nil, err
	}
	if err := v.pc.SetLocalDescription(offer); err != nil {
		return nil, err
	}
	answerSDP, err := pub.HandleOffer(offer.SDP)
	if err != nil {
		return nil, err
	}
	if err := v.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answerSDP}); err != nil {
		return nil, err
	}

	// 帧泵:pipe → publisher(agent 会话同款语义)。
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for f := range sub.FrameCh() {
			if err := pub.WriteFrame(desktop.Frame{Key: f.Key, MonoUs: f.MonoUs, AU: f.AU}); err != nil {
				log.Warn("frame pump stopped", "err", err)
				return
			}
		}
	}()

	if err := v.waitConnected(25 * time.Second); err != nil {
		return v.collect("direct", c.out, time.Since(v.start)), err
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.duration)
	defer cancel()
	v.runFor(ctx, c)

	_ = pub.Close()
	_ = sub.Close()
	<-pumpDone
	_ = v.pc.Close()
	if v.out != nil {
		_ = v.out.Close()
	}
	return v.collect("direct", c.out, time.Since(v.start)), nil
}

func randSubID() (uint32, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	id := binary.LittleEndian.Uint32(b[:])
	if id == 0 {
		id = 1
	}
	return id, nil
}

// ---- server 模式(T5 端点 + session.go 信令词汇;T6 消费)----

type desktopOpenResp struct {
	SessionID    string             `json:"sessionId"`
	Token        string             `json:"token"`
	WebsocketURL string             `json:"websocketUrl"`
	Turn         *desktopTurnConfig `json:"turn"`
}

type desktopTurnConfig struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

func runServer(c *config) (*summary, error) {
	log := slog.Default()
	if c.server == "" || c.node == "" || c.token == "" {
		return nil, errors.New("server mode needs --server, --node and --token")
	}
	// 打开会话(T5 契约:POST /api/nodes/{id}/desktop → websocketUrl+turn)。
	body, _ := json.Marshal(map[string]any{"signaling": "webrtc"})
	req, err := http.NewRequest(http.MethodPost,
		strings.TrimRight(c.server, "/")+"/api/nodes/"+url.PathEscape(c.node)+"/desktop",
		strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("desktop open: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// T5 端点统一走 startSession 路径 → 202 Accepted（异步会话创建语义）。
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated &&
		resp.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf("desktop open: HTTP %d (endpoint arrives with T5)", resp.StatusCode)
	}
	var open desktopOpenResp
	if err := json.Unmarshal(rb, &open); err != nil {
		return nil, fmt.Errorf("desktop open: bad json: %w", err)
	}
	ice := []webrtc.ICEServer{}
	relay := false
	if open.Turn != nil && len(open.Turn.URLs) > 0 {
		ice = append(ice, webrtc.ICEServer{URLs: open.Turn.URLs, Username: open.Turn.Username, Credential: open.Turn.Credential})
		relay = true
	}
	log.Info("session opened", "id", open.SessionID, "relay", relay) // 无凭据

	// 会话 WS(词汇:agent/desktop/session.go)。
	wsURL := open.WebsocketURL
	if strings.HasPrefix(wsURL, "/") {
		wsURL = strings.Replace(strings.Replace(c.server, "https://", "wss://", 1), "http://", "ws://", 1) + wsURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.duration+30*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("session ws: %w", err)
	}
	ws.SetReadLimit(1 << 20)
	defer ws.CloseNow()

	v, err := newViewer(c, ice, relay, log)
	if err != nil {
		return nil, err
	}
	// 信令级关键帧重请(丢包自救):keyframeReqFn 由 runFor 的冷却逻辑调用。
	v.keyframeReqFn = func() {
		v.keyframeReqs.Add(1)
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		defer wcancel()
		b, _ := json.Marshal(map[string]any{"type": "keyframe-req"})
		_ = ws.Write(wctx, websocket.MessageText, b)
	}

	// 入站信令泵:单 goroutine 读全部帧——coder/websocket 禁止并发 Reader。
	// T4 写法里 waitReady 与本泵并发读同一连接,server 模式当时未经 live
	// 验证,T6 实测暴露:泵死于 concurrent read,answer 永远到不了 viewer
	// (表现为 PC 恒 new)。ready 经 channel 交给主流程后再发 offer。
	wsErr := make(chan error, 1)
	readyCh := make(chan struct{}, 1)
	go func() {
		for {
			mt, r, err := ws.Reader(ctx)
			if err != nil {
				wsErr <- err
				return
			}
			if mt != websocket.MessageText {
				continue
			}
			b, err := io.ReadAll(io.LimitReader(r, 1<<20))
			if err != nil {
				wsErr <- err
				return
			}
			var f struct {
				Type      string                   `json:"type"`
				SDP       string                   `json:"sdp"`
				Candidate *webrtc.ICECandidateInit `json:"candidate"`
				Code      string                   `json:"code"`
			}
			if json.Unmarshal(b, &f) != nil {
				continue
			}
			switch f.Type {
			case "ready":
				select {
				case readyCh <- struct{}{}:
				default:
				}
			case "answer":
				if err := v.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: f.SDP}); err != nil {
					wsErr <- fmt.Errorf("set answer: %w", err)
					return
				}
			case "ice":
				if f.Candidate != nil {
					_ = v.pc.AddICECandidate(*f.Candidate)
				}
			case "state":
				log.Info("agent state", "code", f.Code)
			case "error":
				wsErr <- fmt.Errorf("agent error frame: %s", f.Code)
				return
			}
		}
	}()

	// ready → offer;trickle 反向(回调先于 SetLocal 注册,理由见 runDirect)。
	select {
	case <-readyCh:
	case err := <-wsErr:
		return v.collect("server", c.out, time.Since(v.start)), err
	case <-ctx.Done():
		return v.collect("server", c.out, time.Since(v.start)), ctx.Err()
	}
	v.pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand == nil {
			return
		}
		ci := cand.ToJSON()
		b, _ := json.Marshal(map[string]any{"type": "ice", "candidate": ci})
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		_ = ws.Write(wctx, websocket.MessageText, b)
		wcancel()
	})
	offer, err := v.pc.CreateOffer(nil)
	if err != nil {
		return nil, err
	}
	if err := v.pc.SetLocalDescription(offer); err != nil {
		return nil, err
	}
	wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
	jb, _ := json.Marshal(map[string]any{"type": "offer", "sdp": offer.SDP})
	if err := ws.Write(wctx, websocket.MessageText, jb); err != nil {
		wcancel()
		return nil, fmt.Errorf("send offer: %w", err)
	}
	wcancel()

	if err := v.waitConnected(25 * time.Second); err != nil {
		return v.collect("server", c.out, time.Since(v.start)), err
	}
	v.runFor(ctx, c)
	_ = v.pc.Close()
	if v.out != nil {
		_ = v.out.Close()
	}
	return v.collect("server", c.out, time.Since(v.start)), nil
}

func main() {
	c := parseFlags()
	var (
		s   *summary
		err error
	)
	switch {
	case c.directPipe != "":
		s, err = runDirect(c)
	case c.server != "":
		s, err = runServer(c)
	default:
		fmt.Fprintln(os.Stderr, "need --direct-pipe/--direct-secret (direct) or --server/--node/--token (server)")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2eviewer: %v\n", err)
		if s != nil {
			s.evaluate(c)
			s.report(c)
		}
		os.Exit(1)
	}
	s.evaluate(c)
	s.report(c)
	if !s.AssertionsPassed {
		os.Exit(1)
	}
}
