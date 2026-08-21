//go:build windows

// capture_windows.go — 捕获主循环与共享 COM syscall 基础设施。
// 捕获源唯一：Windows.Graphics.Capture（capture_wgc_windows.go）——无
// WGC 回退。采集源均不可用时走 placeholderLoop 报告
// 状态帧保活，不产出画面。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// ---- 哨兵错误 ----

// ErrTimeout — timeout 内无新帧（桌面静止：合成器无更新）。
var ErrTimeout = errors.New("capture acquire timeout")

// ---- 主捕获循环 ----

// bitrateFor 将质量参数（1-100）映射到 H.264 平均码率。
func bitrateFor(quality int) int {
	if quality < 1 {
		quality = 1
	}
	if quality > 100 {
		quality = 100
	}
	return 500_000 + quality*30_000 // q60 ≈ 2.3Mbps（1080p 30fps 预算内）
}

// newScreenCapturer 采集源梯子：DXGI（xnc-dda.dll，主路径）→ WGC（回退）。
// XNC_NO_DDA=1 可禁用 DDA（诊断用）。返回 (capturer, backend名, error)。
func newScreenCapturer() (screenCapturer, string, error) {
	if os.Getenv("XNC_NO_DDA") == "" {
		c, err := NewDDACapturer()
		if err == nil {
			return c, "dxgi", nil
		}
		fmt.Fprintf(os.Stderr, "xnc-screen-helper: dda unavailable (%v), trying wgc\n", err)
	}
	c, err := NewWGCCapturer()
	if err != nil {
		return nil, "", err
	}
	return c, "wgc", nil
}

// captureLoop：采集源（DDA 优先，WGC 回退）→ 编码器（H.264 MFT 优先，
// JPEG 帧流回退）→ 发分辨率 + capturing → 捕获-编码-推送循环。
//
//	静止：合成器无更新 → AcquireFrame 超时 → 不编码不出帧（自适应 0fps）
//	会话/设备失败：整体重建 capturer（秒级退避，有界）——分辨率切换 /
//	      设备移除等场景
//	关键帧：首帧 + 每 gop 帧（新观众可立即入流）
func captureLoopWith(ctx context.Context, conn net.Conn, opts captureOpts, cap screenCapturer, firstFrame []byte, ferr error) error {
	frameInterval := time.Second / time.Duration(opts.fps)

	srcW, srcH := cap.Dims()
	outW, outH := fitDims(srcW, srcH, opts.maxWidth)

	// 编码器在首帧之后创建：部分机器上 MFT 编码器初始化（COM 单元/MTA 交
	// 互）会阻断 WGC 帧池交付——首帧由调用方（listenPipe 之前）预取。
	encoder, eerr := newH264Encoder(outW, outH, bitrateFor(opts.quality), opts.fps*2)
	if eerr != nil {
		fmt.Fprintf(os.Stderr, "xnc-screen-helper: h264 mft unavailable (%v), jpeg fallback\n", eerr)
		encoder = newJPEGStreamEncoder(outW, outH, opts.quality)
	}
	defer encoder.Close()

	// fps 节流 + 时间基 GOP：光标-only 合成帧可按输入设备频率（~125Hz）
	// 到达，编码按帧间隔限速；静止自适应（0fps）与突发帧共存时按帧数
	// 计 GOP 会时间扭曲，改为距上一关键帧 ≥2s 强制 IDR。
	gop := &gopState{}
	var lastEncodeAt time.Time

	stats := &pipeStats{}
	if p := os.Getenv("XNC_DUMP_H264"); p != "" {
		if f, err := os.Create(p); err == nil {
			stats.dump = f
			defer func() { _ = f.Close() }()
			fmt.Fprintf(os.Stderr, "xnc-screen-helper: h264 dump -> %s\n", p)
		} else {
			fmt.Fprintf(os.Stderr, "xnc-screen-helper: dump open failed: %v\n", err)
		}
	}
	statTick := time.NewTicker(10 * time.Second)
	defer statTick.Stop()
	go func() {
		for range statTick.C {
			fmt.Fprintf(os.Stderr, "xnc-screen-helper: stats sent=%d key=%d delta=%d encErr=%d bytes=%d\n",
				stats.frames, stats.keys, stats.deltas, stats.encErrs, stats.bytes)
		}
	}()

	pacedSend := func(frame []byte) error {
		if !lastEncodeAt.IsZero() {
			if wait := frameInterval - time.Since(lastEncodeAt); wait > 0 {
				time.Sleep(wait)
			}
		}
		err := sendEncoded(conn, encoder, frame, gop, stats)
		lastEncodeAt = time.Now()
		return err
	}

	if err := writeFrame(conn, pipeFrameDims, packDims(outW, outH)); err != nil {
		return err
	}
	if err := writeFrame(conn, pipeFrameState, []byte("capturing")); err != nil {
		return err
	}

	// TryGetNextFrame 轮询节流（timeout ≤ 100ms）。首帧 2s 宽限
	// （WGC 通常毫秒级送达，宽限无害）。
	acquireTimeout := frameInterval.Milliseconds()
	if acquireTimeout <= 0 || acquireTimeout > 100 {
		acquireTimeout = 100
	}

	var lastFrame []byte
	if ferr == nil {
		w, h := cap.Dims()
		if len(firstFrame) >= w*h*4 {
			if outW != w {
				firstFrame = scaleBGRA(firstFrame, w, h, outW, outH)
			}
			lastFrame = firstFrame
			if err := pacedSend(firstFrame); err != nil {
				return err
			}
		}
	}
	// 静止桌面自愈：MFT 有 ~gop 帧启动缓冲，若期间桌面转静止，首帧可能
	// 被编码器内部吞掉而始终无输出。静止超时 ~1s 后强制重编码缓存帧。
	// 首关键帧出帧前按节拍持续驱动编码器；出帧后静止即完全静默。
	idleTicks, idleLimit := 0, 1
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cap == nil {
			// 上轮重建失败后的重试节拍。
			time.Sleep(time.Second)
			nc, _, nerr := newScreenCapturer()
			if nerr != nil {
				continue
			}
			cap = nc
			continue
		}

		t := uint(acquireTimeout)
		frame, aerr := cap.AcquireFrame(t)
		if aerr != nil {
			if errors.Is(aerr, ErrTimeout) {
				// 后续静止帧：MFT 启动缓冲自愈逻辑
				idleTicks++
				if !gop.sentKey && lastFrame != nil && idleTicks >= idleLimit {
					if err := pacedSend(lastFrame); err != nil {
						return err
					}
				}
				continue
			}
			// 会话/设备级失败：整体重建（分辨率切换、设备移除等）。
			fmt.Fprintf(os.Stderr, "xnc-screen-helper: capture frame: %v, rebuilding\n", aerr)
			cap.Close()
			cap = nil
			continue
		}
		idleTicks = 0

		w, h := cap.Dims()
		if len(frame) < w*h*4 {
			continue
		}
		if outW != w {
			frame = scaleBGRA(frame, w, h, outW, outH)
		}
		lastFrame = frame

		if err := pacedSend(frame); err != nil {
			return err
		}
	}
}

// gopState 时间基关键帧记账：变帧率（静止自适应 0fps + 光标突发）下按
// 帧数计 GOP 会时间扭曲，改为距上一关键帧 ≥2s 强制 IDR（新观众可立即
// 入流的语义不变——agent 缓存最新 I 帧）。
type gopState struct {
	sentKey   bool
	lastKeyAt time.Time
}

func (g *gopState) keyDue() bool {
	return !g.sentKey || time.Since(g.lastKeyAt) >= 2*time.Second
}

// pipeStats 单管线诊断状态（随 captureLoopWith 生命周期，替代包级全局）：
// XNC_DUMP_H264 落盘目标 + 10s 周期计数器（stderr → agent 日志）。
type pipeStats struct {
	frames, keys, deltas, encErrs int
	bytes                         int64
	dump                          io.Writer
}

// sendEncoded 编码一帧并按关键帧/增量帧类型写 pipe。编码器无输出（MFT
// 启动缓冲）时静默跳过。码流契约（唯一整形规则，其余 NALU 直通）：IDR
// 前必有缓存的 SPS/PPS，统一 4 字节起始码——消费端（WebCodecs Annex-B
// 模式）按此契约配置解码器。
func sendEncoded(conn net.Conn, encoder frameEncoder, frame []byte, gop *gopState, st *pipeStats) error {
	forceKey := gop.keyDue() // 首输出前始终请求关键帧
	data, encErr := encoder.Encode(frame, forceKey, false)
	if encErr != nil {
		fmt.Fprintf(os.Stderr, "xnc-screen-helper: encode: %v\n", encErr)
		return nil
	}
	if len(data) == 0 {
		return nil // 编码器内部缓冲（MFT 启动延迟）
	}

	var frameType byte
	if encoder.LastFrameKey() {
		frameType = pipeFrameKey
		if spspps := encoder.SPSPPS(); len(spspps) > 0 {
			// 关键帧 = 缓存参数集 + VCL NALU。丢弃编码器输出自带的
			// SPS/PPS/AUD（type 7/8/9）——重复参数集与 AUD 前缀会触发
			// 部分浏览器 WebCodecs 解码器 "key frame required" 拒帧。
			data = append(append([]byte{}, spspps...), vclNALUs(data)...)
		}
		gop.sentKey = true
		gop.lastKeyAt = time.Now()
	} else {
		frameType = pipeFrameDelta
		if len(encoder.SPSPPS()) > 0 { // H.264 模式统一 4 字节起始码
			data = vclNALUs(data)
		}
	}
	if st.dump != nil {
		_, _ = st.dump.Write(data) // 整形后码流（= 管道实发字节）
	}
	st.frames++
	st.bytes += int64(len(data))
	if frameType == pipeFrameKey {
		st.keys++
	} else {
		st.deltas++
	}
	return writeFrame(conn, frameType, data)
}

// vclNALUs 重排 Annex-B 码流：丢弃参数集与 AUD（type 7/8/9），其余 NALU
// 统一以 4 字节起始码输出。JPEG 回退模式（无参数集）不应调用。
func vclNALUs(data []byte) []byte {
	var out []byte
	i := 0
	for i < len(data) {
		hdr := -1
		for j := i; j+3 < len(data); j++ {
			if data[j] == 0 && data[j+1] == 0 {
				if data[j+2] == 1 {
					hdr = j + 3
					break
				}
				if data[j+2] == 0 && j+4 < len(data) && data[j+3] == 1 {
					hdr = j + 4
					break
				}
			}
		}
		if hdr < 0 || hdr >= len(data) {
			break
		}
		end := len(data)
		for j := hdr + 1; j+3 <= len(data); j++ {
			if data[j] == 0 && data[j+1] == 0 && (data[j+2] == 1 || (j+4 <= len(data) && data[j+2] == 0 && data[j+3] == 1)) {
				end = j
				break
			}
		}
		for end > hdr && data[end-1] == 0 { // 去起始码前导零
			end--
		}
		if t := data[hdr] & 0x1F; t != 7 && t != 8 && t != 9 && end > hdr {
			out = append(out, 0, 0, 0, 1)
			out = append(out, data[hdr:end]...)
		}
		i = end
	}
	return out
}

// placeholderLoop 采集源不可用时的占位循环（每秒状态帧，保持 pipe 活性）。
func placeholderLoop(ctx context.Context, conn net.Conn, cause error) error {
	_, _ = fmt.Fprintf(os.Stderr, "xnc-screen-helper: wgc unavailable: %v\n", cause)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := writeFrame(conn, pipeFrameState, []byte("capturing")); err != nil {
				return err
			}
		}
	}
}
