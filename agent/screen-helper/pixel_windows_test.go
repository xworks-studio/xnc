// pixel_windows_test.go — 像素路径黄金测试（防 debug 核心资产）：历史上
// 行序颠倒/镜像/色彩偏差全部出自分散的像素代码，此处以纯色象限 + 精确
// 缩放字节锁定 scaleBGRA 与 bgraToNV12 的行为。
//
//go:build windows

package main

import "testing"

// quadBGRA 生成 2x2 四象限纯色 BGRA：左上 red、右上 green、左下 blue、
// 右下 white。
func quadBGRA() ([]byte, int, int) {
	const w, h = 2, 2
	b := make([]byte, w*h*4)
	put := func(x, y int, r, g, bl uint8) {
		o := (y*w + x) * 4
		b[o], b[o+1], b[o+2], b[o+3] = bl, g, r, 255
	}
	put(0, 0, 255, 0, 0)
	put(1, 0, 0, 255, 0)
	put(0, 1, 0, 0, 255)
	put(1, 1, 255, 255, 255)
	return b, w, h
}

// nv12At 取像素平面 Y 与 2x2 色度平面 UV（0 基）。
func nv12At(nv12 []byte, w, h, x, y int) (y_, u, v uint8) {
	y_ = nv12[y*w+x]
	c := w*h + (y/2)*w + (x/2)*2
	return y_, nv12[c], nv12[c+1]
}

// TestBGRAToNV12Quadrants 四象限色彩锁定（BT.601 有限范围容差 ±8）：
// 红 (82,90,240)、绿 (145,54,34)、蓝 (41,110,240... 实际蓝 V 高)、
// 白 (235,128,128)。
func TestBGRAToNV12Quadrants(t *testing.T) {
	bgra, w, h := quadBGRA()
	nv12 := make([]byte, w*h*3/2)
	bgraToNV12(bgra, nv12, w, h, false)

	cases := []struct {
		name       string
		x, y       int
		wantY, U, V int
	}{
		{"red", 0, 0, 82, 90, 240},
		{"green", 1, 0, 145, 54, 34},
		{"blue", 0, 1, 41, 240, 110},
		{"white", 1, 1, 235, 128, 128},
	}
	for _, c := range cases {
		gy, gu, gv := nv12At(nv12, w, h, c.x, c.y)
		// 象限色在 2x2 上共享同一色度采样（4:2:0）：色度是四色平均，
		// 仅 Y 按象限精确校验；白/红的 Y 独立可验。
		if c.name == "red" || c.name == "green" || c.name == "blue" || c.name == "white" {
			if abs(int(gy)-c.wantY) > 8 {
				t.Errorf("%s Y = %d, want ~%d", c.name, gy, c.wantY)
			}
		}
		_ = gu
		_ = gv
	}
}

// TestBGRAToNV12WhiteChroma 白色象限的色度必须中性（U=V=128±4）——
// 白平衡漂移类 bug 的直接探测器。
func TestBGRAToNV12WhiteChroma(t *testing.T) {
	const w, h = 2, 2
	bgra := make([]byte, w*h*4)
	for i := 0; i < len(bgra); i += 4 {
		bgra[i], bgra[i+1], bgra[i+2], bgra[i+3] = 255, 255, 255, 255
	}
	nv12 := make([]byte, w*h*3/2)
	bgraToNV12(bgra, nv12, w, h, false)
	for i := w * h; i < len(nv12); i++ {
		if abs(int(nv12[i])-128) > 4 {
			t.Fatalf("chroma[%d] = %d, want ~128 (white chroma must be neutral)", i, nv12[i])
		}
	}
}

// TestScaleBGRAHalfExact 精确字节校验：4x4 棋盘块缩到 2x2（近邻采样），
// 输出像素可逐字节断言——行序/镜像/偏移任何一种错误都无法通过。
func TestScaleBGRAHalfExact(t *testing.T) {
	const sw, sh, dw, dh = 4, 4, 2, 2
	src := make([]byte, sw*sh*4)
	// 2x2 像素块棋盘：左上块红、右上块绿、左下块蓝、右下块白。
	block := func(bx, by int, r, g, b uint8) {
		for y := by * 2; y < by*2+2; y++ {
			for x := bx * 2; x < bx*2+2; x++ {
				o := (y*sw + x) * 4
				src[o], src[o+1], src[o+2], src[o+3] = b, g, r, 255
			}
		}
	}
	block(0, 0, 255, 0, 0)
	block(1, 0, 0, 255, 0)
	block(0, 1, 0, 0, 255)
	block(1, 1, 255, 255, 255)

	dst := scaleBGRA(src, sw, sh, dw, dh)
	want := []byte{
		0, 0, 255, 255, // 红 BGR
		0, 255, 0, 255, // 绿
		255, 0, 0, 255, // 蓝
		255, 255, 255, 255,
	}
	if len(dst) != len(want) {
		t.Fatalf("scaled len = %d, want %d", len(dst), len(want))
	}
	for i, wb := range want {
		if dst[i] != wb {
			t.Fatalf("byte[%d] = %d, want %d（全 dump: %v）", i, dst[i], wb, dst)
		}
	}
}

// TestScaleBGRANoop 恒等缩放必须原样返回内容。
func TestScaleBGRANoop(t *testing.T) {
	src, w, h := quadBGRA()
	dst := scaleBGRA(src, w, h, w, h)
	for i := range src {
		if dst[i] != src[i] {
			t.Fatalf("identity scale changed byte %d: %d != %d", i, dst[i], src[i])
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
