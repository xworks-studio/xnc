//go:build !windows

package identity

import "bytes"

// 非 Windows：明文落盘（边界 = 文件系统权限：StateDir 0700 + identity.json
// 0600，见 identity.Save）。契约与 Windows DPAPI 版一致：protect 返回**独立
// 缓冲区**——调用方随后会 clear 原切片擦除明文，透传同一底层数组会把
// 落盘内容一起清零（2026-09-28 Linux 节点真机事故：私钥存成 64 字节全零，
// 注册公钥与连接签名密钥分裂，控制连接恒验签失败）。
func protect(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func unprotect(b []byte) ([]byte, error) { return bytes.Clone(b), nil }
