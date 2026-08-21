//go:build !windows

// jpeg_other.go — 非 Windows JPEG 单帧桩。
package main

import "errors"

// writeJPEG 桩（编码本身跨平台，但捕获源仅 Windows）。
func writeJPEG(path string, bgra []byte, w, h, quality int) error {
	return errors.New("jpeg snapshot not supported on this platform")
}
