// frame.go — pipe 帧协议写入：[1B 类型][4B 长度 LE][payload]。
package main

import (
	"encoding/binary"
	"io"
)

// writeFrame 写一帧协议消息（类型 + LE 长度 + payload）。
func writeFrame(w io.Writer, typ byte, payload []byte) error {
	hdr := make([]byte, 5)
	hdr[0] = typ
	binary.LittleEndian.PutUint32(hdr[1:5], uint32(len(payload)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// packDims 打包 0x04 分辨率 payload：width/height int32 LE。
func packDims(w, h int) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b[0:4], uint32(int32(w)))
	binary.LittleEndian.PutUint32(b[4:8], uint32(int32(h)))
	return b
}
