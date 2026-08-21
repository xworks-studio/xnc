//go:build windows

// encode_repro_test.go — 编码像素正确性复现测试：合成非对称图案（四象限
// 纯色 + 大写 F 字形）经 MFT 编码，导出 Annex-B 裸流供 ffmpeg 解码验证。
// 四象限布局用于判定镜像（红↔绿 交换）与翻转（红↔蓝 交换）：
//
//	左上=红   右上=绿
//	左下=蓝   右下=白(黑F)
//
// 运行后用 XNC_DIAG_OUT 环境变量指定的输出文件做 ffmpeg 解码检查：
//
//	go test -run TestH264PixelRepro -v
//	  → %TEMP%/xnc-enc-repro/synth.h264
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestH264PixelRepro(t *testing.T) {
	// 与 TB16G7 生产一致的非对齐尺寸（1600×1000：MFT 需内部对齐到 MB）
	const w, h = 1600, 1000
	enc, err := NewH264Encoder(w, h, 2_300_000, 30)
	if err != nil {
		t.Skipf("MFT H.264 encoder unavailable: %v", err)
	}
	defer enc.Close()

	bgra := make([]byte, w*h*4)
	fillReproPattern(bgra, w, h)

	outDir := filepath.Join(os.TempDir(), "xnc-enc-repro")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(outDir, "synth.h264")
	f, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	total, keys := 0, 0
	// MFT 有启动缓冲：持续喂帧直到拿到关键帧；中途变化像素驱动 P 帧。
	// 输出整形与 sendEncoded 一致（缓存参数集前置 + vclNALUs 去重去 AUD），
	// 验证浏览器实际收到的字节布局。
	for i := 0; i < 60; i++ {
		if i > 0 {
			mutateReproPattern(bgra, w, h, i)
		}
		data, err := enc.Encode(bgra, i == 0 || i == 30, false)
		if err != nil {
			t.Fatalf("encode %d: %v", i, err)
		}
		if len(data) == 0 {
			continue
		}
		if enc.LastFrameKey() {
			keys++
			spspps := enc.SPSPPS()
			data = append(append([]byte{}, spspps...), vclNALUs(data)...)
		} else {
			data = vclNALUs(data)
		}
		f.Write(data)
		total++
		if keys >= 2 && total >= 10 {
			break
		}
	}
	t.Logf("encoded frames=%d keys=%d → %s", total, keys, outPath)
	if keys == 0 {
		t.Fatal("no keyframe produced")
	}
}

// fillReproPattern 四象限纯色 + 白底黑 F（F 非左右/上下对称，人眼与视觉
// 模型均可判定镜像/翻转）。
func fillReproPattern(bgra []byte, w, h int) {
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var b, g, r byte
			switch {
			case y < h/2 && x < w/2: // 左上：红
				r = 255
			case y < h/2: // 右上：绿
				g = 255
			case x < w/2: // 左下：蓝
				b = 255
			default: // 右下：白
				b, g, r = 255, 255, 255
			}
			i := (y*w + x) * 4
			bgra[i], bgra[i+1], bgra[i+2], bgra[i+3] = b, g, r, 255
		}
	}
	// 右下象限中央画黑色大 F（三横一竖，笔画 40px）
	drawF(bgra, w, h, w/2+w/4, h/2+h/4, 200, 40)
}

func drawF(bgra []byte, w, h, cx, cy, size, stroke int) {
	x0, y0 := cx-size/2, cy-size/2
	setBlack := func(x, y int) {
		if x < 0 || y < 0 || x >= w || y >= h {
			return
		}
		i := (y*w + x) * 4
		bgra[i], bgra[i+1], bgra[i+2], bgra[i+3] = 0, 0, 0, 255
	}
	for dy := 0; dy < size; dy++ {
		for dx := 0; dx < size; dx++ {
			// F：竖笔 | 顶横 — 中横（中横略短）
			isVert := dx < stroke
			isTopBar := dy < stroke
			isMidBar := dy >= size*2/5-stroke/2 && dy < size*2/5+stroke/2 && dx < size*3/5
			if isVert || isTopBar || isMidBar {
				setBlack(x0+dx, y0+dy)
			}
		}
	}
}

// mutateReproPattern 每帧小改（右下象限滚动竖线），驱动编码器产出 P 帧。
func mutateReproPattern(bgra []byte, w, h, frame int) {
	x := w/2 + w/4 + (frame*37)%(w/4)
	for y := h / 2; y < h; y++ {
		i := (y*w + x) * 4
		bgra[i], bgra[i+1], bgra[i+2] = 255, 0, 0
	}
}
