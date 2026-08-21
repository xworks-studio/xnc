//go:build windows

// capture_wgc_windows.go — Windows.Graphics.Capture (WGC) 屏幕捕获，纯
// syscall（CGO_ENABLED=0）。这是唯一捕获路径——无 DXGI/GDI 回退。
//
// 流程：
//
//	RoInitialize(MTA) → RoGetActivationFactory("GraphicsCaptureItem",
//	IGraphicsCaptureItemInterop) → CreateForMonitor(主显示器 HMONITOR) →
//	D3D11CreateDevice(硬件, 失败回退 WARP) → QI IDXGIDevice →
//	CreateDirect3D11DeviceFromDXGIDevice(WinRT IDirect3DDevice) →
//	RoGetActivationFactory("Direct3D11CaptureFramePool",
//	IDirect3D11CaptureFramePoolStatics2) → CreateFreeThreaded(2 缓冲) →
//	CreateCaptureSession → put_IsCursorCaptureEnabled(true) → StartCapture →
//	循环 TryGetNextFrame（轮询, 无需 FrameArrived 事件/DispatcherQueue）→
//	Surface 经 IDirect3DDxgiInterfaceAccess 取回 ID3D11Texture2D →
//	CopyResource(staging) → Map → BGRA 字节。
//
// WGC 相比 DXGI Duplication：首帧立即送达（合成器初始化即推一帧，静态桌面
// 也有画面）；分辨率变化经 ContentSize 检测 + framePool.Recreate 处理；
// 会话/设备失败由调用方整体重建（captureLoop 有限重试）。
//
// GUID 与 vtable 槽位来源（双重验证：Windows SDK 10.0.26100.0 头文件 +
// PowerShell WinRT 投影运行时反射）：
//
//	winrt/windows.graphics.capture.h
//	  IGraphicsCaptureItem       79c3f95b-31f7-4ec2-a464-632ef5d30760
//	  IDirect3D11CaptureFramePool    24eb6d22-1975-422e-82e7-780dbd8ddf24
//	  IDirect3D11CaptureFramePoolStatics2 589b103f-6bbc-5df5-a991-02e28b3b66d5
//	  IDirect3D11CaptureFrame     fa50c623-38da-4b32-acf3-fa9734ad800e
//	  IGraphicsCaptureSession     814e42a9-f70f-4ad7-939b-fddcc6eb880d
//	  IGraphicsCaptureSession2    2c39ae40-7d2e-5044-804e-8b6799d4cf9e
//	um/Windows.Graphics.Capture.Interop.h（经典 COM，IUnknown 基）
//	  IGraphicsCaptureItemInterop 3628E81B-3CAC-4C60-B7F4-23CE0E0C3356
//	um/windows.graphics.directx.direct3d11.interop.h（经典 COM）
//	  IDirect3DDxgiInterfaceAccess   A9B3D012-3DF2-4EE3-B8D1-8695F457D3C1
//
// 注意：WinRT 接口继承 IInspectable——自有方法从槽位 6 起（IUnknown 3 +
// IInspectable 3）；经典 COM 互操作接口从槽位 3 起。
package main

import (
	"errors"
	"fmt"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---- vtable 槽位（SDK 头文件方法声明顺序核实，勿改）----

const (
	vtItemInteropCreateForWindow  = 3 // IGraphicsCaptureItemInterop (IUnknown 基)
	vtItemInteropCreateForMonitor = 4

	vtItemGetSize = 7 // IGraphicsCaptureItem (IInspectable 基): DisplayName6 Size7 Closed8/9

	vtPoolStatics2CreateFreeThreaded = 6 // IDirect3D11CaptureFramePoolStatics2

	vtPoolRecreate             = 6 // IDirect3D11CaptureFramePool
	vtPoolTryGetNextFrame      = 7
	vtPoolCreateCaptureSession = 10

	vtFrameGetSurface     = 6 // IDirect3D11CaptureFrame
	vtFrameGetContentSize = 8

	vtSessionStartCapture = 6 // IGraphicsCaptureSession
	vtSession2PutCursor   = 7 // IGraphicsCaptureSession2: get6 put7

	vtDxgiAccessGetInterface = 3 // IDirect3DDxgiInterfaceAccess (IUnknown 基)
)

// ---- GUID（复制自 SDK 头）----

var (
	iidGraphicsCaptureItemInterop = guid{0x3628e81b, 0x3cac, 0x4c60, [8]byte{0xb7, 0xf4, 0x23, 0xce, 0x0e, 0x0c, 0x33, 0x56}}
	iidGraphicsCaptureItem        = guid{0x79c3f95b, 0x31f7, 0x4ec2, [8]byte{0xa4, 0x64, 0x63, 0x2e, 0xf5, 0xd3, 0x07, 0x60}}
	iidFramePool                  = guid{0x24eb6d22, 0x1975, 0x422e, [8]byte{0x82, 0xe7, 0x78, 0x0d, 0xbd, 0x8d, 0xdf, 0x24}}
	iidFramePoolStatics2          = guid{0x589b103f, 0x6bbc, 0x5df5, [8]byte{0xa9, 0x91, 0x02, 0xe2, 0x8b, 0x3b, 0x66, 0xd5}}
	iidCaptureSession             = guid{0x814e42a9, 0xf70f, 0x4ad7, [8]byte{0x93, 0x9b, 0xfd, 0xdc, 0xc6, 0xeb, 0x88, 0x0d}}
	iidCaptureSession2            = guid{0x2c39ae40, 0x7d2e, 0x5044, [8]byte{0x80, 0x4e, 0x8b, 0x67, 0x99, 0xd4, 0xcf, 0x9e}}
	iidDxgiInterfaceAccess        = guid{0xa9b3d012, 0x3df2, 0x4ee3, [8]byte{0xb8, 0xd1, 0x86, 0x95, 0xf4, 0x57, 0xd3, 0xc1}}
	// iidIDXGIDevice / iidID3D11Texture2D 复制自 SDK dxgi.h / d3d11.h。
	iidIDXGIDevice     = guid{0x54ec77fa, 0x1377, 0x44e6, [8]byte{0x8c, 0x32, 0x88, 0xfd, 0x5f, 0x44, 0xc8, 0x4c}}
	iidID3D11Texture2D = guid{0x6f15aaf2, 0xd208, 0x4e89, [8]byte{0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c}}
)

// ---- combase / d3d11 / user32 ----

var (
	combaseDLL                 = windows.NewLazySystemDLL("combase.dll")
	procRoInitialize           = combaseDLL.NewProc("RoInitialize")
	procRoUninitialize         = combaseDLL.NewProc("RoUninitialize")
	procRoGetActivationFactory = combaseDLL.NewProc("RoGetActivationFactory")
	procWindowsCreateString    = combaseDLL.NewProc("WindowsCreateString")
	procWindowsDeleteString    = combaseDLL.NewProc("WindowsDeleteString")

	procCreateDirect3D11DeviceFromDXGIDevice = d3d11DLL.NewProc("CreateDirect3D11DeviceFromDXGIDevice")

	procMonitorFromWindow = user32DLL.NewProc("MonitorFromWindow")
)

const (
	roInitMultithreaded        = 1
	monitorDefaultToPrimary    = 1
	directXPixelFormatB8G8R8A8 = 87 // = DXGI_FORMAT_B8G8R8A8_UNORM（共享数值空间）
	d3dDriverTypeHardware      = 1
	d3dDriverTypeWarp          = 5
)

// sizeInt32 对应 WinRT SizeInt32（按值传参，8 字节 = 单寄存器）。
func sizeInt32(w, h int) uintptr {
	return uintptr(uint32(w))<<32 | uintptr(uint32(h))
}

// wgcRect 对应 SizeInt32 布局（int32 Width, Height）。
type wgcSize struct {
	Width, Height int32
}

// newHString 创建 WinRT HSTRING（用毕 WindowsDeleteString）。
func newHString(s string) (uintptr, error) {
	u16, err := windows.UTF16FromString(s)
	if err != nil {
		return 0, err
	}
	var hs uintptr
	r, _, _ := procWindowsCreateString.Call(
		uintptr(unsafe.Pointer(&u16[0])), uintptr(len(u16)-1), uintptr(unsafe.Pointer(&hs)))
	if r != 0 {
		return 0, fmt.Errorf("WindowsCreateString: hr=0x%08X", uint32(r))
	}
	return hs, nil
}

// activationFactory 按 IID 取运行时类激活工厂接口。
func activationFactory(class string, iid *guid) (comPtr, error) {
	hs, err := newHString(class)
	if err != nil {
		return nilPtr, err
	}
	defer procWindowsDeleteString.Call(hs)
	var p comPtr
	r, _, _ := procRoGetActivationFactory.Call(hs, ptrGUID(iid), uintptr(unsafe.Pointer(&p.p)))
	if r != 0 {
		return nilPtr, fmt.Errorf("RoGetActivationFactory(%s): hr=0x%08X", class, uint32(r))
	}
	return p, nil
}

// ---- WGCCapturer ----

// WGCCapturer 封装 WGC 捕获管线（主显示器）。
type WGCCapturer struct {
	interop       comPtr // IGraphicsCaptureItemInterop（item 工厂互操作）
	item          comPtr // IGraphicsCaptureItem
	d3dDevice     comPtr // WinRT IDirect3DDevice（IInspectable 形态）
	device        comPtr // ID3D11Device
	context       comPtr // ID3D11DeviceContext
	framePool     comPtr // IDirect3D11CaptureFramePool
	session       comPtr // IGraphicsCaptureSession
	staging       comPtr // ID3D11Texture2D（CPU 可读）
	width, height int
	roInit        bool
}

// NewWGCCapturer 初始化主显示器 WGC 捕获。失败返回错误（无回退——调用方
// 走 placeholderLoop 报告不可用）。
func NewWGCCapturer() (*WGCCapturer, error) {
	r, _, _ := procRoInitialize.Call(roInitMultithreaded)
	if r != 0 && uint32(r) != 0x80010106 { // RPC_E_CHANGED_MODE：已是 MTA，可继续
		return nil, fmt.Errorf("RoInitialize: hr=0x%08X", uint32(r))
	}
	c := &WGCCapturer{roInit: r == 0}

	// GraphicsCaptureItem 工厂（经典 COM 互操作接口——绕开 WinRT statics）。
	ip, err := activationFactory("Windows.Graphics.Capture.GraphicsCaptureItem", &iidGraphicsCaptureItemInterop)
	if err != nil {
		c.Close()
		return nil, err
	}
	c.interop = ip
	fmt.Fprintf(os.Stderr, "xnc-screen-helper: wgc: factory ok\n")

	// 主显示器 HMONITOR → CreateForMonitor。
	hmon, _, _ := procMonitorFromWindow.Call(0, monitorDefaultToPrimary)
	if hmon == 0 {
		c.Close()
		return nil, errors.New("MonitorFromWindow: no primary monitor")
	}
	var item comPtr
	if _, err := c.interop.call(vtItemInteropCreateForMonitor, hmon,
		ptrGUID(&iidGraphicsCaptureItem), uintptr(unsafe.Pointer(&item.p))); err != nil {
		c.Close()
		return nil, fmt.Errorf("CreateForMonitor: %w", err)
	}
	c.item = item
	fmt.Fprintf(os.Stderr, "xnc-screen-helper: wgc: item ok\n")

	var sz wgcSize
	if _, err := item.call(vtItemGetSize, uintptr(unsafe.Pointer(&sz))); err != nil {
		c.Close()
		return nil, fmt.Errorf("get_Size: %w", err)
	}
	c.width, c.height = int(sz.Width), int(sz.Height)
	fmt.Fprintf(os.Stderr, "xnc-screen-helper: wgc: size %dx%d\n", c.width, c.height)
	if c.width <= 0 || c.height <= 0 {
		c.Close()
		return nil, fmt.Errorf("invalid item size %dx%d", c.width, c.height)
	}

	// D3D11 设备（硬件 → WARP；WGC 允许任意设备，复制经 D3D 内存）。
	if err := c.createDevice(d3dDriverTypeHardware); err != nil {
		if werr := c.createDevice(d3dDriverTypeWarp); werr != nil {
			c.Close()
			return nil, fmt.Errorf("D3D11CreateDevice: %v / warp: %v", err, werr)
		}
		fmt.Fprintf(os.Stderr, "xnc-screen-helper: wgc: hardware device failed (%v), using WARP\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "xnc-screen-helper: wgc: hardware device ok\n")
	}

	// FramePool（FreeThreaded——无需 DispatcherQueue，可轮询）。
	statics2, err := activationFactory("Windows.Graphics.Capture.Direct3D11CaptureFramePool", &iidFramePoolStatics2)
	if err != nil {
		c.Close()
		return nil, err
	}
	defer statics2.release()
	var pool comPtr
	if _, err := statics2.call(vtPoolStatics2CreateFreeThreaded,
		c.d3dDevice.u(), directXPixelFormatB8G8R8A8, 2,
		sizeInt32(c.width, c.height), uintptr(unsafe.Pointer(&pool.p))); err != nil {
		c.Close()
		return nil, fmt.Errorf("CreateFreeThreaded: %w", err)
	}
	c.framePool = pool
	fmt.Fprintf(os.Stderr, "xnc-screen-helper: wgc: pool ok\n")

	// 会话 + 光标 + 启动。
	var session comPtr
	if _, err := pool.call(vtPoolCreateCaptureSession, c.item.u(), uintptr(unsafe.Pointer(&session.p))); err != nil {
		c.Close()
		return nil, fmt.Errorf("CreateCaptureSession: %w", err)
	}
	c.session = session
	fmt.Fprintf(os.Stderr, "xnc-screen-helper: wgc: session ok\n")
	if s2, qerr := session.queryInterface(&iidCaptureSession2); qerr == nil {
		fmt.Fprintf(os.Stderr, "xnc-screen-helper: wgc: session2 q ok\n")
		_, _ = s2.call(vtSession2PutCursor, 1) // 光标随画面（best-effort）
		s2.release()
		fmt.Fprintf(os.Stderr, "xnc-screen-helper: wgc: cursor set\n")
	}
	fmt.Fprintf(os.Stderr, "xnc-screen-helper: wgc: before start\n")
	if _, err := session.call(vtSessionStartCapture); err != nil {
		c.Close()
		return nil, fmt.Errorf("StartCapture: %w", err)
	}
	fmt.Fprintf(os.Stderr, "xnc-screen-helper: wgc: started\n")

	if err := c.recreateStaging(); err != nil {
		c.Close()
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "xnc-screen-helper: wgc: init done\n")
	return c, nil
}

// createDevice 建 D3D11 设备 + WinRT 包装。driverType 为 Hardware 或 Warp
// （适配器为 NULL——WGC 不要求特定适配器）。
func (c *WGCCapturer) createDevice(driverType uintptr) error {
	var device, context comPtr
	var featureLevel uint32
	r, _, e := procD3D11CreateDevice.Call(
		0, driverType, 0,
		uintptr(d3d11CreateDeviceBgraSupport),
		0, 0, uintptr(d3d11SdkVersion),
		uintptr(unsafe.Pointer(&device.p)), uintptr(unsafe.Pointer(&featureLevel)),
		uintptr(unsafe.Pointer(&context.p)),
	)
	if r != 0 {
		return fmt.Errorf("hr=0x%08X %v", uint32(r), e)
	}
	dxgiDev, err := device.queryInterface(&iidIDXGIDevice)
	if err != nil {
		device.release()
		context.release()
		return fmt.Errorf("QI IDXGIDevice: %w", err)
	}
	defer dxgiDev.release()
	var insp comPtr
	r, _, e = procCreateDirect3D11DeviceFromDXGIDevice.Call(dxgiDev.u(), uintptr(unsafe.Pointer(&insp.p)))
	if r != 0 {
		device.release()
		context.release()
		return fmt.Errorf("CreateDirect3D11DeviceFromDXGIDevice: hr=0x%08X %v", uint32(r), e)
	}
	c.device, c.context, c.d3dDevice = device, context, insp
	return nil
}

// recreateStaging 按 c.width/height 重建 CPU 可读 staging 纹理。
func (c *WGCCapturer) recreateStaging() error {
	c.staging.release()
	texDesc := d3d11Texture2DDesc{
		Width: uint32(c.width), Height: uint32(c.height),
		MipLevels: 1, ArraySize: 1,
		SampleCount:    1,
		Format:         dxgiFormatB8G8R8A8UNorm,
		Usage:          d3d11UsageStaging,
		CPUAccessFlags: d3d11CpuAccessRead,
	}
	var staging comPtr
	if _, err := c.device.call(vtDeviceCreateTexture2D, uintptr(unsafe.Pointer(&texDesc)), 0, uintptr(unsafe.Pointer(&staging.p))); err != nil {
		return fmt.Errorf("CreateTexture2D(staging): %w", err)
	}
	c.staging = staging
	return nil
}

// Dims 返回当前捕获尺寸（分辨率变化时随 ContentSize 更新）。
func (c *WGCCapturer) Dims() (int, int) { return c.width, c.height }

// AcquireFrame 轮询 TryGetNextFrame 直到取到帧或超时。桌面静止（合成器无
// 更新）返回 ErrTimeout。会话/设备级失败返回其他错误（调用方应整体重建）。
// 返回 top-down BGRA（width*height*4 字节）。
func (c *WGCCapturer) AcquireFrame(timeoutMs uint) ([]byte, error) {
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for {
		var frame comPtr
		r1, err := c.framePool.call(vtPoolTryGetNextFrame, uintptr(unsafe.Pointer(&frame.p)))
		if err != nil {
			return nil, fmt.Errorf("TryGetNextFrame: %w", err)
		}
		if !frame.valid() {
			_ = r1 // S_FALSE：暂无新帧
			if time.Now().After(deadline) {
				return nil, ErrTimeout
			}
			time.Sleep(8 * time.Millisecond)
			continue
		}

		// 分辨率变化检测：ContentSize 超出池尺寸需 Recreate，本帧丢弃。
		var sz wgcSize
		if _, serr := frame.call(vtFrameGetContentSize, uintptr(unsafe.Pointer(&sz))); serr == nil {
			if int(sz.Width) > c.width || int(sz.Height) > c.height {
				c.width, c.height = int(sz.Width), int(sz.Height)
				if _, rerr := c.framePool.call(vtPoolRecreate, c.d3dDevice.u(),
					directXPixelFormatB8G8R8A8, 2, sizeInt32(c.width, c.height)); rerr != nil {
					frame.release()
					return nil, fmt.Errorf("Recreate: %w", rerr)
				}
				if rerr := c.recreateStaging(); rerr != nil {
					frame.release()
					return nil, rerr
				}
				frame.release()
				continue
			}
		}

		buf, ferr := c.copyFrameBGRA(frame)
		frame.release()
		if ferr != nil {
			return nil, ferr
		}
		return buf, nil
	}
}

// copyFrameBGRA Surface → ID3D11Texture2D → CopyResource(staging) → Map →
// 行拷贝（RowPitch 对齐）。调用方负责 frame 释放。
func (c *WGCCapturer) copyFrameBGRA(frame comPtr) ([]byte, error) {
	var surface comPtr
	if _, err := frame.call(vtFrameGetSurface, uintptr(unsafe.Pointer(&surface.p))); err != nil {
		return nil, fmt.Errorf("get_Surface: %w", err)
	}
	defer surface.release()

	access, err := surface.queryInterface(&iidDxgiInterfaceAccess)
	if err != nil {
		return nil, fmt.Errorf("QI IDirect3DDxgiInterfaceAccess: %w", err)
	}
	defer access.release()
	var tex comPtr
	if _, err := access.call(vtDxgiAccessGetInterface, ptrGUID(&iidID3D11Texture2D), uintptr(unsafe.Pointer(&tex.p))); err != nil {
		return nil, fmt.Errorf("GetInterface(ID3D11Texture2D): %w", err)
	}
	defer tex.release()

	if _, err := c.context.call(vtContextCopyResource, c.staging.u(), tex.u()); err != nil {
		return nil, fmt.Errorf("CopyResource: %w", err)
	}

	var mapped d3d11MappedSubresource
	if _, err := c.context.call(vtContextMap, c.staging.u(), 0, 1 /*D3D11_MAP_READ*/, 0, uintptr(unsafe.Pointer(&mapped))); err != nil {
		return nil, fmt.Errorf("Map: %w", err)
	}
	defer c.context.call(vtContextUnmap, c.staging.u(), 0)

	stride := c.width * 4
	buf := make([]byte, c.height*stride)
	src := unsafe.Slice((*byte)(mapped.pData), int(mapped.RowPitch)*c.height)
	for row := 0; row < c.height; row++ {
		copy(buf[row*stride:(row+1)*stride], src[row*int(mapped.RowPitch):row*int(mapped.RowPitch)+stride])
	}
	return buf, nil
}

// Close 释放全部资源（幂等）。
func (c *WGCCapturer) Close() {
	for _, p := range []comPtr{c.staging, c.session, c.framePool, c.d3dDevice, c.context, c.device, c.item, c.interop} {
		p.release()
	}
	c.staging, c.session, c.framePool = nilPtr, nilPtr, nilPtr
	c.d3dDevice, c.context, c.device = nilPtr, nilPtr, nilPtr
	c.item, c.interop = nilPtr, nilPtr
	if c.roInit {
		procRoUninitialize.Call()
		c.roInit = false
	}
}

// captureWGCSingle 单帧捕获（快照路径）：建会话 → 等首帧（合成器初始化
// 即推送, 静态桌面也有画面）→ 释放。
func captureWGCSingle(timeoutMs uint) ([]byte, int, int, error) {
	c, err := NewWGCCapturer()
	if err != nil {
		return nil, 0, 0, err
	}
	defer c.Close()
	frame, err := c.AcquireFrame(timeoutMs)
	if err != nil {
		return nil, 0, 0, err
	}
	return frame, c.width, c.height, nil
}
