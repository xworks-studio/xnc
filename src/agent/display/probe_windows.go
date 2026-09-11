//go:build windows

// probe_windows.go — 控制台会话显示拓扑探针（2026-09-10 XIAOXIN 实测）。
//
// agent 是 session 0 服务：GDI/DXGI 枚举在服务会话恒空，"物理输出是否
// 在场"无法自测；且该机盒盖不产生任何可观测事件（无 lid 电源通知、
// 无 LIDACTION 设置项、桌面拓扑不变——外接屏在场时合盖对 Windows 完全
// 透明）。"无物理输出"触发（盒盖 ∧ 无外接 的等价场景）改由本文件在
// 活动控制台会话拉起 `xnc idd-probe`（CLI 隐藏命令）读取真实拓扑：
// 探针进程与 exec 同机制落会话 1，其 GDI 枚举即用户桌面视图。
//
// 失败（无控制台会话/探针超时）一律按"有物理输出"保守处理——不触发
// 建屏，宁可漏触发不可误建（桌面机带屏场景）。
package display

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// probeArgs 由同安装目录的 CLI 提供（与 idd-hold 同机制）。
	probeArgs = "idd-probe"
	// probeTimeout 探针进程整体上界（spawn + 枚举 + 退出）。
	probeTimeout = 3 * time.Second
)

// probeConsoleDisplays 在活动控制台会话运行显示拓扑探针。
func probeConsoleDisplays() (total int, physicalActive bool, ok bool) {
	session := windows.WTSGetActiveConsoleSessionId()
	if session == 0xFFFFFFFF {
		return 0, false, false
	}
	var token windows.Token
	if err := windows.WTSQueryUserToken(session, &token); err != nil {
		return 0, false, false
	}
	defer token.Close()

	var envBlock *uint16
	if err := windows.CreateEnvironmentBlock(&envBlock, token, false); err != nil {
		return 0, false, false
	}
	defer windows.DestroyEnvironmentBlock(envBlock)

	exe, err := os.Executable()
	if err != nil {
		return 0, false, false
	}
	exePath := filepath.Join(filepath.Dir(exe), "xnc.exe")
	exePtr, err := windows.UTF16PtrFromString(exePath)
	if err != nil {
		return 0, false, false
	}
	cmdLine, err := windows.UTF16PtrFromString(fmt.Sprintf("\"%s\" %s", exePath, probeArgs))
	if err != nil {
		return 0, false, false
	}

	// stdout 管道捕获探针 JSON；子进程退出后写端关闭 → EOF。
	var outR, outW windows.Handle
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		return 0, false, false
	}
	defer windows.CloseHandle(outR)
	if err := windows.SetHandleInformation(outR, windows.HANDLE_FLAG_INHERIT, 1); err != nil {
		windows.CloseHandle(outW)
		return 0, false, false
	}

	si := new(windows.StartupInfo)
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdOutput = outW
	si.StdErr = outW
	// 交互桌面（会话 1 的 winsta0\default）：GDI 枚举的 attached/active
	// 标志随调用进程所在桌面变化——服务窗口站上的探针读到的是与用户
	// 桌面不同的（陈旧）视图（2026-09-11 YOGA9 实测：交互探针 physical
	// false 而服务站探针仍 true）。探针只读不改，指定桌面无副作用
	// （与持有者的创建路径不同——那里指定桌面会破坏监视器模式查询）。
	si.Desktop, err = windows.UTF16PtrFromString(`winsta0\default`)
	if err != nil {
		windows.CloseHandle(outW)
		return 0, false, false
	}

	var pi windows.ProcessInformation
	if err := windows.CreateProcessAsUser(token, exePtr, cmdLine, nil, nil, true,
		windows.CREATE_NO_WINDOW|windows.CREATE_UNICODE_ENVIRONMENT, envBlock, nil, si, &pi); err != nil {
		windows.CloseHandle(outW)
		return 0, false, false
	}
	windows.CloseHandle(pi.Thread)
	windows.CloseHandle(outW) // 父进程关闭写端：子进程退出后读到 EOF

	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_, _ = windows.WaitForSingleObject(pi.Process, windows.INFINITE)
	}()
	select {
	case <-exited:
	case <-time.After(probeTimeout):
		_ = windows.TerminateProcess(pi.Process, 1)
		return 0, false, false
	}
	windows.CloseHandle(pi.Process)

	out, err := io.ReadAll(os.NewFile(uintptr(outR), "idd-probe-out"))
	if err != nil || len(out) == 0 {
		return 0, false, false
	}
	var res struct {
		Total          int  `json:"total"`
		PhysicalActive bool `json:"physicalActive"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return 0, false, false
	}
	return res.Total, res.PhysicalActive, true
}
