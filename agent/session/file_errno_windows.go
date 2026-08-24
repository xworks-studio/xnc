//go:build windows

package session

import "syscall"

// errorSharingViolation = ERROR_SHARING_VIOLATION (32)：stdlib syscall 未导出。
const errorSharingViolation = syscall.Errno(32)

// errnoIsAccessDenied：Windows 上 rename/create 目标被锁（如运行中的 exe）
// 或共享冲突的 errno 集合。
func errnoIsAccessDenied(errno syscall.Errno) bool {
	switch errno {
	case syscall.ERROR_ACCESS_DENIED, errorSharingViolation,
		syscall.EACCES, syscall.EPERM:
		return true
	}
	return false
}
