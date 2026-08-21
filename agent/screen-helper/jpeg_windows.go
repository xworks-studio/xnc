//go:build windows

// jpeg_windows.go — --jpeg-single 模式：WGC 单帧捕获（BGRA）→ image/jpeg
// 编码 → 写文件。GDI+ 不经 COM，直接标准库。
package main

import (
	"fmt"
	"image"
	"image/jpeg"
	"os"
)

// writeJPEG 将 BGRA 帧转为 RGBA 图像并以给定质量写 JPEG。
func writeJPEG(path string, bgra []byte, w, h, quality int) error {
	if len(bgra) < w*h*4 {
		return fmt.Errorf("short frame: %d < %d", len(bgra), w*h*4)
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i, j := 0, 0; i+3 < len(bgra) && j+3 < len(img.Pix); i, j = i+4, j+4 {
		img.Pix[j+0] = bgra[i+2] // R <- B channel swap
		img.Pix[j+1] = bgra[i+1] // G
		img.Pix[j+2] = bgra[i+0] // B
		img.Pix[j+3] = 0xFF      // A
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return jpeg.Encode(f, img, &jpeg.Options{Quality: quality})
}
