//go:build !windows

package main

import (
	"fmt"
	"os/exec"
	"strconv"
)

// rdpClientCmd 非 Windows：PATH 上的 xfreerdp（freerdp2-x11 包）；缺失时
// 报可行动错误（隧道仍可自行用任意 RDP 客户端连 127.0.0.1:<port>）。
func rdpClientCmd(port int) (*exec.Cmd, error) {
	if p, err := exec.LookPath("xfreerdp"); err == nil {
		return exec.Command(p, "/v:127.0.0.1:"+strconv.Itoa(port)), nil
	}
	return nil, fmt.Errorf("no RDP client found: install freerdp2-x11 (xfreerdp), or connect your client to 127.0.0.1:%d", port)
}
