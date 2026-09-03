package binding

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sample() *Binding {
	return &Binding{
		Server:       "https://xnc.example",
		ClusterID:    "cluster-42",
		NodeID:       "11111111-2222-3333-4444-555555555555",
		RegisteredAt: time.Date(2026, 9, 3, 12, 30, 0, 0, time.UTC),
		Channel:      "stable",
	}
}

// 字段逐一保真 + 文件名固定 binding.json。
func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := sample()
	require.NoError(t, Save(dir, want))

	got, ok, err := Load(dir)
	require.NoError(t, err)
	require.True(t, ok, "saved binding must load as present")
	assert.Equal(t, want.Server, got.Server)
	assert.Equal(t, want.ClusterID, got.ClusterID)
	assert.Equal(t, want.NodeID, got.NodeID)
	assert.True(t, want.RegisteredAt.Equal(got.RegisteredAt),
		"registeredAt must survive: want %v got %v", want.RegisteredAt, got.RegisteredAt)
	assert.Equal(t, want.Channel, got.Channel)

	// 文件落在 <dir>/binding.json（CLI 侧按此路径只读判断注册状态）。
	fi, err := os.Stat(filepath.Join(dir, "binding.json"))
	require.NoError(t, err)
	assert.False(t, fi.IsDir())
}

// spec §5.2：字段名固定小驼峰 {server, clusterId, nodeId, registeredAt, channel}。
// 落盘形状是跨进程契约（CLI / 安装器 / 控制管道都要读），锁死。
func TestJSONFieldNames(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Save(dir, sample()))
	raw, err := os.ReadFile(filepath.Join(dir, "binding.json"))
	require.NoError(t, err)
	for _, key := range []string{
		`"server"`, `"clusterId"`, `"nodeId"`, `"registeredAt"`, `"channel"`,
	} {
		assert.Contains(t, string(raw), key, "on-disk JSON must use lower camel key %s", key)
	}
}

// 目录不存在 → Save 建目录；文件不存在 → (nil, false, nil) 而非错误。
func TestLoadMissing(t *testing.T) {
	dir := t.TempDir()
	got, ok, err := Load(dir)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, got)

	// Save 自动创建目录（与 identity.Save 行为一致）。
	sub := filepath.Join(dir, "nested", "state")
	require.NoError(t, Save(sub, sample()))
	_, ok, err = Load(sub)
	require.NoError(t, err)
	assert.True(t, ok)
}

// 损坏 JSON → 显式错误（调用方决定忽略与否，不静默当作未绑定）。
func TestLoadCorrupt(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "binding.json"), []byte("{not json"), 0o600))
	_, _, err := Load(dir)
	assert.Error(t, err)
}

// 原子写：完成后目录中不留 tmp 残留；覆盖写同样成立（tmp+rename，
// 进程中断最坏留下一个 tmp 文件，绝不产生半写的 binding.json）。
func TestSaveAtomicNoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Save(dir, sample()))
	assertNoTemp(t, dir)

	// 覆盖写：rename 覆盖既有文件，内容更新、无残留。
	second := sample()
	second.Server = "https://xnc2.example"
	second.NodeID = "99999999-9999-9999-9999-999999999999"
	require.NoError(t, Save(dir, second))
	assertNoTemp(t, dir)

	got, ok, err := Load(dir)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "https://xnc2.example", got.Server)
	assert.Equal(t, "99999999-9999-9999-9999-999999999999", got.NodeID)
}

func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "binding.json.tmp-*"))
	require.NoError(t, err)
	assert.Empty(t, matches, "atomic write must not leave temp files behind")
}
