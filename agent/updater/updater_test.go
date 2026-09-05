// updater_test.go — 编排核心可移植单测：版本比较（§9.5/§9.6）、退避/
// 黑名单（§9.1）、pending 往返（§9.3）、安装器文件定位（T6 命名契约）、
// 延迟审计标记（§9.4）。平台执行层场景见 orchestrate_windows_test.go。
package updater

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"xnc/proto"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.6.2", "0.6.1", 1},
		{"0.6.1", "0.6.2", -1},
		{"0.6.2", "0.6.2", 0},
		{"0.7.0", "0.6.99", 1},
		{"0.6.10", "0.6.9", 1},   // 数值比较，非字典序
		{"0.6", "0.6.0", 0},      // 缺段补零
		{"0.6.2", "0.6.2-dev", 1}, // 预发布更低（§9.5 降级判定）
		{"0.6.2-dev", "0.6.2", -1},
		{"0.6.2-dev", "0.6.2-dev", 0},
		{"0.6.3-dev", "0.6.2", 1}, // 数字优先于预发布语义
		{"0.6.2-rc10", "0.6.2-rc2", -1}, // 双后缀按字典序（文档化：真实频道仅 -dev）
		{"0.0.0-dev", "0.0.0", -1}, // 本地开发构建回落值
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestThrottleBackoffProgression(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	u := &Updater{StateDir: dir, now: func() time.Time { return now }}

	steps := []time.Duration{time.Hour, 4 * time.Hour, 24 * time.Hour, 24 * time.Hour}
	for i, want := range steps {
		u.recordFailure("0.6.2", nil)
		got, throttled := Throttled(dir, "0.6.2", now)
		if !throttled || got != want {
			t.Fatalf("failure %d: throttled=%v wait=%v, want true/%v", i+1, throttled, got, want)
		}
	}
	// 退避过后放行。
	if _, throttled := Throttled(dir, "0.6.2", now.Add(24*time.Hour+time.Second)); throttled {
		t.Fatal("must not throttle after nextTry elapsed")
	}
	// 成功清零（§9.1）。
	ClearThrottle(dir, "0.6.2")
	if _, throttled := Throttled(dir, "0.6.2", now); throttled {
		t.Fatal("throttle must reset after success")
	}
	// 其他版本不受影响。
	if _, throttled := Throttled(dir, "0.6.3", now); throttled {
		t.Fatal("per-version throttle leaked across versions")
	}
}

func TestBlacklistUntilNewVersion(t *testing.T) {
	dir := t.TempDir()
	BlacklistVersion(dir, "0.6.2", "sha256 mismatch")
	if reason, bad := Blacklisted(dir, "0.6.2"); !bad || reason != "sha256 mismatch" {
		t.Fatalf("blacklisted = %v/%q, want true/sha256 mismatch", bad, reason)
	}
	// 新版本天然不在表内（"直至 setup.json 出现新 version"，§9.1）。
	if _, bad := Blacklisted(dir, "0.6.3"); bad {
		t.Fatal("0.6.3 must not be blacklisted")
	}
}

func TestPendingRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok := LoadPending(dir); ok {
		t.Fatal("no pending initially")
	}
	p := &PendingUpdate{From: "0.6.1", To: "0.6.2",
		StartedAt: time.Now().UTC().Truncate(time.Second),
		Deadline:  time.Now().UTC().Add(15 * time.Minute).Truncate(time.Second)}
	if err := SavePending(dir, p); err != nil {
		t.Fatal(err)
	}
	got, ok := LoadPending(dir)
	if !ok || got.From != p.From || got.To != p.To || !got.StartedAt.Equal(p.StartedAt) || !got.Deadline.Equal(p.Deadline) {
		t.Fatalf("round trip mismatch: %+v vs %+v", got, p)
	}
	if err := DeletePending(dir); err != nil {
		t.Fatal(err)
	}
	if err := DeletePending(dir); err != nil { // 幂等
		t.Fatalf("second delete must be nil, got %v", err)
	}
	if _, ok := LoadPending(dir); ok {
		t.Fatal("pending must be gone")
	}
}

func TestInstallerFileNamingAndLookup(t *testing.T) {
	dir := t.TempDir()
	if got := cacheInstallerName("0.6.2", "stable"); got != "XNC-Setup-0.6.2.exe" {
		t.Fatalf("stable name = %q", got)
	}
	if got := cacheInstallerName("0.6.2", "dev"); got != "XNC-Setup-dev-0.6.2.exe" {
		t.Fatalf("dev name = %q", got)
	}
	// 缓存查找：精确名 + 未来频道后缀的后缀匹配；版本前缀不得误配。
	cache := filepath.Join(dir, cacheName)
	os.MkdirAll(cache, 0o755)
	os.WriteFile(filepath.Join(cache, "XNC-Setup-0.6.1.exe"), []byte("a"), 0o644)
	if got := CachedInstaller(dir, "0.6.1"); got == "" {
		t.Fatal("exact stable name must be found")
	}
	if got := CachedInstaller(dir, "0.61"); got != "" {
		t.Fatalf("substring version must not match, got %q", got)
	}
	if got := CachedInstaller(dir, "0.6.2"); got != "" {
		t.Fatalf("absent version must not be found, got %q", got)
	}
	// 后缀匹配兜底（未来频道后缀 XNC-Setup-beta-0.6.3.exe）。
	os.WriteFile(filepath.Join(cache, "XNC-Setup-beta-0.6.3.exe"), []byte("a"), 0o644)
	if got := CachedInstaller(dir, "0.6.3"); got == "" {
		t.Fatal("suffix fallback must find future channel variants")
	}
	// 旧命名兼容（0.7.2 及之前的 xnc-setup-*.exe 缓存/回滚源）：
	// 大小写不敏感后缀匹配必须互认，过渡期不丢回滚源。
	os.WriteFile(filepath.Join(cache, "xnc-setup-0.7.2.exe"), []byte("legacy"), 0o644)
	if got := CachedInstaller(dir, "0.7.2"); got == "" {
		t.Fatal("legacy xnc-setup-* name must still be found (transition)")
	}
}

func TestCopyToCachePrunesAndSamePathNoop(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, cacheName)
	os.MkdirAll(cache, 0o755)
	os.WriteFile(filepath.Join(cache, "XNC-Setup-0.6.0.exe"), []byte("old"), 0o644)

	src := filepath.Join(dir, "XNC-Setup-0.6.1.exe")
	os.WriteFile(src, []byte("new"), 0o644)
	if err := CopyToCache(dir, src); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cache, "XNC-Setup-0.6.1.exe")); err != nil {
		t.Fatal("new entry missing")
	}
	if _, err := os.Stat(filepath.Join(cache, "XNC-Setup-0.6.0.exe")); !os.IsNotExist(err) {
		t.Fatal("cache must keep exactly one entry")
	}
	// 原地路径（源==目的）：不报错、不截断（回滚原地执行契约）。
	if err := CopyToCache(dir, filepath.Join(cache, "XNC-Setup-0.6.1.exe")); err != nil {
		t.Fatalf("same-path copy must be a no-op, got %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(cache, "XNC-Setup-0.6.1.exe"))
	if string(b) != "new" {
		t.Fatalf("same-path copy corrupted file: %q", b)
	}
}

func TestConsumeAuditMarker(t *testing.T) {
	dir := t.TempDir()
	if _, ok := ConsumeAuditMarker(dir); ok {
		t.Fatal("no marker initially")
	}
	u := &Updater{StateDir: dir, Report: func(proto.Message) error { return errOfflineForTest }}
	u.audit(&proto.UpdateAudit{Event: proto.UpdateEventRollback, From: "0.6.1", To: "0.6.2", Reason: "test"})
	got, ok := ConsumeAuditMarker(dir)
	if !ok || got.Event != proto.UpdateEventRollback || got.From != "0.6.1" || got.To != "0.6.2" || got.Reason != "test" {
		t.Fatalf("marker = %+v ok=%v", got, ok)
	}
	if _, ok := ConsumeAuditMarker(dir); ok { // 一次性
		t.Fatal("marker must be consumed exactly once")
	}
}

var errOfflineForTest = &staticErr{"offline"}

type staticErr struct{ s string }

func (e *staticErr) Error() string { return e.s }

func TestCleanupStaleKeepsStagedInstallers(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "update-staging-0.5.0"), 0o755) // bundle 期遗留
	staging := filepath.Join(dir, stagingName)
	os.MkdirAll(staging, 0o755)
	os.WriteFile(filepath.Join(staging, "XNC-Setup-0.6.2.exe"), []byte("keep"), 0o644)
	os.WriteFile(filepath.Join(staging, "XNC-Setup-0.6.3.exe.tmp-123"), []byte("partial"), 0o644)

	CleanupStale(dir)

	if _, err := os.Stat(filepath.Join(dir, "update-staging-0.5.0")); !os.IsNotExist(err) {
		t.Fatal("legacy staging dir must be removed")
	}
	if _, err := os.Stat(filepath.Join(staging, "XNC-Setup-0.6.2.exe.tmp-123")); !os.IsNotExist(err) {
		t.Fatal("partial download tmp must be removed")
	}
	if _, err := os.Stat(filepath.Join(staging, "XNC-Setup-0.6.2.exe")); err != nil {
		t.Fatal("staged installer must survive (self-check cache refresh needs it)")
	}
}
