//go:build windows

// capture_windows.go — 捕获主循环与共享 COM syscall 基础设施。
// 捕获源唯一：Windows.Graphics.Capture（capture_wgc_windows.go）——无
// DXGI/GDI 回退。WGC 不可用（Win10 < 1903 等）时走 placeholderLoop 报告
// 状态帧保活，不产出画面。
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

// ErrTimeout — timeout 内无新帧（桌面静止：合成器无更新）。
var ErrTimeout = errors.New("capture acquire timeout")

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

// newScreenCapturer 采集源梯子：DXGI（xnc-dda.dll，主路径）→ WGC（回退）。
// XNC_NO_DDA=1 可禁用 DDA（诊断用）。返回 (capturer, backend名, error)。
func newScreenCapturer() (screenCapturer, string, error) {
	if os.Getenv("XNC_NO_DDA") == "" {
		c, err := NewDDACapturer()
		if err == nil {
			return c, "dxgi", nil
		}
		fmt.Fprintf(os.Stderr, "xnc-screen-helper: dda unavailable (%v), trying wgc\n", err)
	}
	c, err := NewWGCCapturer()
	if err != nil {
		return nil, "", err
	}
	return c, "wgc", nil
}

// captureLoop：采集源（DDA 优先，WGC 回退）→ 编码器（H.264 MFT 优先，
// JPEG 帧流回退）→ 发分辨率 + capturing → 捕获-编码-推送循环。
//
//	静止：合成器无更新 → AcquireFrame 超时 → 不编码不出帧（自适应 0fps）
//	会话/设备失败：整体重建 capturer（秒级退避，有界）——分辨率切换 /
//	      设备移除等场景
//	关键帧：首帧 + 每 gop 帧（新观众可立即入流）
func captureLoopWith(ctx context.Context, conn net.Conn, opts captureOpts, cap screenCapturer, firstFrame []byte, ferr error) error {
	frameInterval := time.Second / time.Duration(opts.fps)

	srcW, srcH := cap.Dims()
	outW, outH := fitDims(srcW, srcH, opts.maxWidth)

	// 编码器在首帧之后创建：部分机器上 MFT 编码器初始化（COM 单元/MTA 交
	// 互）会阻断 WGC 帧池交付——首帧由调用方（listenPipe 之前）预取。
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

	// TryGetNextFrame 轮询节流（timeout ≤ 100ms）。首帧 2s 宽限
	// （WGC 通常毫秒级送达，宽限无害）。
	acquireTimeout := frameInterval.Milliseconds()
	if acquireTimeout <= 0 || acquireTimeout > 100 {
		acquireTimeout = 100
	}

	framesSinceKey := 0
	sentKey := false
	var lastFrame []byte
	if ferr == nil {
		w, h := cap.Dims()
		if len(firstFrame) >= w*h*4 {
			if outW != w {
				firstFrame = scaleBGRA(firstFrame, w, h, outW, outH)
			}
			lastFrame = firstFrame
			if err := sendEncoded(conn, encoder, firstFrame, gop, &framesSinceKey, &sentKey, false); err != nil {
				return err
			}
		}
	}
	// 静止桌面自愈：MFT 有 ~gop 帧启动缓冲，若期间桌面转静止，首帧可能
	// 被编码器内部吞掉而始终无输出。静止超时 ~1s 后强制重编码缓存帧。
	// 首关键帧出帧前按节拍持续驱动编码器；出帧后静止即完全静默。
	idleTicks, idleLimit := 0, 1
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cap == nil {
			// 上轮重建失败后的重试节拍。
			time.Sleep(time.Second)
			nc, _, nerr := newScreenCapturer()
			if nerr != nil {
				continue
			}
			cap = nc
			continue
		}

		t := uint(acquireTimeout)
		frame, aerr := cap.AcquireFrame(t)
		if aerr != nil {
			if errors.Is(aerr, ErrTimeout) {
				// 后续静止帧：MFT 启动缓冲自愈逻辑
				idleTicks++
				if !sentKey && lastFrame != nil && idleTicks >= idleLimit {
					if err := sendEncoded(conn, encoder, lastFrame, gop, &framesSinceKey, &sentKey, false); err != nil {
						return err
					}
				}
				continue
			}
			// 会话/设备级失败：整体重建（分辨率切换、设备移除等）。
			fmt.Fprintf(os.Stderr, "xnc-screen-helper: capture frame: %v, rebuilding\n", aerr)
			cap.Close()
			cap = nil
			continue
		}
		idleTicks = 0

		w, h := cap.Dims()
		if len(frame) < w*h*4 {
			continue
		}
		if outW != w {
			frame = scaleBGRA(frame, w, h, outW, outH)
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
	forceKey := !*sentKey || *framesSinceKey >= gop // 首输出前始终请求关键帧
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

// placeholderLoop WGC 不可用时的占位循环（每秒状态帧，保持 pipe 活性）。
func placeholderLoop(ctx context.Context, conn net.Conn, cause error) error {
	_, _ = fmt.Fprintf(os.Stderr, "xnc-screen-helper: wgc unavailable: %v\n", cause)
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

// fitDims 按 maxWidth 等比缩放并保证宽高为偶数（NV12/H.264 要求）。
func fitDims(w, h, maxWidth int) (int, int) {
	if maxWidth <= 0 || w <= maxWidth {
		return even(w), even(h)
	}
	ow := even(maxWidth)
	oh := even(h * ow / w)
	if oh < 2 {
		oh = 2
	}
	return ow, oh
}

func even(n int) int {
	if n < 2 {
		return 2
	}
	return n &^ 1
}

// scaleBGRA 最近邻缩放 BGRA 帧。
func scaleBGRA(src []byte, sw, sh, dw, dh int) []byte {
	dst := make([]byte, dw*dh*4)
	for y := 0; y < dh; y++ {
		sy := y * sh / dh
		dRow := dst[y*dw*4 : (y+1)*dw*4]
		sRow := src[sy*sw*4 : (sy+1)*sw*4]
		for x := 0; x < dw; x++ {
			sx := x * sw / dw
			copy(dRow[x*4:(x+1)*4], sRow[sx*4:(sx+1)*4])
		}
	}
	return dst
}
