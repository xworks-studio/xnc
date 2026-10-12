//go:build linux

// shellhost_linux_test.go — direct PTY 宿主的单测（真机/CI ubuntu 运行；
// Windows 开发机经 `go test -c` 交叉编译产物在目标机执行）。
package session

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readUntil 从输出流读到包含 want 或超时；返回累计输出。
func readUntil(t *testing.T, ch <-chan ShellStream, want string, timeout time.Duration) string {
	t.Helper()
	var sb strings.Builder
	deadline := time.After(timeout)
	for {
		select {
		case d, ok := <-ch:
			if !ok {
				return sb.String()
			}
			sb.Write(d.Bytes)
			if strings.Contains(sb.String(), want) {
				return sb.String()
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %q, got: %q", want, sb.String())
		}
	}
}

func TestDirectShellHostOneshotEcho(t *testing.T) {
	h := &directShellHost{}
	p, err := h.CreateShell(ShellSpec{Profile: "SH", Command: "printf xnc-echo"})
	require.NoError(t, err)
	defer p.Close()
	var out strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		for d := range p.Stream() {
			out.Write(d.Bytes)
		}
	}()
	select {
	case code := <-p.Exit():
		assert.EqualValues(t, 0, code)
	case <-time.After(15 * time.Second):
		t.Fatal("exit timeout")
	}
	<-done
	assert.Equal(t, "xnc-echo", out.String())
	assert.Equal(t, "SH", p.Profile())
	assert.EqualValues(t, 0, p.Dropped())
}

func TestDirectShellHostOneshotExitCodeAndStreams(t *testing.T) {
	h := &directShellHost{}
	// stdout/stderr 分流：0x01/0x02 前缀由会话泵加，这里验证 Stderr 标记。
	p, err := h.CreateShell(ShellSpec{Profile: "BASH",
		Command: "echo out; echo err >&2; exit 42"})
	require.NoError(t, err)
	defer p.Close()
	var stdout, stderr strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		for d := range p.Stream() {
			if d.Stderr {
				stderr.Write(d.Bytes)
			} else {
				stdout.Write(d.Bytes)
			}
		}
	}()
	select {
	case code := <-p.Exit():
		assert.EqualValues(t, 42, code)
	case <-time.After(15 * time.Second):
		t.Fatal("exit timeout")
	}
	<-done
	assert.Equal(t, "out\n", stdout.String())
	assert.Equal(t, "err\n", stderr.String())
}

func TestDirectShellHostTimeoutKillsTree(t *testing.T) {
	h := &directShellHost{}
	start := time.Now()
	// 后台子进程 + wait：验证组杀覆盖孙进程（bash 的 pgid == sleep 的 pgid）。
	p, err := h.CreateShell(ShellSpec{Profile: "BASH",
		Command: "sleep 30 & wait", TimeoutSec: 1})
	require.NoError(t, err)
	defer p.Close()
	go func() {
		for range p.Stream() {
		}
	}()
	select {
	case code := <-p.Exit():
		assert.Less(t, time.Since(start), 10*time.Second, "must die on timeout")
		assert.EqualValues(t, 128+9, code) // SIGKILL 组杀 → 137（shell 惯例）
	case <-time.After(15 * time.Second):
		t.Fatal("host-side timeout did not fire")
	}
}

func TestDirectShellHostOutputBudget(t *testing.T) {
	h := &directShellHost{}
	// 5MB stdout 超每流 4MB 预算：保留 4MB，丢弃 1MB，末尾 stderr marker。
	p, err := h.CreateShell(ShellSpec{Profile: "SH",
		Command: "head -c 5242880 /dev/zero | tr '\\0' x"})
	require.NoError(t, err)
	defer p.Close()
	var n int
	var tail strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		for d := range p.Stream() {
			n += len(d.Bytes)
			if d.Stderr {
				tail.Write(d.Bytes)
			}
		}
	}()
	select {
	case <-p.Exit():
	case <-time.After(30 * time.Second):
		t.Fatal("exit timeout")
	}
	<-done
	assert.EqualValues(t, 1024*1024, p.Dropped(), "dropped = 5MB-4MB")
	assert.Contains(t, tail.String(), "over budget", "stderr marker expected")
	assert.Equal(t, 4*1024*1024, n-len(tail.String()), "kept stdout = budget")
}

func TestDirectShellHostInteractive(t *testing.T) {
	h := &directShellHost{}
	p, err := h.CreateShell(ShellSpec{Profile: "BASH", Interactive: true, Cols: 80, Rows: 24})
	require.NoError(t, err)
	defer func() { _ = p.Kill(); _ = p.Close() }()
	require.Equal(t, "BASH", p.Profile())
	require.NoError(t, p.WriteStdin([]byte("echo xnc-pty-ok\r")))
	readUntil(t, p.Stream(), "xnc-pty-ok", 15*time.Second)
	require.NoError(t, p.Resize(100, 30))
	// Kill 后 Exit 必须收敛（组杀幂等）。
	require.NoError(t, p.Kill())
	select {
	case <-p.Exit():
	case <-time.After(10 * time.Second):
		t.Fatal("no exit after kill")
	}
}

func TestDirectShellHostRejects(t *testing.T) {
	h := &directShellHost{}
	// CMD 是 Windows 专属 profile；未知 profile 一律 BAD_PAYLOAD。
	for _, profile := range []string{"CMD", "FISH", ""} {
		_, err := h.CreateShell(ShellSpec{Profile: profile, Command: "true"})
		var she *ShellHostError
		require.ErrorAs(t, err, &she, "profile %q", profile)
		assert.Equal(t, "BAD_PAYLOAD", she.Code)
	}
	// oneshot 空命令 → BAD_PAYLOAD。
	_, err := h.CreateShell(ShellSpec{Profile: "SH"})
	var she *ShellHostError
	require.ErrorAs(t, err, &she)
	assert.Equal(t, "BAD_PAYLOAD", she.Code)
	// 可执行缺失 → SPAWN_FAILED（zsh 通常未装；装了则跳过）。
	if _, err := exec.LookPath("zsh"); err != nil {
		_, err := h.CreateShell(ShellSpec{Profile: "ZSH", Command: "true"})
		var she *ShellHostError
		require.ErrorAs(t, err, &she)
		assert.Equal(t, "SPAWN_FAILED", she.Code)
	}
}
