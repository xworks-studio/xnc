//go:build windows

// cross_test.go — 跨语言 smoke(M0 终验):拉起 native/core 产出的
// xnc-core.exe,Go 侧完成三步握手 + PING/PONG 往返,证明两种语言的
// 帧/握手实现字节级一致。默认跳过;XNC_CORE_EXE 指向 xnc-core.exe 时启用。
// pipe DACL 为 SYSTEM+Administrators(spec §9),非提权 shell 会被设计性地
// 拒绝(Access is denied)——此时 skip 并给出提权重跑指引,提权环境必须 PASS。
package coreclient

import (
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
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
	cmd := exec.Command(exe, "--console", "--pipe-name", `\\.\pipe\xnc-core-smoke`,
		"--smoke-secret", hex.EncodeToString([]byte(secret)))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	time.Sleep(500 * time.Millisecond) // pipe 就绪
	c, err := Dial(`\\.\pipe\xnc-core-smoke`, []byte(secret))
	if err != nil {
		// DACL(SYSTEM+Admins)对非提权 shell 的预期拒绝:skip 而非 fail。
		if strings.Contains(err.Error(), "Access is denied") {
			t.Skipf("run from an elevated shell: XNC_CORE_EXE=%s go test ./coreclient/ -run Cross -v (dial: %v)", exe, err)
		}
		t.Fatalf("handshake with xnc-core failed: %v", err)
	}
	defer c.Close()
	rtt, err := c.Ping()
	if err != nil || rtt <= 0 {
		t.Fatalf("ping: %v %v", err, rtt)
	}
	t.Logf("cross-language PING/PONG ok, rtt=%v", rtt)
}
