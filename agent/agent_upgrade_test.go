package agent

// Task 8：agentctl upgrade op 的编排层（Agent.Upgrade，spec §9.1 手动触发 +
// §9.5 跨频道切换）与 Status 的 update/channel 扩展。假清单服务证明
// ForceCheck 以切换后的频道拉取 setup.json；绑定其余字段在频道切换时保持。

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/agent/binding"
	"xnc/agent/machineinfo"
	"xnc/agent/updater"
)

// fakeManifestServer 服务 /setup.json?channel=（记录频道查询参数；同版本
// 清单 → 无更新）。可选 gate 阻塞首个请求（进行中窗口测试）。
type fakeManifestServer struct {
	mu       sync.Mutex
	srv      *httptest.Server
	channels []string
	version  string
	gate     chan struct{} // 非 nil 时每个请求阻塞至关闭
}

func newFakeManifestServer(t *testing.T, version string) *fakeManifestServer {
	t.Helper()
	f := &fakeManifestServer{version: version}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /installer.json", func(w http.ResponseWriter, r *http.Request) {
		ch := r.URL.Query().Get("channel")
		f.mu.Lock()
		f.channels = append(f.channels, ch)
		gate := f.gate
		f.mu.Unlock()
		if gate != nil {
			<-gate
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"` + f.version + `","url":"/installer?channel=` + ch + `","sha256":"00"}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeManifestServer) seenChannels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.channels...)
}

// seedBinding 写入完整形态绑定（频道切换必须保持其余字段）。
func seedBinding(t *testing.T, dir, server, channel string) *binding.Binding {
	t.Helper()
	b := &binding.Binding{
		Server: server, ClusterID: "c-1", NodeID: "n-1",
		RegisteredAt: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
		Channel:      channel,
	}
	require.NoError(t, binding.Save(dir, b))
	return b
}

// 跨频道切换（§9.5）：stable（空）→ dev — 绑定原子更新（仅 Channel 变，
// server/cluster/node/registeredAt 保持），audit 记 update_channel {from,to}，
// 后台 ForceCheck 以新频道拉清单（假服务收到的 channel=dev）。
func TestUpgradeSwitchesChannelAndChecks(t *testing.T) {
	logs := captureLogs(t)
	f := newFakeManifestServer(t, machineinfo.Version) // 同版本 → 无更新可应用
	dir := t.TempDir()
	seed := seedBinding(t, dir, f.srv.URL, "")
	a := &Agent{StateDir: dir}

	triggered, err := a.Upgrade(t.Context(), "dev")
	require.NoError(t, err)
	assert.True(t, triggered)

	// 绑定：仅 Channel 变化。
	b, ok, err := binding.Load(dir)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "dev", b.Channel)
	assert.Equal(t, seed.Server, b.Server)
	assert.Equal(t, seed.ClusterID, b.ClusterID)
	assert.Equal(t, seed.NodeID, b.NodeID)
	assert.Equal(t, seed.RegisteredAt, b.RegisteredAt)

	// audit（log-only，T7 最小口径）：update_channel {from:stable,to:dev}。
	assert.Contains(t, logs.String(), "update_channel")
	assert.Contains(t, logs.String(), "from=stable")
	assert.Contains(t, logs.String(), "to=dev")

	// 后台 ForceCheck 以切换后的频道拉清单。
	require.Eventually(t, func() bool {
		chs := f.seenChannels()
		return len(chs) > 0 && chs[0] == "dev"
	}, 5*time.Second, 20*time.Millisecond, "manifest must be fetched with the NEW channel")
	// 触发标记随检查收线（无更新 → goroutine 返回）。
	require.Eventually(t, func() bool { return !a.upgradeInFlight() },
		5*time.Second, 20*time.Millisecond)
	assert.Nil(t, a.Status().Update, "no update in flight after a no-op check")
}

// 同频道触发（channel 省略或与绑定一致）：不改绑定、不 audit，直接按当前
// 频道检查。
func TestUpgradeSameChannelNoSwitch(t *testing.T) {
	logs := captureLogs(t)
	f := newFakeManifestServer(t, machineinfo.Version)
	dir := t.TempDir()
	seed := seedBinding(t, dir, f.srv.URL, "dev")
	a := &Agent{StateDir: dir}

	for _, ch := range []string{"", "dev"} {
		triggered, err := a.Upgrade(t.Context(), ch)
		require.NoError(t, err)
		assert.True(t, triggered, "channel %q", ch)
		require.Eventually(t, func() bool { return !a.upgradeInFlight() },
			5*time.Second, 20*time.Millisecond)
	}
	b, ok, err := binding.Load(dir)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, seed, b, "binding untouched for same/no channel")
	assert.NotContains(t, logs.String(), "update_channel")
	for _, ch := range f.seenChannels() {
		assert.Equal(t, "dev", ch)
	}
}

// 未注册：无更新源，同步报错（不落任何触发）。
func TestUpgradeNoBinding(t *testing.T) {
	a := &Agent{StateDir: t.TempDir()}
	_, err := a.Upgrade(t.Context(), "dev")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not_registered")
}

// 显式切回 stable（dev→stable）：binding.Channel 归一为 "stable"，audit
// from=dev to=stable。
func TestUpgradeSwitchBackToStable(t *testing.T) {
	logs := captureLogs(t)
	f := newFakeManifestServer(t, machineinfo.Version)
	dir := t.TempDir()
	seedBinding(t, dir, f.srv.URL, "dev")
	a := &Agent{StateDir: dir}

	triggered, err := a.Upgrade(t.Context(), "stable")
	require.NoError(t, err)
	assert.True(t, triggered)
	b, ok, err := binding.Load(dir)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "stable", b.Channel)
	assert.Contains(t, logs.String(), "from=dev")
	assert.Contains(t, logs.String(), "to=stable")
	require.Eventually(t, func() bool { return !a.upgradeInFlight() },
		5*time.Second, 20*time.Millisecond)
}

// pending 存在（§9.3 串行化）：triggered=false（非错误），不切绑定、不触发
// 新检查。
func TestUpgradePendingInFlight(t *testing.T) {
	f := newFakeManifestServer(t, machineinfo.Version)
	dir := t.TempDir()
	seed := seedBinding(t, dir, f.srv.URL, "stable")
	require.NoError(t, updater.SavePending(dir, &updater.PendingUpdate{
		From: "0.6.1", To: "0.6.2",
		StartedAt: time.Now().UTC(), Deadline: time.Now().Add(15 * time.Minute).UTC(),
	}))
	a := &Agent{StateDir: dir}

	triggered, err := a.Upgrade(t.Context(), "dev")
	require.NoError(t, err, "pending is not an error")
	assert.False(t, triggered)

	b, ok, err := binding.Load(dir)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, seed, b, "pending must block the channel switch too")
	assert.Empty(t, f.seenChannels())

	// status：applying + from/to。
	st := a.Status()
	require.NotNil(t, st.Update)
	assert.Equal(t, "applying", st.Update.Phase)
	assert.Equal(t, "0.6.1", st.Update.From)
	assert.Equal(t, "0.6.2", st.Update.To)
}

// 进行中窗口（触发 → ForceCheck 返回之间）：第二次 upgrade → triggered=false；
// 频道切换也不重复执行。
func TestUpgradeDedupWhileRunning(t *testing.T) {
	f := newFakeManifestServer(t, machineinfo.Version)
	f.mu.Lock()
	f.gate = make(chan struct{})
	f.mu.Unlock()
	dir := t.TempDir()
	seedBinding(t, dir, f.srv.URL, "")
	a := &Agent{StateDir: dir}

	triggered, err := a.Upgrade(t.Context(), "dev")
	require.NoError(t, err)
	assert.True(t, triggered)

	// 检查挂起期间（gate 未开）：status = checking；再触发 → false。
	require.Eventually(t, func() bool { return a.upgradeInFlight() },
		5*time.Second, 5*time.Millisecond)
	st := a.Status()
	require.NotNil(t, st.Update)
	assert.Equal(t, "checking", st.Update.Phase)
	assert.Equal(t, "dev", st.Channel, "status channel reflects the switched binding")

	triggered, err = a.Upgrade(t.Context(), "dev")
	require.NoError(t, err)
	assert.False(t, triggered)

	close(f.gate)
	require.Eventually(t, func() bool { return !a.upgradeInFlight() },
		5*time.Second, 20*time.Millisecond)
	assert.Equal(t, []string{"dev"}, f.seenChannels(), "exactly one manifest fetch")
}

// Status 的 channel 归一：空绑定频道显示为 stable（生效频道）。
func TestStatusChannelNormalized(t *testing.T) {
	dir := t.TempDir()
	seedBinding(t, dir, "https://s", "")
	a := &Agent{StateDir: dir}
	assert.Equal(t, "stable", a.Status().Channel)

	seedBinding(t, dir, "https://s", "dev")
	assert.Equal(t, "dev", a.Status().Channel)

	// 未注册：无频道可报。
	assert.Empty(t, (&Agent{StateDir: t.TempDir()}).Status().Channel)
}

// 切换频道唤醒连接周期（rebind）：绑定变更与 register 同语义——运行中的
// 6h 轮询器/编排器必须按新频道重建（runConnected 全链路由
// TestRunCycleDeregister 覆盖；这里只证明信号已发）。
func TestUpgradeChannelSwitchSignalsRebind(t *testing.T) {
	f := newFakeManifestServer(t, machineinfo.Version)
	dir := t.TempDir()
	seedBinding(t, dir, f.srv.URL, "") // 空 channel = stable
	a := &Agent{StateDir: dir}

	got := make(chan struct{}, 1)
	go func() {
		select {
		case <-a.rebindCh():
			got <- struct{}{}
		case <-time.After(5 * time.Second):
		}
	}()
	triggered, err := a.Upgrade(t.Context(), "dev")
	require.NoError(t, err)
	assert.True(t, triggered)
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("channel switch must signal rebind")
	}
	// 未切换频道（同频道触发）不发信号。
	triggered, err = a.Upgrade(t.Context(), "dev")
	require.NoError(t, err)
	assert.True(t, triggered)
	require.Eventually(t, func() bool { return !a.upgradeInFlight() },
		5*time.Second, 20*time.Millisecond)
	select {
	case <-got:
		t.Fatal("same-channel trigger must not signal rebind")
	default:
	}
}
