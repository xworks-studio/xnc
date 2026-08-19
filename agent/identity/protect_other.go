//go:build !windows

package identity

// 非 Windows 开发/测试环境：明文透传（不入生产）。
func protect(b []byte) []byte            { return b }
func unprotect(b []byte) ([]byte, error) { return b, nil }
