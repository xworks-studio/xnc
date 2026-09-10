//go:build !windows

// killTree 非 Windows 桩(与 agent/session 同源语义:SIGKILL 直杀)。
package main

import "syscall"

func killTree(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
