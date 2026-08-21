// xnc-screen-helper 是独立的捕获进程：在用户控制台会话中运行（由 agent 经
// CreateProcessAsUser 拉起，避免 Session 0 无法访问桌面），通过 DXGI Desktop
// Duplication 捕获屏幕帧，经 named pipe 以帧协议推送给 agent。
//
// 用法：
//
//	xnc-screen-helper.exe --pipe <name> --max-width 1920 --quality 60 [--fps 30]
//	xnc-screen-helper.exe --jpeg-single <path> [--quality 60]
//
// --jpeg-single：单帧截屏 → JPEG → 写文件 → 退出（CLI --snapshot 路径，
// 不走 pipe 与 H.264）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
)

const (
	pipeFrameKey   byte = 0x01 // 关键帧（H.264 I 帧 / JPEG 全帧）
	pipeFrameDelta byte = 0x02 // 增量帧（H.264 P 帧）
	pipeFrameState byte = 0x03
	pipeFrameDims  byte = 0x04
)

// captureOpts — 管道模式捕获参数。
type captureOpts struct {
	fps      int // 帧率上限（1-30）
	maxWidth int // 最大输出宽度（等比缩放）
	quality  int // 质量（1-100），映射 H.264 码率或 JPEG 质量
}

func main() {
	var (
		pipeName   = flag.String("pipe", "", "named pipe 名称（省略且未指定 --jpeg-single 时打印用法退出）")
		maxWidth   = flag.Int("max-width", 1920, "最大输出宽度")
		quality    = flag.Int("quality", 60, "JPEG/H.264 质量（1-100）")
		fps        = flag.Int("fps", 15, "帧率上限（1-30）")
		jpegSingle = flag.String("jpeg-single", "", "单帧截屏输出 JPEG 路径（截完即退出）")
	)
	flag.Parse()

	logLaunchContext()

	if *jpegSingle != "" {
		if err := runJpegSingle(*jpegSingle, *quality); err != nil {
			fmt.Fprintf(os.Stderr, "xnc-screen-helper: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *pipeName == "" {
		fmt.Fprintln(os.Stderr, "usage: xnc-screen-helper.exe --pipe <name> --max-width 1920 --quality 60 [--fps 30] [--jpeg-single <path>]")
		os.Exit(2)
	}
	opts := captureOpts{fps: *fps, maxWidth: *maxWidth, quality: *quality}
	if err := runPipe(*pipeName, opts); err != nil {
		log.Fatalf("xnc-screen-helper: %v", err)
	}
}

// runJpegSingle 捕获一帧并写 JPEG 后退出。DDA 优先（单帧即建即取，
// 静止桌面首帧也立即可得——DXGI 语义），WGC 回退。
func runJpegSingle(path string, quality int) error {
	if quality < 1 || quality > 100 {
		quality = 60
	}
	bgra, w, h, err := ddaSingleShot(2000)
	if err != nil {
		fmt.Fprintf(os.Stderr, "xnc-screen-helper: dda single: %v, trying wgc\n", err)
		bgra, w, h, err = captureWGCSingle(2000)
	}
	if err != nil {
		return fmt.Errorf("capture: %w", err)
	}
	return writeJPEG(path, bgra, w, h, quality)
}

// runPipe 启动 named pipe 服务端并进入捕获主循环，直至 agent 断开或进程被
// 终止（agent 直接 Kill）。采集器与首帧均在 listenPipe 之前获取（TB16G7
// 的 WGC 时序教训；DDA 无此约束但同序无害）。
func runPipe(pipeName string, opts captureOpts) error {
	if opts.fps < 1 || opts.fps > 30 {
		opts.fps = 15
	}
	if opts.quality < 1 || opts.quality > 100 {
		opts.quality = 60
	}
	ctx := context.Background()

	cap, backend, cerr := newScreenCapturer()
	if cerr != nil {
		// 不可用：仍需 listen 报告状态（placeholderLoop）。
		conn, err := listenPipe(ctx, pipeName)
		if err != nil {
			return fmt.Errorf("listen pipe: %w", err)
		}
		defer conn.Close()
		return placeholderLoop(ctx, conn, cerr)
	}
	defer cap.Close()
	fmt.Fprintf(os.Stderr, "xnc-screen-helper: capture backend: %s\n", backend)

	firstFrame, ferr := cap.AcquireFrame(2000)
	fmt.Fprintf(os.Stderr, "xnc-screen-helper: pre-pipe first frame: err=%v len=%d\n",
		ferr, len(firstFrame))

	conn, err := listenPipe(ctx, pipeName)
	if err != nil {
		return fmt.Errorf("listen pipe: %w", err)
	}
	defer conn.Close()

	return captureLoopWith(ctx, conn, opts, cap, firstFrame, ferr)
}
