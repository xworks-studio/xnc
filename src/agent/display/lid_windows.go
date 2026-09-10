//go:build windows

// lid_windows.go — 盒盖状态监听：RegisterPowerSettingNotification
// (GUID_LIDSWITCH_STATE_CHANGE) + 自建 message-only 窗口消息泵线程
// （服务进程无 UI 线程；参考 src/native/core/wts_monitor.cpp 的
// message-only 窗口先例）。状态以原子量暴露给 Manager 轮询。
package display

import (
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procRegisterPowerSettingNotification   = modUser32.NewProc("RegisterPowerSettingNotification")
	procUnregisterPowerSettingNotification = modUser32.NewProc("UnregisterPowerSettingNotification")
	procRegisterClassExW                   = modUser32.NewProc("RegisterClassExW")
	procCreateWindowExW                    = modUser32.NewProc("CreateWindowExW")
	procDefWindowProcW                     = modUser32.NewProc("DefWindowProcW")
	procGetMessageW                        = modUser32.NewProc("GetMessageW")
	procDispatchMessageW                   = modUser32.NewProc("DispatchMessageW")
	procDestroyWindow                      = modUser32.NewProc("DestroyWindow")
	procPostMessageW                       = modUser32.NewProc("PostMessageW")
	procPostQuitMessage                    = modUser32.NewProc("PostQuitMessage")
	procGetModuleHandleW                   = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetModuleHandleW")
)

// guidLidSwitchStateChange {BA3E0F4D-B817-4094-A2D1-D56379C6A0F3}。
var guidLidSwitchStateChange = windows.GUID{
	Data1: 0xBA3E0F4D, Data2: 0xB817, Data3: 0x4094,
	Data4: [8]byte{0xA2, 0xD1, 0xD5, 0x63, 0x79, 0xC6, 0xA0, 0xF3},
}

const (
	wmPowerBroadcast         = 0x0218
	pbtPowerSettingChange    = 0x8013
	wmClose                  = 0x0010
	wmDestroy                = 0x0002
	hwndMessage              = ^uintptr(3) // HWND_MESSAGE = -3
	deviceNotifyWindowHandle = 0
	csGblClass               = 0x0003
	wsOverlappedWindow       = 0
)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

type powerBroadcastSetting struct {
	powerSetting windows.GUID
	dataLength   uint32
	data         [1]byte
}

// lidMonitor 消息泵线程：lid 事件更新 closed/known 原子状态。
type lidMonitor struct {
	closed atomic.Bool
	known  atomic.Bool

	class  [64]uint16
	hwnd   uintptr
	notify uintptr
	done   chan struct{}
}

// lidClosedState 供 backend 查询（进程级单例 monitor）。
func lidClosedState() (closed, known bool) {
	m := lidMonitorSingleton()
	if m == nil {
		return false, false
	}
	return m.closed.Load(), m.known.Load()
}

var lidSingleton atomic.Pointer[lidMonitor]

// lidMonitorSingleton 懒启动 monitor；失败（无窗口环境等极端情况）返回
// nil——盒盖触发退化为仅靠无物理输出判定，功能不崩。
func lidMonitorSingleton() *lidMonitor {
	if m := lidSingleton.Load(); m != nil {
		return m
	}
	m := startLidMonitor()
	if m != nil {
		if lidSingleton.CompareAndSwap(nil, m) {
			return m
		}
		m.stop()
	}
	return lidSingleton.Load()
}

var lidWndProc = windows.NewCallback(func(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmPowerBroadcast:
		if wParam == pbtPowerSettingChange && lParam != 0 {
			// Windows 以 lParam 传递 POWERBROADCAST_SETTING 指针（uintptr
			// 直转 Pointer 会被 vet 拦，经 unsafe.Add 自类型化 nil 偏移）。
			ps := (*powerBroadcastSetting)(unsafe.Add(
				unsafe.Pointer((*powerBroadcastSetting)(nil)), lParam))
			if ps.powerSetting == guidLidSwitchStateChange && ps.dataLength >= 1 {
				m := lidSingleton.Load()
				if m != nil {
					// data[0]：0 = 合盖，1 = 开盖。
					m.closed.Store(ps.data[0] == 0)
					m.known.Store(true)
				}
			}
		}
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r1, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r1
})

func startLidMonitor() *lidMonitor {
	className := windows.StringToUTF16("XncIddLidMonitor")

	var cls wndClassExW
	cls.cbSize = uint32(unsafe.Sizeof(wndClassExW{}))
	cls.style = csGblClass
	cls.lpfnWndProc = lidWndProc
	cls.hInstance, _, _ = procGetModuleHandleW.Call(0)
	cls.lpszClassName = &className[0]

	if r1, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&cls))); r1 == 0 {
		return nil
	}

	hwnd, _, _ := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(&className[0])), 0, wsOverlappedWindow,
		0, 0, 0, 0, hwndMessage, 0, uintptr(cls.hInstance), 0)
	if hwnd == 0 {
		return nil
	}

	notify, _, _ := procRegisterPowerSettingNotification.Call(
		hwnd, uintptr(unsafe.Pointer(&guidLidSwitchStateChange)),
		deviceNotifyWindowHandle)
	if notify == 0 {
		procDestroyWindow.Call(hwnd)
		return nil
	}

	mon := &lidMonitor{hwnd: hwnd, notify: notify, done: make(chan struct{})}
	go func() {
		var mq msg
		for {
			r1, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&mq)), 0, 0, 0)
			if int32(r1) <= 0 { // 0 = WM_QUIT，-1 = 错误
				break
			}
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&mq)))
		}
		close(mon.done)
	}()
	return mon
}

// stop 收线：反注册通知 → 发 WM_CLOSE 让泵线程自毁窗口（跨线程
// DestroyWindow 非法，必须走窗口线程）。
func (m *lidMonitor) stop() {
	if m.notify != 0 {
		procUnregisterPowerSettingNotification.Call(m.notify)
		m.notify = 0
	}
	if m.hwnd != 0 {
		procPostMessageW.Call(m.hwnd, wmClose, 0, 0)
		m.hwnd = 0
	}
	<-m.done
}
