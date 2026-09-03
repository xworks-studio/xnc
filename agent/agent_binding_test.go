package agent

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/agent/binding"
	"xnc/agent/identity"
)

// syncBuf 互斥保护的日志缓冲（空转 goroutine 写、测试断言读，避免竞争）。
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs 临时接管默认 logger（空转日志断言用；不与 t.Parallel 同用）。
func captureLogs(t *testing.T) *syncBuf {
	t.Helper()
	var sb syncBuf
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&sb, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &sb
}

// shortenAwaitInterval 把空转重查周期缩到测试量级（默认 5s）。
func shortenAwaitInterval(t *testing.T) {
	t.Helper()
	old := awaitBindingInterval
	awaitBindingInterval = 20 * time.Millisecond
	t.Cleanup(func() { awaitBindingInterval = old })
}

// 已有 binding.json → 立即返回，不走 token/合成/空转。
func TestAwaitBindingExisting(t *testing.T) {
	dir := t.TempDir()
	want := &binding.Binding{Server: "https://xnc.example", NodeID: "node-1"}
	require.NoError(t, binding.Save(dir, want))

	a := &Agent{StateDir: dir}
	got, err := a.awaitBinding(context.Background())
	require.NoError(t, err)
	assert.Equal(t, want.Server, got.Server)
	assert.Equal(t, want.NodeID, got.NodeID)
}

// 未注册空转（spec §6.1）：无 binding/token/身份 → 周期输出 awaiting
// registration；外部写入 binding.json（Task 4 控制管道的注册落盘）后
// 无需重启即返回该绑定。
func TestAwaitBindingIdlePickup(t *testing.T) {
	shortenAwaitInterval(t)
	logs := captureLogs(t)
	dir := t.TempDir()

	type res struct {
		b   *binding.Binding
		err error
	}
	done := make(chan res, 1)
	go func() {
		b, err := (&Agent{StateDir: dir}).awaitBinding(context.Background())
		done <- res{b, err}
	}()

	// 至少跑过一个空转周期（日志已出现），再模拟外部注册写入。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logs.String(), "awaiting registration") {
		time.Sleep(5 * time.Millisecond)
	}
	assert.Contains(t, logs.String(), "awaiting registration",
		"idle loop must log awaiting registration each poll cycle")

	want := &binding.Binding{Server: "https://xnc.example", NodeID: "node-2", ClusterID: "c1"}
	require.NoError(t, binding.Save(dir, want))

	select {
	case r := <-done:
		require.NoError(t, r.err)
		assert.Equal(t, want.Server, r.b.Server)
		assert.Equal(t, want.NodeID, r.b.NodeID)
		assert.Equal(t, want.ClusterID, r.b.ClusterID)
	case <-time.After(2 * time.Second):
		t.Fatal("awaitBinding did not pick up externally written binding")
	}
}

// 空转态必须尊重优雅停机：ctx 取消（服务停止）→ 返回 ctx.Err()，不空转卡死。
func TestAwaitBindingIdleShutdown(t *testing.T) {
	shortenAwaitInterval(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (&Agent{StateDir: t.TempDir()}).awaitBinding(ctx)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("awaitBinding ignored context cancellation")
	}
}

// 损坏的 binding.json 不致命：按未绑定处理进入空转（不崩溃、不外联），
// 修复/覆盖写入后照常接续。
func TestAwaitBindingCorruptFileIdles(t *testing.T) {
	shortenAwaitInterval(t)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(binding.Path(dir), []byte("{not json"), 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (&Agent{StateDir: dir}).awaitBinding(ctx)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled, "corrupt binding must idle, not fail")
	case <-time.After(2 * time.Second):
		t.Fatal("awaitBinding stuck on corrupt binding.json")
	}
}

// 遗留已注册身份（identity.json 带 NodeID）+ 显式 --server → 保守合成
// binding 并落盘（clusterId/registeredAt 未知留空/近似，Task 7 迁移补全）。
func TestAwaitBindingSynthesizeLegacy(t *testing.T) {
	dir := t.TempDir()
	k := identity.Generate()
	k.NodeID = "legacy-node-1"
	require.NoError(t, identity.Save(k, (&Agent{StateDir: dir}).identityPath()))

	a := &Agent{ServerURL: "https://legacy.example", StateDir: dir}
	got, err := a.awaitBinding(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "https://legacy.example", got.Server)
	assert.Equal(t, "legacy-node-1", got.NodeID)

	// 合成结果已固化：重启（再次 awaitBinding）直接命中已有绑定。
	persisted, ok, err := binding.Load(dir)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "legacy-node-1", persisted.NodeID)
}

// 无 --server（空值）绝不合成 —— 宁可空转也不猜 server（保守策略）。
func TestAwaitBindingNoSynthesizeWithoutServer(t *testing.T) {
	shortenAwaitInterval(t)
	dir := t.TempDir()
	k := identity.Generate()
	k.NodeID = "legacy-node-2"
	require.NoError(t, identity.Save(k, (&Agent{StateDir: dir}).identityPath()))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (&Agent{StateDir: dir}).awaitBinding(ctx)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled, "must idle without explicit --server")
	case <-time.After(2 * time.Second):
		t.Fatal("awaitBinding synthesized a binding without --server")
	}
}

// token 流（dev console / 一次性 token 安装）：已有身份时 EnsureEnrolled
// 不外呼，成功后固化 binding.json —— 重启即绑定，不再依赖 token。
func TestAwaitBindingTokenPersistsBinding(t *testing.T) {
	dir := t.TempDir()
	k := identity.Generate()
	k.NodeID = "token-node-1"
	require.NoError(t, identity.Save(k, (&Agent{StateDir: dir}).identityPath()))

	a := &Agent{ServerURL: "https://xnc.example", Token: "tok-1", StateDir: dir}
	got, err := a.awaitBinding(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "https://xnc.example", got.Server)
	assert.Equal(t, "token-node-1", got.NodeID)

	persisted, ok, err := binding.Load(dir)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "token-node-1", persisted.NodeID)
}
