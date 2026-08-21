//go:build windows

// fallback_gdi_windows.go — GDI BitBlt 捕获回退（DXGI Desktop Duplication
// 不可用时：旧驱动 / 权限受限）。每帧 BitBlt 全屏 → BGRA → 与上帧比较
// （无变化返回 ErrTimeout，维持"静止即无帧"的自适应语义）→ 需要时按
// maxWidth 最近邻缩放。帧率上限由调用方节拍控制（CPU 限制，建议 ≤10fps）。
package main

import "fmt"

// gdiStreamCapturer — 管道模式的 GDI 流捕获器。
type gdiStreamCapturer struct {
	width, height int // 原始捕获分辨率
	outW, outH    int // 缩放后输出分辨率（等于 width/height 时不缩放）
	last          []byte
}

// newGDIStreamCapturer 探测一帧确定分辨率并完成缩放规划。
func newGDIStreamCapturer(maxWidth int) (*gdiStreamCapturer, error) {
	_, w, h, err := captureGDIFrame()
	if err != nil {
		return nil, fmt.Errorf("gdi probe: %w", err)
	}
	c := &gdiStreamCapturer{width: w, height: h}
	c.outW, c.outH = fitDims(w, h, maxWidth)
	return c, nil
}

// AcquireFrame 捕获一帧（timeoutMs 供接口对齐，GDI 无阻塞等待语义）。
// 与上帧相同返回 ErrTimeout。
func (c *gdiStreamCapturer) AcquireFrame(_ uint) ([]byte, []rect, error) {
	frame, _, _, err := captureGDIFrame()
	if err != nil {
		return nil, nil, fmt.Errorf("gdi capture: %w", err)
	}
	if c.outW != c.width {
		frame = scaleBGRA(frame, c.width, c.height, c.outW, c.outH)
	}
	if c.last != nil && bytesEqual(c.last, frame) {
		return nil, nil, ErrTimeout
	}
	if c.last == nil || len(c.last) != len(frame) {
		c.last = make([]byte, len(frame))
	}
	copy(c.last, frame)
	return frame, nil, nil
}

// Close 无长生命周期资源（GDI 句柄在 captureGDIFrame 内即取即还）。
func (c *gdiStreamCapturer) Close() {}

// fitDims 按 maxWidth 等比缩放并保证宽高为偶数（NV12/H.264 要求）。
func fitDims(w, h, maxWidth int) (int, int) {
	if maxWidth <= 0 || w <= maxWidth {
		return even(w), even(h)
	}
	ow := even(maxWidth)
	oh := even(h * ow / w)
	if oh < 2 {
		oh = 2
	}
	return ow, oh
}

func even(n int) int {
	if n < 2 {
		return 2
	}
	return n &^ 1
}

// scaleBGRA 最近邻缩放 BGRA 帧。
func scaleBGRA(src []byte, sw, sh, dw, dh int) []byte {
	dst := make([]byte, dw*dh*4)
	for y := 0; y < dh; y++ {
		sy := y * sh / dh
		dRow := dst[y*dw*4 : (y+1)*dw*4]
		sRow := src[sy*sw*4 : (sy+1)*sw*4]
		for x := 0; x < dw; x++ {
			sx := x * sw / dw
			copy(dRow[x*4:(x+1)*4], sRow[sx*4:(sx+1)*4])
		}
	}
	return dst
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
