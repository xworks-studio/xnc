//go:build !windows

// interactive_other.go — 非 Windows 桩:交互模式仅 Windows(ConPTY)。
package main

import (
	"errors"
	"net"
)

func runInteractive(conn net.Conn, o *serverOpts) error {
	defer conn.Close()
	return errors.New("shellhost: interactive mode requires windows (ConPTY)")
}
