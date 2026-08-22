//go:build windows

// cross_test.go — 跨语言 smoke(M0 终验):拉起 native/core 产出的
// xnc-core.exe,Go 侧完成三步握手 + PING/PONG 往返,证明两种语言的
// 帧/握手实现字节级一致。默认跳过;XNC_CORE_EXE 指向 xnc-core.exe 时启用。
// pipe DACL 为 SYSTEM+Administrators(spec §9),非提权 shell 会被设计性地
// 拒绝(Access is denied)——此时 skip 并给出提权重跑指引,提权环境必须 PASS。
// 服务端 stderr 与退出码落盘到 t.TempDir()(log.h 逐行写 stderr),
// 失败时内容 + 路径一并进测试日志,提权重跑自带服务端证据。
package coreclient

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"xnc/agent/desktoppipe"
)

// XNC_CORE_EXE 指向 xnc-core.exe 时启用(默认跳过);CI 与本地验收:
//
//	XNC_CORE_EXE=../../bin/xnc-core.exe go test ./coreclient/ -run Cross -v
func TestCrossLanguageHandshake(t *testing.T) {
	exe := os.Getenv("XNC_CORE_EXE")
	if exe == "" {
		t.Skip("set XNC_CORE_EXE to run cross-language smoke")
	}
	const secret = "test-pipe-secret"

	// 服务端证据:xnc-core 的握手/连接日志(log.h)全在 stderr,落盘留存。
	stderrPath := filepath.Join(t.TempDir(), "xnc-core-stderr.log")
	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		t.Fatalf("create stderr capture file: %v", err)
	}
	cmd := exec.Command(exe, "--console", "--pipe-name", `\\.\pipe\xnc-core-smoke`,
		"--smoke-secret", hex.EncodeToString([]byte(secret)))
	cmd.Stderr = stderrFile // stdout 一并并入,防未来日志走 stdout
	cmd.Stdout = stderrFile
	if err := cmd.Start(); err != nil {
		stderrFile.Close()
		t.Fatal(err)
	}
	var once sync.Once
	var serverExit error
	finishServer := func() { // 幂等:Kill + Wait + 关文件,记录服务端退出状态
		once.Do(func() {
			_ = cmd.Process.Kill()
			serverExit = cmd.Wait()
			_ = stderrFile.Close()
		})
	}
	defer finishServer()
	// dumpServerEvidence 把服务端 stderr 内容与退出状态打进测试日志
	// (失败路径调用;提权重跑因此自带服务端侧证据)。
	dumpServerEvidence := func() {
		finishServer()
		t.Logf("xnc-core exit after kill: %v", serverExit)
		if b, rerr := os.ReadFile(stderrPath); rerr == nil {
			t.Logf("xnc-core stderr (%s):", stderrPath)
			if len(b) == 0 {
				t.Logf("  <empty>")
			} else {
				for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
					t.Logf("  | %s", line)
				}
			}
		} else {
			t.Logf("xnc-core stderr unreadable (%v), path: %s", rerr, stderrPath)
		}
	}

	time.Sleep(500 * time.Millisecond) // pipe 就绪
	c, err := Dial(`\\.\pipe\xnc-core-smoke`, []byte(secret))
	if err != nil {
		// DACL(SYSTEM+Admins)对非提权 shell 的预期拒绝:skip 而非 fail。
		// errors.Is 对抗系统语言本地化;字符串匹配保留兜底。
		if errors.Is(err, syscall.ERROR_ACCESS_DENIED) || strings.Contains(err.Error(), "Access is denied") {
			t.Skipf("run from an elevated shell: XNC_CORE_EXE=%s go test ./coreclient/ -run Cross -v (dial: %v)", exe, err)
		}
		dumpServerEvidence()
		t.Fatalf("handshake with xnc-core failed: %v (server stderr: %s)", err, stderrPath)
	}
	defer c.Close()
	rtt, err := c.Ping()
	if err != nil {
		dumpServerEvidence()
		t.Fatalf("ping: %v (server stderr: %s)", err, stderrPath)
	}
	// rtt==0s 是合法读数:Windows 单调钟粒度 ~0.5ms(本机实测最小正
	// delta 347µs),本地 pipe 往返常低于一个 tick(实测 476/500 次为
	// 0s)。成功判据是 err==nil —— Ping 仅在严格匹配 MsgPong +
	// FlagResponse + 同 RequestID 的回帧时返回 nil,无 Pong 会以 2s 读
	// 超时报错,不存在"假成功";不得断言 rtt>0。
	t.Logf("cross-language PING/PONG ok, rtt=%v", rtt)
}

// TestStartCaptureCross — M1-Slice2 Task 3 真 core 冒烟:START_CAPTURE
// 经 xnc-core spawn xnc-desktop(session bridge + rt pipe),agent 侧再以
// desktoppipe 订阅收到 HOST_HELLO,STOP_CAPTURE 终止子进程。门:
// XNC_CORE_EXE 指向 xnc-core.exe;DACL 拒绝(非提权)→ skip;TOKEN_
// FAILED(提权 admin 无 SeTcb,需 SYSTEM)/ SPAWN_FAILED(bin 无
// xnc-desktop.exe)→ skip 并给指引;其余失败 fail。子进程清理:
// STOP 先于杀 core(kill core 不会走其优雅清理,避免孤儿 xnc-desktop)。
func TestStartCaptureCross(t *testing.T) {
	exe := os.Getenv("XNC_CORE_EXE")
	if exe == "" {
		t.Skip("set XNC_CORE_EXE to run the start-capture cross smoke")
	}
	const secret = "test-pipe-secret"
	const corePipe = `\\.\pipe\xnc-core-smoke-rt`

	stderrPath := filepath.Join(t.TempDir(), "xnc-core-stderr.log")
	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		t.Fatalf("create stderr capture file: %v", err)
	}
	cmd := exec.Command(exe, "--console", "--pipe-name", corePipe,
		"--smoke-secret", hex.EncodeToString([]byte(secret)))
	cmd.Stderr = stderrFile
	cmd.Stdout = stderrFile
	if err := cmd.Start(); err != nil {
		stderrFile.Close()
		t.Fatal(err)
	}
	var once sync.Once
	finishServer := func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			_ = stderrFile.Close()
		})
	}
	defer finishServer() // LIFO:在 stopCapture 之后执行

	time.Sleep(500 * time.Millisecond)
	c, err := Dial(corePipe, []byte(secret))
	if err != nil {
		if errors.Is(err, syscall.ERROR_ACCESS_DENIED) || strings.Contains(err.Error(), "Access is denied") {
			t.Skipf("run from an elevated shell: XNC_CORE_EXE=%s go test ./coreclient/ -run StartCaptureCross -v (dial: %v)", exe, err)
		}
		finishServer()
		t.Fatalf("handshake with xnc-core failed: %v (server stderr: %s)", err, stderrPath)
	}
	var sub *desktoppipe.Sub
	defer func() { // 先停采集再杀 core,避免孤儿 xnc-desktop
		if sub != nil {
			sub.Close()
		}
		if c != nil {
			_ = c.StopCapture()
		}
	}()
	defer c.Close()

	session := windows.WTSGetActiveConsoleSessionId()
	if session == 0xFFFFFFFF {
		t.Skip("no active console session (headless); StartCapture needs one")
	}
	pid, pipeName, capSecret, gen, err := c.StartCapture(session)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "TOKEN_FAILED"):
			t.Skipf("core not running as SYSTEM (SeTcb needed for the session token): %v", err)
		case strings.Contains(err.Error(), "SPAWN_FAILED"):
			t.Skipf("xnc-desktop.exe missing next to %s: %v", exe, err)
		}
		finishServer()
		t.Fatalf("StartCapture: %v (server stderr: %s)", err, stderrPath)
	}
	wantPipe := fmt.Sprintf(`\\.\pipe\xnc-desktop-rt-%d`, cmd.Process.Pid)
	if pid == 0 || pipeName != wantPipe || len(capSecret) != 32 || gen == 0 {
		t.Fatalf("StartCapture = pid %d pipe %q gen %d secretLen %d", pid, pipeName, gen, len(capSecret))
	}
	t.Logf("start_capture ok: desktop pid=%d pipe=%s gen=%d", pid, pipeName, gen)

	// 幂等:重复 START 返回同一 pid/pipe/secret/gen,不重复 spawn。
	pid2, pipe2, sec2, gen2, err := c.StartCapture(session)
	if err != nil {
		t.Fatalf("idempotent StartCapture: %v", err)
	}
	if pid2 != pid || pipe2 != pipeName || gen2 != gen || !bytes.Equal(sec2, capSecret) {
		t.Fatalf("idempotent StartCapture diverged: pid %d pipe %q gen %d", pid2, pipe2, gen2)
	}

	// 端到端:agent 侧 desktoppipe 订阅真实 xnc-desktop,HOST_HELLO
	// 即 ATTACH 确认;收到即退(帧消费属 T4)。
	subID := uint32(time.Now().UnixNano() & 0xFFFFFFFF)
	if subID == 0 {
		subID = 1
	}
	sub, err = desktoppipe.Dial(pipeName, hex.EncodeToString(capSecret), subID,
		desktoppipe.SubOpts{MaxFPS: 30, MaxW: 1920, Bitrate: 2300000})
	if err != nil {
		t.Fatalf("desktoppipe.Dial against the spawned host: %v", err)
	}
	hello := sub.Hello()
	if hello == nil || hello.W == 0 || hello.H == 0 || hello.Fps == 0 {
		t.Fatalf("HOST_HELLO = %+v", hello)
	}
	t.Logf("desktop host hello: %+v", hello)

	if err := c.StopCapture(); err != nil {
		t.Fatalf("StopCapture: %v", err)
	}
	t.Log("stop_capture ok (child terminated; watcher reaps in core)")
}
