// XNC Gate B spike — Candidate B: direct binding via golang.org/x/sys/windows
// x/sys v0.47.0 exports CreatePseudoConsole/ResizePseudoConsole/ClosePseudoConsole,
// Coord, StartupInfoEx and NewProcThreadAttributeList natively. No LazyProc needed.
package main

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---- the only constants x/sys does not export ----
const (
	extendedStartupinfoPresent     = 0x00080000
	procThreadAttributePseudoconsl = 0x00020016
)

// ---- ConPTY session wrapper: ALL the Windows-specific code lives here ----

type pty struct {
	hpcon     windows.Handle
	proc, thr windows.Handle
	pid       uint32
	inW, outR *os.File // our ends: inW feeds pty input, outR drains pty output
}

func newPty(cols, rows int16, cmdline string) (*pty, error) {
	// Pipe 1: we write inW -> pty reads inR. Pipe 2: pty writes outW -> we read outR.
	var inR, inWraw, outRraw, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inWraw, nil, 0); err != nil {
		return nil, fmt.Errorf("input pipe: %w", err)
	}
	if err := windows.CreatePipe(&outRraw, &outW, nil, 0); err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(inWraw)
		return nil, fmt.Errorf("output pipe: %w", err)
	}

	var hpcon windows.Handle
	err := windows.CreatePseudoConsole(windows.Coord{X: cols, Y: rows}, inR, outW, 0, &hpcon)
	if err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(outW)
		windows.CloseHandle(inWraw)
		windows.CloseHandle(outRraw)
		return nil, fmt.Errorf("CreatePseudoConsole: %w", err)
	}
	// EXPERIMENT: close pty-side pipe ends right after CreatePseudoConsole.
	windows.CloseHandle(inR)
	windows.CloseHandle(outW)

	alist, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		windows.ClosePseudoConsole(hpcon)
		return nil, fmt.Errorf("attribute list: %w", err)
	}
	defer alist.Delete()
	// HPCON is void* in C: lpValue must be the HANDLE VALUE ITSELF, not &handle
	// (passing &hpcon gave child exit 0xC0000142 — observed empirically).
	hp := hpcon
	hpPtr := *(*unsafe.Pointer)(unsafe.Pointer(&hp))
	if err := alist.Update(procThreadAttributePseudoconsl, hpPtr, unsafe.Sizeof(hp)); err != nil {
		windows.ClosePseudoConsole(hpcon)
		return nil, fmt.Errorf("UpdateProcThreadAttribute: %w", err)
	}

	siEx := &windows.StartupInfoEx{ProcThreadAttributeList: alist.List()}
	siEx.StartupInfo.Cb = uint32(unsafe.Sizeof(*siEx))
	// The conpty lib sets this; empirically REQUIRED (without it the child
	// detaches from the pty and output is lost — see FINDINGS).
	siEx.StartupInfo.Flags |= windows.STARTF_USESTDHANDLES

	cmd16, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		windows.ClosePseudoConsole(hpcon)
		return nil, err
	}
	var pi windows.ProcessInformation
	err = windows.CreateProcess(nil, cmd16, nil, nil, false,
		extendedStartupinfoPresent, nil, nil, &siEx.StartupInfo, &pi)
	if err != nil {
		windows.ClosePseudoConsole(hpcon)
		windows.CloseHandle(inWraw)
		windows.CloseHandle(outRraw)
		return nil, fmt.Errorf("CreateProcess: %w", err)
	}

	return &pty{
		hpcon: hpcon, proc: pi.Process, thr: pi.Thread, pid: pi.ProcessId,
		inW:  os.NewFile(uintptr(inWraw), "pty-in"),
		outR: os.NewFile(uintptr(outRraw), "pty-out"),
	}, nil
}

func (p *pty) write(b []byte) (int, error) { return p.inW.Write(b) }

func (p *pty) resize(cols, rows int16) error {
	return windows.ResizePseudoConsole(p.hpcon, windows.Coord{X: cols, Y: rows})
}

func (p *pty) waitExit(timeout time.Duration) (uint32, error) {
	ev, err := windows.WaitForSingleObject(p.proc, uint32(timeout.Milliseconds()))
	if err != nil {
		return 0, err
	}
	if ev != uint32(windows.WAIT_OBJECT_0) {
		return 0, fmt.Errorf("wait event=%d (timeout)", ev)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(p.proc, &code); err != nil {
		return 0, err
	}
	return code, nil
}

func (p *pty) close() {
	windows.ClosePseudoConsole(p.hpcon)
	windows.CloseHandle(p.proc)
	windows.CloseHandle(p.thr)
	p.inW.Close()
	p.outR.Close()
}

// ---------------- 6-step test harness (identical logic to Candidate A) ----------------

var (
	mu  sync.Mutex
	out strings.Builder
)

var passCount, failCount int

func step(name string, ok bool, detail string) {
	st := "PASS"
	if !ok {
		st = "FAIL"
		failCount++
	} else {
		passCount++
	}
	fmt.Printf("[%-4s] %s | %s\n", st, name, detail)
}

func pump(r ioReader) {
	buf := make([]byte, 8192)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

type ioReader interface {
	Read([]byte) (int, error)
}

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

func waitFor(substr string, d time.Duration) (bool, time.Duration) {
	start := time.Now()
	deadline := start.Add(d)
	for time.Now().Before(deadline) {
		mu.Lock()
		s := out.String()
		mu.Unlock()
		if strings.Contains(s, substr) || strings.Contains(ansiRe.ReplaceAllString(s, ""), substr) {
			return true, time.Since(start)
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false, time.Since(start)
}

func headOf(n int) string {
	mu.Lock()
	defer mu.Unlock()
	s := out.String()
	if len(s) > n {
		s = s[:n]
	}
	return s
}

func pidAlive(pid int) bool {
	b, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(b), fmt.Sprintf("%q", fmt.Sprint(pid)))
}

func countProc(name string) int {
	b, err := exec.Command("tasklist", "/FI", fmt.Sprintf("IMAGENAME eq %s", name), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return math.MinInt32
	}
	return strings.Count(string(b), "\""+name+"\"")
}

func discoverShell() string {
	if s := os.Getenv("XNC_SHELL"); s != "" {
		return s
	}
	if p, err := exec.LookPath("pwsh.exe"); err == nil {
		return fmt.Sprintf("%q -NoLogo -NoProfile", p)
	}
	return "powershell.exe -NoLogo -NoProfile"
}

func main() {
	fmt.Println("=== XNC Gate B — Candidate B: direct x/sys/windows binding (v0.47.0) ===")
	shell := discoverShell()
	fmt.Printf("Shell under test: %s\n", shell)
	conhostBefore := countProc("conhost.exe")

	// Step 1: create pty sized 80x25 + spawn shell attached to it.
	start := time.Now()
	p, err := newPty(80, 25, shell)
	if err != nil {
		step("1 create 80x25 + spawn", false, "newPty error: "+err.Error())
		os.Exit(1)
	}
	step("1 create 80x25 + spawn", true, fmt.Sprintf("pid=%d spawnTook=%v", p.pid, time.Since(start).Round(time.Millisecond)))
	go pump(p.outR)

	// Step 2: echo and assert output round-trip.
	p.write([]byte("echo gateb-ok\r"))
	ok, el := waitFor("gateb-ok", 6*time.Second)
	step("2 echo gateb-ok round-trip", ok, fmt.Sprintf("matched after %v (budget 6s)", el.Round(time.Millisecond)))
	fmt.Printf("       raw head: %q\n", headOf(140))

	// Step 3: Ctrl+C (0x03) then echo — session must survive.
	p.write([]byte{0x03})
	time.Sleep(700 * time.Millisecond)
	p.write([]byte("echo after-ctrlc\r"))
	ok, el = waitFor("after-ctrlc", 6*time.Second)
	step("3 Ctrl+C survival", ok, fmt.Sprintf("after-ctrlc matched after %v", el.Round(time.Millisecond)))

	// Step 4: resize to 120x40 then echo.
	rerr := p.resize(120, 40)
	p.write([]byte("echo after-resize\r"))
	ok2, el := waitFor("after-resize", 6*time.Second)
	step("4 resize 120x40 + echo", rerr == nil && ok2, fmt.Sprintf("Resize err=%v, echo matched=%v after %v", rerr, ok2, el.Round(time.Millisecond)))

	// Step 5: exit + wait for process exit with timeout.
	p.write([]byte("exit\r"))
	code, werr := p.waitExit(10 * time.Second)
	step("5 exit + waitExit(10s)", werr == nil && code == 0, fmt.Sprintf("exitCode=%d err=%v", code, werr))

	// Step 6: close pty; verify no residual child by recorded PID.
	p.close()
	time.Sleep(800 * time.Millisecond)
	alive := pidAlive(int(p.pid))
	conhostDelta := conhostBefore - countProc("conhost.exe")
	step("6 close + no residual", !alive,
		fmt.Sprintf("pid(%d)Alive=%v conhostDelta(before-after)=%d", p.pid, alive, conhostDelta))

	fmt.Printf("\n=== Candidate B RESULT: %d PASS / %d FAIL ===\n", passCount, failCount)
	if failCount > 0 {
		os.Exit(1)
	}
}
