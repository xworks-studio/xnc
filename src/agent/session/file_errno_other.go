//go:build !windows

package session

import "syscall"

// errnoIsAccessDenied：非 Windows 平台 rename/create 的权限 errno。
func errnoIsAccessDenied(errno syscall.Errno) bool {
	switch errno {
	case syscall.EACCES, syscall.EPERM:
		return true
	}
	return false
}
