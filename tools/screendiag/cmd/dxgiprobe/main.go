// dxgiprobe — DXGI Desktop Duplication 能力诊断（实验工具，非产品代码）。
//
// 回答实验矩阵的问题：当前上下文（用户/SYSTEM、console/RDP、哪个桌面）
// 里 DuplicateOutput 是否成功、静止桌面首帧延迟、帧时间线（内容帧 /
// 光标-only 帧 / 超时）、黑帧检测、duplication 重建后恢复延迟。
//
// 用法：dxgiprobe [-seconds 8] [-timeout 100] [-dump 3] [-recreate] [-json]
//
//	-seconds   采集时长（秒）
//	-timeout   单次 AcquireNextFrame 超时（毫秒）
//	-dump N    前 N 个内容帧存 BMP 到 %TEMP%\dxgiprobe\
//	-recreate  采集结束后销毁并重建 duplication，测恢复延迟
//	-json      结尾以 JSON 输出汇总到 stdout（过程日志走 stderr）
//
// DXGI COM 基建与 vtable 槽位复制自 agent/screen-helper 历史实现
// （commit 484e38d，槽位经 SDK 10.0.26100 头文件核实）。
//
//go:build windows

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---- COM 基建（复制自 helper，勿改槽位语义）----

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	iidIDXGIFactory1 = guid{0x770aae78, 0xf26f, 0x4dba, [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
	iidIDXGIOutput1  = guid{0x00cddea8, 0x939b, 0x4b83, [8]byte{0xa3, 0x40, 0xa6, 0x85, 0x22, 0x66, 0x66, 0xcc}}
	iidIDXGIOutput5  = guid{0x80a07424, 0xab52, 0x42eb, [8]byte{0x83, 0x3c, 0x0c, 0x42, 0xfd, 0x28, 0x2d, 0x98}}
)

type comPtr struct{ p unsafe.Pointer }

var nilPtr = comPtr{}

func (p comPtr) u() uintptr      { return uintptr(p.p) }
func (p comPtr) valid() bool     { return p.p != nil }

func (p comPtr) vtableSlot(slot int) uintptr {
	vt := *(*unsafe.Pointer)(p.p)
	return *(*uintptr)(unsafe.Add(vt, uintptr(slot)*unsafe.Sizeof(uintptr(0))))
}

func (p comPtr) call(slot int, a ...uintptr) (r1 uintptr, err error) {
	if !p.valid() {
		return 0, fmt.Errorf("nil COM interface")
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
	default:
		return 0, fmt.Errorf("unsupported arg count %d", len(a))
	}
	_, _ = r2, l
	if int32(r1) < 0 {
		return r1, fmt.Errorf("COM call slot %d: hr=0x%08X", slot, uint32(r1))
	}
	return r1, nil
}

func (p comPtr) queryInterface(iid *guid) (comPtr, error) {
	var out comPtr
	_, err := p.call(0, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out.p)))
	if err != nil {
		return nilPtr, err
	}
	return out, nil
}

func (p comPtr) release() {
	if p.valid() {
		_, _, _ = syscall.SyscallN(p.vtableSlot(2), p.u())
	}
}

// ---- vtable 槽位（SDK 头顺序核实，勿改）----

const (
	vtFactoryEnumAdapters1 = 12 // IDXGIFactory1: ...CreateSoftwareAdapter11,EnumAdapters1 12,IsCurrent 13
	vtAdapterEnumOutputs   = 7  // IDXGIAdapter1: IDXGIObject0-6,EnumOutputs7,GetDesc8,...
	vtAdapterGetDesc       = 8
	vtOutputGetDesc        = 7 // IDXGIOutput: IDXGIObject0-6,GetDesc7,...11 个方法至 17

	// IDXGIOutput1: DuplicateOutput 实测槽位 22（真 IDXGIOutput 有 15 个
	// 方法占 7..21；历史代码误按 20 调用 = CheckOverlaySupport 位置，永远
	// 返回 DXGI_ERROR_INVALID_CALL——DXGI 从未真正工作过的根因）。
	// 旁证：23=S_OK 无指针(SupportsDuplicateOutput=FALSE)、24=E_INVALIDARG。
	vtOutput1DuplicateOutput = 19
	// IDXGIOutputDuplication: GetDesc3,AcquireNextFrame4,GetFrameDirtyRects5,
	// GetFrameMoveRects6,GetFramePointerShape7,ReleaseFrame8。
	vtDupAcquireNextFrame   = 4
	vtDupReleaseFrame       = 10
	vtDeviceCreateTexture2D = 5
	vtContextMap            = 14
	vtContextUnmap          = 15
	vtContextCopyResource   = 47
)

// ---- HRESULT / D3D 常量 ----

const (
	dxgiErrAccessLost = 0x887A0026
	waitTimeout       = 0x80070102
	rpcEChangedMode   = 0x80010106

	d3dDriverTypeUnknown         = 0
	d3dDriverTypeHardware        = 1
	d3d11CreateDeviceBgraSupport = 0x20
	d3d11SdkVersion              = 7
	dxgiFormatB8G8R8A8UNorm      = 87
	d3d11UsageStaging            = 3
	d3d11CpuAccessRead           = 0x20000

	coinitApartmentThreaded = 0x2
)

// coinitMode 实际使用的 COM 套房模型；-mta 时切为 COINIT_MULTITHREADED(0)。
var coinitMode uintptr = coinitApartmentThreaded

// ---- 结构体（x64 精确布局）----

type rect struct{ Left, Top, Right, Bottom int32 }

func (r rect) width() int  { return int(r.Right - r.Left) }
func (r rect) height() int { return int(r.Bottom - r.Top) }

// dxgiAdapterDesc 对应 DXGI_ADAPTER_DESC。
type dxgiAdapterDesc struct {
	Description                  [128]uint16
	VendorID, DeviceID, SubSysID uint32
	Revision                     uint32
	DedicatedVideoMemory         uintptr
	DedicatedSystemMemory        uintptr
	SharedSystemMemory           uintptr
	AdapterLuid                  struct{ LowPart, HighPart int32 }
}

const (
	vendorAMD    = 0x1002
	vendorIntel  = 0x8086
	vendorNVIDIA = 0x10DE
	vendorMS     = 0x1414
)

func (d *dxgiAdapterDesc) vendor() string {
	switch d.VendorID {
	case vendorAMD:
		return "AMD"
	case vendorIntel:
		return "Intel"
	case vendorNVIDIA:
		return "NVIDIA"
	case vendorMS:
		return "Microsoft(RDP/Basic)"
	default:
		return fmt.Sprintf("0x%04X", d.VendorID)
	}
}

type dxgiOutputDesc struct {
	DeviceName         [32]uint16
	DesktopCoordinates rect
	AttachedToDesktop  uint32
	Rotation           uint32
	Monitor            uintptr
}

type d3d11Texture2DDesc struct {
	Width, Height, MipLevels, ArraySize uint32
	Format                              uint32
	SampleCount, SampleQuality          uint32
	Usage                               uint32
	BindFlags                           uint32
	CPUAccessFlags, MiscFlags           uint32
}

type d3d11MappedSubresource struct {
	pData      unsafe.Pointer
	RowPitch   uint32
	DepthPitch uint32
}

// dxgiOutduplFrameInfo 对应 DXGI_OUTDUPL_FRAME_INFO（平铺字段保偏移：
// LastPresentTime0 LastUpdateTime8 Accumulated16 RectsCoalesced20
// PointerX24 PointerY28 Visible32 pad36 MetaSize40）。
type dxgiOutduplFrameInfo struct {
	LastPresentTime   int64
	LastUpdateTime    int64
	AccumulatedFrames uint32
	RectsCoalesced    uint32
	PointerX          int32
	PointerY          int32
	PointerVisible    uint32
	_                 uint32
	TotalMetadataSize uint32
}

var (
	dxgiDLL  = windows.NewLazySystemDLL("dxgi.dll")
	d3d11DLL = windows.NewLazySystemDLL("d3d11.dll")
	ole32    = windows.NewLazySystemDLL("ole32.dll")

	procCreateDXGIFactory1 = dxgiDLL.NewProc("CreateDXGIFactory1")
	procD3D11CreateDevice  = d3d11DLL.NewProc("D3D11CreateDevice")
	procCoInitEx           = ole32.NewProc("CoInitializeEx")
	procCoUninit           = ole32.NewProc("CoUninitialize")
)

// ---- 采集器（精简自历史实现 + 诊断枚举）----

type capturer struct {
	factory, adapter, output, output1, duplication, device, context, staging comPtr
	width, height                                                            int
	outDesc                                                                  dxgiOutputDesc
	coInit                                                                   bool
}

// duplCombo 记录一次 adapter/output 组合的尝试结果（诊断枚举）。
type duplCombo struct {
	Adapter     int    `json:"adapter"`
	Output      int    `json:"output"`
	AdapterDesc string `json:"adapterDesc"`
	Vendor      string `json:"vendor"`
	Device      string `json:"device"`
	Geometry    string `json:"geometry"`
	Attached    bool   `json:"attached"`
	Duplicated  bool   `json:"duplicated"`
	NullAdapter bool   `json:"nullAdapter,omitempty"`
	Error       string `json:"error,omitempty"`
}

var duplCombos []duplCombo

// pipeline 一条可用的 duplication 资源集合。
type pipeline struct {
	ai, oi                                                           int
	adapter, output, output1, duplication, device, context, staging comPtr
	desc                                                             dxgiOutputDesc
	width, height                                                    int
	nullAdapter                                                      bool
}

// newCapturer 诊断式初始化：枚举全部 adapter/output 组合并逐一尝试
// DuplicateOutput，逐条打印结果（E1/E2/E4 核心诊断面）。全部失败时再试
// adapter=NULL 硬件设备兜底。
func newCapturer() (*capturer, error) {
	r, _, e := procCoInitEx.Call(0, coinitMode)
	coInit := r == 0
	if r != 0 && uint32(r) != rpcEChangedMode {
		return nil, fmt.Errorf("CoInitializeEx hr=0x%08X: %v", uint32(r), e)
	}
	c := &capturer{coInit: coInit}

	var factory comPtr
	r, _, e = procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidIDXGIFactory1)), uintptr(unsafe.Pointer(&factory.p)))
	if r != 0 {
		c.Close()
		return nil, fmt.Errorf("CreateDXGIFactory1 hr=0x%08X: %v", uint32(r), e)
	}
	c.factory = factory

	type outputInfo struct {
		ai, oi  int
		adapter comPtr
		output  comPtr
		desc    dxgiOutputDesc
		adesc   dxgiAdapterDesc
	}
	var attached []outputInfo

	for ai := 0; ai < 8; ai++ {
		var adapter comPtr
		if _, err := factory.call(vtFactoryEnumAdapters1, uintptr(ai), uintptr(unsafe.Pointer(&adapter.p))); err != nil {
			break
		}
		var adesc dxgiAdapterDesc
		if _, err := adapter.call(vtAdapterGetDesc, uintptr(unsafe.Pointer(&adesc))); err != nil {
			adapter.release()
			continue
		}
		fmt.Fprintf(os.Stderr, "dxgiprobe: adapter %d: %q [%s] vram=%dMB\n",
			ai, windows.UTF16ToString(adesc.Description[:]), adesc.vendor(), adesc.DedicatedVideoMemory>>20)
		adapterTaken := false
		for oi := 0; oi < 4; oi++ {
			var output comPtr
			if _, err := adapter.call(vtAdapterEnumOutputs, uintptr(oi), uintptr(unsafe.Pointer(&output.p))); err != nil {
				break
			}
			var desc dxgiOutputDesc
			if _, err := output.call(vtOutputGetDesc, uintptr(unsafe.Pointer(&desc))); err != nil {
				duplCombos = append(duplCombos, duplCombo{Adapter: ai, Output: oi,
					AdapterDesc: windows.UTF16ToString(adesc.Description[:]), Vendor: adesc.vendor(),
					Error: fmt.Sprintf("GetDesc: %v", err)})
				output.release()
				continue
			}
			geom := fmt.Sprintf("%dx%d@%d,%d", desc.DesktopCoordinates.width(), desc.DesktopCoordinates.height(),
				desc.DesktopCoordinates.Left, desc.DesktopCoordinates.Top)
			fmt.Fprintf(os.Stderr, "dxgiprobe:   output %d: %s %s attached=%v rot=%d\n",
				oi, windows.UTF16ToString(desc.DeviceName[:]), geom, desc.AttachedToDesktop != 0, desc.Rotation)
			if desc.AttachedToDesktop == 0 {
				duplCombos = append(duplCombos, duplCombo{Adapter: ai, Output: oi,
					AdapterDesc: windows.UTF16ToString(adesc.Description[:]), Vendor: adesc.vendor(),
					Device: windows.UTF16ToString(desc.DeviceName[:]), Geometry: geom, Error: "not attached"})
				output.release()
				continue
			}
			attached = append(attached, outputInfo{ai: ai, oi: oi, adapter: adapter, output: output, desc: desc, adesc: adesc})
			adapterTaken = true // output 所有权移交 attached，adapter 暂留
		}
		if !adapterTaken {
			adapter.release()
		}
	}

	// 对每个 attached 组合依序尝试（首个成功即胜出，不再尝试后续——
	// attached 列表持有全部 output 引用，统一在结尾清理）。
	var winner *pipeline
	winnerIdx := -1
	for i := range attached {
		oi := &attached[i]
		cb := duplCombo{Adapter: oi.ai, Output: oi.oi,
			AdapterDesc: windows.UTF16ToString(oi.adesc.Description[:]), Vendor: oi.adesc.vendor(),
			Device:      windows.UTF16ToString(oi.desc.DeviceName[:]),
			Geometry:    fmt.Sprintf("%dx%d@%d,%d", oi.desc.DesktopCoordinates.width(), oi.desc.DesktopCoordinates.height(), oi.desc.DesktopCoordinates.Left, oi.desc.DesktopCoordinates.Top),
			Attached:    true}
		pl, err := buildOnAdapter(oi.adapter, oi.output, oi.desc)
		if err != nil {
			cb.Error = err.Error()
			fmt.Fprintf(os.Stderr, "dxgiprobe:   -> duplicate FAILED: %v\n", err)
			duplCombos = append(duplCombos, cb)
			continue
		}
		pl.ai, pl.oi = oi.ai, oi.oi
		cb.Duplicated = true
		duplCombos = append(duplCombos, cb)
		winner, winnerIdx = pl, i
		break
	}

	// 兜底：adapter=NULL 硬件设备。
	if winner == nil {
		for i := range attached {
			oi := &attached[i]
			pl, err := buildNullAdapter(oi.output, oi.desc)
			if err != nil {
				fmt.Fprintf(os.Stderr, "dxgiprobe:   -> null-adapter duplicate FAILED: %v\n", err)
				continue
			}
			pl.ai, pl.oi, pl.nullAdapter = oi.ai, oi.oi, true
			fmt.Fprintf(os.Stderr, "dxgiprobe: NULL-adapter device duplicate OK (adapter%d/output%d)\n", oi.ai, oi.oi)
			duplCombos = append(duplCombos, duplCombo{Adapter: oi.ai, Output: oi.oi,
				Device: windows.UTF16ToString(oi.desc.DeviceName[:]), Attached: true, Duplicated: true, NullAdapter: true})
			winner, winnerIdx = pl, i
			break
		}
	}

	// 清理：非 winner 的 output 引用释放；winner 的移交 capturer。adapter
	// 引用按（ai 去重）同理——winner 的 adapter 归 capturer。
	winnerAI := -1
	if winner != nil {
		winnerAI = winner.ai
	}
	for i := range attached {
		if i != winnerIdx {
			attached[i].output.release()
		}
	}
	releasedAI := map[int]bool{}
	for i := range attached {
		ai := attached[i].ai
		if ai == winnerAI || releasedAI[ai] {
			continue
		}
		releasedAI[ai] = true
		attached[i].adapter.release()
	}

	if winner == nil {
		c.Close()
		msgs := ""
		for _, cb := range duplCombos {
			msgs += fmt.Sprintf("[a%do%d %s: %s]", cb.Adapter, cb.Output, cb.Geometry, firstLine(cb.Error))
		}
		if msgs == "" {
			msgs = "no outputs enumerated"
		}
		return nil, fmt.Errorf("all duplicate attempts failed: %s", msgs)
	}

	c.adapter, c.output, c.output1 = winner.adapter, winner.output, winner.output1
	c.device, c.context, c.duplication, c.staging = winner.device, winner.context, winner.duplication, winner.staging
	c.width, c.height = winner.width, winner.height
	c.outDesc = winner.desc
	fmt.Fprintf(os.Stderr, "dxgiprobe: using adapter %d/output %d (%dx%d rot=%d nullAdapter=%v)\n",
		winner.ai, winner.oi, c.width, c.height, winner.desc.Rotation, winner.nullAdapter)
	return c, nil
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

// buildOnAdapter 在指定 output 的 QI + adapter 设备上建 duplication。
// 顺序遵循 MS 样例：DuplicateOutput 先行；staging 尺寸取自 duplication
// 的 GetDesc（原生分辨率，与 DPI 虚拟化无关）。
func buildOnAdapter(adapter, output comPtr, desc dxgiOutputDesc) (*pipeline, error) {
	out1, err := output.queryInterface(&iidIDXGIOutput1)
	if err != nil {
		return nil, fmt.Errorf("QI IDXGIOutput1: %v", err)
	}
	pl := &pipeline{output1: out1}
	var featureLevel uint32
	r, _, _ := procD3D11CreateDevice.Call(
		adapter.u(), uintptr(d3dDriverTypeUnknown), 0,
		uintptr(d3d11CreateDeviceBgraSupport),
		0, 0, uintptr(d3d11SdkVersion),
		uintptr(unsafe.Pointer(&pl.device.p)), uintptr(unsafe.Pointer(&featureLevel)),
		uintptr(unsafe.Pointer(&pl.context.p)))
	if r != 0 {
		out1.release()
		return nil, fmt.Errorf("D3D11CreateDevice hr=0x%08X", uint32(r))
	}
	if _, err := out1.call(vtOutput1DuplicateOutput, pl.device.u(), uintptr(unsafe.Pointer(&pl.duplication.p))); err != nil {
		pl.context.release()
		pl.device.release()
		out1.release()
		return nil, fmt.Errorf("DuplicateOutput: %v", err)
	}
	w, h := duplDims(pl.duplication)
	if w == 0 || h == 0 {
		w, h = desc.DesktopCoordinates.width(), desc.DesktopCoordinates.height()
	}
	if err := pl.makeStaging(w, h); err != nil {
		pl.duplication.release()
		pl.context.release()
		pl.device.release()
		out1.release()
		return nil, err
	}
	pl.width, pl.height = w, h
	pl.adapter, pl.output, pl.desc = adapter, output, desc
	return pl, nil
}

// buildNullAdapter 以 adapter=NULL（硬件默认）建设备后 duplication。
func buildNullAdapter(output comPtr, desc dxgiOutputDesc) (*pipeline, error) {
	out1, err := output.queryInterface(&iidIDXGIOutput1)
	if err != nil {
		return nil, fmt.Errorf("QI IDXGIOutput1: %v", err)
	}
	pl := &pipeline{output1: out1}
	var featureLevel uint32
	r, _, _ := procD3D11CreateDevice.Call(
		0, uintptr(d3dDriverTypeHardware), 0,
		uintptr(d3d11CreateDeviceBgraSupport),
		0, 0, uintptr(d3d11SdkVersion),
		uintptr(unsafe.Pointer(&pl.device.p)), uintptr(unsafe.Pointer(&featureLevel)),
		uintptr(unsafe.Pointer(&pl.context.p)))
	if r != 0 {
		out1.release()
		return nil, fmt.Errorf("D3D11CreateDevice(null) hr=0x%08X", uint32(r))
	}
	if _, err := out1.call(vtOutput1DuplicateOutput, pl.device.u(), uintptr(unsafe.Pointer(&pl.duplication.p))); err != nil {
		pl.context.release()
		pl.device.release()
		out1.release()
		return nil, fmt.Errorf("DuplicateOutput: %v", err)
	}
	w, h := duplDims(pl.duplication)
	if w == 0 || h == 0 {
		w, h = desc.DesktopCoordinates.width(), desc.DesktopCoordinates.height()
	}
	if err := pl.makeStaging(w, h); err != nil {
		pl.duplication.release()
		pl.context.release()
		pl.device.release()
		out1.release()
		return nil, err
	}
	pl.width, pl.height = w, h
	pl.output, pl.desc = output, desc
	return pl, nil
}

func (pl *pipeline) makeStaging(w, h int) error {
	texDesc := d3d11Texture2DDesc{
		Width: uint32(w), Height: uint32(h),
		MipLevels: 1, ArraySize: 1, SampleCount: 1,
		Format:         dxgiFormatB8G8R8A8UNorm,
		Usage:          d3d11UsageStaging,
		CPUAccessFlags: d3d11CpuAccessRead,
	}
	if _, err := pl.device.call(vtDeviceCreateTexture2D, uintptr(unsafe.Pointer(&texDesc)), 0, uintptr(unsafe.Pointer(&pl.staging.p))); err != nil {
		return fmt.Errorf("CreateTexture2D(staging): %v", err)
	}
	return nil
}

// dxgiOutduplDesc 取 DXGI_OUTDUPL_DESC 头部（DXGI_MODE_DESC 的宽高）。
type dxgiOutduplDesc struct {
	Width, Height uint32
}

// duplDims 从 duplication GetDesc（槽 3）取原生分辨率。
func duplDims(dup comPtr) (int, int) {
	var d dxgiOutduplDesc
	if _, err := dup.call(3 /*GetDesc*/, uintptr(unsafe.Pointer(&d))); err != nil {
		return 0, 0
	}
	return int(d.Width), int(d.Height)
}

func (pl *pipeline) release() {
	pl.duplication.release()
	pl.staging.release()
	pl.context.release()
	pl.device.release()
	pl.output1.release()
	pl.output.release()
	pl.adapter.release()
}

// acquire 拉取一帧：返回 BGRA 像素（光标-only 帧为 nil）与帧信息。
func (c *capturer) acquire(timeoutMs uint) ([]byte, *dxgiOutduplFrameInfo, error) {
	var info dxgiOutduplFrameInfo
	var texture comPtr
	r1, err := c.duplication.call(vtDupAcquireNextFrame, uintptr(timeoutMs),
		uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&texture.p)))
	if err != nil {
		hr := uint32(r1)
		if hr == dxgiErrAccessLost {
			return nil, nil, errAccessLost
		}
		if hr == waitTimeout {
			return nil, nil, errTimeout
		}
		return nil, nil, fmt.Errorf("AcquireNextFrame hr=0x%08X: %v", hr, err)
	}
	defer c.releaseFrame()

	if !texture.valid() {
		return nil, &info, nil // 光标/元数据-only 帧（无新纹理）
	}
	if _, err := c.context.call(vtContextCopyResource, c.staging.u(), texture.u()); err != nil {
		return nil, &info, fmt.Errorf("CopyResource: %v", err)
	}
	texture.release()

	var mapped d3d11MappedSubresource
	if _, err := c.context.call(vtContextMap, c.staging.u(), 0, 1, 0, uintptr(unsafe.Pointer(&mapped))); err != nil {
		return nil, &info, fmt.Errorf("Map: %v", err)
	}
	defer c.context.call(vtContextUnmap, c.staging.u(), 0)

	stride := c.width * 4
	buf := make([]byte, c.height*stride)
	src := unsafe.Slice((*byte)(mapped.pData), int(mapped.RowPitch)*c.height)
	for row := 0; row < c.height; row++ {
		copy(buf[row*stride:(row+1)*stride], src[row*int(mapped.RowPitch):])
	}
	return buf, &info, nil
}

func (c *capturer) releaseFrame() {
	if c.duplication.valid() {
		_, _ = c.duplication.call(vtDupReleaseFrame)
	}
}

func (c *capturer) recreate() error {
	c.releaseFrame()
	if c.duplication.valid() {
		c.duplication.release()
		c.duplication = nilPtr
	}
	var dup comPtr
	if _, err := c.output1.call(vtOutput1DuplicateOutput, c.device.u(), uintptr(unsafe.Pointer(&dup.p))); err != nil {
		return fmt.Errorf("DuplicateOutput(recreate): %v", err)
	}
	c.duplication = dup
	return nil
}

func (c *capturer) Close() {
	for _, p := range []comPtr{c.duplication, c.staging, c.context, c.device, c.output1, c.output, c.adapter, c.factory} {
		p.release()
	}
	c.duplication, c.staging, c.context, c.device = nilPtr, nilPtr, nilPtr, nilPtr
	c.output1, c.output, c.adapter, c.factory = nilPtr, nilPtr, nilPtr, nilPtr
	if c.coInit {
		procCoUninit.Call()
		c.coInit = false
	}
}

var (
	errTimeout    = fmt.Errorf("wait timeout (desktop static)")
	errAccessLost = fmt.Errorf("dxgi access lost")
)

// ---- 上下文信息 ----

var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	advapi32 = windows.NewLazySystemDLL("advapi32.dll")
	user32   = windows.NewLazySystemDLL("user32.dll")

	procPIDToS = kernel32.NewProc("ProcessIdToSessionId")
	procGetPID = kernel32.NewProc("GetCurrentProcessId")
	procGetTID = kernel32.NewProc("GetCurrentThreadId")
	procGetUN  = advapi32.NewProc("GetUserNameW")
	procGetSM  = user32.NewProc("GetSystemMetrics")
	procGTD    = user32.NewProc("GetThreadDesktop")
	procOID    = user32.NewProc("OpenInputDesktop")
	procGUOI   = user32.NewProc("GetUserObjectInformationW")
	procCD     = user32.NewProc("CloseDesktop")
	procSPDAC  = user32.NewProc("SetProcessDpiAwarenessContext")
)

// setDpiAware 令进程 Per-Monitor V2 DPI aware。DPI-unaware 进程在缩放屏
// 上 GetDesc 坐标被虚拟化且 DuplicateOutput 报 INVALID_CALL（TB16G7
// 3200x2000@200% 历史失败的头号嫌疑）。
func setDpiAware() bool {
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = (HANDLE)-4
	r, _, _ := procSPDAC.Call(^uintptr(3))
	return r != 0
}

func sessionID() uint32 {
	var sid uint32
	pid, _, _ := procGetPID.Call()
	procPIDToS.Call(pid, uintptr(unsafe.Pointer(&sid)))
	return sid
}

func userName() string {
	n := uint32(256)
	b := make([]uint16, n)
	if r, _, _ := procGetUN.Call(uintptr(unsafe.Pointer(&b)), uintptr(unsafe.Pointer(&n))); r == 0 && n <= 256 {
		return windows.UTF16ToString(b[:n])
	}
	return "?"
}

func remoteSession() bool {
	r, _, _ := procGetSM.Call(0x1000) // SM_REMOTESESSION
	return r != 0
}

func desktopName(h uintptr) string {
	if h == 0 {
		return "(null)"
	}
	var n uint32
	procGUOI.Call(h, 2 /*UOI_NAME*/, 0, 0, uintptr(unsafe.Pointer(&n)))
	if n == 0 || n > 512 {
		return "?"
	}
	b := make([]uint16, (n+1)/2+1)
	var got uint32
	if r, _, _ := procGUOI.Call(h, 2, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)*2), uintptr(unsafe.Pointer(&got))); r == 0 {
		return "?"
	}
	return windows.UTF16ToString(b)
}

func printContext() map[string]any {
	tid, _, _ := procGetTID.Call()
	htd, _, _ := procGTD.Call(tid)
	// OpenInputDesktop：安全桌面激活时非 SYSTEM 打不开——失败本身是信号。
	inputName, inputOpened := "", false
	if h, _, e := procOID.Call(0, 0, 0x00020000 /*READ_CONTROL*/); h != 0 {
		inputOpened = true
		inputName = desktopName(h)
		procCD.Call(h)
	} else {
		inputName = fmt.Sprintf("(open failed: %v)", e)
	}
	ctx := map[string]any{
		"user":                userName(),
		"envUser":             os.Getenv("USERNAME"),
		"session":             sessionID(),
		"remote":              remoteSession(),
		"threadDesktop":       desktopName(htd),
		"inputDesktop":        inputName,
		"inputDesktopOpened":  inputOpened,
	}
	j, _ := json.Marshal(ctx)
	fmt.Fprintf(os.Stderr, "dxgiprobe: context %s\n", j)
	return ctx
}

// ---- 黑帧检测与 BMP dump ----

func blackFrame(bgra []byte) bool {
	for i := 0; i+3 < len(bgra); i += 512 { // 每 128 像素采样一点
		if bgra[i] != 0 || bgra[i+1] != 0 || bgra[i+2] != 0 {
			return false
		}
	}
	return true
}

func dumpBMP(path string, bgra []byte, w, h int) error {
	hdr := make([]byte, 54)
	hdr[0], hdr[1] = 'B', 'M'
	le := func(off int, v uint32) {
		hdr[off] = byte(v)
		hdr[off+1] = byte(v >> 8)
		hdr[off+2] = byte(v >> 16)
		hdr[off+3] = byte(v >> 24)
	}
	le(2, uint32(54+len(bgra)))
	le(10, 54)
	le(14, 40)
	le(18, uint32(w))
	le(22, uint32(-h)) // 负高度 = top-down
	hdr[26], hdr[28] = 1, 32
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(hdr); err != nil {
		return err
	}
	_, err = f.Write(bgra)
	return err
}

// scanDuplicateSlots 经验扫描：对 QI 得到的 IDXGIOutput1 逐槽位以
// DuplicateOutput(device, &ptr) 参数调用 16..22，报告每个槽的 HRESULT 与
// 是否写出接口指针——正槽位返回 S_OK 且指针非空。
func scanDuplicateSlots() {
	r, _, e := procCoInitEx.Call(0, coinitMode)
	coInit := r == 0
	if r != 0 && uint32(r) != rpcEChangedMode {
		fmt.Fprintf(os.Stderr, "dxgiprobe: CoInitializeEx: %v\n", e)
		return
	}
	if coInit {
		defer procCoUninit.Call()
	}
	var factory comPtr
	if r, _, e := procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidIDXGIFactory1)), uintptr(unsafe.Pointer(&factory.p))); r != 0 {
		fmt.Fprintf(os.Stderr, "dxgiprobe: factory: %v\n", e)
		return
	}
	defer factory.release()
	var adapter comPtr
	if _, err := factory.call(vtFactoryEnumAdapters1, 0, uintptr(unsafe.Pointer(&adapter.p))); err != nil {
		fmt.Fprintf(os.Stderr, "dxgiprobe: enum adapter: %v\n", err)
		return
	}
	defer adapter.release()
	var output comPtr
	if _, err := adapter.call(vtAdapterEnumOutputs, 0, uintptr(unsafe.Pointer(&output.p))); err != nil {
		fmt.Fprintf(os.Stderr, "dxgiprobe: enum output: %v\n", err)
		return
	}
	defer output.release()
	var desc dxgiOutputDesc
	if _, err := output.call(vtOutputGetDesc, uintptr(unsafe.Pointer(&desc))); err != nil {
		fmt.Fprintf(os.Stderr, "dxgiprobe: GetDesc: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "dxgiprobe: scan on %s %dx%d attached=%v\n",
		windows.UTF16ToString(desc.DeviceName[:]),
		desc.DesktopCoordinates.width(), desc.DesktopCoordinates.height(), desc.AttachedToDesktop != 0)

	out1, err := output.queryInterface(&iidIDXGIOutput1)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dxgiprobe: QI output1: %v\n", err)
		return
	}
	defer out1.release()

	for slot := 16; slot <= 24; slot++ {
		// 每槽全新 device：扫描调用的邻近方法（如 GetGammaControl）可能把
		// 出参写进我们的指针所指内存，复用 device 会被腐蚀。
		var device, context comPtr
		var fl uint32
		r, _, _ = procD3D11CreateDevice.Call(adapter.u(), uintptr(d3dDriverTypeUnknown), 0,
			uintptr(d3d11CreateDeviceBgraSupport), 0, 0, uintptr(d3d11SdkVersion),
			uintptr(unsafe.Pointer(&device.p)), uintptr(unsafe.Pointer(&fl)), uintptr(unsafe.Pointer(&context.p)))
		if r != 0 {
			fmt.Fprintf(os.Stderr, "dxgiprobe: slot %d device hr=0x%08X\n", slot, uint32(r))
			continue
		}
		var ptr comPtr
		fn := out1.vtableSlot(slot)
		r1, _, _ := syscall.SyscallN(fn, out1.u(), device.u(), uintptr(unsafe.Pointer(&ptr.p)))
		fmt.Fprintf(os.Stderr, "dxgiprobe: slot %2d -> hr=0x%08X ptr=%v\n", slot, uint32(r1), ptr.valid())
		if ptr.valid() {
			ptr.release()
		}
		device.release()
		context.release()
	}
}

// scanDupMethods 变体矩阵：新旧 Duplicate API × 设备标志，报告每个组合
// 的 HRESULT 与是否拿到真接口指针（Release 一次验证指针活性）。
func scanDupMethods() {
	r, _, e := procCoInitEx.Call(0, coinitMode)
	coInit := r == 0
	if r != 0 && uint32(r) != rpcEChangedMode {
		fmt.Fprintf(os.Stderr, "dxgiprobe: CoInitializeEx: %v\n", e)
		return
	}
	if coInit {
		defer procCoUninit.Call()
	}
	var factory comPtr
	if r, _, e := procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidIDXGIFactory1)), uintptr(unsafe.Pointer(&factory.p))); r != 0 {
		fmt.Fprintf(os.Stderr, "dxgiprobe: factory: %v\n", e)
		return
	}
	defer factory.release()
	var adapter comPtr
	if _, err := factory.call(vtFactoryEnumAdapters1, 0, uintptr(unsafe.Pointer(&adapter.p))); err != nil {
		fmt.Fprintf(os.Stderr, "dxgiprobe: enum adapter: %v\n", err)
		return
	}
	defer adapter.release()
	var output comPtr
	if _, err := adapter.call(vtAdapterEnumOutputs, 0, uintptr(unsafe.Pointer(&output.p))); err != nil {
		fmt.Fprintf(os.Stderr, "dxgiprobe: enum output: %v\n", err)
		return
	}
	defer output.release()
	out1, err := output.queryInterface(&iidIDXGIOutput1)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dxgiprobe: QI out1: %v\n", err)
		return
	}
	defer out1.release()
	out5, err5 := output.queryInterface(&iidIDXGIOutput5)
	fmt.Fprintf(os.Stderr, "dxgiprobe: QI out5: err=%v valid=%v\n", err5, out5.valid())
	if out5.valid() {
		defer out5.release()
	}

	try := func(name string, fn func(device comPtr) (comPtr, uintptr)) {
		for _, flags := range []uintptr{uintptr(d3d11CreateDeviceBgraSupport), 0} {
			var device, context comPtr
			var fl uint32
			r, _, _ := procD3D11CreateDevice.Call(adapter.u(), uintptr(d3dDriverTypeUnknown), 0,
				flags, 0, 0, uintptr(d3d11SdkVersion),
				uintptr(unsafe.Pointer(&device.p)), uintptr(unsafe.Pointer(&fl)), uintptr(unsafe.Pointer(&context.p)))
			if r != 0 {
				fmt.Fprintf(os.Stderr, "dxgiprobe: %s flags=0x%X device hr=0x%08X\n", name, uint32(flags), uint32(r))
				continue
			}
			dup, hr := fn(device)
			ok := dup.valid()
			fmt.Fprintf(os.Stderr, "dxgiprobe: %-16s flags=0x%-2X -> hr=0x%08X dup=%v", name, uint32(flags), hr, ok)
			if ok {
				// 指针活性验证：调 Release（槽 2）并看返回值是否合理。
				if _, _, r1 := syscall.SyscallN(dup.vtableSlot(2), dup.u()); r1 > 0 && r1 < 0x1000 {
					fmt.Fprintf(os.Stderr, " (release rc=%d, live)", r1)
				} else {
					fmt.Fprintf(os.Stderr, " (release rc=%#x, SUSPECT)", r1)
				}
				dup.p = nil
			}
			fmt.Fprintln(os.Stderr)
			context.release()
			device.release()
		}
	}

	try("DuplicateOutput", func(device comPtr) (comPtr, uintptr) {
		var dup comPtr
		r1, err := out1.call(vtOutput1DuplicateOutput, device.u(), uintptr(unsafe.Pointer(&dup.p)))
		if err != nil {
			return dup, r1
		}
		return dup, 0
	})
	if out5.valid() {
		try("DuplicateOutput1", func(device comPtr) (comPtr, uintptr) {
			var dup comPtr
			r1, err := out5.call(23 /*DuplicateOutput1*/, device.u(), 0, 0, 0, uintptr(unsafe.Pointer(&dup.p)))
			if err != nil {
				return dup, r1
			}
			return dup, 0
		})
		try("DuplicateOutput1+fmt", func(device comPtr) (comPtr, uintptr) {
			var dup comPtr
			var fmts [1]uint32 = [1]uint32{dxgiFormatB8G8R8A8UNorm}
			r1, err := out5.call(23 /*DuplicateOutput1*/, device.u(), 0, 1,
				uintptr(unsafe.Pointer(&fmts[0])), uintptr(unsafe.Pointer(&dup.p)))
			if err != nil {
				return dup, r1
			}
			return dup, 0
		})
	}
}

// scan3 增量二分：同一对象/进程状态上逐步加回嫌疑步骤，每步重试
// DuplicateOutput——破坏者现形。顺序：裸 → +adapterGetDesc → +outputGetDesc
// → +枚举adapter1 →（进程级）printContext。
func scan3() {
	try := func(name string, opts bareOpts) {
		c, err := newCapturerBare(opts)
		ok := err == nil
		if ok {
			c.Close()
		} else {
			fmt.Fprintf(os.Stderr, "dxgiprobe: [scan3] %s err: %v\n", name, err)
		}
		fmt.Fprintf(os.Stderr, "dxgiprobe: [scan3] %-28s duplicate=%v\n", name, ok)
	}
	try("bare", bareOpts{})
	try("+adapterGetDesc", bareOpts{adapterDesc: true})
	try("+outputGetDesc", bareOpts{adapterDesc: true, outputDesc: true})
	try("+enumAdapter1", bareOpts{adapterDesc: true, outputDesc: true, extraEnum: true})
	try("+qiOut5", bareOpts{qiOut5: true})
	try("+qiOut5+descs", bareOpts{adapterDesc: true, outputDesc: true, qiOut5: true})
	printContext()
	try("+printContext", bareOpts{adapterDesc: true, outputDesc: true, extraEnum: true})
	try("+printContext+qiOut5", bareOpts{adapterDesc: true, outputDesc: true, qiOut5: true})
	setDpiAware()
	try("+dpiAware+qiOut5", bareOpts{adapterDesc: true, outputDesc: true, qiOut5: true})
}

type bareOpts struct {
	adapterDesc bool
	outputDesc  bool
	extraEnum   bool
	qiOut5      bool
}

// newCapturerBare — scan2 的成功骨架 + 可选嫌疑步骤（同对象作用）。
func newCapturerBare(opts bareOpts) (*capturer, error) {
	r, _, e := procCoInitEx.Call(0, coinitMode)
	coInit := r == 0
	if r != 0 && uint32(r) != rpcEChangedMode {
		return nil, fmt.Errorf("CoInitializeEx: %v", e)
	}
	c := &capturer{coInit: coInit}
	var factory comPtr
	if r, _, e = procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidIDXGIFactory1)), uintptr(unsafe.Pointer(&factory.p))); r != 0 {
		c.Close()
		return nil, fmt.Errorf("factory hr=0x%08X", uint32(r))
	}
	c.factory = factory
	var adapter comPtr
	if _, err := factory.call(vtFactoryEnumAdapters1, 0, uintptr(unsafe.Pointer(&adapter.p))); err != nil {
		c.Close()
		return nil, err
	}
	c.adapter = adapter
	if opts.adapterDesc {
		var ad dxgiAdapterDesc
		if _, err := adapter.call(vtAdapterGetDesc, uintptr(unsafe.Pointer(&ad))); err != nil {
			c.Close()
			return nil, err
		}
	}
	var output comPtr
	if _, err := adapter.call(vtAdapterEnumOutputs, 0, uintptr(unsafe.Pointer(&output.p))); err != nil {
		c.Close()
		return nil, err
	}
	c.output = output
	if opts.outputDesc {
		var od dxgiOutputDesc
		if _, err := output.call(vtOutputGetDesc, uintptr(unsafe.Pointer(&od))); err != nil {
			c.Close()
			return nil, err
		}
	}
	if opts.extraEnum {
		var a1 comPtr
		if _, err := factory.call(vtFactoryEnumAdapters1, 1, uintptr(unsafe.Pointer(&a1.p))); err == nil {
			a1.release()
		}
	}
	out1, err := output.queryInterface(&iidIDXGIOutput1)
	if err != nil {
		c.Close()
		return nil, err
	}
	c.output1 = out1
	if opts.qiOut5 {
		out5, err5 := output.queryInterface(&iidIDXGIOutput5)
		if err5 == nil && out5.valid() {
			out5.release()
		}
	}
	var featureLevel uint32
	r, _, _ = procD3D11CreateDevice.Call(adapter.u(), uintptr(d3dDriverTypeUnknown), 0,
		uintptr(d3d11CreateDeviceBgraSupport), 0, 0, uintptr(d3d11SdkVersion),
		uintptr(unsafe.Pointer(&c.device.p)), uintptr(unsafe.Pointer(&featureLevel)),
		uintptr(unsafe.Pointer(&c.context.p)))
	if r != 0 {
		c.Close()
		return nil, fmt.Errorf("device hr=0x%08X", uint32(r))
	}
	if _, err := out1.call(vtOutput1DuplicateOutput, c.device.u(), uintptr(unsafe.Pointer(&c.duplication.p))); err != nil {
		c.Close()
		return nil, fmt.Errorf("DuplicateOutput: %v", err)
	}
	w, h := duplDims(c.duplication)
	if w == 0 || h == 0 {
		c.Close()
		return nil, fmt.Errorf("bad dup dims")
	}
	texDesc := d3d11Texture2DDesc{
		Width: uint32(w), Height: uint32(h),
		MipLevels: 1, ArraySize: 1, SampleCount: 1,
		Format:         dxgiFormatB8G8R8A8UNorm,
		Usage:          d3d11UsageStaging,
		CPUAccessFlags: d3d11CpuAccessRead,
	}
	if _, err := c.device.call(vtDeviceCreateTexture2D, uintptr(unsafe.Pointer(&texDesc)), 0, uintptr(unsafe.Pointer(&c.staging.p))); err != nil {
		c.Close()
		return nil, fmt.Errorf("CreateTexture2D: %v", err)
	}
	c.width, c.height = w, h
	return c, nil
}

// ---- 主流程 ----

func main() {
	seconds := flag.Int("seconds", 8, "capture duration in seconds")
	timeoutMs := flag.Int("timeout", 100, "per-acquire timeout in ms")
	dumpN := flag.Int("dump", 0, "dump first N content frames as BMP")
	recreate := flag.Bool("recreate", false, "recreate duplication and measure recovery")
	jsonOut := flag.Bool("json", false, "emit JSON summary on stdout")
	scan := flag.Bool("scan", false, "scan DuplicateOutput vtable slots 16..22 then exit")
	scan2 := flag.Bool("scan2", false, "variant matrix: DuplicateOutput/DuplicateOutput1 x device flags")
	scan3Flag := flag.Bool("scan3", false, "incremental bisect: add suspect steps to known-good skeleton")
	mta := flag.Bool("mta", false, "use COINIT_MULTITHREADED instead of STA")
	dpiAware := flag.Bool("dpi", false, "SetProcessDpiAwarenessContext(PMv2) before init")
	flag.Parse()

	if *mta {
		coinitMode = 0
	}
	if *scan {
		scanDuplicateSlots()
		return
	}
	if *scan2 {
		scanDupMethods()
		return
	}
	if *scan3Flag {
		scan3()
		return
	}

	ctx := printContext()
	ctx["dpiAware"] = false
	if *dpiAware {
		ctx["dpiAware"] = setDpiAware()
	}
	summary := map[string]any{"context": ctx, "init": nil}

	c, err := newCapturer()
	if err != nil {
		fmt.Fprintf(os.Stderr, "dxgiprobe: INIT FAILED: %v\n", err)
		summary["init"] = map[string]any{"ok": false, "error": err.Error()}
		summary["combos"] = duplCombos
		if *jsonOut {
			j, _ := json.Marshal(summary)
			fmt.Println(string(j))
		}
		os.Exit(1)
	}
	defer c.Close()
	summary["init"] = map[string]any{"ok": true, "width": c.width, "height": c.height}
	summary["combos"] = duplCombos

	var dumpDir string
	if *dumpN > 0 {
		dumpDir = filepath.Join(os.TempDir(), "dxgiprobe")
		if err := os.MkdirAll(dumpDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "dxgiprobe: mkdir: %v\n", err)
			*dumpN = 0
		}
	}

	var recreateMs int64 = -1
	var firstFrameMs, firstContentMs int64
	var timeouts, cursorOnly, contentFrames, blackFrames, accessLost, recreates, pointerMoves int
	var lastPtrX, lastPtrY int32 = -1, -1
	dumped := 0
	start := time.Now()
	failed := false

	for !failed && time.Since(start) < time.Duration(*seconds)*time.Second {
		buf, info, err := c.acquire(uint(*timeoutMs))
		ms := time.Since(start).Milliseconds()
		switch {
		case err == errTimeout:
			timeouts++
			continue
		case err == errAccessLost:
			accessLost++
			fmt.Fprintf(os.Stderr, "dxgiprobe: t=%dms ACCESS_LOST -> recreate\n", ms)
			if rerr := c.recreate(); rerr != nil {
				fmt.Fprintf(os.Stderr, "dxgiprobe: recreate failed: %v\n", rerr)
				failed = true
			} else {
				recreates++
			}
			continue
		case err != nil:
			fmt.Fprintf(os.Stderr, "dxgiprobe: t=%dms acquire error: %v\n", ms, err)
			failed = true
			continue
		}

		if firstFrameMs == 0 {
			firstFrameMs = ms
		}
		if info.PointerVisible != 0 && (info.PointerX != lastPtrX || info.PointerY != lastPtrY) {
			pointerMoves++
			lastPtrX, lastPtrY = info.PointerX, info.PointerY
		}

		if buf == nil {
			cursorOnly++
			fmt.Fprintf(os.Stderr, "dxgiprobe: t=%dms CURSOR-ONLY ptr=(%d,%d,v=%d) meta=%d\n",
				ms, info.PointerX, info.PointerY, info.PointerVisible, info.TotalMetadataSize)
			continue
		}

		contentFrames++
		if firstContentMs == 0 {
			firstContentMs = ms
		}
		isBlack := blackFrame(buf)
		if isBlack {
			blackFrames++
		}
		fmt.Fprintf(os.Stderr, "dxgiprobe: t=%dms FRAME#%d %dx%d%s present=%d accum=%d ptr=(%d,%d,v=%d) meta=%d\n",
			ms, contentFrames, c.width, c.height, map[bool]string{true: " BLACK", false: ""}[isBlack],
			info.LastPresentTime, info.AccumulatedFrames,
			info.PointerX, info.PointerY, info.PointerVisible, info.TotalMetadataSize)
		if dumped < *dumpN {
			p := filepath.Join(dumpDir, fmt.Sprintf("frame%02d.bmp", dumped))
			if err := dumpBMP(p, buf, c.width, c.height); err != nil {
				fmt.Fprintf(os.Stderr, "dxgiprobe: dump: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "dxgiprobe: dumped %s\n", p)
			}
			dumped++
		}
	}

	if *recreate && !failed {
		fmt.Fprintf(os.Stderr, "dxgiprobe: recreating duplication...\n")
		if err := c.recreate(); err != nil {
			fmt.Fprintf(os.Stderr, "dxgiprobe: recreate: %v\n", err)
		} else {
			t := time.Now()
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				buf, info, err := c.acquire(200)
				if err == errTimeout {
					continue
				}
				if err != nil {
					fmt.Fprintf(os.Stderr, "dxgiprobe: post-recreate acquire: %v\n", err)
					break
				}
				recreateMs = time.Since(t).Milliseconds()
				fmt.Fprintf(os.Stderr, "dxgiprobe: post-recreate first frame in %dms (tex=%v present=%d)\n",
					recreateMs, buf != nil, info.LastPresentTime)
				break
			}
		}
	}

	summary["run"] = map[string]any{
		"seconds":           *seconds,
		"firstFrameMs":      firstFrameMs,
		"firstContentMs":    firstContentMs,
		"timeouts":          timeouts,
		"contentFrames":     contentFrames,
		"cursorOnlyFrames":  cursorOnly,
		"blackFrames":       blackFrames,
		"accessLost":        accessLost,
		"recreates":         recreates,
		"pointerMoves":      pointerMoves,
		"recreateRecoverMs": recreateMs,
	}
	if *jsonOut {
		j, _ := json.Marshal(summary)
		fmt.Println(string(j))
	} else {
		fmt.Fprintf(os.Stderr, "dxgiprobe: summary %+v\n", summary["run"])
	}
}
