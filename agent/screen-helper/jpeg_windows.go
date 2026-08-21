//go:build windows

// jpeg_windows.go — --jpeg-single 模式：GDI BitBlt 全屏截屏（BGRA）→
// image/jpeg 编码 → 写文件。GDI+ 不经 COM，直接标准库。
package main

import (
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	gdi32                   = windows.NewLazySystemDLL("gdi32.dll")
	procGetDC               = user32.NewProc("GetDC")
	procReleaseDC           = user32.NewProc("ReleaseDC")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procCreateCompatibleDC  = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBmp = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject        = gdi32.NewProc("SelectObject")
	procBitBlt              = gdi32.NewProc("BitBlt")
	procGetDIBits           = gdi32.NewProc("GetDIBits")
	procGetBitmapBits       = gdi32.NewProc("GetBitmapBits")
	procDeleteObject        = gdi32.NewProc("DeleteObject")
	procDeleteDC            = gdi32.NewProc("DeleteDC")
)

const (
	srccopy      = 0x00CC0020
	dibRgbColors = 0
	biRgb        = 0
)

// captureGDIFrame 用 GDI 截取主屏全帧，返回顶层自上而下的 32bpp BGRA 字节。
func captureGDIFrame() ([]byte, int, int, error) {
	screenDC, _, _ := procGetDC.Call(0)
	if screenDC == 0 {
		return nil, 0, 0, fmt.Errorf("GetDC failed")
	}
	defer procReleaseDC.Call(0, screenDC)

	sm0, _, _ := procGetSystemMetrics.Call(0)
	sm1, _, _ := procGetSystemMetrics.Call(1)
	w, h := int(sm0), int(sm1)
	if w <= 0 || h <= 0 {
		return nil, 0, 0, fmt.Errorf("GetSystemMetrics: %dx%d", w, h)
	}

	memDC, _, _ := procCreateCompatibleDC.Call(screenDC)
	if memDC == 0 {
		return nil, 0, 0, fmt.Errorf("CreateCompatibleDC failed")
	}
	defer procDeleteDC.Call(memDC)

	bmp, _, _ := procCreateCompatibleBmp.Call(screenDC, uintptr(w), uintptr(h))
	if bmp == 0 {
		return nil, 0, 0, fmt.Errorf("CreateCompatibleBitmap failed")
	}
	defer procDeleteObject.Call(bmp)
	oldBmp, _, _ := procSelectObject.Call(memDC, bmp)

	if r, _, _ := procBitBlt.Call(memDC, 0, 0, uintptr(w), uintptr(h), screenDC, 0, 0, srccopy); r == 0 {
		return nil, 0, 0, fmt.Errorf("BitBlt failed")
	}
	// 选回默认位图（读取类 API 要求 bitmap 未被选入 DC）。
	procSelectObject.Call(memDC, oldBmp)

	// 优先 GetDIBits（top-down，负 biHeight）；失败回退 GetBitmapBits（bottom-up
	// + 手动翻行——部分环境 GetDIBits 始终失败）。
	var bi [40]byte
	le32(bi[0:4], 40)
	le32(bi[4:8], w)
	le32(bi[8:12], -h)
	le32(bi[12:16], 1)  // planes
	le32(bi[16:20], 32) // bpp
	// 其余 0（BI_RGB）

	buf := make([]byte, w*h*4)
	r1, _, _ := procGetDIBits.Call(memDC, bmp, 0, uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&bi[0])), dibRgbColors)
	if r1 == 0 {
		if n, _, _ := procGetBitmapBits.Call(bmp, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0]))); n == 0 {
			return nil, 0, 0, fmt.Errorf("GetDIBits/GetBitmapBits failed")
		}
		flipRows(buf, w*4)
	}
	return buf, w, h, nil
}

// flipRows 原地垂直翻转（bottom-up → top-down）。
func flipRows(buf []byte, stride int) {
	rows := len(buf) / stride
	for i, j := 0, rows-1; i < j; i, j = i+1, j-1 {
		a := buf[i*stride : (i+1)*stride]
		b := buf[j*stride : (j+1)*stride]
		for k := range a {
			a[k], b[k] = b[k], a[k]
		}
	}
}

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

func le32(b []byte, v int) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}
