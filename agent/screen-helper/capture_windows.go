//go:build windows

// capture_windows.go — DXGI Desktop Duplication GPU 捕获（纯 syscall COM，
// CGO_ENABLED=0）。流程：
//
//	CoInitializeEx → CreateDXGIFactory1 → EnumAdapters1 → EnumOutputs →
//	GetDesc（分辨率）→ QueryInterface(IDXGIOutput1) → D3D11CreateDevice →
//	DuplicateOutput → 循环 AcquireNextFrame → CopyResource(staging) →
//	Map → BGRA 字节 → ReleaseFrame。
//
// 所有 COM 调用走 vtable 槽位 syscall；GUID 从 Windows SDK 头文件复制。
// DXGI 不可用（旧驱动 / 权限）时返回错误，调用方回退 GDI（T7）。
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---- 哨兵错误 ----

var (
	// ErrAccessLost — 锁屏 / 桌面切换 / 模式变化：需重建 duplication。
	ErrAccessLost = errors.New("dxgi access lost")
	// ErrTimeout — timeout 内无新帧（桌面静止）。
	ErrTimeout = errors.New("dxgi acquire timeout")
)

const (
	dxgiErrAccessLost = 0x887A0026
	waitTimeout       = 0x80070102 // HRESULT_FROM_WIN32(WAIT_TIMEOUT)
	rpcEChangedMode   = 0x80010106
)

// ---- GUID（复制自 SDK 头）----

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	iidIDXGIFactory1 = guid{0x770aae78, 0xf26f, 0x4dba, [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
	iidIDXGIOutput1  = guid{0x00cddea8, 0x939b, 0x4b83, [8]byte{0xa3, 0x40, 0xa6, 0x85, 0x22, 0x66, 0x66, 0xcc}}
)

// ---- COM vtable 基础 ----

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

// hrOf 从 call 的 r1 恢复 HRESULT 原值（错误分支也携带）。
func hrOf(r1 uintptr) uint32 { return uint32(r1) }

// ---- vtable 槽位（按 SDK 接口方法顺序）----

const (
	vtFactoryEnumAdapters1 = 12 // IDXGIFactory1: IUnknown3+IDXGIObject4+IDXGIFactory5+EnumAdapters1

	vtAdapterEnumOutputs = 7 // IDXGIAdapter1: ...+GetDesc8,EnumOutputs9,Check...

	vtOutputGetDesc = 9 // IDXGIOutput: ...+GetDisplayModeList7,FindClosest8,GetDesc9,...

	vtOutput1DuplicateOutput = 15 // IDXGIOutput1: ...+GetDisplayModeList1_12,Find1_13,GetSurface1_14,DuplicateOutput

	vtDupAcquireNextFrame = 4  // IDXGIOutputDuplication: GetDesc3,AcquireNextFrame4,...
	vtDupReleaseFrame     = 10 // ...,ReleaseFrame10

	vtDeviceCreateTexture2D = 7 // ID3D11Device: GetImmediateContext3,CreateDeferredContext4,CreateBuffer5,CreateTexture1D6,CreateTexture2D7

	vtContextCopyResource = 9  // ID3D11DeviceContext: devicechild6,UpdateSubresource7,CopySubresourceRegion8,CopyResource9
	vtContextMap          = 34 // ...,Map34,Unmap35
	vtContextUnmap        = 35
)

// ---- DXGI/D3D11 常量 ----

const (
	d3dDriverTypeUnknown         = 0
	d3d11CreateDeviceBgraSupport = 0x20
	d3d11SdkVersion              = 7
	dxgiFormatB8G8R8A8UNorm      = 87
	d3d11UsageStaging            = 3
	d3d11CpuAccessRead           = 0x20000
)

// ---- 结构体（x64 布局）----

// dxgiOutputDesc 对应 DXGI_OUTPUT_DESC（GetDesc 输出）。
type dxgiOutputDesc struct {
	DeviceName         [32]uint16 // 64B
	DesktopCoordinates rect
	AttachedToDesktop  uint32
	Rotation           uint32
	Monitor            uintptr
}

type rect struct {
	Left, Top, Right, Bottom int32
}

func (r rect) width() int  { return int(r.Right - r.Left) }
func (r rect) height() int { return int(r.Bottom - r.Top) }

// d3d11Texture2DDesc 对应 D3D11_TEXTURE2D_DESC。
type d3d11Texture2DDesc struct {
	Width, Height, MipLevels, ArraySize uint32
	SampleCount, SampleQuality          uint32
	Format, Usage, BindFlags            uint32
	CPUAccessFlags, MiscFlags           uint32
}

// d3d11MappedSubresource 对应 D3D11_MAPPED_SUBRESOURCE（pData 为系统指针）。
type d3d11MappedSubresource struct {
	pData      unsafe.Pointer
	RowPitch   uint32
	DepthPitch uint32
}

// dxgiOutduplFrameInfo 对应 DXGI_OUTDUPL_FRAME_INFO（仅用到头部字段）。
type dxgiOutduplFrameInfo struct {
	LastPresentTime           int64
	LastPresentUpdateTime     int64
	AccumulatedFrames         uint32
	RectsCoalesced            uint32
	ProtectedContentMaskedOut uint32
	TotalMetadataSize         uint32
}

// ---- DLL ----

var (
	dxgiDLL  = windows.NewLazySystemDLL("dxgi.dll")
	d3d11DLL = windows.NewLazySystemDLL("d3d11.dll")

	procCreateDXGIFactory1 = dxgiDLL.NewProc("CreateDXGIFactory1")
	procD3D11CreateDevice  = d3d11DLL.NewProc("D3D11CreateDevice")
)

// ---- DXGICapturer ----

// DXGICapturer 封装 Desktop Duplication 管线（主显示器）。
type DXGICapturer struct {
	factory       comPtr // IDXGIFactory1
	adapter       comPtr // IDXGIAdapter1
	output        comPtr // IDXGIOutput
	output1       comPtr // IDXGIOutput1
	duplication   comPtr // IDXGIOutputDuplication
	device        comPtr // ID3D11Device
	context       comPtr // ID3D11DeviceContext
	staging       comPtr // ID3D11Texture2D（CPU 可读）
	width, height int

	coInit bool
}

// NewDXGICapturer 初始化 Desktop Duplication（主适配器 + 主输出）。
func NewDXGICapturer() (*DXGICapturer, error) {
	hr, err := coInitializeEx()
	if err != nil && hr != rpcEChangedMode {
		return nil, fmt.Errorf("CoInitializeEx: %w", err)
	}
	c := &DXGICapturer{coInit: err == nil}

	// CreateDXGIFactory1
	var factory comPtr
	r, _, e := procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidIDXGIFactory1)), uintptr(unsafe.Pointer(&factory.p)))
	if r != 0 {
		return nil, fmt.Errorf("CreateDXGIFactory1: %v", e)
	}
	c.factory = factory

	// EnumAdapters1(0)
	var adapter comPtr
	if _, err := factory.call(vtFactoryEnumAdapters1, 0, uintptr(unsafe.Pointer(&adapter.p))); err != nil {
		c.Close()
		return nil, fmt.Errorf("EnumAdapters1: %w", err)
	}
	c.adapter = adapter

	// EnumOutputs(0)
	var output comPtr
	if _, err := adapter.call(vtAdapterEnumOutputs, 0, uintptr(unsafe.Pointer(&output.p))); err != nil {
		c.Close()
		return nil, fmt.Errorf("EnumOutputs: %w", err)
	}
	c.output = output

	// GetDesc → 分辨率
	var desc dxgiOutputDesc
	if _, err := output.call(vtOutputGetDesc, uintptr(unsafe.Pointer(&desc))); err != nil {
		c.Close()
		return nil, fmt.Errorf("GetDesc: %w", err)
	}
	c.width, c.height = desc.DesktopCoordinates.width(), desc.DesktopCoordinates.height()
	if c.width <= 0 || c.height <= 0 {
		c.Close()
		return nil, fmt.Errorf("invalid output desc %dx%d", c.width, c.height)
	}

	// QI IDXGIOutput1
	out1, err := output.queryInterface(&iidIDXGIOutput1)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("QI IDXGIOutput1: %w", err)
	}
	c.output1 = out1

	// D3D11CreateDevice（adapter 非 NULL → DriverType 必须 UNKNOWN）
	var device, context comPtr
	var featureLevel uint32
	r, _, e = procD3D11CreateDevice.Call(
		adapter.u(), uintptr(d3dDriverTypeUnknown), 0,
		uintptr(d3d11CreateDeviceBgraSupport),
		0, 0, uintptr(d3d11SdkVersion),
		uintptr(unsafe.Pointer(&device.p)), uintptr(unsafe.Pointer(&featureLevel)),
		uintptr(unsafe.Pointer(&context.p)),
	)
	if r != 0 {
		c.Close()
		return nil, fmt.Errorf("D3D11CreateDevice: %v", e)
	}
	c.device, c.context = device, context

	// staging texture（CPU 读）
	texDesc := d3d11Texture2DDesc{
		Width: uint32(c.width), Height: uint32(c.height),
		MipLevels: 1, ArraySize: 1,
		SampleCount:    1,
		Format:         dxgiFormatB8G8R8A8UNorm,
		Usage:          d3d11UsageStaging,
		CPUAccessFlags: d3d11CpuAccessRead,
	}
	var staging comPtr
	if _, err := device.call(vtDeviceCreateTexture2D, uintptr(unsafe.Pointer(&texDesc)), 0, uintptr(unsafe.Pointer(&staging.p))); err != nil {
		c.Close()
		return nil, fmt.Errorf("CreateTexture2D(staging): %w", err)
	}
	c.staging = staging

	// DuplicateOutput
	var dup comPtr
	if _, err := out1.call(vtOutput1DuplicateOutput, device.u(), uintptr(unsafe.Pointer(&dup.p))); err != nil {
		c.Close()
		return nil, fmt.Errorf("DuplicateOutput: %w", err)
	}
	c.duplication = dup
	return c, nil
}

// AcquireFrame 阻塞等待下一帧（timeout 毫秒），返回全帧 BGRA（top-down）。
// 桌面静止超时返回 ErrTimeout；锁屏/访问丢失返回 ErrAccessLost。
// 注：当前返回全帧像素；脏区子矩形优化待编码器接入（T6/T7）后启用。
func (c *DXGICapturer) AcquireFrame(timeoutMs uint) ([]byte, []rect, error) {
	var info dxgiOutduplFrameInfo
	var texture comPtr
	r1, err := c.duplication.call(vtDupAcquireNextFrame, uintptr(timeoutMs),
		uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&texture.p)))
	if err != nil {
		hr := hrOf(r1)
		if hr == dxgiErrAccessLost {
			return nil, nil, ErrAccessLost
		}
		if hr == waitTimeout {
			return nil, nil, ErrTimeout
		}
		return nil, nil, fmt.Errorf("AcquireNextFrame: %w", err)
	}
	defer c.ReleaseFrame()

	// GPU 纹理 → staging → Map → 内存
	if _, err := c.context.call(vtContextCopyResource, c.staging.u(), texture.u()); err != nil {
		return nil, nil, fmt.Errorf("CopyResource: %w", err)
	}
	texture.release()

	var mapped d3d11MappedSubresource
	if _, err := c.context.call(vtContextMap, c.staging.u(), 0, 1 /*D3D11_MAP_READ*/, 0, uintptr(unsafe.Pointer(&mapped))); err != nil {
		return nil, nil, fmt.Errorf("Map: %w", err)
	}
	defer c.context.call(vtContextUnmap, c.staging.u(), 0)

	stride := c.width * 4
	buf := make([]byte, c.height*stride)
	src := unsafe.Slice((*byte)(mapped.pData), int(mapped.RowPitch)*c.height)
	for row := 0; row < c.height; row++ {
		copy(buf[row*stride:(row+1)*stride], src[row*int(mapped.RowPitch):row*int(mapped.RowPitch)+stride])
	}
	return buf, nil, nil
}

// ReleaseFrame 归还当前帧给 duplication（AcquireFrame 内部已 defer 调用；
// 导出供异常路径手动归还）。
func (c *DXGICapturer) ReleaseFrame() {
	if c.duplication.valid() {
		_, _ = c.duplication.call(vtDupReleaseFrame)
	}
}

// Close 释放全部 COM 资源。
func (c *DXGICapturer) Close() {
	for _, p := range []comPtr{c.duplication, c.staging, c.context, c.device, c.output1, c.output, c.adapter, c.factory} {
		p.release()
	}
	c.duplication, c.staging, c.context, c.device = nilPtr, nilPtr, nilPtr, nilPtr
	c.output1, c.output, c.adapter, c.factory = nilPtr, nilPtr, nilPtr, nilPtr
	if c.coInit {
		coUninitialize()
		c.coInit = false
	}
}

// ---- ole32 ----

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

// ---- 主捕获循环 ----

// captureLoop：初始化 DXGI → 发送分辨率 + capturing → 按帧率上限循环
// AcquireFrame。像素帧的编码/推送由 T6（H.264 MFT）接入；当前阶段捕获后
// 丢弃（维持协议与自适应帧率语义：静止即无动作）。DXGI 初始化失败时回退
// 占位状态循环（T7 接入 GDI fallback）。
func captureLoop(ctx context.Context, conn net.Conn, frameInterval time.Duration) error {
	cap, err := NewDXGICapturer()
	if err != nil {
		return placeholderLoop(ctx, conn, err)
	}
	defer cap.Close()

	if err := writeFrame(conn, pipeFrameDims, packDims(cap.width, cap.height)); err != nil {
		return err
	}
	if err := writeFrame(conn, pipeFrameState, []byte("capturing")); err != nil {
		return err
	}

	timeout := frameInterval.Milliseconds()
	if timeout <= 0 || timeout > 100 {
		timeout = 100
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		data, _, err := cap.AcquireFrame(uint(timeout))
		_ = data // T6：BGRA → YUV → H.264 → writeFrame(0x01/0x02)
		if err != nil {
			if errors.Is(err, ErrAccessLost) {
				if werr := writeFrame(conn, pipeFrameState, []byte("locked")); werr != nil {
					return werr
				}
				time.Sleep(time.Second)
				continue
			}
			if errors.Is(err, ErrTimeout) {
				continue // 桌面静止：自适应 0fps
			}
			return fmt.Errorf("acquire: %w", err)
		}
	}
}

// placeholderLoop DXGI 不可用时的占位循环（每秒状态帧，保持 pipe 活性）。
func placeholderLoop(ctx context.Context, conn net.Conn, cause error) error {
	_, _ = fmt.Fprintf(os.Stderr, "xnc-screen-helper: dxgi unavailable: %v\n", cause)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := writeFrame(conn, pipeFrameState, []byte("capturing")); err != nil {
				return err
			}
		}
	}
}
