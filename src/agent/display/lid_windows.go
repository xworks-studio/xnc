//go:build windows

// lid_windows.go — 盒盖状态监听：RegisterPowerSettingNotification
// (GUID_LIDSWITCH_STATE_CHANGE) + 顶层隐藏窗口消息泵（服务进程无 UI
// 线程；参考 src/native/core/wts_monitor.cpp 的窗口先例）。
//
// 2026-09-10 XIAOXIN 实测踩过两个坑（都导致 lid 事件静默丢失）：
//  1. message-only 窗口（HWND_MESSAGE）收不到电源广播——WM_POWERBROADCAST
//     经 HWND_BROADCAST 投递，只达顶层窗口；改用不可见顶层窗口。
//  2. **线程亲和**：窗口消息按"创建窗口的线程"入队，GetMessage 只取
//     本线程队列。Go goroutine 不绑定 OS 线程——原实现窗口创建与消息泵
//     分处两个线程，事件全部落进无泵线程的队列。现以
//     runtime.LockOSThread 把注册类、建窗口、注册通知、消息泵全部锁在
//     同一 goroutine/OS 线程内完成。
//
// 状态以原子量暴露给 Manager 轮询；启动失败/每个事件全程有日志。
package display

import (
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
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
	deviceNotifyWindowHandle = 0
	csGblClass               = 0x0003
	wsOverlappedWindow       = 0
	// errorClassAlreadyExists：重试启动时类已在进程内注册（属正常）。
	errorClassAlreadyExists = 1410
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

// lidMonitor 消息泵线程（LockOSThread 固定）：lid 事件更新 closed/known
// 原子状态，并即时信号 events 通道（Manager 据此跳过轮询直接重评估）。
type lidMonitor struct {
	closed atomic.Bool
	known  atomic.Bool
	events chan struct{}

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

// lidNotifyChan 供 Manager 订阅 lid 事件（monitor 未启动 = nil，select
// 天然禁用该分支）。
func lidNotifyChan() <-chan struct{} {
	if m := lidSingleton.Load(); m != nil {
		return m.events
	}
	return nil
}

var (
	lidSingleton atomic.Pointer[lidMonitor]
	lidStartMu   sync.Mutex
)

// lidMonitorSingleton 懒启动 monitor（互斥守卫，单例）。启动全异步：
// 失败经 run() 日志可见——盒盖触发退化为仅靠无物理输出判定，功能不崩。
func lidMonitorSingleton() *lidMonitor {
	if m := lidSingleton.Load(); m != nil {
		return m
	}
	lidStartMu.Lock()
	defer lidStartMu.Unlock()
	if m := lidSingleton.Load(); m != nil {
		return m
	}
	m := startLidMonitor()
	lidSingleton.Store(m)
	return m
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
					closed := ps.data[0] == 0
					m.closed.Store(closed)
					m.known.Store(true)
					slog.Default().Info("display: lid event received", "closed", closed)
					// 非阻塞信号：Manager 即时重评估（不等 3s 轮询）。
					select {
					case m.events <- struct{}{}:
					default:
					}
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

// startLidMonitor 在专用 OS 线程上建窗口/注册通知并进入消息泵。全异步：
// 不等待 setup 结果（调用方可能持有 Manager 锁，同步等待 = 潜在死锁，
// 2026-09-10 停机挂起排查）；失败经 run() 内日志可见，lid 恒未知即症状。
func startLidMonitor() *lidMonitor {
	mon := &lidMonitor{events: make(chan struct{}, 1), done: make(chan struct{})}
	go func() {
		// 线程亲和：创建窗口、注册通知、GetMessage 泵必须同一 OS 线程
		// （窗口消息按创建线程入队；goroutine 不绑定线程）。
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := mon.run(); err != nil {
			slog.Default().Warn("display: lid monitor start failed", "err", err)
		}
		close(mon.done)
	}()
	return mon
}

// run 在锁定的 OS 线程上执行：注册类 → 建顶层隐藏窗口 → 注册电源通知
// → 消息泵（阻塞至 WM_QUIT）。setup 失败返回错误。
func (m *lidMonitor) run() error {
	className := windows.StringToUTF16("XncIddLidMonitor")

	var cls wndClassExW
	cls.cbSize = uint32(unsafe.Sizeof(wndClassExW{}))
	cls.style = csGblClass
	cls.lpfnWndProc = lidWndProc
	cls.hInstance, _, _ = procGetModuleHandleW.Call(0)
	cls.lpszClassName = &className[0]

	r1, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&cls)))
	if r1 == 0 && windows.GetLastError() != syscall.Errno(errorClassAlreadyExists) {
		return fmt.Errorf("display: lid monitor RegisterClassExW: %v", windows.GetLastError())
	}

	// 顶层隐藏窗口（非 HWND_MESSAGE）：电源广播按 HWND_BROADCAST 投递，
	// message-only 窗口不在广播名单（2026-09-10 实测事件静默丢失）。
	hwnd, _, _ := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(&className[0])), 0, wsOverlappedWindow,
		0, 0, 0, 0, 0, 0, uintptr(cls.hInstance), 0)
	if hwnd == 0 {
		return fmt.Errorf("display: lid monitor CreateWindowExW: %v", windows.GetLastError())
	}

	notify, _, _ := procRegisterPowerSettingNotification.Call(
		hwnd, uintptr(unsafe.Pointer(&guidLidSwitchStateChange)),
		deviceNotifyWindowHandle)
	if notify == 0 {
		procDestroyWindow.Call(hwnd)
		return fmt.Errorf("display: lid monitor RegisterPowerSettingNotification: %v", windows.GetLastError())
	}

	m.hwnd = hwnd
	m.notify = notify
	slog.Default().Info("display: lid monitor started", "hwnd", hwnd)

	var mq msg
	for {
		r1, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&mq)), 0, 0, 0)
		if int32(r1) <= 0 { // 0 = WM_QUIT，-1 = 错误
			break
		}
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&mq)))
	}
	return nil
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
