//go:build !windows

// pipe_other.go — 非 Windows 桩:xnc-shell 仅部署于 Windows(core spawn)。
package main

import (
	"errors"
	"net"
)

func listenNetPipe(pipeName, sddlOverride string) (net.Listener, error) {
	return nil, errors.New("shellhost: named pipe requires windows")
}
