//go:build windows

package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 回归覆盖（评审 Critical）：服务参数必须是逐词的 argv 元素。
// 修复前是单个拼好的字符串，被 CreateService 整体引用成一个 argv 词，
// 服务启动后 os.Args[1] 为整串、cobra 报未知命令，服务启动失败。
func TestBuildServiceArgs(t *testing.T) {
	t.Run("no token", func(t *testing.T) {
		got := buildServiceArgs("http://127.0.0.1:8080", "", `C:\ProgramData\XNCAgent`, "", "", "")
		assert.Equal(t, []string{
			"run",
			"--server=http://127.0.0.1:8080",
			`--state-dir=C:\ProgramData\XNCAgent`,
		}, got)
	})

	t.Run("with token appended last", func(t *testing.T) {
		got := buildServiceArgs("https://xnc.example", "tok-123", `C:\ProgramData\XNCAgent`, "", "", "")
		assert.Equal(t, []string{
			"run",
			"--server=https://xnc.example",
			`--state-dir=C:\ProgramData\XNCAgent`,
			"--token=tok-123",
		}, got)
	})

	t.Run("every element is a single argv word", func(t *testing.T) {
		for _, el := range buildServiceArgs("http://s", "t", `D:\State Dir\XNCAgent`, "", "", "") {
			assert.NotContains(t, el, " --",
				"joined-string shape regressed: element %q contains multiple flags", el)
		}
	})

	// M2-Slice3 Task 2: dev profile(XNCAgentDev + 隔离 state 目录 + core
	// pipe 凭据)整体进 argv;生产名 XNCAgent 不追加 --service-name(默认)。
	t.Run("dev profile", func(t *testing.T) {
		got := buildServiceArgs("http://10.0.0.5:8080", "tok", `C:\ProgramData\XNCAgentDev`,
			"XNCAgentDev", `\\.\pipe\xnc-core-dev`, "746573")
		assert.Equal(t, []string{
			"run",
			"--server=http://10.0.0.5:8080",
			`--state-dir=C:\ProgramData\XNCAgentDev`,
			"--service-name=XNCAgentDev",
			`--desktop-core-pipe=\\.\pipe\xnc-core-dev`,
			"--desktop-core-secret-hex=746573",
			"--token=tok",
		}, got)
	})
}

// 状态目录统一为 %ProgramData%\XNC（设计 §3.3/§14）。
func TestDefaultStateDir(t *testing.T) {
	t.Setenv("ProgramData", `D:\ProgramData`)
	assert.Equal(t, `D:\ProgramData\XNC`, defaultStateDir())
}

func TestMigrateLegacyStateDir(t *testing.T) {
	write := func(dir, name, content string) {
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}

	t.Run("legacy only: wholesale move, identity survives", func(t *testing.T) {
		base := t.TempDir()
		old, new := filepath.Join(base, "XNCAgent"), filepath.Join(base, "XNC")
		write(old, "identity.json", "{}")
		write(old, "agent-service.log", "log")

		got, logs := migrateLegacyStateDir(new)
		assert.Equal(t, new, got)
		require.Len(t, logs, 1, "successful move buffers exactly the info entry")
		assert.Equal(t, slog.LevelInfo, logs[0].level)
		assert.NoDirExists(t, old, "successful move removes the legacy dir (contents moved, not copied)")
		for _, f := range []string{"identity.json", "agent-service.log"} {
			assert.FileExists(t, filepath.Join(new, f), "%s must survive migration", f)
		}
	})

	t.Run("both exist: prefer new, leave legacy untouched", func(t *testing.T) {
		base := t.TempDir()
		old, new := filepath.Join(base, "XNCAgent"), filepath.Join(base, "XNC")
		write(old, "legacy-marker.txt", "old")
		write(new, "new-marker.txt", "new")

		got, logs := migrateLegacyStateDir(new)
		assert.Equal(t, new, got)
		require.Len(t, logs, 1)
		assert.Equal(t, slog.LevelWarn, logs[0].level,
			"both-exist is the diagnostic the ruling requires to be logged")
		assert.FileExists(t, filepath.Join(old, "legacy-marker.txt"))
		assert.FileExists(t, filepath.Join(new, "new-marker.txt"))
	})

	t.Run("neither exists: create new dir", func(t *testing.T) {
		base := t.TempDir()
		new := filepath.Join(base, "XNC")
		got, logs := migrateLegacyStateDir(new)
		assert.Equal(t, new, got)
		assert.Empty(t, logs, "uneventful first boot buffers nothing")
		assert.DirExists(t, new, "idle-mode first boot still needs the dir for service logs")
	})

	// 搬移失败（目录被占用，如另一进程 CWD）：绝不删旧目录，本次退回
	// 旧目录运行（identity 必须存活），下次启动重试。
	t.Run("move fails: fall back to legacy dir, nothing deleted", func(t *testing.T) {
		base := t.TempDir()
		old, new := filepath.Join(base, "XNCAgent"), filepath.Join(base, "XNC")
		write(old, "identity.json", "{}")
		t.Chdir(old) // 占用旧目录，让 rename 确定性失败

		got, logs := migrateLegacyStateDir(new)
		assert.Equal(t, old, got, "must fall back to legacy dir this run")
		require.Len(t, logs, 1)
		assert.Equal(t, slog.LevelWarn, logs[0].level,
			"ruling: migration failure must be logged (buffered for replay)")
		assert.Contains(t, logs[0].msg, "migration failed")
		assert.FileExists(t, filepath.Join(old, "identity.json"), "legacy identity must never be destroyed")
		assert.NoDirExists(t, new)
	})
}

// 服务路径的日志契约（评审 Important 修复）：迁移先于文件 logger 安装
// 执行（日志文件在新目录、句柄会占住目录），SCM 上下文里默认 logger 写
// 无效句柄——迁移消息必须缓冲、待 logger 装好后回放，否则生产丢失
// 「失败保留旧目录」的诊断。此处不真起 SCM 服务，以捕获 logger 等价
// 验证回放路径。
func TestReplayMigrationLogsAfterLoggerInstall(t *testing.T) {
	write := func(dir, name, content string) {
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}

	// both-exist 场景缓冲一条 warn。
	base := t.TempDir()
	old, new := filepath.Join(base, "XNCAgent"), filepath.Join(base, "XNC")
	write(old, "legacy-marker.txt", "old")
	write(new, "new-marker.txt", "new")
	_, entries := migrateLegacyStateDir(new)
	require.NotEmpty(t, entries)

	// 模拟 runAgent 的服务分支顺序：迁移 → 安装 logger → 回放。
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	replayMigrationLogs(entries)

	out := buf.String()
	assert.Contains(t, out, "both exist; using new", "buffered warning must reach the installed logger")
	assert.Contains(t, out, old, "warning must carry the legacy path")
	assert.Contains(t, out, new, "warning must carry the new path")
}
