// rtvload — XNC RTV 合成 viewer（自 mvp loadclient 移植）：传输质量测量
// + H.264 流落盘校验。连接期鉴权：URL 携带会话 client token（?token=，
// 与浏览器 viewer 同源语义；POST /api/nodes/{id}/desktop 的响应取得）。
//
// 测量口径（协议规范 §4）：
//   - e2e = 本机时钟 - captureUnixUs（跨时钟，含偏差；仅供趋势参考）
//   - 到帧间隔 p50/p95（首包时间差）
//   - 帧完整性：缺 shard 数、FEC 可恢复数（parity ≥ 缺失数）、不可恢复数
//   - 控制环 RTT（heartbeat 回显）
// 落盘：仅完整帧按序拼接 Annex-B（ffmpeg 校验用；不完整帧跳过并在报告中计数）。
package main

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

const hdrLen = 34

type mediaHdr struct {
	flags        uint8
	blockIdx     uint8
	shardIndex   uint16
	dataShards   uint16
	parityShards uint16
	blockTotal   uint32
	frameIndex   uint32
	codecID      uint8
	keyframe     bool
	captureUs    uint64
	payloadLen   uint16
}

func parseHdr(b []byte) (mediaHdr, bool) {
	if len(b) < hdrLen || string(b[0:4]) != "MVP1" {
		return mediaHdr{}, false
	}
	return mediaHdr{
		flags:        b[4],
		blockIdx:     b[5],
		shardIndex:   binary.LittleEndian.Uint16(b[6:8]),
		dataShards:   binary.LittleEndian.Uint16(b[8:10]),
		parityShards: binary.LittleEndian.Uint16(b[10:12]),
		blockTotal:   binary.LittleEndian.Uint32(b[12:16]),
		frameIndex:   binary.LittleEndian.Uint32(b[16:20]),
		codecID:      b[20],
		keyframe:     b[21] == 1,
		captureUs:    binary.LittleEndian.Uint64(b[24:32]),
		payloadLen:   binary.LittleEndian.Uint16(b[32:34]),
	}, true
}

const (
	flagSOF = 1
	flagEOF = 2
	flagPIC = 4
)

type shard struct {
	block, idx int
	data       []byte
}

type frameAgg struct {
	frameIndex uint32
	shards     map[int]shard // key = blockIdx*65536 + shardIndex
	blockTotal int
	kMap       map[int]int // blockIdx -> dataShards（包头推断）
	keyframe   bool
	firstSeen  time.Time
	captureUs  uint64
	finalized  bool
}

func main() {
	var (
		url      = flag.String("url", "https://127.0.0.1/wt", "WebTransport URL（须含 ?token=<会话client token>）")
		dur      = flag.Duration("dur", 30*time.Second, "采集时长")
		dump     = flag.String("dump", "", "完整帧 Annex-B 落盘路径（空=不落盘）")
		insecure = flag.Bool("insecure", false, "跳过服务端证书校验（dev 自签时用）")
		// -esc-after：连上 N 秒后向 host 注入一次 Escape down+up（单发）。
		// 验收场景：CAD 菜单（SAS 触发）经 Esc 注销——覆盖安全桌面输入路径。
		escAfter = flag.Duration("esc-after", 0, "连上 N 秒后注入一次 Escape（0=不注入）")
	)
	flag.Parse()
	if !strings.Contains(*url, "token=") {
		log.Fatalf("url 必须携带 ?token=<会话 client token>（POST /api/nodes/{id}/desktop 取得）")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *dur+5*time.Second)
	defer cancel()
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT)
		<-c
		cancel()
	}()

	d := &webtransport.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: *insecure},
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	defer d.Close()
	t0 := time.Now()
	resp, wt, err := d.Dial(ctx, *url, nil)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	log.Printf("connected in %dms (status %d)", time.Since(t0).Milliseconds(), resp.StatusCode)

	stream, err := wt.OpenStreamSync(ctx)
	if err != nil {
		log.Fatalf("open stream: %v", err)
	}
	helloMsg := map[string]any{"type": "hello", "role": "viewer", "transport": "wt", "codecs": []string{"h264"}, "maxBitrateKbps": 50000, "fps": 60}
	sendCtrl(stream, helloMsg)
	gotConfig := false

	// ---------------- 状态 ----------------
	var mu sync.Mutex
	frames := map[uint32]*frameAgg{}
	var maxFrame uint32
	type counters struct {
		pkts, bytes       uint64
		complete, missing uint64 // missing = 有缺 shard 但 FEC 可恢复
		unrecoverable     uint64
		parityArrived     uint64
		ctrlIn            uint64
		lastRTTms         float64
		e2eSamples        []float64
		gapSamples        []float64
		droppedBeforeKF   uint64
		gotKeyframe       bool
		lastFirstArrival  time.Time
	}
	ctr := &counters{}
	var dumpFile *os.File
	var dumpedFrames uint64
	if *dump != "" {
		dumpFile, err = os.Create(*dump)
		if err != nil {
			log.Fatalf("dump create: %v", err)
		}
		defer dumpFile.Close()
	}

	// finalize 判定一帧的完整性
	finalCount := 0
	finalize := func(f *frameAgg) {
		finalCount++
		if finalCount <= 3 || finalCount%100 == 0 {
			log.Printf("DEBUG finalize frame=%d shards=%d blockTotal=%d k0=%d keyframe=%v", f.frameIndex, len(f.shards), f.blockTotal, f.kOf(0), f.keyframe)
		}
		missingData := 0
		parity := 0
		for key := range f.shards {
			idx := key & 0xffff
			blk := key >> 16
			if int(idx) >= f.kOf(blk) {
				parity++
			}
		}
		// 逐 block 统计缺失数据 shard（整块全丢时 k 未知，无法计入——测量略偏乐观）
		for blk := 0; blk < f.blockTotal; blk++ {
			k := f.kOf(blk)
			for i := 0; i < k; i++ {
				if _, ok := f.shards[blk<<16|i]; !ok {
					missingData++
				}
			}
		}
		switch {
		case missingData == 0:
			ctr.complete++
			if dumpFile != nil && (f.keyframe || ctr.gotKeyframe) {
				// 按序拼接数据 shard payload
				for blk := 0; blk < f.blockTotal; blk++ {
					for i := 0; i < f.kOf(blk); i++ {
						if s, ok := f.shards[blk<<16|i]; ok {
							dumpFile.Write(s.data)
						}
					}
				}
				dumpedFrames++
			}
			if f.keyframe {
				ctr.gotKeyframe = true
			}
		case missingData > 0 && parity >= missingData:
			ctr.missing++ // FEC 可恢复（Go 端不做恢复，仅计数）
		default:
			ctr.unrecoverable++
			sendCtrl(stream, map[string]any{"type": "frameLoss", "frameIndex": f.frameIndex, "reason": "unrecoverable"})
		}
	}

	// kOf：从收到的头推断每 block 数据 shard 数（记录在同 block 任意包头里）
	// ---------------- 控制接收 ----------------
	go func() {
		var lenBuf [4]byte
		for {
			if _, err := io.ReadFull(stream, lenBuf[:]); err != nil {
				return
			}
			n := binary.LittleEndian.Uint32(lenBuf[:])
			body := make([]byte, n)
			if _, err := io.ReadFull(stream, body); err != nil {
				return
			}
			mu.Lock()
			ctr.ctrlIn++
			mu.Unlock()
			var m struct {
				Type      string  `json:"type"`
				Encoder   string  `json:"encoder"`
				Width     int     `json:"width"`
				Height    int     `json:"height"`
				Fps       int     `json:"fps"`
				Fec       int     `json:"fecPercentage"`
				HostNowMs float64 `json:"hostNowMs"`
				TMs       float64 `json:"tMs"`
			}
			if json.Unmarshal(body, &m) == nil {
				switch m.Type {
				case "config":
					log.Printf("config: %dx%d@%d enc=%s fec=%d%%", m.Width, m.Height, m.Fps, m.Encoder, m.Fec)
					gotConfig = true
				case "heartbeat":
					if m.TMs > 0 {
						rtt := float64(time.Now().UnixMilli()) - m.TMs
						mu.Lock()
						ctr.lastRTTms = rtt
						mu.Unlock()
					}
				}
			}
		}
	}()

	// ---------------- heartbeat + feedback ----------------
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		var escSent bool
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if !gotConfig {
					sendCtrl(stream, helloMsg) // host offline/not yet up: retry
				}
				sendCtrl(stream, map[string]any{"type": "heartbeat", "tMs": time.Now().UnixMilli()})
				mu.Lock()
				rtt := ctr.lastRTTms
				var gapP95 float64
				if len(ctr.gapSamples) > 0 {
					gapP95 = p95(ctr.gapSamples)
				}
				mu.Unlock()
				sendCtrl(stream, map[string]any{
					"type": "feedback", "rttMs": rtt, "decodeQueueDepth": 0,
					"decodedFps": 0, "arrivalGapP95Ms": gapP95,
				})
				// 一次性 Escape 注入（安全桌面输入验收）
				if *escAfter > 0 && !escSent && time.Since(t0) >= *escAfter {
					escSent = true
					for _, kind := range []string{"down", "up"} {
						sendCtrl(stream, map[string]any{
							"type": "input", "event": "keyboard", "kind": kind, "code": "Escape",
						})
					}
					log.Printf("injected Escape (after %s)", *escAfter)
				}
			}
		}
	}()

	// ---------------- datagram 接收 ----------------
	dgramDone := make(chan struct{})
	go func() {
		defer close(dgramDone)
		for {
			b, err := wt.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			h, ok := parseHdr(b)
			if !ok {
				continue
			}
			now := time.Now()
			mu.Lock()
			ctr.pkts++
			ctr.bytes += uint64(len(b))
			// e2e 样本（首包）
			if e2e := float64(now.UnixMicro()) - float64(h.captureUs); e2e > 0 && e2e < 60_000 {
				ctr.e2eSamples = append(ctr.e2eSamples, e2e/1000.0)
				if len(ctr.e2eSamples) > 4096 {
					ctr.e2eSamples = ctr.e2eSamples[2048:]
				}
			}
			// 帧聚合
			f, okf := frames[h.frameIndex]
			if !okf {
				f = &frameAgg{
					frameIndex: h.frameIndex,
					shards:     map[int]shard{},
					firstSeen:  now,
					keyframe:   h.keyframe,
					captureUs:  h.captureUs,
				}
				frames[h.frameIndex] = f
				if !ctr.lastFirstArrival.IsZero() {
					ctr.gapSamples = append(ctr.gapSamples, float64(now.Sub(ctr.lastFirstArrival).Microseconds())/1000.0)
					if len(ctr.gapSamples) > 4096 {
						ctr.gapSamples = ctr.gapSamples[2048:]
					}
				}
				ctr.lastFirstArrival = now
			}
			f.blockTotal = max(f.blockTotal, int(h.blockTotal))
			f.blockK(int(h.blockIdx), int(h.dataShards))
			// 保存数据 shard（校验包也存 parity 数计数用）
			payload := make([]byte, h.payloadLen)
			copy(payload, b[hdrLen:hdrLen+h.payloadLen])
			f.shards[int(h.blockIdx)<<16|int(h.shardIndex)] = shard{int(h.blockIdx), int(h.shardIndex), payload}
			// 帧推进：finalize 所有更早帧
			if h.frameIndex > maxFrame {
				for idx, ff := range frames {
					if idx < h.frameIndex && !ff.finalized {
						ff.finalized = true
						finalize(ff)
					}
				}
				maxFrame = h.frameIndex
			}
			mu.Unlock()
		}
	}()

	// 秒级进度
	progress := time.NewTicker(time.Second)
	defer progress.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-dgramDone:
			break loop
		case <-progress.C:
			mu.Lock()
			pkts, bytes := ctr.pkts, ctr.bytes
			comp, miss, unrec := ctr.complete, ctr.missing, ctr.unrecoverable
			mu.Unlock()
			log.Printf("pkt=%d bytes=%d (%.2f Mbps) frames complete=%d fecOk=%d unrecov=%d",
				pkts, bytes, float64(bytes*8)/1e6, comp, miss, unrec)
		}
	}

	// ---------------- 汇总 ----------------
	mu.Lock()
	defer mu.Unlock()
	elapsed := time.Since(t0).Seconds()
	fmt.Println("\n================ 传输质量汇总 ================")
	fmt.Printf("时长: %.1fs   包: %d   吞吐: %.2f Mbps   控制环RTT: %.1f ms\n",
		elapsed, ctr.pkts, float64(ctr.bytes*8)/1e6/elapsed, ctr.lastRTTms)
	fmt.Printf("帧: 完整=%d  FEC可恢复=%d  不可恢复=%d  (丢失率 %.2f%%)\n",
		ctr.complete, ctr.missing, ctr.unrecoverable,
		100.0*float64(ctr.missing+ctr.unrecoverable)/max(1, float64(ctr.complete+ctr.missing+ctr.unrecoverable)))
	if len(ctr.e2eSamples) > 0 {
		fmt.Printf("e2e 时延(含时钟偏差): p50=%.1fms p95=%.1fms min=%.1fms\n", p50(ctr.e2eSamples), p95(ctr.e2eSamples), minOf(ctr.e2eSamples))
	}
	if len(ctr.gapSamples) > 0 {
		fmt.Printf("到帧间隔: p50=%.1fms p95=%.1fms\n", p50(ctr.gapSamples), p95(ctr.gapSamples))
	}
	if dumpFile != nil {
		fmt.Printf("落盘: %s（%d 个完整帧）\n", *dump, dumpedFrames)
	}
	summary, _ := json.MarshalIndent(map[string]any{
		"elapsedSec": elapsed, "pkts": ctr.pkts, "mbps": float64(ctr.bytes*8) / 1e6 / elapsed,
		"framesComplete": ctr.complete, "framesFecRecoverable": ctr.missing, "framesUnrecoverable": ctr.unrecoverable,
		"ctrlRttMs": ctr.lastRTTms, "e2eP50ms": p50(ctr.e2eSamples), "e2eP95ms": p95(ctr.e2eSamples),
		"gapP50ms": p50(ctr.gapSamples), "gapP95ms": p95(ctr.gapSamples),
	}, "", "  ")
	fmt.Println(string(summary))
}

// ---------------- helpers ----------------

// frameAgg 辅助方法（Go 不允许方法上加锁语义，这里配合外部 mu 使用）
func (f *frameAgg) kOf(blk int) int {
	if k, ok := f.kMap[blk]; ok {
		return k
	}
	return 0
}
func (f *frameAgg) blockK(blk, k int) {
	if f.kMap == nil {
		f.kMap = map[int]int{}
	}
	if k > f.kMap[blk] {
		f.kMap[blk] = k
	}
}

var sendMu sync.Mutex

func sendCtrl(w io.Writer, v any) {
	b, _ := json.Marshal(v)
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(b)))
	sendMu.Lock()
	defer sendMu.Unlock()
	if _, err := w.Write(lenBuf[:]); err != nil {
		log.Printf("DEBUG sendCtrl hdr err: %v", err)
	}
	if _, err := w.Write(b); err != nil {
		log.Printf("DEBUG sendCtrl body err: %v", err)
	}
}

func sortedCopy(s []float64) []float64 {
	c := append([]float64(nil), s...)
	sort.Float64s(c)
	return c
}
func p50(s []float64) float64 { return pct(s, 50) }
func p95(s []float64) float64 { return pct(s, 95) }
func pct(s []float64, p float64) float64 {
	if len(s) == 0 {
		return 0
	}
	c := sortedCopy(s)
	i := int(float64(len(c)-1) * p / 100)
	return c[i]
}
func minOf(s []float64) float64 {
	if len(s) == 0 {
		return 0
	}
	return sortedCopy(s)[0]
}
