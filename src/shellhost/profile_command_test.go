package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeProfileWhitelist(t *testing.T) {
	for in, want := range map[string]string{
		"powershell": "POWERSHELL", "PowerShell": "POWERSHELL",
		"pwsh": "PWSH", "PWSH": "PWSH",
		"cmd": "CMD", "Cmd": "CMD",
		"bash": "BASH",
	} {
		got, err := normalizeProfile(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	// 路径参数绝不接受(spec §8.2)。
	for _, hostile := range []string{
		`C:\Windows\System32\cmd.exe`, `/bin/sh`, "cmd.exe /c evil", "", "zsh",
	} {
		_, err := normalizeProfile(hostile)
		assert.Errorf(t, err, "profile %q must be refused", hostile)
	}
}

func TestResolveProfileMatrix(t *testing.T) {
	origLook, origStat := lookPathProc, statProc
	t.Cleanup(func() { lookPathProc, statProc = origLook, origStat })

	lookPathProc = func(name string) (string, error) {
		if name == "cmd.exe" || name == "powershell.exe" {
			return `C:\Windows\System32\` + name, nil
		}
		return "", os.ErrNotExist
	}
	statProc = func(p string) (os.FileInfo, error) { return nil, os.ErrNotExist }

	// CMD/POWERSHELL 恒可解析。
	for p, want := range map[string]string{
		"CMD":        `C:\Windows\System32\cmd.exe`,
		"POWERSHELL": `C:\Windows\System32\powershell.exe`,
	} {
		exe, err := resolveProfile(p)
		require.NoError(t, err, p)
		assert.Equal(t, want, exe)
	}

	// PWSH 缺失:明确错误(exit 2 语义)。
	_, err := resolveProfile("PWSH")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PWSH not available")

	// BASH 缺失(PATH 与常见路径全空)。
	_, err = resolveProfile("BASH")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BASH not available")

	// BASH 经 PATH 找回。
	lookPathProc = func(name string) (string, error) {
		if name == "bash" {
			return `/usr/bin/bash`, nil
		}
		return "", os.ErrNotExist
	}
	exe, err := resolveProfile("BASH")
	require.NoError(t, err)
	assert.Equal(t, `/usr/bin/bash`, exe)
}

// TestBuildOneshotCommandMatrix 与 agent/session/exec.go buildCommand 命令
// 模式分支逐参数对齐(脚本模式不在 xnc-shell 范围)。
func TestBuildOneshotCommandMatrix(t *testing.T) {
	t.Run("bash inline", func(t *testing.T) {
		cmd, err := buildOneshotCommand("BASH", `C:\Git\bin\bash.exe`, "echo hi", nil)
		require.NoError(t, err)
		assert.Equal(t, []string{`C:\Git\bin\bash.exe`, "-c", "echo hi"}, cmd.Args)
	})
	t.Run("bash env prefix", func(t *testing.T) {
		cmd, err := buildOneshotCommand("BASH", `bash`, "echo $K", []string{"K=V"})
		require.NoError(t, err)
		assert.Equal(t, []string{"bash", "-c", "export K='V'; echo $K"}, cmd.Args)
	})
	t.Run("cmd inline", func(t *testing.T) {
		cmd, err := buildOneshotCommand("CMD", `C:\Windows\System32\cmd.exe`, "echo hi", nil)
		require.NoError(t, err)
		assert.Equal(t, []string{`C:\Windows\System32\cmd.exe`, "/c", "echo hi"}, cmd.Args)
	})
	t.Run("cmd env prefix", func(t *testing.T) {
		cmd, err := buildOneshotCommand("CMD", "cmd", "echo %K%", []string{"K=V"})
		require.NoError(t, err)
		assert.Equal(t, []string{"cmd", "/c", "set K=V&& echo %K%"}, cmd.Args)
	})
	t.Run("pwsh flags", func(t *testing.T) {
		cmd, err := buildOneshotCommand("PWSH", `C:\Program Files\PowerShell\7\pwsh.exe`, "echo hi", nil)
		require.NoError(t, err)
		assert.Equal(t, []string{
			`C:\Program Files\PowerShell\7\pwsh.exe`,
			"-NoLogo", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", "echo hi",
		}, cmd.Args)
	})
	t.Run("powershell env prefix", func(t *testing.T) {
		cmd, err := buildOneshotCommand("POWERSHELL", "powershell.exe", "echo $env:K", []string{"K=V"})
		require.NoError(t, err)
		assert.Equal(t, []string{
			"powershell.exe", "-NoLogo", "-NonInteractive", "-ExecutionPolicy", "Bypass",
			"-Command", "$env:K='V'; echo $env:K",
		}, cmd.Args)
	})
	t.Run("malformed env skipped", func(t *testing.T) {
		cmd, err := buildOneshotCommand("CMD", "cmd", "echo hi", []string{"NOEQUALS", "K=V"})
		require.NoError(t, err)
		assert.Equal(t, []string{"cmd", "/c", "set K=V&& echo hi"}, cmd.Args)
	})
	t.Run("unsupported profile refused", func(t *testing.T) {
		_, err := buildOneshotCommand("zsh", "zsh", "x", nil)
		assert.Error(t, err)
	})
}
