package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/server/internal/db/sqlc"
)

// seedChannelRelease 建 release + setup.exe 制品（真实 sha256/size，供
// setup.json 一致性断言）；相邻 seed 间隔 2ms 保证 created_at 严格递增
// （GetLatestReleaseByChannel 按 created_at DESC 取最新）。
func seedChannelRelease(t *testing.T, env *TestEnv, channel, version string) string {
	t.Helper()
	rel, err := env.Store.Q().CreateRelease(t.Context(), sqlc.CreateReleaseParams{
		Version: version, Notes: "seed " + version, Channel: channel,
	})
	require.NoError(t, err)
	data := []byte("setup-bytes-" + channel + "-" + version)
	sum := sha256.Sum256(data)
	require.NoError(t, env.Store.Q().PutArtifact(t.Context(), sqlc.PutArtifactParams{
		ReleaseID: rel.ID, Name: setupArtifactName,
		Sha256: hex.EncodeToString(sum[:]), Size: int64(len(data)), Data: data,
	}))
	time.Sleep(2 * time.Millisecond)
	return rel.ID.String()
}

// getSetup 发 GET 并读全响应体（安装器端点无认证，无需 doJSON 的 token 面）。
func getSetup(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, b
}

type setupManifestOut struct {
	Version    string    `json:"version"`
	URL        string    `json:"url"`
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	ReleasedAt time.Time `json:"releasedAt"`
}

func getSetupManifest(t *testing.T, url string) setupManifestOut {
	t.Helper()
	resp, b := getSetup(t, url)
	require.Equal(t, 200, resp.StatusCode, "body: %s", b)
	var out setupManifestOut
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestSetupDownload：/installer 双频道各两版本取最新；channel 缺省 stable；
// 200 直流 + application/octet-stream + X-Xnc-Sha256 与制品/响应体一致。
func TestSetupDownload(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()

	seedChannelRelease(t, env, "stable", "0.4.0")
	seedChannelRelease(t, env, "stable", "0.4.5")
	seedChannelRelease(t, env, "dev", "0.5.0-dev.1")
	seedChannelRelease(t, env, "dev", "0.5.0-dev.3")

	// channel 缺省 → stable 最新 0.4.5。
	resp, body := getSetup(t, srv.URL+"/installer")
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, "setup-bytes-stable-0.4.5", string(body))
	assert.Equal(t, sha256Hex(body), resp.Header.Get("X-Xnc-Sha256"))

	// 显式 channel=stable 同结果。
	resp, body = getSetup(t, srv.URL+"/installer?channel=stable")
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "setup-bytes-stable-0.4.5", string(body))

	// dev 频道 → dev 最新 0.5.0-dev.3。
	resp, body = getSetup(t, srv.URL+"/installer?channel=dev")
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "setup-bytes-dev-0.5.0-dev.3", string(body))
	assert.Equal(t, sha256Hex(body), resp.Header.Get("X-Xnc-Sha256"))
}

// TestSetupManifest：installer.json 双频道各两版本取最新；channel 缺省 stable；
// 字段与制品 sha256/size 一致；url 为同源绝对地址（/installer?channel=<ch>）；
// releasedAt 为 release 的 created_at。
func TestSetupManifest(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()

	seedChannelRelease(t, env, "stable", "0.4.0")
	seedChannelRelease(t, env, "stable", "0.4.5")
	seedChannelRelease(t, env, "dev", "0.5.0-dev.1")
	seedChannelRelease(t, env, "dev", "0.5.0-dev.3")

	// channel 缺省 → stable 最新 0.4.5，url 指向 /setup.exe?channel=stable。
	mf := getSetupManifest(t, srv.URL+"/installer.json")
	assert.Equal(t, "0.4.5", mf.Version)
	assert.Equal(t, srv.URL+"/installer?channel=stable", mf.URL)
	assert.Equal(t, sha256Hex([]byte("setup-bytes-stable-0.4.5")), mf.SHA256)
	assert.Equal(t, int64(len("setup-bytes-stable-0.4.5")), mf.Size)
	rel, err := env.Store.Q().GetReleaseByVersion(t.Context(), "0.4.5")
	require.NoError(t, err)
	assert.True(t, rel.CreatedAt.Equal(mf.ReleasedAt),
		"releasedAt = %v, want release created_at %v", mf.ReleasedAt, rel.CreatedAt)

	// dev 频道 → dev 最新 0.5.0-dev.3。
	mf = getSetupManifest(t, srv.URL+"/installer.json?channel=dev")
	assert.Equal(t, "0.5.0-dev.3", mf.Version)
	assert.Equal(t, srv.URL+"/installer?channel=dev", mf.URL)
	assert.Equal(t, sha256Hex([]byte("setup-bytes-dev-0.5.0-dev.3")), mf.SHA256)
	assert.Equal(t, int64(len("setup-bytes-dev-0.5.0-dev.3")), mf.Size)
}

// TestSetupNotFound：无 release 404；release 存在但无 setup.exe 制品
// （仅 cli——异常发布形态）404；未知频道 404（该频道从未有 release）。
func TestSetupNotFound(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()

	// 全新环境无任何 release → 两端点全 404。
	for _, p := range []string{"/installer", "/installer.json"} {
		resp, _ := getSetup(t, srv.URL+p)
		assert.Equal(t, 404, resp.StatusCode, p)
		resp, _ = getSetup(t, srv.URL+p+"?channel=dev")
		assert.Equal(t, 404, resp.StatusCode, p+"?channel=dev")
	}

	// release 存在但无 setup.exe 制品（seedRelease 仅带 cli）→ 仍 404。
	seedRelease(t, env, "0.4.5")
	for _, p := range []string{"/installer", "/installer.json"} {
		resp, _ := getSetup(t, srv.URL+p)
		assert.Equal(t, 404, resp.StatusCode, p)
	}

	// 未知频道 → 404。
	seedChannelRelease(t, env, "stable", "0.4.6")
	for _, p := range []string{"/installer", "/installer.json"} {
		resp, _ := getSetup(t, srv.URL+p+"?channel=beta")
		assert.Equal(t, 404, resp.StatusCode, p+"?channel=beta")
	}
}
