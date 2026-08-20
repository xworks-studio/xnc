//go:build !windows

package session

import (
	"syscall"
)

// killTree 非 Windows：SIGKILL 直杀进程本身（Linux 进程组/会话杀为 Phase 8
// 议题，届时再演进为组信号）。
func killTree(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
