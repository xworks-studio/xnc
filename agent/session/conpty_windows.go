//go:build windows

// conpty_windows.go — Windows ConPTY 封装，逐段移植自 Gate B 真机验证过的
// 原型（docs/superpowers/spikes/gateb-conpty/prototype-direct-xsys.go）。
// 两个 Gate B 验证的不变量（违反即子进程 0xC0000142 或静默脱离 pty）：
//  1. UpdateProcThreadAttribute 的 lpValue 必须传 HPCON 句柄【值】（经
//     uintptr 重解释的 unsafe.Pointer），不是 &handle——传 &hpcon 实测得到
//     子进程退出码 0xC0000142；
//  2. 子进程 STARTUPINFO 必须置 STARTF_USESTDHANDLES，否则子进程静默不挂
//     pty（输出丢失但退出码 0）。
package session

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type conPTY struct {
	hpc       windows.Handle
	proc, thr windows.Handle
	pid       int
	inW       io.WriteCloser // CLI → pty input（写端）
	outR      io.ReadCloser  // pty output → CLI（读端）
	closeMu   sync.Mutex
	closed    bool
}

func startConPTY(cols, rows int, exe string, args ...string) (*conPTY, error) {
	// 管道 1：CLI 写 inW → pty 读 inR。管道 2：pty 写 outW → CLI 读 outR。
	var inR, inWraw, outRraw, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inWraw, nil, 0); err != nil {
		return nil, fmt.Errorf("input pipe: %w", err)
	}
	if err := windows.CreatePipe(&outRraw, &outW, nil, 0); err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(inWraw)
		return nil, fmt.Errorf("output pipe: %w", err)
	}

	var hpc windows.Handle
	if err := windows.CreatePseudoConsole(windows.Coord{X: int16(cols), Y: int16(rows)}, inR, outW, 0, &hpc); err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(outW)
		windows.CloseHandle(inWraw)
		windows.CloseHandle(outRraw)
		return nil, fmt.Errorf("CreatePseudoConsole: %w", err)
	}
	// pty 侧的管道端点 CreatePseudoConsole 已自行持有，立即关闭本侧副本。
	windows.CloseHandle(inR)
	windows.CloseHandle(outW)

	alist, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inWraw)
		windows.CloseHandle(outRraw)
		return nil, fmt.Errorf("attribute list: %w", err)
	}
	defer alist.Delete()
	// 不变量 1：HPCON 是 C 的 void*——lpValue 必须是句柄值本身，不是 &hpc。
	hp := hpc
	hpPtr := *(*unsafe.Pointer)(unsafe.Pointer(&hp))
	if err := alist.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, hpPtr, unsafe.Sizeof(hp)); err != nil {
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inWraw)
		windows.CloseHandle(outRraw)
		return nil, fmt.Errorf("UpdateProcThreadAttribute: %w", err)
	}

	siEx := &windows.StartupInfoEx{ProcThreadAttributeList: alist.List()}
	siEx.StartupInfo.Cb = uint32(unsafe.Sizeof(*siEx))
	// 不变量 2：conpty 库会设置该标志，实测缺它子进程脱离 pty（输出丢失）。
	siEx.StartupInfo.Flags |= windows.STARTF_USESTDHANDLES

	// lpApplicationName=nil 时 CreateProcess 自行解析命令行首 token 并搜
	// PATH；exe 含空格（如 Program Files 下的 pwsh 全路径）须加引号。
	cmdline := quoteWindowsArg(exe)
	for _, a := range args {
		cmdline += " " + a
	}
	cmd16, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inWraw)
		windows.CloseHandle(outRraw)
		return nil, err
	}
	// 属性表必须伴随 EXTENDED_STARTUPINFO_PRESENT；伪控制台句柄经属性表
	// 传递，inheritHandles 必须为 false（照原型）。
	var pi windows.ProcessInformation
	err = windows.CreateProcess(nil, cmd16, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT, nil, nil, &siEx.StartupInfo, &pi)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inWraw)
		windows.CloseHandle(outRraw)
		return nil, fmt.Errorf("CreateProcess: %w", err)
	}

	return &conPTY{
		hpc: hpc, proc: pi.Process, thr: pi.Thread, pid: int(pi.ProcessId),
		inW:  os.NewFile(uintptr(inWraw), "pty-in"),
		outR: os.NewFile(uintptr(outRraw), "pty-out"),
	}, nil
}

// quoteWindowsArg 仅对含空白/引号的 exe 补引号（反斜杠不动——原型用 %q 会
// 把路径反斜杠翻倍，靠 Win32 路径归一化侥幸可用；此处不复刻）。
func quoteWindowsArg(s string) string {
	if strings.ContainsAny(s, " \t\"") {
		return `"` + strings.ReplaceAll(s, `"`, ``) + `"`
	}
	return s
}

func (p *conPTY) Resize(cols, rows int) error {
	return windows.ResizePseudoConsole(p.hpc, windows.Coord{X: int16(cols), Y: int16(rows)})
}

// Wait 阻塞至 shell 进程退出（无超时；调用方以 select + ctx 兜底）。非零
// 退出码是 shell 的正常终态，不算错误。
func (p *conPTY) Wait() error {
	ev, err := windows.WaitForSingleObject(p.proc, windows.INFINITE)
	if err != nil {
		return err
	}
	if ev != uint32(windows.WAIT_OBJECT_0) {
		return fmt.Errorf("conpty wait event=%d", ev)
	}
	var code uint32
	return windows.GetExitCodeProcess(p.proc, &code)
}

// KillAndClose 幂等收尾：杀进程树 → 等退出 → 关 pty 与管道。关 outR 会让
// 阻塞中的输出泵读侧解除阻塞（os.File 走 netpoll，Close 即唤醒）。
func (p *conPTY) KillAndClose() {
	p.closeMu.Lock()
	defer p.closeMu.Unlock()
	if p.closed {
		return
	}
	p.closed = true

	killTree(p.pid)
	// taskkill /T /F 异步落定：5s 兜底等句柄受信，防极端场景悬挂收尾。
	if ev, err := windows.WaitForSingleObject(p.proc, 5000); err == nil && ev == uint32(windows.WAIT_OBJECT_0) {
		var code uint32
		_ = windows.GetExitCodeProcess(p.proc, &code)
	}
	windows.ClosePseudoConsole(p.hpc)
	windows.CloseHandle(p.proc)
	windows.CloseHandle(p.thr)
	_ = p.inW.Close()
	_ = p.outR.Close()
}
