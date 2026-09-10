//go:build windows

// idd_windows.go — Windows 后端：设备接口 IOCTL 热插拔 + 显示器枚举 +
// 驱动包检测。软件设备创建经会话内持有进程（holder_windows.go——
// 会话亲和修复，2026-09-10）；协议逐字节镜像 vendored
// src/third_party/xncidd（Public.h / IddController.c，2026-09-10）。
package display

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	modSetupAPI = windows.NewLazySystemDLL("setupapi.dll")
	modUser32   = windows.NewLazySystemDLL("user32.dll")

	procSetupDiGetClassDevsW             = modSetupAPI.NewProc("SetupDiGetClassDevsW")
	procSetupDiEnumDeviceInterfaces      = modSetupAPI.NewProc("SetupDiEnumDeviceInterfaces")
	procSetupDiGetDeviceInterfaceDetailW = modSetupAPI.NewProc("SetupDiGetDeviceInterfaceDetailW")
	procSetupDiDestroyDeviceInfoList     = modSetupAPI.NewProc("SetupDiDestroyDeviceInfoList")
	procEnumDisplayDevicesW              = modUser32.NewProc("EnumDisplayDevicesW")
)

// guidDevInterfaceXncIdd 设备接口 GUID（XNC 生成，见 XncIddDriver/Driver.cpp）。
var guidDevInterfaceXncIdd = windows.GUID{
	Data1: 0x0b6910e4, Data2: 0xb09f, Data3: 0x48a9,
	Data4: [8]byte{0x85, 0x13, 0x66, 0x28, 0x41, 0x5e, 0x1b, 0x45},
}

// IOCTL 码 = CTL_CODE(IOCTL_CHANGER_BASE=0x30, fn, METHOD_BUFFERED,
// FILE_READ_ACCESS|FILE_WRITE_ACCESS)。注：SDK 的 CTL_CODE 把 Function
// 截断到 12 位（0x1001→1）——实测驱动接收码如下（C 探针编译 Public.h
// 打印验证，2026-09-10）。
const (
	ioctlPlugIn    = 0x0030C004
	ioctlPlugOut   = 0x0030C008
	ioctlGetStatus = 0x0030C010 // 0x1004（XNC 增补）
)

const (
	digcfPresent            = 0x02
	digcfDeviceInterface    = 0x10
	errorInsufficientBuffer = syscall.Errno(122)

	displayDeviceActive    = 0x1
	displayDeviceMirroring = 0x8

	// virtualDisplayName 与驱动 EDID 的显示器名一致（镜像 CLI 侧
	// swDeviceCreate 的 deviceDescription），用于 GDI 枚举识别虚拟屏。
	virtualDisplayName = "XWorks XNC Virtual Display"

	// 调试旋钮（契约 4 的 XNC_* 惯例）：注册表环境，值 open|closed。
	forceLidValue = "XNC_IDD_FORCE_LID"
	envRegKey     = `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`
)

// ---- 设备接口枚举 + CreateFile（镜像 IddController.c GetDevicePath2）----

type spDeviceInterfaceData struct {
	cbSize             uint32
	interfaceClassGUID windows.GUID
	flags              uint32
	reserved           uintptr
}

type spDeviceInterfaceDetailData struct {
	cbSize     uint32
	devicePath [1]uint16
}

// openDeviceInterface 枚举 GUID 设备接口并打开（share=0）。
func openDeviceInterface() (windows.Handle, error) {
	info, _, e1 := procSetupDiGetClassDevsW.Call(
		uintptr(unsafe.Pointer(&guidDevInterfaceXncIdd)),
		0, 0,
		digcfPresent|digcfDeviceInterface,
	)
	if info == 0 || info == ^uintptr(0) {
		return 0, fmt.Errorf("display: SetupDiGetClassDevs failed (%v)", e1)
	}
	defer procSetupDiDestroyDeviceInfoList.Call(info)

	data := spDeviceInterfaceData{cbSize: uint32(unsafe.Sizeof(spDeviceInterfaceData{}))}
	r1, _, e1 := procSetupDiEnumDeviceInterfaces.Call(
		info, 0, uintptr(unsafe.Pointer(&guidDevInterfaceXncIdd)), 0,
		uintptr(unsafe.Pointer(&data)),
	)
	if r1 == 0 {
		return 0, fmt.Errorf("display: no XncIdd device interface present (%v)", e1)
	}

	var req uint32
	r1, _, e1 = procSetupDiGetDeviceInterfaceDetailW.Call(
		info, uintptr(unsafe.Pointer(&data)), 0, 0,
		uintptr(unsafe.Pointer(&req)), 0,
	)
	if r1 != 0 || req == 0 || e1 != errorInsufficientBuffer {
		return 0, fmt.Errorf("display: SetupDiGetDeviceInterfaceDetail probe failed (r=%d err=%v)", r1, e1)
	}

	buf := make([]byte, req)
	detail := (*spDeviceInterfaceDetailData)(unsafe.Pointer(&buf[0]))
	detail.cbSize = uint32(unsafe.Sizeof(spDeviceInterfaceDetailData{}))
	r1, _, e1 = procSetupDiGetDeviceInterfaceDetailW.Call(
		info, uintptr(unsafe.Pointer(&data)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(req),
		uintptr(unsafe.Pointer(&req)), 0,
	)
	if r1 == 0 {
		return 0, fmt.Errorf("display: SetupDiGetDeviceInterfaceDetail failed (%v)", e1)
	}
	path := windows.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(&buf[unsafe.Offsetof(spDeviceInterfaceDetailData{}.devicePath)])), (int(req)-int(unsafe.Offsetof(spDeviceInterfaceDetailData{}.devicePath)))/2))
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(path),
		windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return 0, fmt.Errorf("display: open device interface: %w", err)
	}
	return h, nil
}

// ---- IOCTL 热插拔（payload 镜像 Public.h CtlPlugIn/CtlPlugOut）----

type ctlPlugIn struct {
	connectorIndex uint32
	monitorEDID    uint32
	containerID    windows.GUID
}

type ctlPlugOut struct {
	connectorIndex uint32
}

// ctlMonitorStatus 镜像 Public.h CtlMonitorStatus（C BOOL = 4 字节；
// C sizeof=84，逐字节对齐）。
type ctlMonitorStatus struct {
	count uint32
	conns [10]struct {
		plugged uint32
		active  uint32
	}
}

func randomGUID() windows.GUID {
	var g windows.GUID
	b := make([]byte, 16)
	if _, err := rand.Read(b); err == nil {
		copy(g.Data4[:], b[8:])
		g.Data1 = uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
		g.Data2 = uint16(b[4]) | uint16(b[5])<<8
		g.Data3 = uint16(b[6]) | uint16(b[7])<<8
		g.Data3 = (g.Data3 & 0x0FFF) | 0x4000 // v4
		g.Data4[0] = (g.Data4[0] & 0x3F) | 0x80
	}
	return g
}

func ioctlPlugInMonitor(h uintptr) error {
	in := ctlPlugIn{connectorIndex: 0, monitorEDID: 0, containerID: randomGUID()}
	var junk uint32
	err := windows.DeviceIoControl(windows.Handle(h), ioctlPlugIn,
		(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
		nil, 0, &junk, nil)
	if err != nil {
		return fmt.Errorf("display: PLUG_IN ioctl: %w", err)
	}
	return nil
}

func ioctlPlugOutMonitor(h uintptr) error {
	in := ctlPlugOut{connectorIndex: 0}
	var junk uint32
	err := windows.DeviceIoControl(windows.Handle(h), ioctlPlugOut,
		(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
		nil, 0, &junk, nil)
	if err != nil {
		return fmt.Errorf("display: PLUG_OUT ioctl: %w", err)
	}
	return nil
}

// ioctlGetMonitorStatus 查询驱动状态（session 0 唯一权威信号：GDI/DXGI
// 枚举在服务会话均不可用）。返回 0 号连接器的插拔/激活状态。
func ioctlGetMonitorStatus() (plugged, active bool, err error) {
	dev, err := openDeviceInterface()
	if err != nil {
		return false, false, err
	}
	defer windows.CloseHandle(dev)
	out := ctlMonitorStatus{}
	var junk uint32
	if err := windows.DeviceIoControl(dev, ioctlGetStatus,
		nil, 0, (*byte)(unsafe.Pointer(&out)), uint32(unsafe.Sizeof(out)),
		&junk, nil); err != nil {
		return false, false, fmt.Errorf("display: GET_STATUS ioctl: %w", err)
	}
	if out.count == 0 || out.count > 10 {
		return false, false, fmt.Errorf("display: GET_STATUS bad count %d", out.count)
	}
	return out.conns[0].plugged != 0, out.conns[0].active != 0, nil
}

// ---- 显示器枚举（EnumDisplayDevicesW）----

type displayDevice struct {
	cb    uint32
	name  [32]uint16
	str   [128]uint16
	flags uint32
	id    [128]uint16
	key   [128]uint16
}

// enumDisplays 枚举显示设备（GDI）。注意：session 0（服务）下该 API 恒
// 返回空（2026-09-10 XIAOXIN 实测）——仅在交互上下文（dev-console/控制台
// 会话）有真实语义；调用方须按"空 = 未知"处理。
func enumDisplays() (total int, physicalActive bool, virtualActive bool) {
	for i := uint32(0); ; i++ {
		dd := displayDevice{cb: uint32(unsafe.Sizeof(displayDevice{}))}
		r1, _, _ := procEnumDisplayDevicesW.Call(0, uintptr(i),
			uintptr(unsafe.Pointer(&dd)), 0)
		if r1 == 0 {
			return
		}
		total++
		if dd.flags&displayDeviceActive == 0 {
			continue
		}
		if windows.UTF16ToString(dd.str[:]) == virtualDisplayName {
			virtualActive = true
			continue
		}
		if dd.flags&displayDeviceMirroring != 0 {
			continue
		}
		physicalActive = true
	}
}

// ---- 驱动包 store 检测 ----

// driverPackageInStore 驱动包是否已入 store。pnputil 将包落于
// DriverStore\FileRepository，目录名 <inf>_<arch>_<hash>（实测不进
// DriverPackages 注册表键；文件系统判据语言无关、跨版本稳定）。
func driverPackageInStore() bool {
	repo := filepath.Join(os.Getenv("SystemRoot"), "System32", "DriverStore", "FileRepository")
	entries, err := os.ReadDir(repo)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasPrefix(strings.ToLower(e.Name()), "xncidd") {
			return true
		}
	}
	return false
}

// forceLidFromRegistry 读调试旋钮（直接读注册表环境，不经进程环境缓存）。
func forceLidFromRegistry() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, envRegKey, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue(forceLidValue)
	if err != nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "open":
		return "open"
	case "closed":
		return "closed"
	}
	return ""
}

// ---- backend 装配 ----

// winBackend 持有设备持有进程的 stdin 管道写端（单设备：Manager 在
// 互斥锁内串行调用，无并发访问）。
type winBackend struct {
	stdinWrite windows.Handle
}

func newBackend() backend { return &winBackend{} }

func (b *winBackend) driverInstalled() bool { return driverPackageInStore() }

// startDevice 会话内创建软件设备（持有进程，见 holder_windows.go）：
// 返回持有进程句柄；无控制台会话/超时返回错误（可重试）。
func (b *winBackend) startDevice() (uintptr, error) {
	proc, stdinW, err := startHolder()
	if err != nil {
		return 0, err
	}
	b.stdinWrite = stdinW
	return uintptr(proc), nil
}

func (b *winBackend) stopDevice(h uintptr) {
	if b.stdinWrite != 0 {
		stopHolder(windows.Handle(h), b.stdinWrite)
		b.stdinWrite = 0
	}
}

func (winBackend) plugMonitor(h uintptr) error {
	// 接口就绪等待：200ms×10 快速重试 + 1s×5 兜底（会话已预创建设备，
	// 触发插屏时设备通常已就绪，接口在 1s 内可达；新驱动包首次加载的
	// 慢路径由兜底覆盖）。
	var lastErr error
	for i := 0; i < 15; i++ {
		dev, err := openDeviceInterface()
		if err == nil {
			defer windows.CloseHandle(dev)
			return ioctlPlugInMonitor(uintptr(dev))
		}
		lastErr = err
		if i < 10 {
			time.Sleep(200 * time.Millisecond)
		} else {
			time.Sleep(time.Second)
		}
	}
	return lastErr
}

func (winBackend) unplugMonitor(h uintptr) error {
	dev, err := openDeviceInterface()
	if err != nil {
		return err
	}
	defer windows.CloseHandle(dev)
	return ioctlPlugOutMonitor(uintptr(dev))
}

func (winBackend) physicalOutputActive() bool {
	total, p, _ := enumDisplays()
	if total == 0 {
		// session 0 下枚举恒空 = "未知"，按有物理屏保守处理（不触发建屏）：
		// 避免桌面机带屏时每次会话误建虚拟屏；盒盖场景由 lid 事件覆盖。
		return true
	}
	return p
}

func (winBackend) virtualDisplayActive() bool {
	plugged, active, err := ioctlGetMonitorStatus()
	if err != nil {
		// 设备未创建或驱动为旧版（无 GET_STATUS）：回落 GDI（仅交互
		// 上下文有意义；session 0 下返回 false 即"未激活"）。
		_, _, v := enumDisplays()
		return v
	}
	return plugged && active
}

func (winBackend) lidClosed() (bool, bool) { return lidClosedState() }

func (winBackend) forceLid() string { return forceLidFromRegistry() }
