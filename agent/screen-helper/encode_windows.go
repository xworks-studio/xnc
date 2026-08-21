//go:build windows

// encode_windows.go — H.264 编码：Windows Media Foundation Transform
// （CMSH264EncoderMFT，微软软件 H.264 编码器）经纯 syscall COM 调用
// （CGO_ENABLED=0）。
//
// 流程：
//
//	CoCreateInstance(CLSID_CMSH264EncoderMFT, IID_IMFTransform) →
//	MFCreateMediaType × 2（输出 H.264 / 输入 NV12，属性见下）→
//	SetOutputType(0) → SetInputType(0) → ICodecAPI 设 GOP 大小（best-effort）
//	→ ProcessMessage(BEGIN_STREAMING/START_OF_STREAM) →
//	每帧：BGRA→NV12（BT.601）→ IMFSample → ProcessInput → 循环
//	ProcessOutput 收集 Annex-B NALU → 首个 IDR 帧提取 SPS/PPS。
//
// vtable 槽位与 GUID 已对照 wine/mingw-w64 SDK 头核实（mfobjects.idl /
// mftransform.idl / mfapi.h / codecapi.h / wmcodecdsp.h）：
//
//	IMFTransform: SetInputType=15, SetOutputType=16, ProcessMessage=23,
//	              ProcessInput=24, ProcessOutput=25
//	IMFAttributes: SetUINT32=21, SetUINT64=22, SetGUID=24
//	IMFSample: SetSampleTime=36, ConvertToContiguousBuffer=41, AddBuffer=42
//	IMFMediaBuffer: Lock=3, Unlock=4, SetCurrentLength=6
//	ICodecAPI: SetValue=9
package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---- vtable 槽位（SDK 头顺序核实，勿改）----

const (
	vtAttrSetUINT32 = 21 // IMFAttributes
	vtAttrSetUINT64 = 22
	vtAttrSetGUID   = 24

	vtMFTSetInputType        = 15 // IMFTransform
	vtMFTSetOutputType       = 16
	vtMFTGetOutputStreamInfo = 7
	vtMFTProcessMessage      = 23
	vtMFTProcessInput        = 24
	vtMFTProcessOutput       = 25
	vtMFTGetOutputStatus     = 20

	vtSampleSetSampleTime             = 36 // IMFSample（继承 IMFAttributes 0-32）
	vtSampleSetSampleDuration         = 38
	vtSampleConvertToContiguousBuffer = 41
	vtSampleAddBuffer                 = 42

	vtBufferLock             = 3 // IMFMediaBuffer
	vtBufferUnlock           = 4
	vtBufferGetCurrentLength = 5
	vtBufferSetCurrentLength = 6

	vtCodecAPISetValue = 9 // ICodecAPI
)

// ---- MFT 消息 / 错误常量 ----

const (
	mftMsgNotifyBeginStreaming = 0x10000000
	mftMsgNotifyStartOfStream  = 0x10000003
	mftMsgNotifyEndStreaming   = 0x10000001

	mfETransformNeedMoreInput = 0xC00D6D72
	mfETransformStreamChange  = 0xC00D6D61

	mfVideoInterlaceProgressive = 2
	vtUI4                       = 19 // VARIANT 类型 VT_UI4

	mftOutputStreamProvidesSamples = 0x100

	clsctxInprocServer = 0x1
)

// ---- GUID ----

var (
	// CLSID_CMSH264EncoderMFT（wmcodecdsp.h；注意 62ce7e72-… 是解码器）。
	clsidCMSH264EncoderMFT = guid{0x6ca50344, 0x051a, 0x4ded, [8]byte{0x97, 0x79, 0xa4, 0x33, 0x05, 0x16, 0x5e, 0x35}}
	iidIMFTransform        = guid{0xbf94c121, 0x5b05, 0x4e6f, [8]byte{0x80, 0x00, 0xba, 0x59, 0x89, 0x61, 0x41, 0x4d}}
	iidICodecAPI           = guid{0x901db4c7, 0x31ce, 0x41a2, [8]byte{0x85, 0xdc, 0x8f, 0xa0, 0xbf, 0x41, 0xb8, 0xda}}

	attrMFMTMajorType     = guid{0x48eba18e, 0xf8c9, 0x4687, [8]byte{0xbf, 0x11, 0x0a, 0x74, 0xc9, 0xf9, 0x6a, 0x8f}}
	attrMFMTSubtype       = guid{0xf7e34c9a, 0x42e8, 0x4714, [8]byte{0xb7, 0x4b, 0xcb, 0x29, 0xd7, 0x2c, 0x35, 0xe5}}
	attrMFMTFrameSize     = guid{0x1652c33d, 0xd6b2, 0x4012, [8]byte{0xb8, 0x34, 0x72, 0x03, 0x08, 0x49, 0xa3, 0x7d}}
	attrMFMTFrameRate     = guid{0xc459a2e8, 0x3d2c, 0x4e44, [8]byte{0xb1, 0x32, 0xfe, 0xe5, 0x15, 0x6c, 0x7b, 0xb0}}
	attrMFMTInterlace     = guid{0xe2724bb8, 0xe676, 0x4806, [8]byte{0xb4, 0xb2, 0xa8, 0xd6, 0xef, 0xb4, 0x4c, 0xcd}}
	attrMFMTDefaultStride = guid{0x82e1bf1f, 0x83b1, 0x4b81, [8]byte{0x9a, 0x4b, 0xdc, 0xf5, 0xd6, 0xc3, 0x35, 0x22}}
	attrMFMTAvgBitrate    = guid{0x20332624, 0xfb0d, 0x4d9e, [8]byte{0xbd, 0x0d, 0xcb, 0xf6, 0x78, 0x6c, 0x10, 0x2e}}
	guidMFMediaTypeVideo  = guid{0x73646976, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}}
	guidMFVideoFormatH264 = guid{0x34363248, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}} // 'H264'
	guidMFVideoFormatNV12 = guid{0x3231564e, 0x0000, 0x0010, [8]byte{0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}} // 'NV12'

	codecAPIAVEncMPVGOPSize         = guid{0x95f31b26, 0x95a4, 0x41aa, [8]byte{0x93, 0x03, 0x24, 0x6a, 0x7f, 0xc6, 0xee, 0xf1}}
	codecAPIAVEncVideoForceKeyFrame = guid{0x398c1b98, 0x8353, 0x475a, [8]byte{0x9e, 0xf2, 0x8f, 0x26, 0x5d, 0x26, 0x03, 0x45}}
)

// ---- mfplat.dll ----

var (
	mfplatDLL                = windows.NewLazySystemDLL("mfplat.dll")
	procMFCreateMediaType    = mfplatDLL.NewProc("MFCreateMediaType")
	procMFCreateMemoryBuffer = mfplatDLL.NewProc("MFCreateMemoryBuffer")
	procMFCreateSample       = mfplatDLL.NewProc("MFCreateSample")
)

var procCoCreateInstance = ole32.NewProc("CoCreateInstance")

// ---- 结构体 ----

// mftOutputDataBuffer 对应 MFT_OUTPUT_DATA_BUFFER（x64：含对齐填充共 32B）。
type mftOutputDataBuffer struct {
	dwStreamID uint32
	_pad1      uint32
	pSample    comPtr
	dwStatus   uint32
	_pad2      uint32
	pEvents    comPtr
}

// mftOutputStreamInfo 对应 MFT_OUTPUT_STREAM_INFO。
type mftOutputStreamInfo struct {
	dwFlags     uint32
	cbSize      uint32
	cbAlignment uint32
}

// variantUI4 对应 VARIANT(VT_UI4)（x64 布局 24B）。
type variantUI4 struct {
	vt  uint16
	_r  [6]byte
	val uint32
	_p  [12]byte
}

// ---- H264Encoder ----

// H264Encoder 封装微软软件 H.264 MFT 编码器（同步 MFT，NV12 输入，
// Annex-B 输出）。
type H264Encoder struct {
	mft      comPtr // IMFTransform
	codecAPI comPtr // ICodecAPI（可选，GOP/关键帧控制）

	width, height int
	bitrate       uint32
	gopSize       uint32

	nv12               []byte // 复用的转换缓冲（w*h*3/2）
	spsPPS             []byte // 首个 IDR 帧后提取
	lastKey            bool   // 最近一次 Encode 输出是否关键帧
	rtStart            int64  // 合成时间戳（100ns 单位）
	mftProvidesSamples bool   // 输出 sample 由 MFT 分配
	outBufSize         int    // 客户端输出缓冲大小
	coInit             bool
}

// 编码器假定帧率（MFT 元数据用；实际帧率由捕获侧自适应控制）。
const encAssumedFps = 30

// newH264Encoder 以 frameEncoder 接口形式创建 MFT H.264 编码器。
func newH264Encoder(width, height, bitrate, gopSize int) (frameEncoder, error) {
	return NewH264Encoder(width, height, bitrate, gopSize)
}

// NewH264Encoder 创建并初始化 MFT H.264 编码器。width/height 需为偶数。
func NewH264Encoder(width, height, bitrate, gopSize int) (*H264Encoder, error) {
	if width <= 0 || height <= 0 || width%2 != 0 || height%2 != 0 {
		return nil, fmt.Errorf("invalid dimensions %dx%d", width, height)
	}
	if bitrate <= 0 {
		bitrate = 2_000_000
	}
	if gopSize <= 0 {
		gopSize = 60
	}

	hr, err := coInitializeEx()
	if err != nil && hr != rpcEChangedMode {
		return nil, fmt.Errorf("CoInitializeEx: %w", err)
	}
	e := &H264Encoder{
		width: width, height: height,
		bitrate: uint32(bitrate), gopSize: uint32(gopSize),
		nv12:   make([]byte, width*height*3/2),
		coInit: err == nil,
	}

	// CoCreateInstance(CMSH264EncoderMFT)
	var mft comPtr
	r, _, ce := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidCMSH264EncoderMFT)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidIMFTransform)), uintptr(unsafe.Pointer(&mft.p)))
	if r != 0 {
		e.Close()
		return nil, fmt.Errorf("CoCreateInstance(CMSH264EncoderMFT): hr=0x%08X %v", uint32(r), ce)
	}
	e.mft = mft

	frameSize := uint64(width)<<32 | uint64(height)
	frameRate := uint64(encAssumedFps)<<32 | 1

	// 输出类型：H.264
	outMT, err := e.createMediaType(func(mt comPtr) error {
		if _, err := mt.call(vtAttrSetGUID, ptrGUID(&attrMFMTMajorType), ptrGUID(&guidMFMediaTypeVideo)); err != nil {
			return err
		}
		if _, err := mt.call(vtAttrSetGUID, ptrGUID(&attrMFMTSubtype), ptrGUID(&guidMFVideoFormatH264)); err != nil {
			return err
		}
		if _, err := mt.call(vtAttrSetUINT64, ptrGUID(&attrMFMTFrameSize), uintptr(frameSize)); err != nil {
			return err
		}
		if _, err := mt.call(vtAttrSetUINT32, ptrGUID(&attrMFMTInterlace), mfVideoInterlaceProgressive); err != nil {
			return err
		}
		if _, err := mt.call(vtAttrSetUINT64, ptrGUID(&attrMFMTFrameRate), uintptr(frameRate)); err != nil {
			return err
		}
		_, err := mt.call(vtAttrSetUINT32, ptrGUID(&attrMFMTAvgBitrate), uintptr(e.bitrate))
		return err
	})
	if err != nil {
		e.Close()
		return nil, fmt.Errorf("output media type: %w", err)
	}
	if _, err := e.mft.call(vtMFTSetOutputType, 0, outMT.u(), 0); err != nil {
		outMT.release()
		e.Close()
		return nil, fmt.Errorf("SetOutputType: %w", err)
	}
	outMT.release()

	// 输入类型：NV12
	inMT, err := e.createMediaType(func(mt comPtr) error {
		if _, err := mt.call(vtAttrSetGUID, ptrGUID(&attrMFMTMajorType), ptrGUID(&guidMFMediaTypeVideo)); err != nil {
			return err
		}
		if _, err := mt.call(vtAttrSetGUID, ptrGUID(&attrMFMTSubtype), ptrGUID(&guidMFVideoFormatNV12)); err != nil {
			return err
		}
		// 显式声明 NV12 紧凑 stride = width（默认 MFT 可能用对齐 stride）
		if _, err := mt.call(vtAttrSetUINT32, ptrGUID(&attrMFMTDefaultStride), uintptr(width)); err != nil {
			return err
		}
		if _, err := mt.call(vtAttrSetUINT64, ptrGUID(&attrMFMTFrameSize), uintptr(frameSize)); err != nil {
			return err
		}
		if _, err := mt.call(vtAttrSetUINT32, ptrGUID(&attrMFMTInterlace), mfVideoInterlaceProgressive); err != nil {
			return err
		}
		_, err := mt.call(vtAttrSetUINT64, ptrGUID(&attrMFMTFrameRate), uintptr(frameRate))
		return err
	})
	if err != nil {
		e.Close()
		return nil, fmt.Errorf("input media type: %w", err)
	}
	if _, err := e.mft.call(vtMFTSetInputType, 0, inMT.u(), 0); err != nil {
		inMT.release()
		e.Close()
		return nil, fmt.Errorf("SetInputType: %w", err)
	}
	inMT.release()

	// ICodecAPI：GOP 大小（best-effort——失败不影响正确性，仅关键帧间隔
	// 退化为编码器默认值）。
	if api, qerr := e.mft.queryInterface(&iidICodecAPI); qerr == nil {
		e.codecAPI = api
		_ = e.codecAPISetUint(&codecAPIAVEncMPVGOPSize, uint32(gopSize))
	}

	// 输出缓冲需求：MFT_OUTPUT_STREAM_PROVIDES_SAMPLES 未置位时由客户端
	// 提供 sample（CMSH264EncoderMFT 即如此——传 nil 会得到 E_INVALIDARG）。
	var osi mftOutputStreamInfo
	if _, err := e.mft.call(vtMFTGetOutputStreamInfo, 0, uintptr(unsafe.Pointer(&osi))); err == nil {
		e.mftProvidesSamples = osi.dwFlags&mftOutputStreamProvidesSamples != 0
		if int(osi.cbSize) > e.outBufSize {
			e.outBufSize = int(osi.cbSize)
		}
	}
	if e.outBufSize <= 0 {
		e.outBufSize = width*height*4 + 65536 // 宽裕上界
	}

	// 启动流。
	if _, err := e.mft.call(vtMFTProcessMessage, mftMsgNotifyBeginStreaming, 0); err != nil {
		e.Close()
		return nil, fmt.Errorf("ProcessMessage(begin streaming): %w", err)
	}
	if _, err := e.mft.call(vtMFTProcessMessage, mftMsgNotifyStartOfStream, 0); err != nil {
		e.Close()
		return nil, fmt.Errorf("ProcessMessage(start of stream): %w", err)
	}
	return e, nil
}

// createMediaType 调 MFCreateMediaType 并执行 setup 中的属性设置。
func (e *H264Encoder) createMediaType(setup func(mt comPtr) error) (comPtr, error) {
	var mt comPtr
	r, _, ce := procMFCreateMediaType.Call(uintptr(unsafe.Pointer(&mt.p)))
	if r != 0 {
		return nilPtr, fmt.Errorf("MFCreateMediaType: hr=0x%08X %v", uint32(r), ce)
	}
	if err := setup(mt); err != nil {
		mt.release()
		return nilPtr, err
	}
	return mt, nil
}

// codecAPISetUint 经 ICodecAPI::SetValue 写 UINT 参数（best-effort）。
func (e *H264Encoder) codecAPISetUint(api *guid, v uint32) error {
	if !e.codecAPI.valid() {
		return fmt.Errorf("no ICodecAPI")
	}
	val := variantUI4{vt: vtUI4, val: v}
	_, err := e.codecAPI.call(vtCodecAPISetValue, uintptr(unsafe.Pointer(api)), uintptr(unsafe.Pointer(&val)))
	return err
}

// Encode 编码一帧 BGRA（w*h*4 字节）。flipY=true 时源为 bottom-up（垂直
// 翻转后转 NV12）。forceKey 请求 IDR。输出为 Annex-B NALU 序列。
func (e *H264Encoder) Encode(frame []byte, forceKey bool, flipY bool) ([]byte, error) {
	if len(frame) < e.width*e.height*4 {
		return nil, fmt.Errorf("short frame: %d < %d", len(frame), e.width*e.height*4)
	}
	bgraToNV12(frame, e.nv12, e.width, e.height, flipY)

	if forceKey && e.codecAPI.valid() {
		// 部分实现要求 VT_BOOL，失败忽略——关键帧仍按 GOP 周期产生。
		_ = e.codecAPISetUint(&codecAPIAVEncVideoForceKeyFrame, 1)
	}

	// 组装输入 IMFSample。
	var sample, buffer comPtr
	if r, _, ce := procMFCreateSample.Call(uintptr(unsafe.Pointer(&sample.p))); r != 0 {
		return nil, fmt.Errorf("MFCreateSample: hr=0x%08X %v", uint32(r), ce)
	}
	defer sample.release()
	if r, _, ce := procMFCreateMemoryBuffer.Call(uintptr(len(e.nv12)), uintptr(unsafe.Pointer(&buffer.p))); r != 0 {
		return nil, fmt.Errorf("MFCreateMemoryBuffer: hr=0x%08X %v", uint32(r), ce)
	}
	defer buffer.release()

	var base unsafe.Pointer
	var maxLen, curLen uint32
	if _, err := buffer.call(vtBufferLock, uintptr(unsafe.Pointer(&base)), uintptr(unsafe.Pointer(&maxLen)), uintptr(unsafe.Pointer(&curLen))); err != nil {
		return nil, fmt.Errorf("buffer Lock: %w", err)
	}
	copy(unsafe.Slice((*byte)(base), len(e.nv12)), e.nv12)
	_, _ = buffer.call(vtBufferUnlock)
	if _, err := buffer.call(vtBufferSetCurrentLength, uintptr(len(e.nv12))); err != nil {
		return nil, fmt.Errorf("SetCurrentLength: %w", err)
	}
	if _, err := sample.call(vtSampleAddBuffer, buffer.u()); err != nil {
		return nil, fmt.Errorf("AddBuffer: %w", err)
	}
	_, _ = sample.call(vtSampleSetSampleTime, uintptr(e.rtStart))
	_, _ = sample.call(vtSampleSetSampleDuration, uintptr(10_000_000/encAssumedFps))
	e.rtStart += 10_000_000 / encAssumedFps

	if _, err := e.mft.call(vtMFTProcessInput, 0, sample.u(), 0); err != nil {
		return nil, fmt.Errorf("ProcessInput: %w", err)
	}

	// 收集输出（可能 0..N 个 sample）。
	var out []byte
	isKey := false
	for {
		var ob mftOutputDataBuffer
		if !e.mftProvidesSamples {
			sample, buf, cerr := newBufferSample(e.outBufSize)
			if cerr != nil {
				return nil, cerr
			}
			ob.pSample = sample
			defer func() { sample.release(); buf.release() }()
		}
		var status uint32
		r1, err := e.mft.call(vtMFTProcessOutput, 0, 1, uintptr(unsafe.Pointer(&ob)), uintptr(unsafe.Pointer(&status)))
		if err != nil {
			hr := hrOf(r1)
			if hr == mfETransformNeedMoreInput {
				break
			}
			if hr == mfETransformStreamChange {
				continue // 格式协商变化：重试
			}
			return nil, fmt.Errorf("ProcessOutput: %w", err)
		}
		if ob.pSample.valid() {
			data, gerr := sampleBytes(ob.pSample)
			if e.mftProvidesSamples {
				// MFT 分配的 sample 归我们释放；客户端自带 sample 由
				// 循环顶部的 defer 释放，不可重复。
				ob.pSample.release()
			}
			if gerr != nil {
				if ob.pEvents.valid() {
					ob.pEvents.release()
				}
				return nil, gerr
			}
			if len(data) > 0 {
				out = append(out, data...)
				if hasNALType(data, 5) { // IDR
					isKey = true
				}
			}
		}
		if ob.pEvents.valid() {
			ob.pEvents.release()
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	e.lastKey = isKey
	if isKey && len(e.spsPPS) == 0 {
		e.spsPPS = extractNALs(out, 7, 8) // SPS + PPS（含起始码）
	}
	return out, nil
}

// SPSPPS 返回首个 IDR 帧提取的 SPS/PPS（含起始码），未就绪时为 nil。
func (e *H264Encoder) SPSPPS() []byte { return e.spsPPS }

// LastFrameKey 报告最近一次 Encode 的输出是否关键帧（JPEG 回退编码器
// 恒为 true，统一走 frameEncoder 接口）。
func (e *H264Encoder) LastFrameKey() bool { return e.lastKey }

// Close 结束流并释放 COM 资源。
func (e *H264Encoder) Close() {
	if e.mft.valid() {
		_, _ = e.mft.call(vtMFTProcessMessage, mftMsgNotifyEndStreaming, 0)
	}
	e.codecAPI.release()
	e.mft.release()
	e.codecAPI, e.mft = nilPtr, nilPtr
	if e.coInit {
		coUninitialize()
		e.coInit = false
	}
}

// newBufferSample 创建带固定大小内存缓冲的空 IMFSample（客户端提供输出
// 缓冲时用）。
func newBufferSample(size int) (sample, buffer comPtr, err error) {
	if r, _, ce := procMFCreateSample.Call(uintptr(unsafe.Pointer(&sample.p))); r != 0 {
		return nilPtr, nilPtr, fmt.Errorf("MFCreateSample: hr=0x%08X %v", uint32(r), ce)
	}
	if r, _, ce := procMFCreateMemoryBuffer.Call(uintptr(size), uintptr(unsafe.Pointer(&buffer.p))); r != 0 {
		sample.release()
		return nilPtr, nilPtr, fmt.Errorf("MFCreateMemoryBuffer: hr=0x%08X %v", uint32(r), ce)
	}
	if _, aerr := sample.call(vtSampleAddBuffer, buffer.u()); aerr != nil {
		sample.release()
		buffer.release()
		return nilPtr, nilPtr, fmt.Errorf("AddBuffer: %w", aerr)
	}
	return sample, buffer, nil
}

// sampleBytes 取出 IMFSample 的连续缓冲数据副本。
func sampleBytes(sample comPtr) ([]byte, error) {
	var buffer comPtr
	if _, err := sample.call(vtSampleConvertToContiguousBuffer, uintptr(unsafe.Pointer(&buffer.p))); err != nil {
		return nil, fmt.Errorf("ConvertToContiguousBuffer: %w", err)
	}
	defer buffer.release()

	var base unsafe.Pointer
	var maxLen, curLen uint32
	if _, err := buffer.call(vtBufferLock, uintptr(unsafe.Pointer(&base)), uintptr(unsafe.Pointer(&maxLen)), uintptr(unsafe.Pointer(&curLen))); err != nil {
		return nil, fmt.Errorf("buffer Lock: %w", err)
	}
	defer buffer.call(vtBufferUnlock)
	if base == nil {
		return nil, nil
	}
	return append([]byte(nil), unsafe.Slice((*byte)(base), int(curLen))...), nil
}

// ptrGUID 取 GUID 地址（syscall 实参转换）。
func ptrGUID(g *guid) uintptr { return uintptr(unsafe.Pointer(g)) }

// ---- BGRA → NV12（BT.601，整数近似）----

// bgraToNV12 将 top-down BGRA 帧转换为 NV12（Y 平面 + 交错的 UV 半分辨率
// 平面）。dst 需为 w*h*3/2 字节。flipY 控制垂直翻转（DXGI 某些驱动返回
// bottom-up 行序时需要翻转为 top-down）。
func bgraToNV12(bgra, dst []byte, w, h int, flipY bool) {
	yPlane := dst[:w*h]
	uvPlane := dst[w*h:]
	stride := w * 4

	rowIdx := func(row int) int {
		if flipY {
			return (h - 1 - row) * stride
		}
		return row * stride
	}

	for row := 0; row < h; row++ {
		yRow := yPlane[row*w : (row+1)*w]
		srcRow := bgra[rowIdx(row) : rowIdx(row)+stride]
		for x := 0; x < w; x++ {
			b := int(srcRow[x*4])
			g := int(srcRow[x*4+1])
			r := int(srcRow[x*4+2])
			yRow[x] = byte((66*r + 129*g + 25*b + 128) >> 8)
			yRow[x] += 16
		}
	}
	for row := 0; row < h/2; row++ {
		uvRow := uvPlane[row*w : (row+1)*w]
		srcRow0 := rowIdx(row * 2)
		srcRow1 := rowIdx(row*2 + 1)
		for cx := 0; cx < w/2; cx++ {
			b0, g0, r0 := bgra[srcRow0+cx*8], bgra[srcRow0+cx*8+1], bgra[srcRow0+cx*8+2]
			b1, g1, r1 := bgra[srcRow0+cx*8+4], bgra[srcRow0+cx*8+5], bgra[srcRow0+cx*8+6]
			b2, g2, r2 := bgra[srcRow1+cx*8], bgra[srcRow1+cx*8+1], bgra[srcRow1+cx*8+2]
			b3, g3, r3 := bgra[srcRow1+cx*8+4], bgra[srcRow1+cx*8+5], bgra[srcRow1+cx*8+6]
			b := (int(b0) + int(b1) + int(b2) + int(b3)) / 4
			g := (int(g0) + int(g1) + int(g2) + int(g3)) / 4
			r := (int(r0) + int(r1) + int(r2) + int(r3)) / 4
			u := ((-38*r - 74*g + 112*b + 128) >> 8) + 128
			v := ((112*r - 94*g - 18*b + 128) >> 8) + 128
			uvRow[cx*2] = byte(u)
			uvRow[cx*2+1] = byte(v)
		}
	}
}

// ---- Annex-B NALU 解析 ----

// hasNALType 检查 Annex-B 码流是否包含指定 NAL 类型的 NALU（5=IDR 等）。
func hasNALType(data []byte, want byte) bool {
	return findNAL(data, want) >= 0
}

// findNAL 返回指定类型 NALU 的 payload 起始下标，无则 -1。
func findNAL(data []byte, want byte) int {
	i := 0
	for i+4 < len(data) {
		// 定位起始码（3 或 4 字节）
		if data[i] == 0 && data[i+1] == 0 {
			sc, hdr := 0, -1
			if data[i+2] == 1 {
				sc, hdr = 3, i+3
			} else if i+4 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
				sc, hdr = 4, i+4
			}
			if hdr >= 0 && hdr < len(data) {
				if data[hdr]&0x1F == want {
					return hdr
				}
				i += sc + 1
				continue
			}
		}
		i++
	}
	return -1
}

// extractNALs 复制指定类型的 NALU（含 4 字节起始码，逐个拼接）。
func extractNALs(data []byte, types ...byte) []byte {
	var out []byte
	for _, t := range types {
		i := 0
		for {
			idx := findNALFrom(data, i, t)
			if idx < 0 {
				break
			}
			start := idx - 4 // 含起始码（findNALFrom 保证 idx>=4 或前有 3 字节码）
			if start < 0 {
				start = idx - 3
			}
			// NALU 结束 = 下一个起始码或流尾。
			end := len(data)
			for j := idx + 1; j+3 <= len(data); j++ {
				if data[j] == 0 && data[j+1] == 0 && (data[j+2] == 1 || (j+4 <= len(data) && data[j+2] == 0 && data[j+3] == 1)) {
					// 回退尾部零字节
					end = j
					for end > idx && data[end-1] == 0 {
						end--
					}
					break
				}
			}
			out = append(out, data[start:end]...)
			i = end
		}
	}
	return out
}

// findNALFrom 从 from 开始查找指定类型 NALU payload 下标。
func findNALFrom(data []byte, from int, want byte) int {
	if from < 0 {
		from = 0
	}
	for i := from; i+4 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 {
			hdr := -1
			if data[i+2] == 1 {
				hdr = i + 3
			} else if i+4 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
				hdr = i + 4
			}
			if hdr >= 0 && hdr < len(data) {
				if data[hdr]&0x1F == want {
					return hdr
				}
			}
		}
	}
	return -1
}
