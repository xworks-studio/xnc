// cursor_windows.go — DDA 指针形状合成（纯函数，黄金测试锁定）。
//
// DDA 帧不含光标（WGC 由系统合成，DDA 需自行叠加）。三种形状（对应
// DXGI_OUTDUPL_POINTER_SHAPE_TYPE）：
//
//	monochrome(1)：AND 掩码 + XOR 掩码各 h 行、每行 pitch 字节（w 位）。
//	  经典四象限：AND=1,XOR=0 → 透明；AND=0,XOR=0 → 黑；AND=0,XOR=1 →
//	  白；AND=1,XOR=1 → 反相。
//	color(2)：32bpp BGRA 位图，A=0 透明。
//	masked(4)：32bpp，A=255 像素处屏幕反相（遮罩指针），其余透明。
//
// 位置 ci.X/ci.Y 为 DXGI 给出的桌面坐标（本输出左上为原点），按左上角
// 绘制。v1 不做彩色指针热点校正（DDA 不提供热点，误差通常 1-2px）。
//
//go:build windows

package main

// composeCursorBGRA 把形状 shape 合成进 BGRA 帧（原地）。越界裁剪。
func composeCursorBGRA(bgra []byte, w, h int, shape []byte, ci ddaCursor) {
	switch ci.Type {
	case ddaPointerMonochrome:
		composeMonoCursor(bgra, w, h, shape, ci)
	case ddaPointerColor, ddaPointerMasked:
		composeColorCursor(bgra, w, h, shape, ci)
	}
}

// bitAt 读第 row 行第 col 位（每行 pitch 字节）。
func bitAt(mask []byte, row, col, pitch, x0, y0 int) int {
	i := (row+y0)*pitch + (col+x0)/8
	if i >= len(mask) {
		return 0
	}
	return int(mask[i]>>uint(7-uint((col+x0)%8))) & 1
}

// composeMonoCursor 双掩码合成。
func composeMonoCursor(bgra []byte, w, h int, shape []byte, ci ddaCursor) {
	cw, ch := int(ci.W), int(ci.H)
	maskRow := int(ci.Pitch) // 每个掩码的行距
	x0, y0 := int(ci.X), int(ci.Y)
	for y := 0; y < ch; y++ {
		dy := y + y0
		if dy < 0 || dy >= h {
			continue
		}
		for x := 0; x < cw; x++ {
			dx := x + x0
			if dx < 0 || dx >= w {
				continue
			}
			and := bitAt(shape, y, x, maskRow, 0, 0)
			xor := bitAt(shape, y, x, maskRow, 0, ch) // XOR 掩码在 AND 之后
			if and == 1 && xor == 0 {
				continue // 透明
			}
			o := (dy*w + dx) * 4
			switch {
			case and == 0 && xor == 0: // 黑
				bgra[o], bgra[o+1], bgra[o+2] = 0, 0, 0
			case and == 0 && xor == 1: // 白
				bgra[o], bgra[o+1], bgra[o+2] = 255, 255, 255
			default: // 反相
				bgra[o] = ^bgra[o]
				bgra[o+1] = ^bgra[o+1]
				bgra[o+2] = ^bgra[o+2]
			}
		}
	}
}

// composeColorCursor 32bpp 形状合成（color=直写、masked=A=255 反相）。
func composeColorCursor(bgra []byte, w, h int, shape []byte, ci ddaCursor) {
	cw, ch := int(ci.W), int(ci.H)
	pitch := int(ci.Pitch)
	x0, y0 := int(ci.X), int(ci.Y)
	invert := ci.Type == ddaPointerMasked
	for y := 0; y < ch; y++ {
		dy := y + y0
		if dy < 0 || dy >= h {
			continue
		}
		rowOff := y * pitch
		for x := 0; x < cw; x++ {
			dx := x + x0
			if dx < 0 || dx >= w {
				continue
			}
			s := rowOff + x*4
			if s+3 >= len(shape) {
				return
			}
			b, g, r, a := shape[s], shape[s+1], shape[s+2], shape[s+3]
			if a == 0 {
				continue
			}
			o := (dy*w + dx) * 4
			if invert {
				bgra[o] = ^bgra[o]
				bgra[o+1] = ^bgra[o+1]
				bgra[o+2] = ^bgra[o+2]
			} else {
				bgra[o], bgra[o+1], bgra[o+2] = b, g, r
			}
		}
	}
}
