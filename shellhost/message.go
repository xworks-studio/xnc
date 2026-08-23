// message.go — xnc-shell pipe 消息层(spec §8.5, M2-Slice2 Task 2 契约)。
//
// 全部走 XNIP 帧(proto/ipc, 16B 小端头),payload 为固定二进制布局:
//
//	MSG_SHELL_BEGIN  0x0122 server→client [u16 cols][u16 rows][char profile[16]]
//	MSG_SHELL_DATA   0x0123 双向           [u8 stream 0=stdin,1=stdout,2=stderr]
//	                                           [u32 len][bytes]
//	MSG_SHELL_RESIZE 0x0124 client→server  [u16 cols][u16 rows]
//	MSG_SHELL_EXIT   0x0125 server→client  [u32 exit_code]
//	MSG_SHELL_KILL   0x0126 client→server  (无 payload)
//	MSG_SHELL_STATE  0x0127 server→client  [char code[24]](NUL 填充)
//
// char[] 字段 NUL 填充,超长截断。本文件跨平台(编解码单测)。
package main

import (
	"encoding/binary"
	"fmt"
)

// 消息类型注册表(native/core Task 3 将按同一契约实现 CreateShell)。
const (
	msgShellBegin  uint16 = 0x0122
	msgShellData   uint16 = 0x0123
	msgShellResize uint16 = 0x0124
	msgShellExit   uint16 = 0x0125
	msgShellKill   uint16 = 0x0126
	msgShellState  uint16 = 0x0127
)

// stream 字节值。
const (
	streamStdin  uint8 = 0
	streamStdout uint8 = 1
	streamStderr uint8 = 2
)

// 字段宽度。
const (
	profileFieldLen = 16
	stateFieldLen   = 24
)

// 稳定 state code(spec §8.5)。
const (
	stateSpawnFailed    = "spawn_failed"
	stateProfileMissing = "profile_missing"
)

// encodeBegin 编码 SHELL_BEGIN payload [u16 cols][u16 rows][char profile[16]]。
func encodeBegin(cols, rows uint16, profile string) []byte {
	p := make([]byte, 4+profileFieldLen)
	binary.LittleEndian.PutUint16(p, cols)
	binary.LittleEndian.PutUint16(p[2:], rows)
	copy(p[4:], profile)
	return p
}

func decodeBegin(p []byte) (cols, rows uint16, profile string, err error) {
	if len(p) != 4+profileFieldLen {
		return 0, 0, "", fmt.Errorf("shellhost: begin payload %d bytes, want %d", len(p), 4+profileFieldLen)
	}
	return binary.LittleEndian.Uint16(p), binary.LittleEndian.Uint16(p[2:]),
		nulString(p[4:]), nil
}

// encodeData 编码 SHELL_DATA payload [u8 stream][u32 len][bytes]。
func encodeData(stream uint8, data []byte) []byte {
	p := make([]byte, 5+len(data))
	p[0] = stream
	binary.LittleEndian.PutUint32(p[1:], uint32(len(data)))
	copy(p[5:], data)
	return p
}

func decodeData(p []byte) (stream uint8, data []byte, err error) {
	if len(p) < 5 {
		return 0, nil, fmt.Errorf("shellhost: data payload %d bytes, want >= 5", len(p))
	}
	n := binary.LittleEndian.Uint32(p[1:])
	if uint64(len(p)) != 5+uint64(n) {
		return 0, nil, fmt.Errorf("shellhost: data length mismatch: len field %d, payload %d", n, len(p))
	}
	return p[0], p[5:], nil
}

// encodeResize 编码 SHELL_RESIZE payload [u16 cols][u16 rows]。
func encodeResize(cols, rows uint16) []byte {
	p := make([]byte, 4)
	binary.LittleEndian.PutUint16(p, cols)
	binary.LittleEndian.PutUint16(p[2:], rows)
	return p
}

func decodeResize(p []byte) (cols, rows uint16, err error) {
	if len(p) != 4 {
		return 0, 0, fmt.Errorf("shellhost: resize payload %d bytes, want 4", len(p))
	}
	return binary.LittleEndian.Uint16(p), binary.LittleEndian.Uint16(p[2:]), nil
}

// encodeExit 编码 SHELL_EXIT payload [u32 exit_code]。
func encodeExit(code uint32) []byte {
	p := make([]byte, 4)
	binary.LittleEndian.PutUint32(p, code)
	return p
}

func decodeExit(p []byte) (uint32, error) {
	if len(p) != 4 {
		return 0, fmt.Errorf("shellhost: exit payload %d bytes, want 4", len(p))
	}
	return binary.LittleEndian.Uint32(p), nil
}

// encodeState 编码 SHELL_STATE payload [char code[24]](截断 23)。
func encodeState(code string) []byte {
	p := make([]byte, stateFieldLen)
	copy(p, code)
	return p
}

func decodeState(p []byte) (string, error) {
	if len(p) != stateFieldLen {
		return "", fmt.Errorf("shellhost: state payload %d bytes, want %d", len(p), stateFieldLen)
	}
	return nulString(p), nil
}

// nulString 取 NUL 填充字段的有效前缀。
func nulString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
