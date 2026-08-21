//go:build windows

// dpi_windows.go — helper 启动时声明 per-monitor DPI 感知。否则高 DPI 缩放
// 桌面（如 3200×2000 @200%）上 GDI GetSystemMetrics 返回虚拟化逻辑分辨率
// （1600×1000），BitBlt 得到经系统降采样的模糊帧。声明感知后 GDI 按物理
// 分辨率捕获，画质与 DXGI 一致。
package main

var procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
var procSetProcessDPIAware = user32.NewProc("SetProcessDPIAware")

// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = (HANDLE)-4。
const dpiAwarePerMonitorV2 = ^uintptr(3)

// setDPIAware 尽力声明 DPI 感知（Win10 1703+ 用 V2 上下文，失败回退经典
// SetProcessDPIAware）。重复调用或已声明时无害（返回失败仅意味着未变更）。
func setDPIAware() {
	if r, _, _ := procSetProcessDpiAwarenessContext.Call(dpiAwarePerMonitorV2); r != 0 {
		return
	}
	_, _, _ = procSetProcessDPIAware.Call()
}
