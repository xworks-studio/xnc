//go:build windows

package identity

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// protect 使用 DPAPI CryptProtectData 保护私钥。
// 服务以 SYSTEM 运行、密钥需跨用户上下文可读，因此必须置 CRYPTPROTECT_LOCAL_MACHINE。
// 失败即 panic：无法保护私钥时落盘明文是不可接受的。
func protect(b []byte) []byte {
	if len(b) == 0 {
		panic("dpapi protect: empty input")
	}
	in := windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil,
		windows.CRYPTPROTECT_LOCAL_MACHINE, &out); err != nil {
		panic("dpapi protect: " + err.Error())
	}
	res, err := outBytes(&out)
	if err != nil {
		panic("dpapi protect: " + err.Error())
	}
	return res
}

func unprotect(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, errInvalidKey
	}
	in := windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil,
		windows.CRYPTPROTECT_LOCAL_MACHINE, &out); err != nil {
		return nil, err
	}
	return outBytes(&out)
}

// outBytes 拷贝 DPAPI 输出 blob（crypt32 用 LocalAlloc 分配）到 Go 内存并释放原缓冲。
func outBytes(blob *windows.DataBlob) ([]byte, error) {
	if blob.Data == nil || blob.Size == 0 {
		return nil, errInvalidKey
	}
	res := make([]byte, blob.Size)
	copy(res, unsafe.Slice(blob.Data, blob.Size))
	_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(blob.Data)))
	return res, nil
}
