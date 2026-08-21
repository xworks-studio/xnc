// ddacapture_windows.go — 基于 xnc-dda.dll 的采集器（DXGI Desktop
// Duplication，主路径）。
//
// 语义（对齐 WGCCapturer 供 captureLoop 复用）：
//   - AcquireFrame 返回 BGRA top-down 全帧；桌面静止返回 ErrTimeout。
//   - 光标-only 帧：以缓存的上一内容帧为底、合成最新光标后返回（远端
//     可见光标移动）；尚无缓存帧时按静止处理。
//   - ACCESS_LOST（DLL 内已重建）：本帧按静止处理，下一帧自然恢复。
//   - Dims 返回当前 duplication 尺寸（重建后可能变化——分辨率切换场景，
//     调用方在帧循环里每次重读）。
//
//go:build windows

package main

import (
	"errors"
	"fmt"
	"unsafe"
)

// DDACapturer 封装 xnc-dda.dll 实例。
type DDACapturer struct {
	dll       *ddaDLL
	inst      uintptr        // DLL 实例句柄（区别于高度 h）
	w, h      int
	buf       []byte // DLL 写入的目标缓冲（w*h*4）
	lastFrame []byte // 最近内容帧（光标-only 合成的底图）
}

// NewDDACapturer 加载 DLL 并建立 duplication。
func NewDDACapturer() (*DDACapturer, error) {
	d, err := loadDDA()
	if err != nil {
		return nil, err
	}
	var w, h int32
	r1, _, _ := d.create.Call(uintptr(unsafe.Pointer(&w)), uintptr(unsafe.Pointer(&h)))
	if r1 == 0 {
		return nil, fmt.Errorf("dda: %s", d.lastErrorStr())
	}
	c := &DDACapturer{dll: d, inst: r1, w: int(w), h: int(h)}
	c.buf = make([]byte, int(w)*int(h)*4)
	return c, nil
}

// Dims 返回当前 duplication 尺寸。
func (c *DDACapturer) Dims() (int, int) { return c.w, c.h }

// Close 销毁实例（DLL 侧释放全部 COM 资源）。
func (c *DDACapturer) Close() {
	if c.inst != 0 {
		c.dll.destroy.Call(c.inst)
		c.inst = 0
	}
}

// AcquireFrame 拉取一帧（详见文件头语义说明）。
func (c *DDACapturer) AcquireFrame(timeoutMs uint) ([]byte, error) {
	// 尺寸守卫：安全桌面 GDI 模式的分辨率可能与 DXGI 不同（DPI 虚拟化/
	// 模式切换），DLL 侧更新尺寸后按 Dims 重配缓冲再取帧。
	var dw, dh int32
	c.dll.dims.Call(c.inst, uintptr(unsafe.Pointer(&dw)), uintptr(unsafe.Pointer(&dh)))
	if int(dw) != c.w || int(dh) != c.h {
		c.w, c.h = int(dw), int(dh)
		if c.w > 0 && c.h > 0 {
			c.buf = make([]byte, c.w*c.h*4)
			c.lastFrame = nil
		}
	}

	var f ddaFrame
	for attempt := 0; ; attempt++ {
		f = ddaFrame{}
		r1, _, _ := c.dll.acquire.Call(c.inst, uintptr(unsafe.Pointer(&c.buf[0])), uintptr(len(c.buf)), uintptr(timeoutMs), uintptr(unsafe.Pointer(&f)))
		switch int32(r1) {
		case ddaFrameContent:
			// DXGI 语义：LastPresentTime==0 的帧不含新桌面图像（常携带
			// 空纹理，读回全黑）——duplication 建立后的首个 acquire 常返
			// 回这种初始空帧，真首帧紧随其后（实测 ≤16ms）。跳过重试。
			if f.PresentTime == 0 && attempt < 2 {
				continue
			}
			frame := make([]byte, len(c.buf))
			copy(frame, c.buf)
			// 光标合成进内容帧：DDA 帧不含指针（与 WGC 不同），在此补齐。
			if f.Cursor.Visible != 0 && f.Cursor.Len > 0 {
				if shape := c.cursorShape(f.Cursor.Len); shape != nil {
					composeCursorBGRA(frame, c.w, c.h, shape, f.Cursor)
				}
			}
			c.lastFrame = frame
			return frame, nil
		case ddaFrameCursorOnly:
			if c.lastFrame == nil {
				return nil, ErrTimeout
			}
			frame := make([]byte, len(c.lastFrame))
			copy(frame, c.lastFrame)
			if f.Cursor.Visible != 0 && f.Cursor.Len > 0 {
				if shape := c.cursorShape(f.Cursor.Len); shape != nil {
					composeCursorBGRA(frame, c.w, c.h, shape, f.Cursor)
				}
			}
			return frame, nil
		case ddaFrameTimeout:
			return nil, ErrTimeout
		case ddaFrameAccessLost:
			// DLL 内已重建 duplication；尺寸可能已变。重建被拒期间若
			// 安全桌面（UAC）激活，显式报告——观众看到 locked 暂停态
			// 而非冻结的旧帧。
			c.refreshDims()
			if secureDesktopActive() {
				return nil, ErrSecureDesktop
			}
			return nil, ErrTimeout
		default:
			return nil, errors.New("dda: " + c.dll.lastErrorStr())
		}
	}
}

// Format 返回 duplication 的 DXGI_FORMAT（诊断用；87=BGRA8）。
func (c *DDACapturer) Format() int32 {
	r1, _, _ := c.dll.format.Call(c.inst)
	return int32(r1)
}

// ddaSingleShot 单帧快照路径（--jpeg-single DDA 侧）：建实例、取一帧、
// 销毁。静止桌面首帧立即可得（DXGI 语义），无需轮询。
func ddaSingleShot(timeoutMs uint) ([]byte, int, int, error) {
	c, err := NewDDACapturer()
	if err != nil {
		return nil, 0, 0, err
	}
	defer c.Close()
	frame, aerr := c.AcquireFrame(timeoutMs)
	if aerr != nil {
		return nil, 0, 0, aerr
	}
	return frame, c.w, c.h, nil
}

// cursorShape 取形状缓冲副本（DLL 内部缓冲随帧更新；拷出防悬挂）。
func (c *DDACapturer) cursorShape(length int32) []byte {
	p, _, _ := c.dll.cursorShape.Call(c.inst)
	if p == 0 {
		return nil
	}
	out := make([]byte, length)
	copy(out, unsafe.Slice((*byte)(uintptrToPtr(p)), length))
	return out
}

// refreshDims 重建后同步尺寸与缓冲（分辨率切换场景）。
func (c *DDACapturer) refreshDims() {
	var w, h int32
	c.dll.dims.Call(c.inst, uintptr(unsafe.Pointer(&w)), uintptr(unsafe.Pointer(&h)))
	if int(w) != c.w || int(h) != c.h {
		c.w, c.h = int(w), int(h)
		c.buf = make([]byte, int(w)*int(h)*4)
		c.lastFrame = nil // 旧帧分辨率失效
	}
}
