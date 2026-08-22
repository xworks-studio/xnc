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
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
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
	if err != nil || rtt <= 0 {
		dumpServerEvidence()
		t.Fatalf("ping: %v %v (server stderr: %s)", err, rtt, stderrPath)
	}
	t.Logf("cross-language PING/PONG ok, rtt=%v", rtt)
}
