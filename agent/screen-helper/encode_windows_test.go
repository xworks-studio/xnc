//go:build windows

// encode_windows_test.go — MFT H.264 编码器冒烟测试（真机 Windows 才有意义；
// MFT 不可用时跳过）。同时覆盖 BGRA→NV12 与 NALU 解析。
package main

import (
	"bytes"
	"testing"
)

func TestH264EncoderSmoke(t *testing.T) {
	const w, h = 128, 96
	enc, err := NewH264Encoder(w, h, 500_000, 15)
	if err != nil {
		t.Skipf("MFT H.264 encoder unavailable: %v", err)
	}
	defer enc.Close()

	frame := make([]byte, w*h*4)
	for i := 0; i < len(frame); i += 4 {
		frame[i], frame[i+2] = 0x80, 0xA0 // 均匀 BGR
	}
	if _, err := enc.Encode(frame, true); err != nil {
		t.Fatalf("first encode: %v", err)
	}
	// MFT 内部有 ~17 帧启动延迟——继续喂帧直到出关键帧。
	var keyData []byte
	for i := 0; i < 40 && keyData == nil; i++ {
		frame[16] ^= 0xFF // 每帧微变，驱动编码器
		data, err := enc.Encode(frame, i == 20)
		if err != nil {
			t.Fatalf("encode %d: %v", i, err)
		}
		if data != nil && enc.LastFrameKey() {
			keyData = data
		}
	}
	if keyData == nil {
		t.Fatal("no keyframe within 40 frames")
	}
	if len(enc.SPSPPS()) == 0 {
		t.Fatal("SPS/PPS not extracted after keyframe")
	}
	if !hasNALType(keyData, 7) {
		t.Error("keyframe missing SPS NAL")
	}
}

func TestBGRAToNV12(t *testing.T) {
	const w, h = 4, 4
	bgra := make([]byte, w*h*4)
	for i := 0; i < len(bgra); i += 4 {
		// 纯红：BGR = 00,00,FF
		bgra[i+2] = 0xFF
	}
	nv12 := make([]byte, w*h*3/2)
	bgraToNV12(bgra, nv12, w, h)
	// 红：Y ≈ 82，U ≈ 90，V ≈ 240（BT.601 有限范围）
	y, u, v := nv12[0], nv12[w*h], nv12[w*h+1]
	if y < 75 || y > 90 {
		t.Errorf("Y for red = %d, want ~82", y)
	}
	if u < 80 || u > 100 {
		t.Errorf("U for red = %d, want ~90", u)
	}
	if v < 230 || v > 250 {
		t.Errorf("V for red = %d, want ~240", v)
	}
}

func TestNALParsing(t *testing.T) {
	// SPS(7) + PPS(8) + IDR(5) + 非 IDR(1)
	stream := []byte{
		0, 0, 0, 1, 0x67, 0xAA,
		0, 0, 0, 1, 0x68, 0xBB,
		0, 0, 0, 1, 0x65, 0xCC,
		0, 0, 0, 1, 0x41, 0xDD,
	}
	if !hasNALType(stream, 5) || !hasNALType(stream, 7) || hasNALType(stream, 9) {
		t.Error("hasNALType failed")
	}
	spspps := extractNALs(stream, 7, 8)
	want := append([]byte{0, 0, 0, 1, 0x67, 0xAA}, 0, 0, 0, 1, 0x68, 0xBB)
	if !bytes.Equal(spspps, want) {
		t.Errorf("extractNALs = %X, want %X", spspps, want)
	}
}
