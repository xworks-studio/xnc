//go:build !windows

// conpty_other.go — 非 Windows 的 conPTY 桩：startConPTY 一律失败（shell 走
// failStart 提前返回），字段集与 windows 版保持一致以便 shell.go 双平台编译。
// Linux pty 是 Phase 8 议题。
package session

import (
	"errors"
	"io"
)

type conPTY struct {
	pid  int
	inW  io.WriteCloser
	outR io.ReadCloser
}

func startConPTY(cols, rows int, exe string, args ...string) (*conPTY, error) {
	return nil, errors.New("conpty: windows only (linux pty arrives in Phase 8)")
}

func (p *conPTY) Resize(cols, rows int) error { return nil }
func (p *conPTY) Wait() error                 { return nil }
func (p *conPTY) KillAndClose()               {}
