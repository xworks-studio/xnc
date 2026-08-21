// sessrun — 以 SYSTEM 令牌 + 指定交互会话运行命令（psexec -s -i 原理，
// 实验工具，非产品代码）。
//
// 用法：sessrun -session 1 -- cmd /c foo.exe args...
//
// 机制：DuplicateTokenEx(自身 SYSTEM 令牌) → 启用 SeTcbPrivilege →
// SetTokenInformation(TokenSessionId=N) → CreateProcessAsUserW
// (lpDesktop="winsta0\default")。等待子进程退出并透传退出码——子进程
// 的输出重定向由调用方在命令行里自理。
//
//go:build windows

package main

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	advapi32           = windows.NewLazySystemDLL("advapi32.dll")
	kernel32           = windows.NewLazySystemDLL("kernel32.dll")
	procOpenProcToken  = advapi32.NewProc("OpenProcessToken")
	procDupTokenEx     = advapi32.NewProc("DuplicateTokenEx")
	procSetTokenInfo   = advapi32.NewProc("SetTokenInformation")
	procLookupPriv     = advapi32.NewProc("LookupPrivilegeValueW")
	procAdjustPrivs    = advapi32.NewProc("AdjustTokenPrivileges")
	procCreateProcAsUser = kernel32.NewProc("CreateProcessAsUserW")
	procWaitForObj     = kernel32.NewProc("WaitForSingleObject")
	procGetExitCode    = kernel32.NewProc("GetExitCodeProcess")
)

const (
	tokenSessionID = 12 // TOKEN_INFORMATION_CLASS::TokenSessionId
	tokenPrimary   = 1  // TokenType::TokenPrimary
)

type luid struct{ LowPart, HighPart int32 }

type tokenPrivileges1 struct {
	PrivilegeCount uint32
	Luid           luid
	Attributes     uint32
}

type startupInfoW struct {
	Cb            uint32
	_             *uint16
	Desktop       *uint16
	Title         *uint16
	X, Y, XSize, YSize, XCountChars, YCountChars, FillAttribute, Flags uint32
	ShowWindow    uint16
	CbReserved2   uint16
	LpReserved2   *byte
	StdInput, StdOutput, StdError uintptr
}

type processInformation struct {
	Process, Thread uintptr
	ProcessID, ThreadID uint32
}

func main() {
	session := uint32(1)
	args := os.Args[1:]
	for len(args) > 0 && args[0] == "-session" && len(args) >= 2 {
		if _, err := fmt.Sscanf(args[1], "%d", &session); err != nil {
			fatal("bad -session: %v", err)
		}
		args = args[2:]
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fatal("usage: sessrun -session N -- <command> [args...]")
	}

	self, _, _ := kernel32.NewProc("GetCurrentProcess").Call()
	var hToken uintptr
	if r, _, e := procOpenProcToken.Call(self, uintptr(0x0002|0x0008|0x0100|0x0001) /*DUPLICATE|QUERY|ADJUST_SESSIONID|ASSIGN_PRIMARY*/, uintptr(unsafe.Pointer(&hToken))); r == 0 {
		fatal("OpenProcessToken: %v", e)
	}
	defer windows.Close(windows.Handle(hToken))

	// 启用 SeTcbPrivilege（SetTokenInformation(TokenSessionId) 必需）。
	var tp tokenPrivileges1
	if r, _, e := procLookupPriv.Call(0, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("SeTcbPrivilege"))), uintptr(unsafe.Pointer(&tp.Luid))); r == 0 {
		fatal("LookupPrivilegeValue(SeTcbPrivilege): %v", e)
	}
	tp.PrivilegeCount = 1
	tp.Attributes = 0x2 /*SE_PRIVILEGE_ENABLED*/
	procAdjustPrivs.Call(hToken, uintptr(unsafe.Pointer(&tp)), uintptr(4+unsafe.Sizeof(tp)), 0, 0)

	// 复制主令牌并改会话。ImpersonationLevel=SecurityImpersonation(2)，
	// TokenType=TokenPrimary(1)。
	var hDup uintptr
	if r, _, e := procDupTokenEx.Call(hToken, uintptr(0x0F01FF) /*TOKEN_ALL_ACCESS*/, 0,
		uintptr(2), uintptr(tokenPrimary), uintptr(unsafe.Pointer(&hDup))); r == 0 {
		fatal("DuplicateTokenEx: %v", e)
	}
	defer windows.Close(windows.Handle(hDup))
	if r, _, e := procSetTokenInfo.Call(hDup, uintptr(tokenSessionID),
		uintptr(unsafe.Pointer(&session)), 4); r == 0 {
		fatal("SetTokenInformation(TokenSessionId=%d): %v", session, e)
	}

	// 命令行 = args 空格连接；lpApplicationName 置 NULL，由命令行解析
	// （cmd 等可走 PATH）。
	line := ""
	for i, a := range args {
		if i > 0 {
			line += " "
		}
		line += a
	}
	line16 := windows.StringToUTF16Ptr(line)
	desktop16 := windows.StringToUTF16Ptr(`winsta0\default`)
	si := &startupInfoW{Cb: uint32(unsafe.Sizeof(startupInfoW{})), Desktop: desktop16}
	// StdInput/Output/Error 保持 0：调用方在命令行里用重定向拿输出。
	var pi processInformation
	if r, _, e := procCreateProcAsUser.Call(hDup,
		0, uintptr(unsafe.Pointer(line16)),
		0, 0, 1 /*bInheritHandles*/,
		0x08000000 /*CREATE_NO_WINDOW*/,
		0, 0, uintptr(unsafe.Pointer(si)), uintptr(unsafe.Pointer(&pi))); r == 0 {
		fatal("CreateProcessAsUserW: %v", e)
	}
	defer windows.Close(windows.Handle(pi.Process))
	defer windows.Close(windows.Handle(pi.Thread))
	fmt.Fprintf(os.Stderr, "sessrun: spawned pid=%d session=%d cmd=%s\n", pi.ProcessID, session, line)

	s, _, _ := procWaitForObj.Call(pi.Process, uintptr(0xFFFFFFFF))
	if s != 0 { // WAIT_OBJECT_0
		fatal("WaitForSingleObject: state=%d", s)
	}
	var code uint32
	procGetExitCode.Call(pi.Process, uintptr(unsafe.Pointer(&code)))
	fmt.Fprintf(os.Stderr, "sessrun: exit code %d\n", code)
	os.Exit(int(code))
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "sessrun: "+f+"\n", a...)
	os.Exit(1)
}
