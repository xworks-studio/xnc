// cursor_windows_test.go — 指针形状合成黄金测试（防 debug 的核心资产：
// 历史上像素类 bug 全靠这类逐字节锁定测试拦截）。
//
//go:build windows

package main

import (
	"bytes"
	"testing"
)

// newFrame 生成 w*h 纯色 BGRA 帧。
func newFrame(w, h int, v byte) []byte {
	f := make([]byte, w*h*4)
	for i := 0; i < len(f); i += 4 {
		f[i], f[i+1], f[i+2], f[i+3] = v, v, v, 255
	}
	return f
}

func TestComposeColorCursorOpaqueAndTransparent(t *testing.T) {
	// 2x2 彩色指针：左上不透明白、右上透明、左下不透明红、右下透明。
	shape := []byte{
		0xFF, 0xFF, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x00,
	}
	ci := ddaCursor{Type: ddaPointerColor, W: 2, H: 2, Pitch: 8, X: 1, Y: 1}
	frame := newFrame(4, 4, 0x80) // 全灰底
	composeCursorBGRA(frame, 4, 4, shape, ci)

	get := func(x, y int) (byte, byte, byte) {
		o := (y*4 + x) * 4
		return frame[o], frame[o+1], frame[o+2]
	}
	// 白：BGRA 全 255。
	if b, g, r := get(1, 1); b != 255 || g != 255 || r != 255 {
		t.Fatalf("white pixel = (%d,%d,%d)", r, g, b)
	}
	// 红：R=255 G=B=0。
	if b, g, r := get(1, 2); b != 0 || g != 0 || r != 255 {
		t.Fatalf("red pixel = (%d,%d,%d)", r, g, b)
	}
	// 透明位与未覆盖区保持灰底。
	if b, g, r := get(2, 1); b != 0x80 || g != 0x80 || r != 0x80 {
		t.Fatalf("transparent pixel = (%d,%d,%d)", r, g, b)
	}
	if b, g, _ := get(0, 0); b != 0x80 || g != 0x80 {
		t.Fatal("uncovered pixel changed")
	}
}

func TestComposeColorCursorClipBounds(t *testing.T) {
	// 位置 (3,3) 的 2x2 指针在 4x4 帧上：只有 (3,3) 一个像素可见。
	shape := []byte{
		0x11, 0x22, 0x33, 0xFF, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	ci := ddaCursor{Type: ddaPointerColor, W: 2, H: 2, Pitch: 8, X: 3, Y: 3}
	frame := newFrame(4, 4, 0x10)
	composeCursorBGRA(frame, 4, 4, shape, ci)
	o := (3*4 + 3) * 4
	if frame[o] != 0x11 || frame[o+1] != 0x22 || frame[o+2] != 0x33 {
		t.Fatalf("clipped pixel = %v", frame[o:o+3])
	}
	for i := 0; i < len(frame); i += 4 {
		if i == o {
			continue
		}
		if frame[i] != 0x10 {
			t.Fatalf("unexpected change at %d", i)
		}
	}
}

func TestComposeMaskedCursorInverts(t *testing.T) {
	// masked：A=255 处反相底色（0x80 → 0x7F），A=0 处不变。
	shape := []byte{0, 0, 0, 0xFF, 0, 0, 0, 0}
	ci := ddaCursor{Type: ddaPointerMasked, W: 2, H: 1, Pitch: 8, X: 0, Y: 0}
	frame := newFrame(2, 1, 0x80)
	composeCursorBGRA(frame, 2, 2, shape, ci)
	if frame[0] != 0x7F || frame[1] != 0x7F || frame[2] != 0x7F {
		t.Fatalf("invert pixel = %v", frame[0:3])
	}
	if frame[4] != 0x80 {
		t.Fatal("transparent masked pixel changed")
	}
}

func TestComposeMonoCursorQuadrants(t *testing.T) {
	// 2x2 单色指针，每行掩码 1 字节（pitch=1，位序 MSB-first）：
	//   AND=0b01, XOR=0b00 → 左黑(bit0: and=0,xor=0)、右透明(bit1: and=1,xor=0)
	//   AND=0b11, XOR=0b01 → 左透明(bit0: and=1,xor=0)、右反相(bit1: and=1,xor=1)
	and := []byte{0b01000000, 0b11000000}
	xor := []byte{0b00000000, 0b01000000}
	shape := append(append([]byte{}, and...), xor...)
	ci := ddaCursor{Type: ddaPointerMonochrome, W: 2, H: 2, Pitch: 1, X: 0, Y: 0}
	frame := newFrame(2, 2, 0x40)
	composeCursorBGRA(frame, 2, 2, shape, ci)

	if frame[0] != 0 || frame[1] != 0 || frame[2] != 0 {
		t.Fatalf("black pixel = %v", frame[0:3])
	}
	if frame[4] != 0x40 {
		t.Fatal("transparent pixel changed")
	}
	if frame[8] != 0x40 {
		t.Fatal("transparent pixel changed (row 1)")
	}
	if frame[12] != ^byte(0x40) {
		t.Fatalf("invert pixel = %d want %d", frame[12], ^byte(0x40))
	}
}

func TestComposeNoopOnInvisibleOrEmpty(t *testing.T) {
	frame := newFrame(2, 2, 0x55)
	want := bytes.Clone(frame)
	composeCursorBGRA(frame, 2, 2, nil, ddaCursor{Type: ddaPointerColor, W: 4, H: 4, X: 0, Y: 0, Len: 0})
	if !bytes.Equal(frame, want) {
		t.Fatal("empty shape changed frame")
	}
}
