// dpi_windows.go — 进程 Per-Monitor V2 DPI 感知（启动即设）。
//
// 动机：安全桌面 GDI 路径的 GetSystemMetrics/BitBlt 在 DPI-unaware 进程
// 里拿到虚拟化坐标（2880x1800 → 1440x900，画面减半）。DDA 的 dup-desc
// 尺寸不受影响（原生），故全局设置无副作用。
//
//go:build windows

package main

import "unsafe"

var procSetProcessDpiAwarenessContext = user32DLL.NewProc("SetProcessDpiAwarenessContext")

func init() {
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = (HANDLE)-4；失败（已设/
	// 旧系统）无害。
	procSetProcessDpiAwarenessContext.Call(^uintptr(3))
	_ = unsafe.Pointer(nil)
}
