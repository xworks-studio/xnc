//go:build windows

// ctxdiag_windows.go — 启动上下文诊断（会话/关键环境），排查服务桥接与
// 交互直跑的差异。生产保留：screen-helper.log 首行即上下文快照。
package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func logLaunchContext() {
	var sid uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &sid); err != nil {
		sid = 0xFFFFFFFF
	}
	fmt.Fprintf(os.Stderr, "xnc-screen-helper: ctx session=%d sessionname=%q userprofile=%q",
		sid, os.Getenv("SESSIONNAME"), os.Getenv("USERPROFILE"))
}
