// pixel_windows.go — 像素路径唯一实现（黄金测试 pixel_windows_test.go
// 逐字节锁定）：尺寸适配、缩放、色彩空间转换。历史上行序颠倒/镜像/
// 色彩偏差全部出自分散的像素代码——收敛于此，勿在别处新增转换。
//
//go:build windows

package main

import "unsafe"

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
func ptrGUID(g *guid) uintptr { return uintptr(unsafe.Pointer(g)) }

// ---- BGRA → NV12（BT.601，整数近似）----

// bgraToNV12 将 top-down BGRA 帧转换为 NV12（Y 平面 + 交错的 UV 半分辨率
// 平面）。dst 需为 w*h*3/2 字节。flipY 控制垂直翻转（DXGI 某些驱动返回
// bottom-up 行序时需要翻转为 top-down）。
func bgraToNV12(bgra, dst []byte, w, h int, flipY bool) {
	yPlane := dst[:w*h]
	uvPlane := dst[w*h:]
	stride := w * 4

	rowIdx := func(row int) int {
		if flipY {
			return (h - 1 - row) * stride
		}
		return row * stride
	}

	for row := 0; row < h; row++ {
		yRow := yPlane[row*w : (row+1)*w]
		srcRow := bgra[rowIdx(row) : rowIdx(row)+stride]
		for x := 0; x < w; x++ {
			b := int(srcRow[x*4])
			g := int(srcRow[x*4+1])
			r := int(srcRow[x*4+2])
			yRow[x] = byte((66*r + 129*g + 25*b + 128) >> 8)
			yRow[x] += 16
		}
	}
	for row := 0; row < h/2; row++ {
		uvRow := uvPlane[row*w : (row+1)*w]
		srcRow0 := rowIdx(row * 2)
		srcRow1 := rowIdx(row*2 + 1)
		for cx := 0; cx < w/2; cx++ {
			b0, g0, r0 := bgra[srcRow0+cx*8], bgra[srcRow0+cx*8+1], bgra[srcRow0+cx*8+2]
			b1, g1, r1 := bgra[srcRow0+cx*8+4], bgra[srcRow0+cx*8+5], bgra[srcRow0+cx*8+6]
			b2, g2, r2 := bgra[srcRow1+cx*8], bgra[srcRow1+cx*8+1], bgra[srcRow1+cx*8+2]
			b3, g3, r3 := bgra[srcRow1+cx*8+4], bgra[srcRow1+cx*8+5], bgra[srcRow1+cx*8+6]
			b := (int(b0) + int(b1) + int(b2) + int(b3)) / 4
			g := (int(g0) + int(g1) + int(g2) + int(g3)) / 4
			r := (int(r0) + int(r1) + int(r2) + int(r3)) / 4
			u := ((-38*r - 74*g + 112*b + 128) >> 8) + 128
			v := ((112*r - 94*g - 18*b + 128) >> 8) + 128
			uvRow[cx*2] = byte(u)
			uvRow[cx*2+1] = byte(v)
		}
	}
}
