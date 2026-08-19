//go:build !windows

package machineinfo

import (
	"os"
	"runtime"
)

func machineID() string {
	h, _ := os.Hostname()
	return "nonwindows-" + h
}

func osVersion() string { return runtime.GOOS }
