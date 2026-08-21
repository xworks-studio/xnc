// ddaloader_windows.go — xnc-dda.dll（DXGI 采集层 C ABI）的加载与绑定。
//
// 加载顺序：exe 同目录 xnc-dda.dll → 缺失则把 go:embed 副本解压到 exe
// 同目录再加载（单 exe 部署形态；DLL 只在首次运行时落盘一次）。加载后
// 校验 dda_abi_version 防 helper/DLL 部署偏斜。
//
// 仅绑定导出函数——Go 对导出函数的 syscall 调用完全可靠；COM vtable
// 方法调用在本仓库已实证存在系统性故障（DuplicateOutput 三机全败、
// C 实现三机全成），故 COM 层整体下沉到 C。
//
//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"

	"xnc/screen-helper/embedded"
)

const (
	ddaFrameContent    = 0 // bgra 已写入新桌面内容
	ddaFrameCursorOnly = 1 // 仅光标变化：bgra 未写
	ddaFrameTimeout    = 2 // 桌面静止
	ddaFrameAccessLost = 3 // 已在 DLL 内重建，本帧无数据
	ddaErrCode         = -1

	ddaPointerMonochrome = 1
	ddaPointerColor      = 2
	ddaPointerMasked     = 4
)

// ddaCursor 镜像 C 侧 dda_cursor（平铺 int32 + uint8，4 字节对齐，总 32B）。
type ddaCursor struct {
	Type, W, H, Pitch, Len, X, Y int32
	Visible                      uint8
	_                            [3]byte
}

// ddaFrame 镜像 C 侧 dda_frame：kind0 reserved4 present8 accum16 cursor20。
type ddaFrame struct {
	Kind, Reserved int32
	PresentTime    int64
	Accumulated    uint32
	Cursor         ddaCursor
}

type ddaDLL struct {
	mod         *windows.DLL
	create      *windows.Proc
	acquire     *windows.Proc
	cursorShape *windows.Proc
	format      *windows.Proc
	dims        *windows.Proc
	destroy     *windows.Proc
	lastError   *windows.Proc
	abiVersion  *windows.Proc
}

var (
	ddaOnce   sync.Once
	ddaLoaded *ddaDLL
	ddaErr    error
)

// loadDDA 返回已加载的 DLL 绑定；不可用时返回带原因的错误（调用方回退
// WGC）。DLL 缺失（未跑 dda/build.bat）与加载失败都只影响本进程。
func loadDDA() (*ddaDLL, error) {
	ddaOnce.Do(func() {
		ddl := filepath.Join(exeDir(), "xnc-dda.dll")
		if _, statErr := os.Stat(ddl); statErr != nil {
			// exe 同目录没有：解压嵌入副本（首次运行落盘一次）。
			bts, embErr := embedded.DLLBytes()
			if embErr != nil {
				ddaErr = fmt.Errorf("xnc-dda.dll neither staged next to helper nor embedded (run agent/screen-helper/dda/build.bat before go build): %w", embErr)
				return
			}
			if werr := os.WriteFile(ddl, bts, 0o644); werr != nil {
				ddaErr = fmt.Errorf("extract embedded xnc-dda.dll: %w", werr)
				return
			}
		}
		mod, lerr := windows.LoadDLL(ddl)
		if lerr != nil {
			ddaErr = fmt.Errorf("load xnc-dda.dll: %w", lerr)
			return
		}
		d := &ddaDLL{mod: mod}
		proc := func(name string) *windows.Proc {
			p, err := mod.FindProc(name)
			if err != nil {
				ddaErr = fmt.Errorf("xnc-dda.dll missing export %s: %w", name, err)
				return nil
			}
			return p
		}
		d.abiVersion = proc("dda_abi_version")
		d.create = proc("dda_create")
		d.acquire = proc("dda_acquire")
		d.cursorShape = proc("dda_cursor_shape")
		d.format = proc("dda_format")
		d.dims = proc("dda_dims")
		d.destroy = proc("dda_destroy")
		d.lastError = proc("dda_last_error")
		if ddaErr != nil {
			return
		}
		if v, _, _ := d.abiVersion.Call(); uint32(v) != 1 {
			ddaErr = fmt.Errorf("xnc-dda.dll abi version %d != 1 (helper/DLL 部署偏斜)", uint32(v))
			return
		}
		ddaLoaded = d
	})
	return ddaLoaded, ddaErr
}

// exeDir 返回 helper 可执行文件所在目录（DLL 解压/日志同目录约定）。
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// ddaLastError 读取 DLL 内的错误描述（步骤前缀 + HRESULT）。
func (d *ddaDLL) lastErrorStr() string {
	p, _, _ := d.lastError.Call()
	if p == 0 {
		return "unknown"
	}
	return windows.BytePtrToString((*byte)(uintptrToPtr(p)))
}

// uintptrToPtr 将 syscall 返回的指针值转回 unsafe.Pointer（vet 安全的
// 双重解引用写法；直接转换会被 go vet unsafeptr 标记）。
func uintptrToPtr(p uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&p))
}
