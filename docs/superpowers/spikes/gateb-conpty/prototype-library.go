// XNC Gate B spike — Candidate A: github.com/UserExistsError/conpty
// Throwaway feasibility test. 6-step PASS/FAIL harness.
package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	conpty "github.com/UserExistsError/conpty"
)

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

// pump drains pty output into `out` (blocking reads in a goroutine).
func pump(r interface {
	Read([]byte) (int, error)
}) {
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

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// waitFor searches raw output AND ansi-stripped output (ASCII-safe match).
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
	fmt.Println("=== XNC Gate B — Candidate A: UserExistsError/conpty v0.1.4 ===")
	fmt.Printf("ConPTY available on this OS: %v\n", conpty.IsConPtyAvailable())
	shell := discoverShell()
	fmt.Printf("Shell under test: %s\n", shell)
	conhostBefore := countProc("conhost.exe")

	// Step 1: create pty sized 80x25 + spawn shell attached to it.
	start := time.Now()
	pty, err := conpty.Start(shell, conpty.ConPtyDimensions(80, 25))
	if err != nil {
		step("1 create 80x25 + spawn", false, "Start error: "+err.Error())
		os.Exit(1)
	}
	pid := pty.Pid()
	step("1 create 80x25 + spawn", true, fmt.Sprintf("pid=%d spawnTook=%v", pid, time.Since(start).Round(time.Millisecond)))
	go pump(pty)

	// Step 2: echo and assert output round-trip.
	pty.Write([]byte("echo gateb-ok\r"))
	ok, el := waitFor("gateb-ok", 6*time.Second)
	step("2 echo gateb-ok round-trip", ok, fmt.Sprintf("matched after %v (budget 6s)", el.Round(time.Millisecond)))
	fmt.Printf("       raw head: %q\n", headOf(140))

	// Step 3: Ctrl+C (0x03) then echo — session must survive.
	pty.Write([]byte{0x03})
	time.Sleep(700 * time.Millisecond)
	pty.Write([]byte("echo after-ctrlc\r"))
	ok, el = waitFor("after-ctrlc", 6*time.Second)
	step("3 Ctrl+C survival", ok, fmt.Sprintf("after-ctrlc matched after %v", el.Round(time.Millisecond)))

	// Step 4: resize to 120x40 then echo.
	rerr := pty.Resize(120, 40)
	pty.Write([]byte("echo after-resize\r"))
	ok2, el := waitFor("after-resize", 6*time.Second)
	step("4 resize 120x40 + echo", rerr == nil && ok2, fmt.Sprintf("Resize err=%v, echo matched=%v after %v", rerr, ok2, el.Round(time.Millisecond)))

	// Step 5: exit + wait for process exit with timeout.
	pty.Write([]byte("exit\r"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code, werr := pty.Wait(ctx)
	step("5 exit + Wait(10s)", werr == nil && code == 0, fmt.Sprintf("exitCode=%d err=%v", code, werr))

	// Step 6: close pty; verify no residual child by recorded PID.
	clerr := pty.Close()
	time.Sleep(800 * time.Millisecond)
	alive := pidAlive(pid)
	conhostDelta := conhostBefore - countProc("conhost.exe")
	step("6 Close + no residual", clerr == nil && !alive,
		fmt.Sprintf("closeErr=%v pid(%d)Alive=%v conhostDelta(before-after)=%d", clerr, pid, alive, conhostDelta))

	fmt.Printf("\n=== Candidate A RESULT: %d PASS / %d FAIL ===\n", passCount, failCount)
	if failCount > 0 {
		os.Exit(1)
	}
}
