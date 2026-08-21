// xnc-screen-helper 是独立的捕获进程：在用户控制台会话中运行（由 agent 经
// CreateProcessAsUser 拉起，避免 Session 0 无法访问桌面），通过 DXGI Desktop
// Duplication 捕获屏幕帧，经 named pipe 以帧协议推送给 agent。
//
// 用法：
//
//	xnc-screen-helper.exe --pipe <name> --max-width 1920 --quality 60 [--fps 30]
//	xnc-screen-helper.exe --jpeg-single <path> [--quality 60]
//
// --jpeg-single：GDI 单帧截屏 → JPEG → 写文件 → 退出（CLI --snapshot 路径，
// 不走 pipe 与 H.264）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"
)

const (
	pipeFrameState byte = 0x03
	pipeFrameDims  byte = 0x04
)

func main() {
	var (
		pipeName   = flag.String("pipe", "", "named pipe 名称（省略且未指定 --jpeg-single 时打印用法退出）")
		maxWidth   = flag.Int("max-width", 1920, "最大输出宽度")
		quality    = flag.Int("quality", 60, "JPEG/H.264 质量（1-100）")
		fps        = flag.Int("fps", 15, "帧率上限（1-30）")
		jpegSingle = flag.String("jpeg-single", "", "单帧 GDI 截屏输出 JPEG 路径（截完即退出）")
	)
	flag.Parse()
	_ = maxWidth

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
	if err := runPipe(*pipeName, *fps); err != nil {
		log.Fatalf("xnc-screen-helper: %v", err)
	}
}

// runJpegSingle 捕获一帧并写 JPEG 后退出。
func runJpegSingle(path string, quality int) error {
	if quality < 1 || quality > 100 {
		quality = 60
	}
	bgra, w, h, err := captureGDIFrame()
	if err != nil {
		return fmt.Errorf("gdi capture: %w", err)
	}
	return writeJPEG(path, bgra, w, h, quality)
}

// runPipe 启动 named pipe 服务端并进入捕获主循环，直至 agent 断开或进程被
// 终止（ctx 取消于 SIGINT/SIGTERM）。
func runPipe(pipeName string, fps int) error {
	if fps < 1 || fps > 30 {
		fps = 15
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	conn, err := listenPipe(ctx, pipeName)
	if err != nil {
		return fmt.Errorf("listen pipe: %w", err)
	}
	defer conn.Close()

	frameInterval := time.Second / time.Duration(fps)
	return captureLoop(ctx, conn, frameInterval)
}
