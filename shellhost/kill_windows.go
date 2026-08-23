//go:build windows

package main

import (
	"os/exec"
	"strconv"
)

// killTree 结束 pid 及其整棵子进程树：taskkill /T /F（强杀后代）是 Windows
// 上唯一可靠的树终结手段。
func killTree(pid int) {
	if pid <= 0 {
		return
	}
	_ = exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
}
