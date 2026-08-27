//go:build windows

// frame_v2_fuzz_test.go — M1 Task 5:decodeFrameV2 的有界模糊验证。种子取
// native 黄金向量(client_test.go 的 v2GoldenHex,与 native/desktop/
// desktop_selftest.cpp 的逐字节输出同源)及其截断头 / 截断载荷 / 尾随字节 /
// 超 8 MiB 上限 / CRC 损坏变体,让 fuzzer 从校验状态机的各个失败分支附近
// 起步。目标纯函数无环境依赖:任何输入都必须无 panic、无过度分配、无挂起
// 地返回(帧, nil)或(零值, error)。
package desktoppipe

import (
	"encoding/binary"
	"encoding/hex"
	"testing"
)

// nativeGoldenFrameV2 返回 native Task 2 EncodeFrameEventV2 的逐字节输出
// (77 字节,含 5 字节载荷;见 client_test.go 的 v2GoldenHex 注释)。
func nativeGoldenFrameV2() []byte {
	b, err := hex.DecodeString(v2GoldenHex)
	if err != nil {
		panic(err) // v2GoldenHex 是固定常量,解码不可能失败
	}
	return b
}

// corruptV2CRC 复制 seed 并翻转载荷一字节(CRC 必失配)。
func corruptV2CRC(seed []byte) []byte {
	c := append([]byte(nil), seed...)
	c[len(c)-1] ^= 1
	return c
}

// overCapV2Header 构造 72 字节头:header_bytes=72 但 payload_len = 8 MiB + 1
// (校验须在分配前拒绝)。
func overCapV2Header() []byte {
	h := make([]byte, v2HeaderBytes)
	binary.LittleEndian.PutUint32(h, v2HeaderBytes)
	binary.LittleEndian.PutUint32(h[64:], maxAuBytes+1)
	return h
}

// FuzzDecodeFrameV2:任意字节串喂给 decodeFrameV2,只要求不 panic / 不挂起
// (错误返回即合法输出)。运行:`go test ./desktoppipe -run '^$' -fuzz
// FuzzDecodeFrameV2 -fuzztime 30s`。
func FuzzDecodeFrameV2(f *testing.F) {
	golden := nativeGoldenFrameV2()
	f.Add(golden)                                       // 合法黄金帧(完整通过校验)
	f.Add(golden[:71])                                  // 截断头(< 72)
	f.Add(golden[:74])                                  // 截断载荷(payload_len=5,仅 2 字节在场)
	f.Add(append(append([]byte(nil), golden...), 0xEE)) // 尾随多余字节
	f.Add(overCapV2Header())                            // payload_len 超 8 MiB 上限
	f.Add(corruptV2CRC(golden))                         // CRC 损坏(载荷位翻转)
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = decodeFrameV2(b) })
}
