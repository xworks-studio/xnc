//go:build windows

// orchestrate_windows_test.go — 编排状态机场景测试（spec §9）：假安装器
// = 普通 .cmd（退出 0 / 退出非 0 / 挂起），真进程执行（RunInstallerProcess
// 含超时击杀）；setup.json/下载用 httptest 真 HTTP。绝不运行真 setup.exe。
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xnc/proto"
)

// fakeSetupServer — setup.json + setup.exe 的 httptest 假源（可变清单，
// 供跨场景改版本）。
type fakeSetupServer struct {
	mu       sync.Mutex
	version  string
	blob     []byte
	manifest int // /setup.json 命中计数（断言"pending 时不再拉取"等）
	download int
	srv      *httptest.Server
}

func startFakeSetupServer(t *testing.T, version string) *fakeSetupServer {
	t.Helper()
	f := &fakeSetupServer{version: version, blob: []byte("fake-installer-binary")}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/setup.json":
			f.manifest++
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"version": f.version, "url": "/setup.exe?channel=stable",
				"sha256": fileSHA256OrPanic(f.blob), "size": len(f.blob),
			})
		case "/setup.exe":
			f.download++
			w.Write(f.blob)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSetupServer) setVersion(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version = v
}

func (f *fakeSetupServer) stats() (manifests, downloads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.manifest, f.download
}

func fileSHA256OrPanic(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fakeInstaller — 假安装器 .cmd 工厂。
type fakeInstaller struct {
	dir string
}

func newFakeInstaller(t *testing.T) *fakeInstaller {
	return &fakeInstaller{dir: t.TempDir()}
}

// ok 退出 0 的安装器；marker 追加一行（记录"安装发生"）。
func (f *fakeInstaller) ok(name string) string {
	return f.write(name, "@echo ran-"+name+" >> \""+filepath.Join(f.dir, name+".log")+"\"\r\n@exit /b 0\r\n")
}

// fail 退出码 code。
func (f *fakeInstaller) fail(name string, code int) string {
	return f.write(name, "@echo failed >> \""+filepath.Join(f.dir, name+".log")+"\"\r\n@exit /b "+itoa(code)+"\r\n")
}

// hang 睡眠远超执行超时（RunInstallerProcess 必须击杀）。ping 每秒一拍、
// 共 30s；不用 choice——隐藏窗口 + NUL stdin 下 choice 立即返回。
func (f *fakeInstaller) hang(name string) string {
	return f.write(name, "@ping -n 30 127.0.0.1 >nul\r\n@exit /b 0\r\n")
}

func (f *fakeInstaller) ran(name string) bool {
	b, err := os.ReadFile(filepath.Join(f.dir, name+".log"))
	return err == nil && strings.Contains(string(b), "ran-"+name)
}

func (f *fakeInstaller) write(name, content string) string {
	p := filepath.Join(f.dir, name+".cmd")
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		panic(err)
	}
	return p
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// harness — 一个编排器 + 注入缝 + 观测点。
type harness struct {
	u  *Updater
	f  *fakeSetupServer
	fi *fakeInstaller

	execTimeout time.Duration // 假安装器进程等待上限（挂起场景缩短）

	mu        sync.Mutex
	execPaths []string // 每次 execInstaller 的"安装器路径"（编排层视角）
	cmdQueue  []string // 依次实际执行的 .cmd（耗尽后复用最后一个）
	healthErr error
	regAt     []time.Time
	deletes   int
	audits    []proto.UpdateAudit
}

func newHarness(t *testing.T, version string) *harness {
	t.Helper()
	f := startFakeSetupServer(t, "9.9.9") // 版本由各场景设置
	h := &harness{f: f, fi: newFakeInstaller(t), execTimeout: 3 * time.Second}
	u := New(f.srv.URL, t.TempDir(), version, "stable", nil)
	h.u = u
	u.execInstaller = h.exec
	u.serviceHealthy = func() error {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.healthErr
	}
	u.registerWatchdogTask = func(at time.Time) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.regAt = append(h.regAt, at)
		return nil
	}
	u.deleteWatchdogTask = func() error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.deletes++
		return nil
	}
	u.Report = func(m proto.Message) error {
		var a proto.UpdateAudit
		if m.Type == proto.TypeUpdateAudit && m.Decode(&a) == nil {
			h.mu.Lock()
			h.audits = append(h.audits, a)
			h.mu.Unlock()
		}
		return nil
	}
	return h
}

// exec 编排层"安装器路径"记账后，真进程执行队列中对应的假 .cmd。
func (h *harness) exec(path string) error {
	h.mu.Lock()
	h.execPaths = append(h.execPaths, path)
	i := len(h.execPaths) // 第 i 次执行（1 起）
	var cmd string
	switch {
	case len(h.cmdQueue) == 0:
		cmd = filepath.Join(h.fi.dir, "missing.cmd")
	case i <= len(h.cmdQueue):
		cmd = h.cmdQueue[i-1]
	default:
		cmd = h.cmdQueue[len(h.cmdQueue)-1]
	}
	timeout := h.execTimeout
	h.mu.Unlock()
	return RunInstallerProcess(cmd, h.u.installDir(), timeout)
}

func (h *harness) queue(cmds ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cmdQueue = append(h.cmdQueue, cmds...)
}

func (h *harness) setHealth(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.healthErr = err
}

func (h *harness) snapshot() (execPaths []string, regAt []time.Time, deletes int, audits []proto.UpdateAudit) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.execPaths...), append([]time.Time{}, h.regAt...), h.deletes, append([]proto.UpdateAudit{}, h.audits...)
}

// seedCache 预置 installer-cache 中某版本的安装器。
func seedCache(t *testing.T, stateDir, version, content string) {
	t.Helper()
	os.MkdirAll(filepath.Join(stateDir, cacheName), 0o755)
	os.WriteFile(filepath.Join(stateDir, cacheName, "xnc-setup-"+version+".exe"), []byte(content), 0o644)
}

// --- 场景 1：成功流程（假安装器退出 0）---

func TestOrchestrateSuccessFlow(t *testing.T) {
	h := newHarness(t, "0.6.1")
	h.f.setVersion("0.6.2")
	h.queue(h.fi.ok("install"))
	seedCache(t, h.u.StateDir, "0.6.1", "current-installer")

	if err := h.u.CheckNow(context.Background()); err != nil {
		t.Fatalf("CheckNow: %v", err)
	}

	// staging 下载 + 校验通过。
	staged := filepath.Join(h.u.StateDir, stagingName, "xnc-setup-0.6.2.exe")
	if b, err := os.ReadFile(staged); err != nil || string(b) != "fake-installer-binary" {
		t.Fatalf("staged installer missing/corrupt: %v %q", err, b)
	}
	// pending {from,to,startedAt,deadline=+15min}。
	p, ok := LoadPending(h.u.StateDir)
	if !ok || p.From != "0.6.1" || p.To != "0.6.2" {
		t.Fatalf("pending = %+v ok=%v", p, ok)
	}
	window := p.Deadline.Sub(p.StartedAt)
	if window < DefaultPendingWindow-time.Second || window > DefaultPendingWindow+time.Second {
		t.Fatalf("pending window = %v, want %v", window, DefaultPendingWindow)
	}
	// 看门狗触发 = deadline + 宽限，恰好一次。
	execPaths, regAt, deletes, audits := h.snapshot()
	if len(regAt) != 1 || regAt[0].Sub(p.Deadline) != WatchdogRestartGrace {
		t.Fatalf("watchdog registrations = %v, want one at deadline+grace (pending deadline %v)", regAt, p.Deadline)
	}
	if deletes != 0 {
		t.Fatalf("watchdog deleted %d times on success path, want 0", deletes)
	}
	// 安装器以 staging 路径执行。
	if len(execPaths) != 1 || execPaths[0] != staged {
		t.Fatalf("exec = %v, want [%s]", execPaths, staged)
	}
	// 假安装器真跑过。
	if !h.fi.ran("install") {
		t.Fatal("fake installer did not run")
	}
	// 退出 0：等新 agent 自检收尾——pending 保留、无审计、无黑名单。
	if _, ok := LoadPending(h.u.StateDir); !ok {
		t.Fatal("pending must survive until new-agent self-check")
	}
	if len(audits) != 0 {
		t.Fatalf("audits = %v, want none yet", audits)
	}
	if _, bad := Blacklisted(h.u.StateDir, "0.6.2"); bad {
		t.Fatal("0.6.2 must not be blacklisted")
	}
}

// --- 场景 2：sha256 不符 → 黑名单 + 后续同版本跳过 ---

func TestOrchestrateShaMismatchBlacklists(t *testing.T) {
	h := newHarness(t, "0.6.1")
	h.f.setVersion("0.6.2")
	// server 始终给与 blob 一致的哈希，因此用显式错误哈希的推送构造
	// "下载内容 ≠ 清单哈希"。
	h.queue(h.fi.ok("never"))
	seedCache(t, h.u.StateDir, "0.6.1", "cur")

	push := proto.UpdateAvailable{Version: "0.6.2",
		URL: "/setup.exe?channel=stable", SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}
	err := h.u.HandlePush(context.Background(), push)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v, want sha256 mismatch", err)
	}
	if _, ok := LoadPending(h.u.StateDir); ok {
		t.Fatal("pending must not be written on verify failure")
	}
	if reason, bad := Blacklisted(h.u.StateDir, "0.6.2"); !bad || !strings.Contains(reason, "sha256") {
		t.Fatalf("blacklist = %v %q", bad, reason)
	}
	if _, _, deletes, _ := h.snapshot(); deletes != 0 {
		t.Fatal("no watchdog should have been registered/deleted")
	}
	// staging 残留清除。
	if _, err := os.Stat(filepath.Join(h.u.StateDir, stagingName, "xnc-setup-0.6.2.exe")); !os.IsNotExist(err) {
		t.Fatal("corrupt download must be removed from staging")
	}
	// 同版本再推：黑名单直接跳过（不执行安装器）。
	if err := h.u.HandlePush(context.Background(), push); err != nil {
		t.Fatalf("blacklisted push must be a no-op, got %v", err)
	}
	if paths, _, _, _ := h.snapshot(); len(paths) != 0 {
		t.Fatalf("blacklisted version must not execute installer, exec=%v", paths)
	}
}

// --- 场景 3：安装器退出码非 0 → 直接回滚（原地重跑缓存旧安装器）---

func TestOrchestrateInstallerFailureRollsBack(t *testing.T) {
	h := newHarness(t, "0.6.1")
	h.f.setVersion("0.6.2")
	fi := newFakeInstaller(t)
	h.queue(fi.fail("broken", 3), fi.ok("rollback"))
	seedCache(t, h.u.StateDir, "0.6.1", "rollback-source")

	if err := h.u.CheckNow(context.Background()); err == nil {
		t.Fatal("CheckNow must surface installer failure")
	}

	execPaths, regAt, deletes, audits := h.snapshot()
	// 第一次执行新安装器（失败），第二次原地执行缓存的 0.6.1 安装器。
	wantCache := filepath.Join(h.u.StateDir, cacheName, "xnc-setup-0.6.1.exe")
	if len(execPaths) != 2 || execPaths[0] != filepath.Join(h.u.StateDir, stagingName, "xnc-setup-0.6.2.exe") ||
		execPaths[1] != wantCache {
		t.Fatalf("exec = %v, want [staged 0.6.2, cached 0.6.1 in place]", execPaths)
	}
	if !fi.ran("rollback") {
		t.Fatal("rollback installer did not run")
	}
	// pending/看门狗已清。
	if _, ok := LoadPending(h.u.StateDir); ok {
		t.Fatal("pending must be deleted on rollback")
	}
	if deletes != 1 || len(regAt) != 1 {
		t.Fatalf("watchdog register/delete = %d/%d, want 1/1", len(regAt), deletes)
	}
	// 版本入黑名单 + audit update_rollback。
	if reason, bad := Blacklisted(h.u.StateDir, "0.6.2"); !bad || !strings.Contains(reason, "installer failed") {
		t.Fatalf("blacklist = %v %q", bad, reason)
	}
	if len(audits) != 1 || audits[0].Event != proto.UpdateEventRollback ||
		audits[0].From != "0.6.1" || audits[0].To != "0.6.2" {
		t.Fatalf("audits = %+v", audits)
	}
	if !strings.Contains(audits[0].Reason, "installer failed") {
		t.Fatalf("rollback reason = %q", audits[0].Reason)
	}
}

// --- 场景 4：安装器挂起 → 超时击杀 → 回滚 ---

func TestOrchestrateInstallerHangTimesOutAndRollsBack(t *testing.T) {
	h := newHarness(t, "0.6.1")
	h.f.setVersion("0.6.2")
	h.execTimeout = 700 * time.Millisecond // 缩短进程等待上限
	h.queue(h.fi.hang("hang"), h.fi.ok("rollback"))
	seedCache(t, h.u.StateDir, "0.6.1", "rollback-source")

	start := time.Now()
	err := h.u.CheckNow(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("hang not killed in time: %v", elapsed)
	}
	if !h.fi.ran("rollback") {
		t.Fatal("rollback must run after installer timeout")
	}
	if _, ok := LoadPending(h.u.StateDir); ok {
		t.Fatal("pending must be deleted")
	}
	if _, _, deletes, audits := h.snapshot(); deletes != 1 || len(audits) != 1 {
		t.Fatalf("deletes=%d audits=%d, want 1/1", deletes, len(audits))
	}
}

// --- 场景 5：pending 存在 → 拒绝新触发（串行化）---

func TestOrchestratePendingRefusesNewTrigger(t *testing.T) {
	h := newHarness(t, "0.6.1")
	h.f.setVersion("0.6.2")
	SavePending(h.u.StateDir, &PendingUpdate{From: "0.6.0", To: "0.6.1",
		StartedAt: time.Now().UTC(), Deadline: time.Now().UTC().Add(15 * time.Minute)})

	if err := h.u.CheckNow(context.Background()); err != nil {
		t.Fatalf("refused trigger must be silent no-op, got %v", err)
	}
	if paths, _, _, _ := h.snapshot(); len(paths) != 0 {
		t.Fatalf("must not execute installer while pending exists: %v", paths)
	}
	if m, _ := h.f.stats(); m != 0 {
		t.Fatalf("must not even fetch setup.json while pending exists: %d", m)
	}
}

// --- 场景 6：降级保护（§9.5）---

func TestOrchestrateDowngradeSkipped(t *testing.T) {
	h := newHarness(t, "0.6.2")
	h.f.setVersion("0.6.1") // 旧版本
	h.queue(h.fi.ok("never"))
	seedCache(t, h.u.StateDir, "0.6.2", "cur")

	if err := h.u.CheckNow(context.Background()); err != nil {
		t.Fatalf("downgrade skip must be silent, got %v", err)
	}
	if paths, regAt, _, _ := h.snapshot(); len(paths) != 0 || len(regAt) != 0 {
		t.Fatalf("downgrade must not trigger anything: exec=%v reg=%v", paths, regAt)
	}
	if _, ok := LoadPending(h.u.StateDir); ok {
		t.Fatal("no pending on downgrade")
	}
}

// --- 场景 7：回滚源缺失且 server 不再供应 → 跳过更新 ---

func TestOrchestrateRollbackSourceMissingSkips(t *testing.T) {
	h := newHarness(t, "0.6.1")
	h.f.setVersion("0.6.2") // server 最新 = 0.6.2 ≠ 当前 0.6.1 → 不再供应 0.6.1
	h.queue(h.fi.ok("never"))
	// 不 seed cache。

	err := h.u.CheckNow(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rollback source") {
		t.Fatalf("err = %v, want rollback source missing", err)
	}
	if _, ok := LoadPending(h.u.StateDir); ok {
		t.Fatal("no pending without rollback source")
	}
	if paths, regAt, _, _ := h.snapshot(); len(paths) != 0 || len(regAt) != 0 {
		t.Fatalf("must not execute/register anything: %v %v", paths, regAt)
	}
}

// --- 场景 8：回滚源缺失但同频道 setup.json 仍供应当前版本 → 补拷后继续
//（推送固定目标 + 频道最新 == 当前的组合）---

func TestOrchestrateRollbackSourceDownloadedForPush(t *testing.T) {
	h := newHarness(t, "0.6.1")
	h.f.setVersion("0.6.1") // 频道最新 == 当前 → server 仍供应 0.6.1
	fi := newFakeInstaller(t)
	h.queue(fi.ok("rollbacksrc"), fi.ok("install"))
	// 不 seed cache：补源须走下载。

	// 推送更高目标（pin 场景）：setup.json 供应的是回滚源而非目标。
	push := proto.UpdateAvailable{Version: "0.7.0", URL: "/setup.exe?channel=stable",
		SHA256: fileSHA256OrPanic([]byte("fake-installer-binary"))}
	if err := h.u.HandlePush(context.Background(), push); err != nil {
		t.Fatalf("HandlePush: %v", err)
	}

	// 下载补拷当前版本安装器到 cache。
	if CachedInstaller(h.u.StateDir, "0.6.1") == "" {
		t.Fatal("rollback source must have been downloaded into installer-cache")
	}
	p, ok := LoadPending(h.u.StateDir)
	if !ok || p.From != "0.6.1" || p.To != "0.7.0" {
		t.Fatalf("pending = %+v ok=%v", p, ok)
	}
	paths, _, _, _ := h.snapshot()
	if len(paths) != 1 { // 补源是下载+拷贝（不执行），仅目标安装器执行一次
		t.Fatalf("exec = %v, want only the target installer", paths)
	}
}

// --- 场景 9：自检成功收尾（§9.4）---

func TestSelfCheckFinalizes(t *testing.T) {
	h := newHarness(t, "0.6.2") // 新 agent：自报 = pending.to
	h.f.setVersion("0.6.2")
	h.setHealth(nil)
	SavePending(h.u.StateDir, &PendingUpdate{From: "0.6.1", To: "0.6.2",
		StartedAt: time.Now().UTC(), Deadline: time.Now().UTC().Add(15 * time.Minute)})
	seedCache(t, h.u.StateDir, "0.6.1", "old")
	// staging 留着刚执行过的 0.6.2 安装器（自检从这刷新缓存）。
	staging := filepath.Join(h.u.StateDir, stagingName)
	os.MkdirAll(staging, 0o755)
	os.WriteFile(filepath.Join(staging, "xnc-setup-0.6.2.exe"), []byte("new-installer"), 0o644)
	// 预置一次失败记录：成功后清零。
	BlacklistVersion(h.u.StateDir, "other", "x")
	h.u.recordFailure("0.6.2", nil)

	h.u.SelfCheck(context.Background())

	if _, ok := LoadPending(h.u.StateDir); ok {
		t.Fatal("pending must be deleted on success")
	}
	if _, _, deletes, audits := h.snapshot(); deletes != 1 {
		t.Fatalf("watchdog deletes = %d, want 1", deletes)
	} else if len(audits) != 1 || audits[0].Event != proto.UpdateEventOK ||
		audits[0].From != "0.6.1" || audits[0].To != "0.6.2" {
		t.Fatalf("audits = %+v, want single update_ok", audits)
	}
	// installer-cache 刷新为新版且仅此一份。
	got := CachedInstaller(h.u.StateDir, "0.6.2")
	if got == "" {
		t.Fatal("cache must hold the new installer")
	}
	if b, _ := os.ReadFile(got); string(b) != "new-installer" {
		t.Fatalf("cache content = %q", b)
	}
	if _, err := os.Stat(filepath.Join(h.u.StateDir, cacheName, "xnc-setup-0.6.1.exe")); !os.IsNotExist(err) {
		t.Fatal("old cache entry must be pruned")
	}
	// 退避清零。
	if _, throttled := Throttled(h.u.StateDir, "0.6.2", time.Now()); throttled {
		t.Fatal("throttle must reset on success")
	}
}

// --- 场景 10：自检失败 → 主动回滚 ---

func TestSelfCheckFailureRollsBack(t *testing.T) {
	h := newHarness(t, "0.6.2")
	h.f.setVersion("0.6.2")
	h.setHealth(&staticErr{"XNCCore state=Stopped"})
	h.u.HealthWindow = 200 * time.Millisecond
	fi := newFakeInstaller(t)
	h.queue(fi.ok("rollback"))
	SavePending(h.u.StateDir, &PendingUpdate{From: "0.6.1", To: "0.6.2",
		StartedAt: time.Now().UTC(), Deadline: time.Now().UTC().Add(15 * time.Minute)})
	seedCache(t, h.u.StateDir, "0.6.1", "old")

	h.u.SelfCheck(context.Background())

	if _, ok := LoadPending(h.u.StateDir); ok {
		t.Fatal("pending must be deleted on rollback")
	}
	if !fi.ran("rollback") {
		t.Fatal("self-check failure must roll back via cached installer")
	}
	_, regAt, deletes, audits := h.snapshot()
	if deletes != 1 || len(regAt) != 0 {
		t.Fatalf("deletes=%d reg=%d, want 1/0", deletes, len(regAt))
	}
	if len(audits) != 1 || audits[0].Event != proto.UpdateEventRollback ||
		!strings.Contains(audits[0].Reason, "self-check") {
		t.Fatalf("audits = %+v", audits)
	}
	if _, bad := Blacklisted(h.u.StateDir, "0.6.2"); !bad {
		t.Fatal("failed version must be blacklisted")
	}
}

// --- 场景 11：过期 pending（>24h）启动兜底回滚（§9.4 看门狗不可靠）---

func TestStartupPendingExpiredRollsBack(t *testing.T) {
	h := newHarness(t, "0.6.1") // 仍是旧 agent（新 agent 从未上线）
	h.f.setVersion("9.9.9")
	fi := newFakeInstaller(t)
	h.queue(fi.ok("rollback"))
	seedCache(t, h.u.StateDir, "0.6.1", "old")
	SavePending(h.u.StateDir, &PendingUpdate{From: "0.6.1", To: "0.6.2",
		StartedAt: time.Now().UTC().Add(-25 * time.Hour),
		Deadline:  time.Now().UTC().Add(-24*time.Hour - 45*time.Minute)})

	h.u.StartupPendingCheck(context.Background())

	if _, ok := LoadPending(h.u.StateDir); ok {
		t.Fatal("expired pending must be cleaned by backstop rollback")
	}
	if !fi.ran("rollback") {
		t.Fatal("backstop must re-run cached installer")
	}
	if _, _, _, audits := h.snapshot(); len(audits) != 1 || audits[0].Event != proto.UpdateEventRollback {
		t.Fatalf("audits = %+v", audits)
	}
}

// 场景 11b：新鲜 pending 且自报==from（安装器尚未收尾）→ 交给看门狗，
// 启动巡检不得动它。
func TestStartupPendingFreshFromIsLeftToWatchdog(t *testing.T) {
	h := newHarness(t, "0.6.1")
	h.queue(h.fi.ok("never"))
	SavePending(h.u.StateDir, &PendingUpdate{From: "0.6.1", To: "0.6.2",
		StartedAt: time.Now().UTC(), Deadline: time.Now().UTC().Add(15 * time.Minute)})

	h.u.StartupPendingCheck(context.Background())

	if _, ok := LoadPending(h.u.StateDir); !ok {
		t.Fatal("fresh pending must survive (watchdog owns it)")
	}
	if paths, _, deletes, _ := h.snapshot(); len(paths) != 0 || deletes != 0 {
		t.Fatalf("startup check must not act on fresh pending: %v %d", paths, deletes)
	}
}

// --- 场景 12：版本一致性失败（§9.6：自报既非 from 也非 to）→ 回滚 ---

func TestStartupVersionMismatchRollsBack(t *testing.T) {
	h := newHarness(t, "0.5.0") // 既非 from 0.6.1 也非 to 0.6.2
	fi := newFakeInstaller(t)
	h.queue(fi.ok("rollback"))
	seedCache(t, h.u.StateDir, "0.6.1", "old")
	SavePending(h.u.StateDir, &PendingUpdate{From: "0.6.1", To: "0.6.2",
		StartedAt: time.Now().UTC(), Deadline: time.Now().UTC().Add(15 * time.Minute)})

	h.u.StartupPendingCheck(context.Background())

	if !fi.ran("rollback") {
		t.Fatal("version mismatch must roll back")
	}
	_, _, _, audits := h.snapshot()
	if len(audits) != 1 || !strings.Contains(audits[0].Reason, "neither") {
		t.Fatalf("audits = %+v", audits)
	}
}

// --- 场景 13：离线回滚 → 延迟审计标记（bundle 期 failed-marker 模式）---

func TestOfflineRollbackWritesDeferredAudit(t *testing.T) {
	h := newHarness(t, "0.6.2")
	h.setHealth(&staticErr{"down"})
	h.u.HealthWindow = 100 * time.Millisecond
	h.u.Report = nil // 离线：无控制连接
	fi := newFakeInstaller(t)
	h.queue(fi.ok("rollback"))
	SavePending(h.u.StateDir, &PendingUpdate{From: "0.6.1", To: "0.6.2",
		StartedAt: time.Now().UTC(), Deadline: time.Now().UTC().Add(15 * time.Minute)})
	seedCache(t, h.u.StateDir, "0.6.1", "old")

	h.u.SelfCheck(context.Background())

	a, ok := ConsumeAuditMarker(h.u.StateDir)
	if !ok || a.Event != proto.UpdateEventRollback || a.From != "0.6.1" || a.To != "0.6.2" {
		t.Fatalf("deferred audit = %+v ok=%v", a, ok)
	}
	if !strings.Contains(a.Reason, "self-check") {
		t.Fatalf("reason = %q", a.Reason)
	}
}

// --- 场景 14：退避节流 + ForceCheck 绕过（手动升级路径）---

func TestThrottledCheckSkippedAndForced(t *testing.T) {
	h := newHarness(t, "0.6.1")
	h.f.setVersion("0.6.2")
	seedCache(t, h.u.StateDir, "0.6.1", "cur")
	// 预置一次失败（退避 1h）。
	h.u.recordFailure("0.6.2", &staticErr{"network"})

	if err := h.u.CheckNow(context.Background()); err != nil {
		t.Fatalf("throttled check must be silent skip, got %v", err)
	}
	if paths, _, _, _ := h.snapshot(); len(paths) != 0 {
		t.Fatalf("throttled version must not run: %v", paths)
	}

	// 手动：跳过退避，仍走完整编排。
	h.queue(newFakeInstaller(t).ok("install"))
	if err := h.u.ForceCheck(context.Background()); err != nil {
		t.Fatalf("ForceCheck: %v", err)
	}
	if paths, _, _, _ := h.snapshot(); len(paths) != 1 {
		t.Fatalf("ForceCheck must execute installer: %v", paths)
	}
	if _, ok := LoadPending(h.u.StateDir); !ok {
		t.Fatal("ForceCheck must reach pending+watchdog stage")
	}
}

// --- 场景 15：跨源下载拒绝（setup.json url 必须同源）---

func TestDownloadRejectsCrossOriginURL(t *testing.T) {
	h := newHarness(t, "0.6.1")
	push := proto.UpdateAvailable{Version: "0.7.0", URL: "http://evil.example/setup.exe",
		SHA256: "aa"}
	if err := h.u.HandlePush(context.Background(), push); err == nil ||
		!strings.Contains(err.Error(), "same-origin") {
		t.Fatalf("err = %v, want same-origin rejection", err)
	}
	if _, ok := LoadPending(h.u.StateDir); ok {
		t.Fatal("no pending on cross-origin rejection")
	}
}

// --- 平台执行层：真 .cmd 进程的三剧本 ---

func TestRunInstallerProcessCmdSuccess(t *testing.T) {
	fi := newFakeInstaller(t)
	if err := RunInstallerProcess(fi.ok("plain"), DefaultInstallDir, 5*time.Second); err != nil {
		t.Fatalf("exit-0 cmd: %v", err)
	}
}

func TestRunInstallerProcessCmdFailure(t *testing.T) {
	fi := newFakeInstaller(t)
	err := RunInstallerProcess(fi.fail("plain", 7), DefaultInstallDir, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "7") {
		t.Fatalf("err = %v, want exit code 7", err)
	}
}

func TestRunInstallerProcessCmdTimeoutKills(t *testing.T) {
	fi := newFakeInstaller(t)
	start := time.Now()
	err := RunInstallerProcess(fi.hang("plain"), DefaultInstallDir, 700*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("process was not killed at timeout")
	}
}

// --- 看门狗脚本生成 ---

func TestWriteWatchdogScriptContent(t *testing.T) {
	dir := t.TempDir()
	if err := writeWatchdogScript(dir, `C:\Program Files\XNC`); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(watchdogScriptPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"update-pending.json",
		"Start-Service -Name 'XNCAgent'",
		"update-audit.json",
		"update_rollback",
		"xnc-setup-' + $from + '.exe",
		"/VERYSILENT",
		"installer-cache",
		"XNC", // installDir baked
	} {
		if !strings.Contains(s, want) {
			t.Errorf("watchdog.ps1 missing %q", want)
		}
	}
	if b[len(b)-1] != '\n' || b[len(b)-2] != '\r' {
		t.Error("script must use CRLF")
	}
	// 单引号路径转义。
	if err := writeWatchdogScript(dir, `C:\O'Brien\XNC`); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(watchdogScriptPath(dir))
	if !strings.Contains(string(b2), "C:\\O''Brien\\XNC") {
		t.Error("single quotes in paths must be doubled for PS literals")
	}
}
