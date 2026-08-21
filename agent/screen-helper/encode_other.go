//go:build !windows

// encode_other.go — 非 Windows 桩：H.264 MFT 编码仅支持 Windows。
// JPEG 帧流回退编码器（encoder.go）跨平台可用，但捕获源本身仅 Windows。
package main

// newH264Encoder 桩：非 Windows 无 MFT。
func newH264Encoder(width, height, bitrate, gopSize int) (frameEncoder, error) {
	return nil, errUnsupported
}
