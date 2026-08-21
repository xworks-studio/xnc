// desktop_windows.go — 输入桌面探测（安全桌面检测）。
//
// UAC/Ctrl+Alt+Del 激活安全桌面（winlogon desktop）期间：DXGI 重建被拒
// （E_ACCESSDENIED，实测 SYSTEM 亦然），画面不可得——此时观众应看到
// locked 暂停态而非冻结的旧帧。用户令牌打不开 winlogon 桌面，OpenInput-
// Desktop 失败本身即"安全桌面激活"的信号。
//
//go:build windows

package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procOpenInputDesk  = user32DLL.NewProc("OpenInputDesktop")
	procGetUserObjInfo = user32DLL.NewProc("GetUserObjectInformationW")
	procCloseDesktop   = user32DLL.NewProc("CloseDesktop")
)

// secureDesktopActive 报告输入桌面是否非常规 Default（安全桌面）。
// 用户令牌下 OpenInputDesktop 失败（打不开 winlogon 桌面）视为激活。
func secureDesktopActive() bool {
	h, _, _ := procOpenInputDesk.Call(0, 0, 0x00020000 /*READ_CONTROL*/)
	if h == 0 {
		return true
	}
	defer procCloseDesktop.Call(h)
	// UOI_NAME=2 取名字（先探长度再取内容；注意传 &b[0] 而非切片头地址）。
	var need uint32
	procGetUserObjInfo.Call(h, 2, 0, 0, uintptr(unsafe.Pointer(&need)))
	if need == 0 || need > 512 {
		return false // 读不到名字：按非激活处理（保守）
	}
	b := make([]uint16, (need+1)/2+1)
	var got uint32
	if r, _, _ := procGetUserObjInfo.Call(h, 2, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)*2), uintptr(unsafe.Pointer(&got))); r == 0 {
		return false
	}
	return windows.UTF16ToString(b) != "Default"
}
