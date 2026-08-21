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
	"io"
	"net"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---- 哨兵错误 ----

// ErrTimeout — timeout 内无新帧（桌面静止：合成器无更新）。
var ErrTimeout = errors.New("capture acquire timeout")

// ErrSecureDesktop 安全桌面（UAC/Ctrl+Alt+Del）激活：画面不可得。
// 调用方应向观众发 locked 状态并按节拍重试，桌面切回后自然恢复。
var ErrSecureDesktop = errors.New("secure desktop active")

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
	// 线程钉扎：安全桌面路径的 SetThreadDesktop/GDI 句柄是线程状态，
	// goroutine 迁移会使其失效（UAC 期间帧流冻结事故）。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	frameInterval := time.Second / time.Duration(opts.fps)

	srcW, srcH := cap.Dims()
	outW, outH := fitDims(srcW, srcH, opts.maxWidth)

	// 编码器在首帧之后创建：部分机器上 MFT 编码器初始化（COM 单元/MTA 交
	// 互）会阻断 WGC 帧池交付——首帧由调用方（listenPipe 之前）预取。
	encoder, eerr := newH264Encoder(outW, outH, bitrateFor(opts.quality), opts.fps*2)
	if eerr != nil {
		fmt.Fprintf(os.Stderr, "xnc-screen-helper: h264 mft unavailable (%v), jpeg fallback\n", eerr)
		encoder = newJPEGStreamEncoder(outW, outH, opts.quality)
	}
	defer encoder.Close()

	// fps 节流 + 时间基 GOP：光标-only 合成帧可按输入设备频率（~125Hz）
	// 到达，编码按帧间隔限速；静止自适应（0fps）与突发帧共存时按帧数
	// 计 GOP 会时间扭曲，改为距上一关键帧 ≥2s 强制 IDR。
	gop := &gopState{}
	var lastEncodeAt time.Time

	// 可观测性：XNC_DUMP_H264=path 把最终 Annex-B 码流（整形后、即管道
	// 实发字节）落盘——ffplay 直接可播，30 秒内二分定位"采集/编码侧还是
	// 下游"的现场分界线工具。周期计数器（10s）走 stderr 进 agent 日志。
	if p := os.Getenv("XNC_DUMP_H264"); p != "" {
		if f, err := os.Create(p); err == nil {
			dumpFile = f
			defer func() { _ = f.Close(); dumpFile = nil }()
			fmt.Fprintf(os.Stderr, "xnc-screen-helper: h264 dump -> %s\n", p)
		} else {
			fmt.Fprintf(os.Stderr, "xnc-screen-helper: dump open failed: %v\n", err)
		}
	}
	statTick := time.NewTicker(10 * time.Second)
	defer statTick.Stop()
	go func() {
		for range statTick.C {
			fmt.Fprintf(os.Stderr, "xnc-screen-helper: stats sent=%d key=%d delta=%d encErr=%d bytes=%d\n",
				statFrames, statKeys, statDeltas, statEncErrs, statBytes)
		}
	}()

	pacedSend := func(frame []byte) error {
		if !lastEncodeAt.IsZero() {
			if wait := frameInterval - time.Since(lastEncodeAt); wait > 0 {
				time.Sleep(wait)
			}
		}
		err := sendEncoded(conn, encoder, frame, gop)
		lastEncodeAt = time.Now()
		return err
	}
	go func() {
		for range statTick.C {
			fmt.Fprintf(os.Stderr, "xnc-screen-helper: stats sent=%d key=%d delta=%d encErr=%d bytes=%d\n",
				statFrames, statKeys, statDeltas, statEncErrs, statBytes)
		}
	}()

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

	var lastFrame []byte
	if ferr == nil {
		w, h := cap.Dims()
		if len(firstFrame) >= w*h*4 {
			if outW != w {
				firstFrame = scaleBGRA(firstFrame, w, h, outW, outH)
			}
			lastFrame = firstFrame
			if err := pacedSend(firstFrame); err != nil {
				return err
			}
		}
	}
	// 静止桌面自愈：MFT 有 ~gop 帧启动缓冲，若期间桌面转静止，首帧可能
	// 被编码器内部吞掉而始终无输出。静止超时 ~1s 后强制重编码缓存帧。
	// 首关键帧出帧前按节拍持续驱动编码器；出帧后静止即完全静默。
	// stateSent 状态机：安全桌面（UAC）期间发 locked，画面恢复发
	// capturing——观众看到暂停态而非冻结的旧帧。
	stateSent := "capturing"
	idleTicks, idleLimit := 0, 1
	setState := func(s string) {
		if stateSent == s {
			return
		}
		if err := writeFrame(conn, pipeFrameState, []byte(s)); err == nil {
			stateSent = s
		}
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cap == nil {
			// 上轮重建失败后的重试节拍。安全桌面（UAC）期间 dda_create
			// 也会被拒——此路径同样要发 locked，防静默冻结。
			if secureDesktopActive() {
				setState("locked")
			}
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
			if errors.Is(aerr, ErrSecureDesktop) {
				setState("locked")
				continue // 按节拍重试；桌面切回后自然恢复
			}
			if errors.Is(aerr, ErrTimeout) {
				// 后续静止帧：MFT 启动缓冲自愈逻辑
				idleTicks++
				if !gop.sentKey && lastFrame != nil && idleTicks >= idleLimit {
					if err := pacedSend(lastFrame); err != nil {
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

		if err := pacedSend(frame); err != nil {
			return err
		}
		setState("capturing") // 安全桌面结束/画面恢复
	}
}

// gopState 时间基关键帧记账：变帧率（静止自适应 0fps + 光标突发）下按
// 帧数计 GOP 会时间扭曲，改为距上一关键帧 ≥2s 强制 IDR（新观众可立即
// 入流的语义不变——agent 缓存最新 I 帧）。
type gopState struct {
	sentKey   bool
	lastKeyAt time.Time
}

func (g *gopState) keyDue() bool {
	return !g.sentKey || time.Since(g.lastKeyAt) >= 2*time.Second
}

// sendEncoded 编码一帧并按关键帧/增量帧类型写 pipe。编码器无输出（MFT
// 启动缓冲）时静默跳过。码流契约（唯一整形规则，其余 NALU 直通）：IDR
// 前必有缓存的 SPS/PPS，统一 4 字节起始码——消费端（WebCodecs Annex-B
// 模式）按此契约配置解码器。
func sendEncoded(conn net.Conn, encoder frameEncoder, frame []byte, gop *gopState) error {
	forceKey := gop.keyDue() // 首输出前始终请求关键帧
	data, encErr := encoder.Encode(frame, forceKey, false)
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
		gop.sentKey = true
		gop.lastKeyAt = time.Now()
	} else {
		frameType = pipeFrameDelta
		if len(encoder.SPSPPS()) > 0 { // H.264 模式统一 4 字节起始码
			data = vclNALUs(data)
		}
	}
	if dumpFile != nil {
		_, _ = dumpFile.Write(data) // 整形后码流（= 管道实发字节）
	}
	statFrames++
	statBytes += int64(len(data))
	if frameType == pipeFrameKey {
		statKeys++
	} else {
		statDeltas++
	}
	return writeFrame(conn, frameType, data)
}

// 单管线诊断状态：helper 每进程恰好一条采集管线，进程级即可。
var (
	dumpFile                                      io.Writer // XNC_DUMP_H264 落盘目标（整形后码流）
	statFrames, statKeys, statDeltas, statEncErrs int
	statBytes                                     int64
)

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
