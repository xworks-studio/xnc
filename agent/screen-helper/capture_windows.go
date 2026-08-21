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

	vtOutputGetDesc = 7 // IDXGIOutput: IUnknown3+IDXGIObject4+GetDesc7,GetDisplayModeList8,...

	vtOutput1DuplicateOutput = 20 // IDXGIOutput1: IDXGIOutput17+GetDisplaySurfaceData18,ReleaseFrameOwnership19,DuplicateOutput20

	vtDupAcquireNextFrame = 8  // IDXGIOutputDuplication: IUnknown0-2,GetDesc3,...,AcquireNextFrame8
	vtDupReleaseFrame     = 12 // GetFrameDirtyRects9,GetFrameMoveRects10,GetFramePointerShape11,ReleaseFrame12

	vtDeviceCreateTexture2D = 5 // ID3D11Device: IUnknown0-2,CreateBuffer3,CreateTexture1D4,CreateTexture2D5

	vtContextCopyResource = 47 // ID3D11DeviceContext: IUnknown0-2+ID3D11DeviceChild3-4,...,Map14,Unmap15,...,CopyResource47
	vtContextMap          = 14 // ...,Map14,Unmap15
	vtContextUnmap        = 15
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

// NewDXGICapturer 初始化 Desktop Duplication。遍历所有适配器/输出，
// 取第一个连桌面且 DuplicateOutput 成功的组合（混合显卡设备上 adapter 0
// 未必持有桌面输出——单设备创建在 dGPU 上会 INVALID_CALL）。全部失败返回
// 最后一个错误，调用方回退 GDI。
func NewDXGICapturer() (*DXGICapturer, error) {
	hr, err := coInitializeEx()
	if err != nil && hr != rpcEChangedMode {
		return nil, fmt.Errorf("CoInitializeEx: %w", err)
	}
	c := &DXGICapturer{coInit: err == nil}

	var factory comPtr
	r, _, e := procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidIDXGIFactory1)), uintptr(unsafe.Pointer(&factory.p)))
	if r != 0 {
		return nil, fmt.Errorf("CreateDXGIFactory1: %v", e)
	}
	c.factory = factory

	var lastErr error
	for ai := 0; ai < 4; ai++ {
		var adapter comPtr
		if _, err := factory.call(vtFactoryEnumAdapters1, uintptr(ai), uintptr(unsafe.Pointer(&adapter.p))); err != nil {
			break // 适配器列表耗尽
		}
		matched := false
		for oi := 0; oi < 4; oi++ {
			var output comPtr
			if _, err := adapter.call(vtAdapterEnumOutputs, uintptr(oi), uintptr(unsafe.Pointer(&output.p))); err != nil {
				break // 该适配器输出耗尽
			}
			cc, err := c.tryDuplicate(adapter, output)
			if err != nil {
				lastErr = err
				output.release()
				continue
			}
			// 成功：接管资源所有权并返回。
			c.adapter, c.output, c.output1 = adapter, output, cc.output1
			c.device, c.context, c.duplication, c.staging = cc.device, cc.context, cc.duplication, cc.staging
			c.width, c.height = cc.width, cc.height
			matched = true
			return c, nil
		}
		if !matched {
			adapter.release()
		}
	}
	c.Close()
	if lastErr == nil {
		lastErr = fmt.Errorf("no desktop-attached output")
	}
	return nil, lastErr
}

// duplAttempt — tryDuplicate 成功结果的资源集合。
type duplAttempt struct {
	output1       comPtr // IDXGIOutput1
	duplication   comPtr // IDXGIOutputDuplication
	device        comPtr // ID3D11Device
	context       comPtr // ID3D11DeviceContext
	staging       comPtr // ID3D11Texture2D（CPU 可读）
	width, height int
}

// tryDuplicate 在指定 adapter/output 上创建设备并尝试 DuplicateOutput。
// 成功时返回全部资源（所有权归调用方）；失败时清理自身并返回错误。
func (c *DXGICapturer) tryDuplicate(adapter, output comPtr) (*duplAttempt, error) {
	var desc dxgiOutputDesc
	if _, err := output.call(vtOutputGetDesc, uintptr(unsafe.Pointer(&desc))); err != nil {
		return nil, fmt.Errorf("GetDesc: %w", err)
	}
	if desc.AttachedToDesktop == 0 {
		return nil, fmt.Errorf("output not attached to desktop")
	}
	w, h := desc.DesktopCoordinates.width(), desc.DesktopCoordinates.height()
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("invalid output desc %dx%d", w, h)
	}

	out1, err := output.queryInterface(&iidIDXGIOutput1)
	if err != nil {
		return nil, fmt.Errorf("QI IDXGIOutput1: %w", err)
	}
	a := &duplAttempt{output1: out1, width: w, height: h}
	fail := func(err error) (*duplAttempt, error) {
		a.staging.release()
		a.context.release()
		a.device.release()
		out1.release()
		return nil, err
	}

	// D3D11CreateDevice（adapter 非 NULL → DriverType 必须 UNKNOWN）。
	var featureLevel uint32
	r, _, e := procD3D11CreateDevice.Call(
		adapter.u(), uintptr(d3dDriverTypeUnknown), 0,
		uintptr(d3d11CreateDeviceBgraSupport),
		0, 0, uintptr(d3d11SdkVersion),
		uintptr(unsafe.Pointer(&a.device.p)), uintptr(unsafe.Pointer(&featureLevel)),
		uintptr(unsafe.Pointer(&a.context.p)),
	)
	if r != 0 {
		return fail(fmt.Errorf("D3D11CreateDevice: %v", e))
	}

	// staging texture（CPU 读）。
	texDesc := d3d11Texture2DDesc{
		Width: uint32(w), Height: uint32(h),
		MipLevels: 1, ArraySize: 1,
		SampleCount:    1,
		Format:         dxgiFormatB8G8R8A8UNorm,
		Usage:          d3d11UsageStaging,
		CPUAccessFlags: d3d11CpuAccessRead,
	}
	if _, err := a.device.call(vtDeviceCreateTexture2D, uintptr(unsafe.Pointer(&texDesc)), 0, uintptr(unsafe.Pointer(&a.staging.p))); err != nil {
		return fail(fmt.Errorf("CreateTexture2D(staging): %w", err))
	}
	if _, err := out1.call(vtOutput1DuplicateOutput, a.device.u(), uintptr(unsafe.Pointer(&a.duplication.p))); err != nil {
		return fail(fmt.Errorf("DuplicateOutput: %w", err))
	}
	return a, nil
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

// recreate 销毁并重建 duplication（ErrAccessLost 后恢复：锁屏返回 /
// 桌面切换 / 显示模式变化）。设备等长生命周期接口保持不动。
func (c *DXGICapturer) recreate() error {
	// 若仍持有帧，先归还（access lost 后通常已失效，忽略错误）。
	c.ReleaseFrame()
	if c.duplication.valid() {
		c.duplication.release()
		c.duplication = nilPtr
	}
	var dup comPtr
	if _, err := c.output1.call(vtOutput1DuplicateOutput, c.device.u(), uintptr(unsafe.Pointer(&dup.p))); err != nil {
		return fmt.Errorf("DuplicateOutput(recreate): %w", err)
	}
	c.duplication = dup
	return nil
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

// bitrateFor 将质量参数（1-100）映射到 H.264 平均码率。
func bitrateFor(quality int) int {
	if quality < 1 {
		quality = 1
	}
	if quality > 100 {
		quality = 100
	}
	return 500_000 + quality*30_000 // q60 ≈ 2.3Mbps（1080p 30fps 预算内）
}

// captureLoop：DXGI 优先（GDI 回退）→ 编码器（H.264 MFT 优先，JPEG 流
// 回退）→ 发分辨率 + capturing → 捕获-编码-推送循环。
//
//	静止：AcquireFrame 超时/无变化 → 不编码不出帧（自适应 0fps）
//	锁屏：ErrAccessLost → 状态 0x03 locked + 重建 duplication；重建后
//	      首次成功取帧再发 0x03 capturing（观众得以感知恢复）
//	关键帧：首帧 + 每 gop 帧（新观众可立即入流）
func captureLoop(ctx context.Context, conn net.Conn, opts captureOpts) error {
	frameInterval := time.Second / time.Duration(opts.fps)

	// 捕获源：DXGI → GDI。
	var dxgi *DXGICapturer
	var gdi *gdiStreamCapturer
	var srcW, srcH int
	c, cerr := NewDXGICapturer()
	if cerr == nil {
		dxgi = c
		srcW, srcH = dxgi.width, dxgi.height
	} else {
		g, gerr := newGDIStreamCapturer(opts.maxWidth)
		if gerr != nil {
			return placeholderLoop(ctx, conn, gerr)
		}
		gdi = g
		// GDI 捕获器内部完成缩放——源分辨率即其输出分辨率，
		// 避免 captureLoop 重复缩放。
		srcW, srcH = gdi.outW, gdi.outH
		fmt.Fprintf(os.Stderr, "xnc-screen-helper: dxgi unavailable (%v), gdi fallback\n", cerr)
	}
	defer func() {
		if dxgi != nil {
			dxgi.Close()
		}
		if gdi != nil {
			gdi.Close()
		}
	}()

	outW, outH := fitDims(srcW, srcH, opts.maxWidth)

	// 编码器：H.264 MFT → JPEG 帧流。
	gop := opts.fps * 2 // 关键帧间隔 ≈ 2s
	encoder, eerr := newH264Encoder(outW, outH, bitrateFor(opts.quality), gop)
	if eerr != nil {
		fmt.Fprintf(os.Stderr, "xnc-screen-helper: h264 mft unavailable (%v), jpeg fallback\n", eerr)
		encoder = newJPEGStreamEncoder(outW, outH, opts.quality)
	}
	defer encoder.Close()

	if err := writeFrame(conn, pipeFrameDims, packDims(outW, outH)); err != nil {
		return err
	}
	if err := writeFrame(conn, pipeFrameState, []byte("capturing")); err != nil {
		return err
	}

	// DXGI：AcquireNextFrame 超时即节流（timeout ≤ 100ms）；GDI：ticker 节拍。
	// 首帧使用 2s 超时：静止桌面 DuplicateOutput 后仍需一次全屏脏区标记
	// 才能产出首帧，100ms 不够。
	acquireTimeout := frameInterval.Milliseconds()
	if acquireTimeout <= 0 || acquireTimeout > 100 {
		acquireTimeout = 100
	}
	firstAcquireTimeout := uint(2000)
	var tickC <-chan time.Time
	if gdi != nil {
		t := time.NewTicker(frameInterval)
		defer t.Stop()
		tickC = t.C
	}

	framesSinceKey := 0
	sentKey := false
	var lastFrame []byte
	// locked：处于锁屏 / 访问丢失状态（去重 0x03 locked 通告；重建并成功
	// 取到下一帧后通告 capturing 复位）。
	locked := false
	// 静止桌面自愈：MFT 有 ~gop 帧启动延迟，若期间桌面转静止，首帧可能
	// 被编码器内部吞掉而始终无输出。静止超时 ~1s 后强制重编码缓存帧。
	// 首关键帧出帧前按节拍持续驱动编码器；出帧后静止即完全静默。
	idleTicks, idleLimit := 0, 1
	// 首帧强制输出：即使桌面完全静止（DXGI 无脏区 / GDI 无差异），也必须
	// 捕获并编码至少一个 I 帧——新观众需要立即看到画面而非空白。
	firstFrame := true
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if tickC != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tickC:
			}
		}

		var frame []byte
		var err error
		if dxgi != nil {
			t := uint(acquireTimeout)
			if firstFrame {
				t = firstAcquireTimeout
			}
			frame, _, err = dxgi.AcquireFrame(t)
		} else {
			frame, _, err = gdi.AcquireFrame(0)
		}
		if err != nil {
			if errors.Is(err, ErrAccessLost) {
				if !locked {
					if werr := writeFrame(conn, pipeFrameState, []byte("locked")); werr != nil {
						return werr
					}
					locked = true
				}
				// 重建 duplication 后继续；连续失败则按秒退避重试。
				if rerr := dxgi.recreate(); rerr != nil {
					fmt.Fprintf(os.Stderr, "xnc-screen-helper: recreate duplication: %v\n", rerr)
					time.Sleep(time.Second)
				}
				continue
			}
			if errors.Is(err, ErrTimeout) {
				// 首帧尚未输出且桌面静止：用 GDI 直接截一帧强制输出，
				// 而非等待 DXGI 脏区（完全静止桌面 DXGI 永远不触发）。
				if firstFrame {
					if g, _, _, gerr := captureGDIFrame(); gerr == nil {
						// GDI 帧是全分辨率 BGRA，需要缩放到编码器尺寸
						scaled := scaleBGRA(g, srcW, srcH, outW, outH)
						if serr := sendEncoded(conn, encoder, scaled, gop, &framesSinceKey, &sentKey, false); serr == nil {
							firstFrame = false
							lastFrame = scaled
						}
					}
					continue
				}
				// 后续静止帧：MFT 启动缓冲自愈逻辑
				idleTicks++
				if !sentKey && lastFrame != nil && idleTicks >= idleLimit {
					if err := sendEncoded(conn, encoder, lastFrame, gop, &framesSinceKey, &sentKey, false); err != nil {
						return err
					}
				}
				continue
			}
			return fmt.Errorf("acquire: %w", err)
		}
		idleTicks = 0
		// 锁屏恢复：重建后首次成功取帧 → 通告 capturing，观众状态条复位。
		if locked {
			if werr := writeFrame(conn, pipeFrameState, []byte("capturing")); werr != nil {
				return werr
			}
			locked = false
		}
		if len(frame) < srcW*srcH*4 {
			continue
		}
		if outW != srcW {
			frame = scaleBGRA(frame, srcW, srcH, outW, outH)
		}
		lastFrame = frame

		if err := sendEncoded(conn, encoder, frame, gop, &framesSinceKey, &sentKey, false); err != nil {
			return err
		}
	}
}

// sendEncoded 编码一帧并按关键帧/增量帧类型写 pipe。编码器无输出（MFT
// 启动缓冲）时静默跳过。flipY 传递给编码器（BGRA 行序翻转）。
func sendEncoded(conn net.Conn, encoder frameEncoder, frame []byte, gop int, framesSinceKey *int, sentKey *bool, flipY bool) error {
	forceKey := !*sentKey || *framesSinceKey >= gop
	data, encErr := encoder.Encode(frame, forceKey, flipY)
	if encErr != nil {
		fmt.Fprintf(os.Stderr, "xnc-screen-helper: encode: %v\n", encErr)
		return nil
	}
	if len(data) == 0 {
		return nil // 编码器内部缓冲（MFT 启动延迟）
	}

	var frameType byte
	if encoder.LastFrameKey() {
		frameType = pipeFrameKey
		if spspps := encoder.SPSPPS(); len(spspps) > 0 {
			// 关键帧 = 缓存参数集 + VCL NALU。丢弃编码器输出自带的
			// SPS/PPS/AUD（type 7/8/9）——重复参数集与 AUD 前缀会触发
			// 部分浏览器 WebCodecs 解码器 "key frame required" 拒帧。
			data = append(append([]byte{}, spspps...), vclNALUs(data)...)
		}
		*framesSinceKey = 0
		*sentKey = true
	} else {
		frameType = pipeFrameDelta
		if len(encoder.SPSPPS()) > 0 { // H.264 模式统一 4 字节起始码
			data = vclNALUs(data)
		}
		(*framesSinceKey)++
	}
	return writeFrame(conn, frameType, data)
}

// vclNALUs 重排 Annex-B 码流：丢弃参数集与 AUD（type 7/8/9），其余 NALU
// 统一以 4 字节起始码输出。JPEG 回退模式（无参数集）不应调用。
func vclNALUs(data []byte) []byte {
	var out []byte
	i := 0
	for i < len(data) {
		hdr := -1
		for j := i; j+3 < len(data); j++ {
			if data[j] == 0 && data[j+1] == 0 {
				if data[j+2] == 1 {
					hdr = j + 3
					break
				}
				if data[j+2] == 0 && j+4 < len(data) && data[j+3] == 1 {
					hdr = j + 4
					break
				}
			}
		}
		if hdr < 0 || hdr >= len(data) {
			break
		}
		end := len(data)
		for j := hdr + 1; j+3 <= len(data); j++ {
			if data[j] == 0 && data[j+1] == 0 && (data[j+2] == 1 || (j+4 <= len(data) && data[j+2] == 0 && data[j+3] == 1)) {
				end = j
				break
			}
		}
		for end > hdr && data[end-1] == 0 { // 去起始码前导零
			end--
		}
		if t := data[hdr] & 0x1F; t != 7 && t != 8 && t != 9 && end > hdr {
			out = append(out, 0, 0, 0, 1)
			out = append(out, data[hdr:end]...)
		}
		i = end
	}
	return out
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
