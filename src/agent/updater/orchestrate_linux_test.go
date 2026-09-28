//go:build linux

// orchestrate_linux_test.go — tar.gz 在线升级编排单测（linux 运行；布局
// 根与 systemctl 动作经包级 var 注入，不触碰真实 /opt/xnc 与 systemd）。
package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// linuxFixt 重定向安装布局到临时目录并记录 systemctl restart 次数。
type linuxFixt struct {
	root     string
	versions string
	restarts int
	restore  func()
}

func newLinuxFixt(t *testing.T, currentVersion string) *linuxFixt {
	t.Helper()
	root := t.TempDir()
	f := &linuxFixt{root: root, versions: filepath.Join(root, "versions")}
	// 模拟 install.sh 布局：versions/<cur>/bin/xnc-agent + current 链接。
	require.NoError(t, os.MkdirAll(filepath.Join(f.versions, currentVersion, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(f.versions, currentVersion, "bin", "xnc-agent"),
		[]byte("#!/bin/sh\necho "+currentVersion+"\n"), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(f.versions, currentVersion), filepath.Join(root, "current")))

	oldRoot, oldVersions, oldCurrent := xncInstallRoot, xncVersionsDir, xncCurrentLink
	oldRestart := systemctlRestart
	xncInstallRoot = root
	xncVersionsDir = f.versions
	xncCurrentLink = filepath.Join(root, "current")
	systemctlRestart = func() error { f.restarts++; return nil }
	f.restore = func() {
		xncInstallRoot, xncVersionsDir, xncCurrentLink = oldRoot, oldVersions, oldCurrent
		systemctlRestart = oldRestart
	}
	t.Cleanup(f.restore)
	return f
}

// buildTarball 构造制品：bin/xnc-agent 为 shell 脚本（--version 输出 ver）。
func buildTarball(t *testing.T, ver string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	script := "#!/bin/sh\necho " + ver + "\n"
	hdr := &tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755}
	require.NoError(t, tw.WriteHeader(hdr))
	hdr = &tar.Header{Name: "bin/xnc-agent", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(script))}
	require.NoError(t, tw.WriteHeader(hdr))
	_, err := tw.Write([]byte(script))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func newLinuxUpdater(t *testing.T, server, stateDir, version string) *Updater {
	t.Helper()
	u := &Updater{ServerURL: server, StateDir: stateDir, Version: version,
		Log: slog.Default(), now: func() time.Time { return time.Now() }}
	u.platform()
	return u
}

func TestLinuxApplyTarballUpdate(t *testing.T) {
	f := newLinuxFixt(t, "0.1.0")
	payload := buildTarball(t, "0.2.0")
	sum := sha256.Sum256(payload)
	m := &SetupManifest{Version: "0.2.0", URL: "/dl",
		SHA256: hex.EncodeToString(sum[:]), Size: int64(len(payload))}
	// download 需要真 HTTP 源（applyUpdate 第 1 步）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dl" {
			_, _ = w.Write(payload)
			return
		}
		_ = json.NewEncoder(w).Encode(m)
	}))
	defer srv.Close()

	stateDir := t.TempDir()
	u := newLinuxUpdater(t, srv.URL, stateDir, "0.1.0")
	require.NoError(t, u.applyUpdate(context.Background(), m))

	// 版本目录落位 + current 切换 + 重启一次 + pending 标记。
	target := filepath.Join(f.versions, "0.2.0", "bin", "xnc-agent")
	require.FileExists(t, target)
	link, err := os.Readlink(filepath.Join(f.root, "current"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(f.versions, "0.2.0"), filepath.Clean(link))
	assert.Equal(t, 1, f.restarts)
	_, perr := os.Stat(filepath.Join(stateDir, pendingName))
	assert.NoError(t, perr, "pending marker expected (awaiting self-check)")
	// 回滚源形态：无缓存 tarball，但版本目录 0.1.0 在（rollbackReady）。
	assert.True(t, u.rollbackReady("0.1.0"))
}

func TestLinuxRollbackViaVersionDir(t *testing.T) {
	f := newLinuxFixt(t, "0.1.0")
	// current 已指向坏版本 0.2.0（无对应目录也行——仅链接名义）。
	require.NoError(t, os.Remove(filepath.Join(f.root, "current")))
	require.NoError(t, os.Symlink(filepath.Join(f.versions, "0.2.0"), filepath.Join(f.root, "current")))

	stateDir := t.TempDir()
	u := newLinuxUpdater(t, "https://xnc.invalid", stateDir, "0.2.0")
	p := &PendingUpdate{From: "0.1.0", To: "0.2.0",
		StartedAt: time.Now().UTC(), Deadline: time.Now().Add(15 * time.Minute).UTC()}
	require.NoError(t, SavePending(stateDir, p))

	require.NoError(t, u.Rollback(p, "self-check: services unhealthy"))
	link, err := os.Readlink(filepath.Join(f.root, "current"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(f.versions, "0.1.0"), filepath.Clean(link))
	assert.Equal(t, 1, f.restarts)
	_, err = os.Stat(filepath.Join(stateDir, pendingName))
	assert.True(t, os.IsNotExist(err), "pending must be cleared after rollback")
}

func TestLinuxPayloadVersionMismatch(t *testing.T) {
	f := newLinuxFixt(t, "0.1.0")
	// 文件名宣称 0.2.0，包内二进制自报 0.3.0 → 拒绝。
	payload := buildTarball(t, "0.3.0")
	staged := filepath.Join(t.TempDir(), "xnc-linux-amd64-0.2.0.tar.gz")
	require.NoError(t, os.WriteFile(staged, payload, 0o644))

	u := newLinuxUpdater(t, "https://xnc.invalid", t.TempDir(), "0.1.0")
	err := u.execInstaller(staged)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version mismatch")
	assert.Equal(t, 0, f.restarts, "no restart on rejected payload")
}

func TestLinuxExtractRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3}
	require.NoError(t, tw.WriteHeader(hdr))
	_, err := tw.Write([]byte("bad"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	src := filepath.Join(t.TempDir(), "t.tar.gz")
	require.NoError(t, os.WriteFile(src, buf.Bytes(), 0o644))
	err = extractTarGz(src, t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes")
}
