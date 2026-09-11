//go:build windows

// cmd_idd_hold.go — 隐藏命令 `xnc idd-hold`：IDD 虚拟显示器软件设备的
// 创建与持有者（agent 在活动控制台会话拉起本命令，见
// src/agent/display/holder_windows.go）。
//
// 会话亲和背景（2026-09-10 XIAOXIN 实测修复）：SwDeviceCreate 由哪个
// 会话的进程调用，UMDF 驱动宿主（WUDFHost）就落在哪个会话，IddCx
// 适配器随之绑定该会话。agent 是 SYSTEM 服务（会话 0），直接创建设备
// 会把虚拟屏挂到会话 0 的不可见显示上——插屏成功、驱动激活，但用户
// 会话里永远看不到。因此设备创建+句柄持有移到这里；插/拔屏 IOCTL 仍
// 由 agent 经全局设备接口执行（openDeviceInterface，会话无关）。
//
// 生命周期：agent 持 stdin 管道写端，本进程阻塞读 stdin；agent 退出或
// 崩溃 → 写端关闭 → 读到 EOF → 关闭设备句柄 → 设备被 PnP 移除（等价
// 于 SwDeviceCreate 的 Handle 生命周期，agent 崩溃不留残影）。
package main

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"github.com/spf13/cobra"

	"golang.org/x/sys/windows"
)

const (
	// 设备标识：与驱动 INF 一致（硬件 ID XncIdd，显示名
	// "XWorks XNC Virtual Display"，见 src/third_party/xncidd）。
	hwIDXncIdd        = "XncIdd"
	deviceDescription = "XWorks XNC Virtual Display"
	// SWDeviceCapabilities（swdevicedef.h 真值 0x01/0x02/0x08；含未定义位
	// 时 SwDeviceCreate 返回 E_INVALIDARG——2026-09-10 XIAOXIN 实测踩过）。
	swDeviceCapRemovable      = 0x01
	swDeviceCapSilentInstall  = 0x02
	swDeviceCapDriverRequired = 0x08
)

// SwDevice* 在现代 Windows 由 CFGMGR32 导出（System32 无实体
// swdevice.dll——SDK 的 swdevice.lib 也转发到 cfgmgr32，2026-09-10
// 实测 dumpbin）。老 Win10 上实体 swdevice.dll 仍导出同名函数：
// cfgmgr32 优先，缺则回落裸名 swdevice.dll（api-set 重定向）。
var (
	modCfgMgr32 = windows.NewLazySystemDLL("cfgmgr32.dll")
	modSwDevice = windows.NewLazyDLL("swdevice.dll")

	procSwDeviceCreate = findSwDeviceProc("SwDeviceCreate")
	procSwDeviceClose  = findSwDeviceProc("SwDeviceClose")
)

// findSwDeviceProc 解析 SwDevice* 符号：cfgmgr32 → swdevice.dll 回落。
func findSwDeviceProc(name string) *windows.LazyProc {
	if p := modCfgMgr32.NewProc(name); p.Find() == nil {
		return p
	}
	return modSwDevice.NewProc(name)
}

// ---- SwDeviceCreate（异步 + 回调，镜像 vendor IddController.c DeviceCreate）----

type swDeviceCreateInfo struct {
	cbSize       uint32
	_            uint32
	instanceID   *uint16
	hwIDs        *uint16
	compIDs      *uint16
	containerID  *windows.GUID // 恒 NULL；布局对齐 swdevicedef.h
	capFlags     uint32        // ULONG（header 语义；字节版实测触 E_INVALIDARG）
	description  *uint16
	location     *uint16
	securityDesc *windows.GUID // 恒 NULL；布局对齐 swdevicedef.h
}

type createCallbackCtx struct {
	event     windows.Handle
	hSwDevice uintptr
	hr        uintptr
}

// createCtx 是 SwDeviceCreate 回调上下文（进程内单次创建，无并发）。
var createCtx createCallbackCtx

var createCallback = windows.NewCallback(func(hSwDevice, hr, _, _ uintptr) uintptr {
	createCtx.hSwDevice = hSwDevice
	createCtx.hr = hr
	windows.SetEvent(createCtx.event)
	return 0
})

// swDeviceCreate 创建软件设备（Handle 生命周期）。返回的句柄由调用方
// 持至关闭；句柄关闭设备即被 PnP 移除。
func swDeviceCreate() (uintptr, error) {
	instanceID, err := windows.UTF16PtrFromString(hwIDXncIdd)
	if err != nil {
		return 0, err
	}
	// multi-sz（单条目 + 双 NUL）：UTF16PtrFromString 拒绝内嵌 NUL，
	// 故从干净串构造后手动补终止符。
	ids, err := windows.UTF16FromString(hwIDXncIdd)
	if err != nil {
		return 0, err
	}
	ids = append(ids, 0)
	hwIDs := &ids[0]
	desc, err := windows.UTF16PtrFromString(deviceDescription)
	if err != nil {
		return 0, err
	}
	enumName, _ := windows.UTF16PtrFromString(hwIDXncIdd)
	parent, _ := windows.UTF16PtrFromString(`HTREE\ROOT\0`)

	info := swDeviceCreateInfo{
		cbSize:      uint32(unsafe.Sizeof(swDeviceCreateInfo{})),
		instanceID:  instanceID,
		hwIDs:       hwIDs,
		compIDs:     hwIDs,
		capFlags:    swDeviceCapRemovable | swDeviceCapSilentInstall | swDeviceCapDriverRequired,
		description: desc,
	}

	event, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(event)
	createCtx = createCallbackCtx{event: event}

	var h uintptr
	r1, _, e1 := procSwDeviceCreate.Call(
		uintptr(unsafe.Pointer(enumName)),
		uintptr(unsafe.Pointer(parent)),
		uintptr(unsafe.Pointer(&info)),
		0, 0,
		createCallback,
		// pContext：C 参考实现传非空上下文（cfgmgr32 实测对 NULL 报
		// E_INVALIDARG）；回调侧忽略该值，传本 package 上下文地址即可。
		uintptr(unsafe.Pointer(&createCtx)),
		uintptr(unsafe.Pointer(&h)),
	)
	if r1 != 0 { // HRESULT != S_OK
		return 0, fmt.Errorf("idd-hold: SwDeviceCreate failed 0x%x (%v)", uint32(r1), e1)
	}
	res, err := windows.WaitForSingleObject(event, 10000)
	if err != nil || res != windows.WAIT_OBJECT_0 {
		return 0, fmt.Errorf("idd-hold: SwDeviceCreate callback wait failed (res=%d err=%v)", res, err)
	}
	if createCtx.hr != 0 {
		return 0, fmt.Errorf("idd-hold: SwDeviceCreate device creation failed 0x%x", uint32(createCtx.hr))
	}
	return createCtx.hSwDevice, nil
}

func swDeviceCloseHandle(h uintptr) {
	procSwDeviceClose.Call(h)
}

// ---- 设备接口 + IOCTL 热插拔（会话亲和关键，见下方 runIddHold）----
//
// 2026-09-11 YOGA9 驱动追踪定位：IddCx 把监视器路由到**触发
// IddCxMonitorArrival 的 IOCTL 调用者所在会话**。agent（会话 0）直接
// 发 PLUG_IN → 监视器到达会话 0 的显示路径（CommitModes ACTIVE 发生、
// 但会话 0 无 DWM → swapchain 永不分配、虚拟屏永不出现）。因此插/拔
// IOCTL 必须由本进程（会话 1）执行——agent 经 stdin 管道发命令。

// guidDevInterfaceXncIdd 设备接口 GUID（镜像 agent/display 同值）。
var guidDevInterfaceXncIdd = windows.GUID{
	Data1: 0x0b6910e4, Data2: 0xb09f, Data3: 0x48a9,
	Data4: [8]byte{0x85, 0x13, 0x66, 0x28, 0x41, 0x5e, 0x1b, 0x45},
}

// IOCTL 码（CTL_CODE(0x30, fn, METHOD_BUFFERED, RW)，Function 12 位截断后
// 的实测值，见 agent/display/idd_windows.go）。
const (
	iddIoctlPlugIn  = 0x0030C004
	iddIoctlPlugOut = 0x0030C008
)

const (
	iddDigcfPresent         = 0x02
	iddDigcfDeviceInterface = 0x10
	iddErrorInsufficientBuf = 122
)

var (
	modSetupAPI = windows.NewLazySystemDLL("setupapi.dll")

	procSetupDiGetClassDevsW             = modSetupAPI.NewProc("SetupDiGetClassDevsW")
	procSetupDiEnumDeviceInterfaces      = modSetupAPI.NewProc("SetupDiEnumDeviceInterfaces")
	procSetupDiGetDeviceInterfaceDetailW = modSetupAPI.NewProc("SetupDiGetDeviceInterfaceDetailW")
	procSetupDiDestroyDeviceInfoList     = modSetupAPI.NewProc("SetupDiDestroyDeviceInfoList")
)

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

type ctlPlugIn struct {
	connectorIndex uint32
	monitorEDID    uint32
	containerID    windows.GUID
}

type ctlPlugOut struct {
	connectorIndex uint32
}

// openDeviceInterface 枚举 GUID 设备接口并打开（share=0）。
func openDeviceInterface() (windows.Handle, error) {
	info, _, e1 := procSetupDiGetClassDevsW.Call(
		uintptr(unsafe.Pointer(&guidDevInterfaceXncIdd)),
		0, 0,
		iddDigcfPresent|iddDigcfDeviceInterface,
	)
	if info == 0 || info == ^uintptr(0) {
		return 0, fmt.Errorf("idd-hold: SetupDiGetClassDevs failed (%v)", e1)
	}
	defer procSetupDiDestroyDeviceInfoList.Call(info)

	data := spDeviceInterfaceData{cbSize: uint32(unsafe.Sizeof(spDeviceInterfaceData{}))}
	r1, _, e1 := procSetupDiEnumDeviceInterfaces.Call(
		info, 0, uintptr(unsafe.Pointer(&guidDevInterfaceXncIdd)), 0,
		uintptr(unsafe.Pointer(&data)),
	)
	if r1 == 0 {
		return 0, fmt.Errorf("idd-hold: no XncIdd device interface present (%v)", e1)
	}

	var req uint32
	r1, _, e1 = procSetupDiGetDeviceInterfaceDetailW.Call(
		info, uintptr(unsafe.Pointer(&data)), 0, 0,
		uintptr(unsafe.Pointer(&req)), 0,
	)
	if r1 != 0 || req == 0 || e1 != syscall.Errno(iddErrorInsufficientBuf) {
		return 0, fmt.Errorf("idd-hold: SetupDiGetDeviceInterfaceDetail probe failed (r=%d err=%v)", r1, e1)
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
		return 0, fmt.Errorf("idd-hold: SetupDiGetDeviceInterfaceDetail failed (%v)", e1)
	}
	path := windows.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(&buf[unsafe.Offsetof(spDeviceInterfaceDetailData{}.devicePath)])), (int(req)-int(unsafe.Offsetof(spDeviceInterfaceDetailData{}.devicePath)))/2))
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(path),
		windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return 0, fmt.Errorf("idd-hold: open device interface: %w", err)
	}
	return h, nil
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

func iddIoPlugin() error {
	dev, err := openDeviceInterface()
	if err != nil {
		return err
	}
	defer windows.CloseHandle(dev)
	in := ctlPlugIn{connectorIndex: 0, monitorEDID: 0, containerID: randomGUID()}
	var junk uint32
	return windows.DeviceIoControl(dev, iddIoctlPlugIn,
		(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
		nil, 0, &junk, nil)
}

func iddIoPlugout() error {
	dev, err := openDeviceInterface()
	if err != nil {
		return err
	}
	defer windows.CloseHandle(dev)
	in := ctlPlugOut{connectorIndex: 0}
	var junk uint32
	return windows.DeviceIoControl(dev, iddIoctlPlugOut,
		(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
		nil, 0, &junk, nil)
}

// newIddHoldCmd 隐藏命令：创建并持有 IDD 软件设备直到 stdin 关闭。
// stdin 命令协议（agent 写、持有者读）：一行一个命令，
//   plug   → 经设备接口 IOCTL 插屏（本进程会话 = 控制台会话）
//   unplug → IOCTL 拔屏
//   EOF    → 释放设备退出（agent 崩溃/退出即自动清理）
func newIddHoldCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "idd-hold",
		Short:  "hold the IDD virtual display device (internal, spawned by the agent)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIddHold()
		},
	}
}

func runIddHold() error {
	h, err := swDeviceCreate()
	if err != nil {
		return err
	}
	fmt.Printf("idd device created (pid=%d)\n", os.Getpid())
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		switch strings.TrimSpace(sc.Text()) {
		case "plug":
			if err := iddIoPlugin(); err != nil {
				fmt.Printf("plug error: %v\n", err)
			} else {
				fmt.Printf("plug ok\n")
			}
		case "unplug":
			if err := iddIoPlugout(); err != nil {
				fmt.Printf("unplug error: %v\n", err)
			} else {
				fmt.Printf("unplug ok\n")
			}
		}
	}
	swDeviceCloseHandle(h)
	return nil
}
