// com_windows.go — 手写 COM vtable 基建（纯 syscall，CGO_ENABLED=0），
// 供 DDA DLL 加载层、WGC 捕获器与 MFT 编码器共享。vtable 槽位以 SDK
// 头文件方法顺序为准（历史教训：槽位错位是静默调用错误方法）。
//
//go:build windows

package main

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---- COM vtable 基础（共享：捕获 + 编码器）----

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// comPtr 包装裸 COM 接口指针。底层存 unsafe.Pointer（vtable 指针），
// uintptr 转换仅出现在 syscall 实参位置（go vet 合规）。
type comPtr struct{ p unsafe.Pointer }

// nilPtr 为零值接口。
var nilPtr = comPtr{}

// u 返回 uintptr 形式（仅用于 syscall 实参传递）。
func (p comPtr) u() uintptr { return uintptr(p.p) }

// valid 报告指针非空。
func (p comPtr) valid() bool { return p.p != nil }

// vtableSlot 返回接口 vtable 第 slot 项的函数指针。
func (p comPtr) vtableSlot(slot int) uintptr {
	vt := *(*unsafe.Pointer)(p.p) // 接口首字段 = vtable 指针
	return *(*uintptr)(unsafe.Add(vt, uintptr(slot)*unsafe.Sizeof(uintptr(0))))
}

// call 按槽位调用接口方法（this 隐含在首参）。syscall.SyscallN 是编译器
// intrinsic，不支持切片展开——按参数个数分派。
func (p comPtr) call(slot int, a ...uintptr) (r1 uintptr, err error) {
	if !p.valid() {
		return 0, errors.New("nil COM interface")
	}
	fn := p.vtableSlot(slot)
	var r2 uintptr
	var l syscall.Errno
	switch len(a) {
	case 0:
		r1, r2, l = syscall.SyscallN(fn, p.u())
	case 1:
		r1, r2, l = syscall.SyscallN(fn, p.u(), a[0])
	case 2:
		r1, r2, l = syscall.SyscallN(fn, p.u(), a[0], a[1])
	case 3:
		r1, r2, l = syscall.SyscallN(fn, p.u(), a[0], a[1], a[2])
	case 4:
		r1, r2, l = syscall.SyscallN(fn, p.u(), a[0], a[1], a[2], a[3])
	case 5:
		r1, r2, l = syscall.SyscallN(fn, p.u(), a[0], a[1], a[2], a[3], a[4])
	case 6:
		r1, r2, l = syscall.SyscallN(fn, p.u(), a[0], a[1], a[2], a[3], a[4], a[5])
	default:
		return 0, fmt.Errorf("unsupported arg count %d", len(a))
	}
	_, _ = r2, l
	if int32(r1) < 0 {
		return r1, fmt.Errorf("COM call slot %d: hr=0x%08X", slot, uint32(r1))
	}
	return r1, nil
}

// queryInterface 标准槽位 0 QI。
func (p comPtr) queryInterface(iid *guid) (comPtr, error) {
	var out comPtr
	_, err := p.call(0, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out.p)))
	if err != nil {
		return nilPtr, err
	}
	return out, nil
}

// release 释放接口引用（槽位 2）。
func (p comPtr) release() {
	if p.valid() {
		_, _, _ = syscall.SyscallN(p.vtableSlot(2), p.u())
	}
}

// ---- vtable 槽位（D3D11，SDK 头顺序核实，勿改）----

const (
	vtDeviceCreateTexture2D = 5 // ID3D11Device: IUnknown0-2,CreateBuffer3,CreateTexture1D4,CreateTexture2D5
	vtContextMap            = 14
	vtContextUnmap          = 15
	vtContextCopyResource   = 47
)

// ---- D3D11 常量 ----

const (
	d3d11CreateDeviceBgraSupport = 0x20
	d3d11SdkVersion              = 7
	dxgiFormatB8G8R8A8UNorm      = 87
	d3d11UsageStaging            = 3
	d3d11CpuAccessRead           = 0x20000
)

// ---- 结构体（x64 布局）----

// d3d11Texture2DDesc 对应 D3D11_TEXTURE2D_DESC。
type d3d11Texture2DDesc struct {
	Width, Height, MipLevels, ArraySize uint32
	Format                              uint32
	SampleCount, SampleQuality          uint32
	Usage                               uint32
	BindFlags                           uint32
	CPUAccessFlags, MiscFlags           uint32
}

// d3d11MappedSubresource 对应 D3D11_MAPPED_SUBRESOURCE（pData 为系统指针）。
type d3d11MappedSubresource struct {
	pData      unsafe.Pointer
	RowPitch   uint32
	DepthPitch uint32
}

// ---- DLL ----

var (
	user32DLL = windows.NewLazySystemDLL("user32.dll")
	d3d11DLL  = windows.NewLazySystemDLL("d3d11.dll")

	procD3D11CreateDevice = d3d11DLL.NewProc("D3D11CreateDevice")
)

// hrOf 从 call 的 r1 恢复 HRESULT 原值（错误分支也携带）。
func hrOf(r1 uintptr) uint32 { return uint32(r1) }

const rpcEChangedMode = 0x80010106

// ---- ole32（编码器 MFT 路径使用）----

var (
	ole32        = windows.NewLazySystemDLL("ole32.dll")
	procCoInitEx = ole32.NewProc("CoInitializeEx")
	procCoUninit = ole32.NewProc("CoUninitialize")
)

const coinitApartmentThreaded = 0x2

func coInitializeEx() (uintptr, error) {
	r, _, e := procCoInitEx.Call(0, coinitApartmentThreaded)
	if r != 0 {
		return r, e
	}
	return 0, nil
}

func coUninitialize() { procCoUninit.Call() }
