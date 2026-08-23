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
	"sync"
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
	pliRetryAfter       time.Duration // 0=off;待验证 PLI 超时重发(frames>0 亦触发,上限 3 发/轮)
	inputScript         string        // server 模式:连接+首关键帧后按 JSON 步骤注入输入
	inputBeforeKeyframe bool          // 探针:跳过首关键帧门(锁屏静止场景注入)
	sas                 bool          // server 模式:连接后发一次 secure_attention,结果+计时进 summary
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
	flag.DurationVar(&c.pliRetryAfter, "pli-retry-after", 1500*time.Millisecond,
		"re-send a pending (unanswered) PLI after this long — even while frames flow (bounded: 3 sends per episode; 0=off)")
	flag.StringVar(&c.inputScript, "input-script", "",
		"server mode: JSON step file (lease/move/button/wheel/key/text/wait) run after connect + first keyframe; results land in the summary JSON (schema: inputscript.go)")
	flag.BoolVar(&c.inputBeforeKeyframe, "input-before-keyframe", false,
		"probe only: run the input script without waiting for the first decoded keyframe (e.g. static locked-screen scenarios where no IDR ever emerges)")
	flag.BoolVar(&c.sas, "sas", false,
		"server mode: after connect, send one {secure_attention} control frame (core 0x0110) and record the result + timing in the summary JSON (sas field)")
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

	pliMu       atomic.Int64 // 最近一次 PLI 发出(UnixNano;0=无待验证)
	pliFirstAt  atomic.Int64 // 本轮首 PLI(UnixNano;0=无)——PLI→IDR 从首发起算
	pliAttempts atomic.Int32 // 本轮(episode)PLI 已发送数(含首发)
	pliRetries  atomic.Uint64
	pliIDRMax   atomic.Int64 // PLI→IDR 最大时延(ms;0=未发生)

	// cursor 通道记录(server 模式;计数全量,样本截 cap —— T6 门④
	// 时延核对用,TMs = 相对 viewer start)。
	cursorMu      sync.Mutex
	cursorEvents  uint64
	cursorSamples []cursorSample

	// display_changed 记录(M2-Slice1 Task 2;server 模式信令帧 + direct
	// 模式 0x010A 通道,两路共用)。计数全量,样本截 cap。
	displayMu      sync.Mutex
	displayEvents  uint64
	displaySamples []displaySample

	// --sas(--input-script sas op 同路)结果记录(M2-Slice1 Task 5):
	// 请求发出时刻 + 回执(ok/hr/code)+ rtt;summary.sas。
	sasMu     sync.Mutex
	sasResult *sasOutcome
}

// sasReply 是一条 secure_attention_result 帧的解析形态。
type sasReply struct {
	ok   bool
	hr   uint32
	code string
}

// sasOutcome 是 --sas 的 summary 记录:请求 + 结果 + 计时(相对 viewer
// start;StartUnixMs 是绝对零点)。RttMs=0 且 Sent=true = 无回执(超时)。
type sasOutcome struct {
	Sent  bool   `json:"sent"`
	OK    bool   `json:"ok"`
	HR    uint32 `json:"hr"`
	Code  string `json:"code,omitempty"`
	AtMs  int64  `json:"atMs,omitempty"`
	RttMs int64  `json:"rttMs,omitempty"`
}

// beginSas 记一次请求发出,返回发出时刻(viewer 相对 ms)。
func (v *viewer) beginSas() int64 {
	at := time.Since(v.start).Milliseconds()
	v.sasMu.Lock()
	v.sasResult = &sasOutcome{Sent: true, AtMs: at}
	v.sasMu.Unlock()
	return at
}

// completeSas 填入回执(ok=false + code=timeout 为无回执)。
func (v *viewer) completeSas(r sasReply, sendAtMs int64, timedOut bool) {
	v.sasMu.Lock()
	defer v.sasMu.Unlock()
	o := &sasOutcome{Sent: true, AtMs: sendAtMs, OK: r.ok, HR: r.hr, Code: r.code}
	if !timedOut {
		o.RttMs = time.Since(v.start).Milliseconds() - sendAtMs
	}
	v.sasResult = o
}

// sasSnapshot 返回 summary 快照(未发生 = nil)。
func (v *viewer) sasSnapshot() *sasOutcome {
	v.sasMu.Lock()
	defer v.sasMu.Unlock()
	if v.sasResult == nil {
		return nil
	}
	o := *v.sasResult
	return &o
}

// cursorSample 是一条 cursor 通道事件(summary.cursorSamples 元素)。
type cursorSample struct {
	TMs     int64 `json:"tMs"`
	X       int32 `json:"x"`
	Y       int32 `json:"y"`
	Visible uint8 `json:"visible"`
}

// displaySample 是一条 display_changed 事件(summary.displaySamples 元素;
// M2-Slice1 Task 2 验收:gen 递增、新 w/h、reason 稳定串)。
type displaySample struct {
	TMs    int64  `json:"tMs"`
	Gen    uint32 `json:"gen"`
	W      uint32 `json:"w"`
	H      uint32 `json:"h"`
	Reason string `json:"reason"`
}

// cursorSampleCap 限制 summary 体积;超出后样本丢弃(计数仍全量)。
const cursorSampleCap = 4096

// displaySampleCap 同上(display_changed 事件极稀疏,防御性上限)。
const displaySampleCap = 1024

// recordDisplay 记一条 display_changed 事件(server 信令帧 / direct 0x010A)。
func (v *viewer) recordDisplay(gen, w, h uint32, reason string) {
	v.displayMu.Lock()
	defer v.displayMu.Unlock()
	v.displayEvents++
	if len(v.displaySamples) < displaySampleCap {
		v.displaySamples = append(v.displaySamples, displaySample{
			TMs: time.Since(v.start).Milliseconds(), Gen: gen, W: w, H: h, Reason: reason})
	}
}

// displayStats 返回(累计事件数,样本副本)。
func (v *viewer) displayStats() (uint64, []displaySample) {
	v.displayMu.Lock()
	defer v.displayMu.Unlock()
	out := make([]displaySample, len(v.displaySamples))
	copy(out, v.displaySamples)
	return v.displayEvents, out
}

// recordCursor 记一条 cursor 通道事件(server 模式 OnDataChannel 调用)。
func (v *viewer) recordCursor(x, y int32, visible bool) {
	v.cursorMu.Lock()
	defer v.cursorMu.Unlock()
	v.cursorEvents++
	if len(v.cursorSamples) < cursorSampleCap {
		s := cursorSample{TMs: time.Since(v.start).Milliseconds(), X: x, Y: y}
		if visible {
			s.Visible = 1
		}
		v.cursorSamples = append(v.cursorSamples, s)
	}
}

// cursorStats 返回(累计事件数,样本副本)。
func (v *viewer) cursorStats() (uint64, []cursorSample) {
	v.cursorMu.Lock()
	defer v.cursorMu.Unlock()
	out := make([]cursorSample, len(v.cursorSamples))
	copy(out, v.cursorSamples)
	return v.cursorEvents, out
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
					if t := v.pliMu.Swap(0); t != 0 {
						// PLI→IDR 从本轮首 PLI 起算(重发不重置表尺:
						// 慢就是慢,门④不能被 retry 稀释)。
						if f := v.pliFirstAt.Swap(0); f != 0 {
							ms := time.Since(time.Unix(0, f)).Milliseconds()
							for {
								old := v.pliIDRMax.Load()
								if ms <= old || v.pliIDRMax.CompareAndSwap(old, ms) {
									break
								}
							}
						}
						v.pliAttempts.Store(0) // episode 关闭
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
// 发送成功即开/续一个 PLI episode(attempts 计数;见 pliRetryDecision)。
func (v *viewer) sendPLI(d time.Duration) bool {
	select {
	case tr := <-v.trackCh:
		v.trackCh <- tr // 放回供下次使用
		if err := v.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(tr.SSRC())}}); err != nil {
			v.log.Warn("send PLI failed", "err", err)
			return false
		}
		v.plisSent.Add(1)
		now := time.Now().UnixNano()
		v.pliMu.Store(now)
		if v.pliAttempts.Add(1) == 1 {
			v.pliFirstAt.Store(now)
		}
		return true
	case <-time.After(d):
		return false
	}
}

// maxPliEpisodeSends:单个待验证 PLI 轮的上限发送数(首发 + 2 次重发;
// M1-Slice3 keyframe-retry 承接:pending PLI 1.5s 无 IDR 即重发,
// frames>0 亦触发)。--pli-interval 心跳是操作者显式行为,不受此限。
const maxPliEpisodeSends = 3

// pliRetryDecision:待验证 PLI(最近一次发出于 since 前,本轮已发
// attempts 次)是否应重发。纯函数(单测锁定)。
func pliRetryDecision(since time.Duration, attempts, maxSends int, retryAfter time.Duration) bool {
	return attempts > 0 && attempts < maxSends && since >= retryAfter
}

// summary 是断言输入与 --json 输出形态(T6 脚本契约)。
type summary struct {
	Mode             string          `json:"mode"`
	StartUnixMs      int64           `json:"startUnixMs"` // viewer 起点绝对时刻(cursorSamples.tMs 的零点)
	Connected        bool            `json:"connected"`
	FirstFrameMs     int64           `json:"firstFrameMs"`
	FirstKey         bool            `json:"firstKey"`
	RtpPackets       uint64          `json:"rtpPackets"`
	Frames           uint64          `json:"frames"`
	Keyframes        uint64          `json:"keyframes"`
	Bytes            uint64          `json:"bytes"`
	DumpFile         string          `json:"dumpFile,omitempty"`
	Relay            bool            `json:"relay"`
	PlisSent         uint64          `json:"plisSent"`
	PliRetries       uint64          `json:"pliRetries"`
	KeyframeReqs     uint64          `json:"keyframeReqs"`
	PliToIdrMaxMs    int64           `json:"pliToIdrMaxMs"`
	CursorEvents     uint64          `json:"cursorEvents"`
	CursorSamples    []cursorSample  `json:"cursorSamples,omitempty"`
	DisplayEvents    uint64          `json:"displayEvents"`
	DisplaySamples   []displaySample `json:"displaySamples,omitempty"`
	Sas              *sasOutcome     `json:"sas,omitempty"`
	Input            *scriptResult   `json:"input,omitempty"`
	DurationMs       int64           `json:"durationMs"`
	AssertionsPassed bool            `json:"assertionsPassed"`
	Failures         []string        `json:"failures,omitempty"`
}

func (v *viewer) collect(mode, dump string, ran time.Duration) *summary {
	cur, samples := v.cursorStats()
	dEvents, dSamples := v.displayStats()
	s := &summary{
		Mode: mode, Relay: v.relay, DumpFile: dump,
		StartUnixMs: v.start.UnixMilli(),
		RtpPackets:  v.rtpPkts.Load(), Frames: v.frames.Load(),
		Keyframes: v.keyframes.Load(), Bytes: v.bytes.Load(),
		PlisSent: v.plisSent.Load(), PliRetries: v.pliRetries.Load(),
		KeyframeReqs: v.keyframeReqs.Load(), PliToIdrMaxMs: v.pliIDRMax.Load(),
		CursorEvents: cur, CursorSamples: samples,
		DisplayEvents: dEvents, DisplaySamples: dSamples,
		Sas:        v.sasSnapshot(),
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

// runFor 跑满 d:PLI 心跳(可选)+ 单发 PLI(--pli-at)+ pending-PLI
// 超时重发(--pli-retry-after)+ 中途进度一行。d 与 c.duration 分离:
// --input-script 场景脚本结束后仍有观察尾段。
func (v *viewer) runFor(ctx context.Context, c *config, d time.Duration) {
	deadline := time.Now().Add(d)
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
			// 丢包自救 ①:首帧迟迟未落地(首 IDR 被链路打散)→ 经信令重请
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
			// 丢包自救 ②(M1-Slice3 承接):PLI 已发但 IDR 未回(链路把
			// PLI 或 IDR 打散,frames>0 亦可能)→ 1.5s 重发,轮上限 3 发。
			if c.pliRetryAfter > 0 {
				if t := v.pliMu.Load(); t != 0 {
					since := time.Since(time.Unix(0, t))
					if pliRetryDecision(since, int(v.pliAttempts.Load()), maxPliEpisodeSends, c.pliRetryAfter) {
						if v.sendPLI(2 * time.Second) {
							v.pliRetries.Add(1)
							v.log.Info("pending PLI unanswered, re-sent", "sinceMs", since.Milliseconds())
						}
					}
				}
			}
			v.log.Info("progress", "frames", v.frames.Load(), "keyframes", v.keyframes.Load())
		}
	}
}

// ---- direct 模式 ----

func runDirect(c *config) (*summary, error) {
	log := slog.Default()
	if c.inputScript != "" {
		return nil, errors.New("--input-script requires server mode (direct-pipe publisher has no input datachannels)")
	}
	if c.sas {
		return nil, errors.New("--sas requires server mode (secure_attention rides the session control WS; direct mode bypasses the agent)")
	}
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

	// 0x010A 泵(direct 模式;M2-Slice1 Task 2):display 事件进 summary。
	go func() {
		for ev := range sub.DisplayCh() {
			log.Info("display changed (direct)", "gen", ev.Gen, "w", ev.W, "h", ev.H, "reason", ev.Reason)
			v.recordDisplay(ev.Gen, ev.W, ev.H, ev.Reason)
		}
	}()

	if err := v.waitConnected(25 * time.Second); err != nil {
		return v.collect("direct", c.out, time.Since(v.start)), err
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.duration)
	defer cancel()
	v.runFor(ctx, c, c.duration)

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
	// 输入脚本解析期即全量校验(未知 op/字段、键码、上限;错误带下标)。
	var steps []scriptStep
	if c.inputScript != "" {
		f, err := os.Open(c.inputScript)
		if err != nil {
			return nil, fmt.Errorf("--input-script: %w", err)
		}
		steps, err = parseInputScript(f)
		_ = f.Close()
		if err != nil {
			return nil, err
		}
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
	timeout := c.duration + 30*time.Second
	if steps != nil {
		timeout += scriptWaitBudget(steps) // 脚本预算(含等首关键帧 + wait 全额)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
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

	// m=application 前提(T3 报告结论):pion viewer 的 offer 需先建一条
	// 占位 DC 才含 SCTP 段,agent 预建的 input/mouse/cursor 通道(in-band
	// 协商)才有传输可骑。占位通道本身不用(浏览器 offer 默认含,同效)。
	if _, err := v.pc.CreateDataChannel("xnc-viewer-sctp", nil); err != nil {
		return nil, fmt.Errorf("placeholder datachannel: %w", err)
	}
	chs := &channelSet{}
	leaseCh := make(chan leaseEvent, 8)
	leaseState := &leaseNote{}
	v.pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		switch dc.Label() {
		case dcLabelInput, dcLabelMouse:
			chs.put(dc)
		case dcLabelCursor:
			dc.OnMessage(func(m webrtc.DataChannelMessage) {
				if x, y, vis, ok := decodeCursorWire(m.Data); ok {
					v.recordCursor(x, y, vis)
				}
			})
		}
	})

	// 入站信令泵:单 goroutine 读全部帧——coder/websocket 禁止并发 Reader。
	// T4 写法里 waitReady 与本泵并发读同一连接,server 模式当时未经 live
	// 验证,T6 实测暴露:泵死于 concurrent read,answer 永远到不了 viewer
	// (表现为 PC 恒 new)。ready 经 channel 交给主流程后再发 offer。
	// sasCh(容量 8,满即丢):--sas 与 input-script sas op 的回执交接
	// (两者顺序使用,不并发争用)。
	wsErr := make(chan error, 1)
	readyCh := make(chan struct{}, 1)
	sasCh := make(chan sasReply, 8)
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
				Type       string                   `json:"type"`
				SDP        string                   `json:"sdp"`
				Candidate  *webrtc.ICECandidateInit `json:"candidate"`
				Code       string                   `json:"code"`
				LeaseID    string                   `json:"leaseId"`
				Reason     string                   `json:"reason"`
				Generation uint32                   `json:"generation"`
				W          uint32                   `json:"w"`
				H          uint32                   `json:"h"`
				OK         *bool                    `json:"ok"`
				HR         uint32                   `json:"hr"`
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
			case "display_changed":
				// M2-Slice1 Task 2:0x010A 透传帧 {generation,w,h,reason}。
				log.Info("display changed", "gen", f.Generation, "w", f.W, "h", f.H, "reason", f.Reason)
				v.recordDisplay(f.Generation, f.W, f.H, f.Reason)
			case "secure_attention_result":
				// M2-Slice1 Task 5:SAS 回执 {ok,hr[,code]}——交给等待方
				// (--sas 主流程 / sas 脚本步),无人等则丢(容量 8)。
				rep := sasReply{hr: f.HR, code: f.Code}
				if f.OK != nil {
					rep.ok = *f.OK
				}
				log.Info("secure attention result", "ok", rep.ok,
					"hr", fmt.Sprintf("%#x", rep.hr), "code", rep.code)
				select {
				case sasCh <- rep:
				default:
				}
			case "lease_granted", "lease_denied", "lease_revoked":
				// T3 lease 词汇(inputlive.go stepEnv 消费;revoked 另记
				// 原因进 input 脚本结果)。
				ev := leaseEvent{kind: strings.TrimPrefix(f.Type, "lease_"), leaseID: f.LeaseID, reason: f.Reason}
				if ev.kind == "revoked" {
					leaseState.setRevoked(f.Reason)
				}
				select {
				case leaseCh <- ev:
				default: // 满 = 无人消费(stale 事件),丢
				}
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
		s := v.collect("server", c.out, time.Since(v.start))
		if steps != nil {
			s.Input = &scriptResult{Err: "not connected: " + err.Error()}
		}
		return s, err
	}
	if c.sas {
		// --sas(M2-Slice1 Task 5):连接后发一次 secure_attention(core
		// 0x0110 经 agent 转发),回执 + rtt 进 summary.sas。门控拒绝亦是
		// 有效观测(T6 门⑤:ok=false code=SAS_DENIED),不构成断言失败。
		sendAt := v.beginSas()
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		jb, _ := json.Marshal(map[string]any{"type": "secure_attention"})
		if err := ws.Write(wctx, websocket.MessageText, jb); err != nil {
			wcancel()
			v.completeSas(sasReply{code: "send_failed"}, sendAt, true)
			log.Warn("--sas send failed", "err", err)
		} else {
			wcancel()
			select {
			case rep := <-sasCh:
				v.completeSas(rep, sendAt, false)
			case <-time.After(10 * time.Second):
				v.completeSas(sasReply{code: "timeout"}, sendAt, true)
				log.Warn("--sas: no secure_attention_result within 10s")
			case <-ctx.Done():
				v.completeSas(sasReply{code: "ctx_done"}, sendAt, true)
			}
		}
	}
	// 输入脚本(连接 + 首关键帧后跑;结果进 summary.input,不参与
	// --expect-* 断言 —— 场景期望由 e2e 脚本对 steps 自行判定)。
	var inputRes *scriptResult
	if steps != nil {
		inputRes = runServerScript(ctx, steps, v, chs, leaseCh, leaseState, ws, sasCh, log,
			c.inputBeforeKeyframe)
	}
	// 观察尾段:脚本结束后至少 3s(cursor 事件/PLI-IDR 余波可见),
	// 且不短于 --duration。
	tail := c.duration
	if inputRes != nil && tail < 3*time.Second {
		tail = 3 * time.Second
	}
	v.runFor(ctx, c, tail)
	_ = v.pc.Close()
	if v.out != nil {
		_ = v.out.Close()
	}
	s := v.collect("server", c.out, time.Since(v.start))
	s.Input = inputRes
	return s, nil
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
