package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 状态目录隔离（Task 1 关键需求）：路径形状 + 每次全新 + 位置只在 temp。
func TestFreshDevStateDir(t *testing.T) {
	dir, err := freshDevStateDir()
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	// 路径形状：%TEMP%\xnc-dev-agent-<pid>
	assert.Equal(t, DevStateDirForPid(os.Getpid()), dir)
	assert.True(t, strings.HasPrefix(dir, os.TempDir()),
		"state dir must live under temp: %s", dir)
	base := filepath.Base(dir)
	assert.Equal(t, "xnc-dev-agent-"+strconv.Itoa(os.Getpid()), base)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	// 全新：同 pid 残留（上次运行 pid 复用）必须被清掉
	stale := filepath.Join(dir, "identity.json")
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o600))
	dir2, err := freshDevStateDir()
	require.NoError(t, err)
	defer os.RemoveAll(dir2)
	assert.Equal(t, dir, dir2)
	_, err = os.Stat(stale)
	assert.True(t, os.IsNotExist(err), "stale identity must not survive a fresh run")
}

func TestDevNodeName(t *testing.T) {
	assert.Equal(t, "XIAOXIN-DEV", devNodeName("XIAOXIN-DEV"))

	h, err := os.Hostname()
	require.NoError(t, err)
	assert.Equal(t, h+"-DEV", devNodeName(""))
}

func TestDevMachineID(t *testing.T) {
	id := devMachineID("XIAOXIN-DEV")
	assert.True(t, strings.HasPrefix(id, "dev-"), id)
	assert.True(t, strings.HasSuffix(id, "-XIAOXIN-DEV"), id)
	// 每次运行唯一（同 machineId 复用会被 dev server enroll 409）
	assert.NotEqual(t, id, devMachineID("XIAOXIN-DEV"))
}

// flag 解析路径：run-dev-console 注册在命令树上且 server/token 必填。
func TestRunDevConsoleFlags(t *testing.T) {
	root := newRootCmd()
	assert.NotNil(t, root)
	dev := root.Commands()
	found := false
	for _, c := range dev {
		if c.Name() == "run-dev-console" {
			found = true
			break
		}
	}
	assert.True(t, found, "run-dev-console subcommand must be registered")

	// 缺必填 flag → 报错而非启动 agent
	root2 := newRootCmd()
	root2.SetArgs([]string{"run-dev-console"})
	root2.SetOut(os.Stderr)
	err := root2.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "required flag")

	// 未知 flag 拒绝（防 typo 静默丢参）
	root3 := newRootCmd()
	root3.SetArgs([]string{"run-dev-console", "--server", "http://s", "--token", "t", "--srv", "x"})
	root3.SetOut(os.Stderr)
	err = root3.Execute()
	require.Error(t, err)
}
