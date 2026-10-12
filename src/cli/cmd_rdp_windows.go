//go:build windows

package main

import (
	"os/exec"
	"strconv"
)

// rdpClientCmd Windows：mstsc 指向本地转发端口（恒可用）。
func rdpClientCmd(port int) (*exec.Cmd, error) {
	return exec.Command("mstsc", "/v:127.0.0.1:"+strconv.Itoa(port)), nil
}
