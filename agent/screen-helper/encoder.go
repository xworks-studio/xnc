// encoder.go — 捕获管线使用的编码器抽象。目标编码为 H.264（Windows MFT，
// encode_windows.go）；JPEG 帧流为 MFT 不可用时的回退实现（每帧独立
// keyframe，带宽效率低但功能正确，H.264 仍是既定目标）。
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
)

// frameEncoder 是捕获主循环使用的编码器接口。Encode 输入 BGRA 帧
// （top-down，width*height*4 字节）；H.264 模式输出 Annex-B NALU，
// JPEG 模式输出完整 JPEG（恒为关键帧）。
type frameEncoder interface {
	// Encode 编码一帧。forceKey 请求关键帧。flipY 控制 BGRA 行序翻转。
	// 返回 nil, nil 表示编码器暂无输出（可跳过）。
	Encode(frame []byte, forceKey bool, flipY bool) ([]byte, error)
	// SPSPPS 返回 H.264 参数集（JPEG 模式恒 nil）。
	SPSPPS() []byte
	// LastFrameKey 报告最近一次 Encode 输出是否关键帧。
	LastFrameKey() bool
	// Close 释放资源。
	Close()
}

// jpegStreamEncoder — JPEG 帧流回退编码器（H.264 MFT 不可用时）。
// 每帧独立编码，帧类型恒为 0x01（keyframe），解码端无需解码器状态。
type jpegStreamEncoder struct {
	width, height int
	quality       int
	rgba          []byte
}

// newJPEGStreamEncoder 创建 JPEG 流编码器（quality 1-100）。
func newJPEGStreamEncoder(width, height, quality int) *jpegStreamEncoder {
	if quality < 1 || quality > 100 {
		quality = 60
	}
	return &jpegStreamEncoder{
		width: width, height: height, quality: quality,
		rgba: make([]byte, width*height*4),
	}
}

// Encode 实现 frameEncoder：BGRA→RGBA→JPEG。flipY 未使用（JPEG path 不需要翻转）。
func (j *jpegStreamEncoder) Encode(frame []byte, _ bool, _ bool) ([]byte, error) {
	if len(frame) < j.width*j.height*4 {
		return nil, fmt.Errorf("short frame: %d < %d", len(frame), j.width*j.height*4)
	}
	fillRGBA(j.rgba, frame)
	img := &image.RGBA{Pix: j.rgba, Stride: j.width * 4, Rect: image.Rect(0, 0, j.width, j.height)}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: j.quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (j *jpegStreamEncoder) SPSPPS() []byte     { return nil }
func (j *jpegStreamEncoder) LastFrameKey() bool { return true }
func (j *jpegStreamEncoder) Close()             {}

// fillRGBA 就地转换 BGRA（top-down）到 RGBA。
func fillRGBA(dst, src []byte) {
	for i, j := 0, 0; i+3 < len(src) && j+3 < len(dst); i, j = i+4, j+4 {
		dst[j+0] = src[i+2]
		dst[j+1] = src[i+1]
		dst[j+2] = src[i+0]
		dst[j+3] = 0xFF
	}
}
