package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
	"xnc/server/internal/db/sqlc"
)

// uploadRelease 经 multipart POST /api/admin/releases 上传；parts 为可选
// 文件部件（setup/cli），键即部件名。
func uploadRelease(t *testing.T, srvURL, token, version string, parts map[string][]byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	require.NoError(t, mw.WriteField("version", version))
	for name, data := range parts {
		fw, err := mw.CreateFormFile(name, name)
		require.NoError(t, err)
		_, err = fw.Write(data)
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	req, err := http.NewRequest("POST", srvURL+"/api/admin/releases", &body)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// seedRelease 直接经 store 建 release + cli 制品（删除语义测试无需走
// 上传端点的 multipart 校验）；相邻 seed 间隔 2ms 保证 created_at 严格递增
// （GetLatestReleaseByChannel 按 created_at DESC 取最新）。
func seedRelease(t *testing.T, env *TestEnv, version string) string {
	t.Helper()
	rel, err := env.Store.Q().CreateRelease(t.Context(), sqlc.CreateReleaseParams{
		Version: version, Notes: "seed " + version, Channel: "stable",
	})
	require.NoError(t, err)
	require.NoError(t, env.Store.Q().PutArtifact(t.Context(), sqlc.PutArtifactParams{
		ReleaseID: rel.ID, Name: cliArtifactName,
		Sha256: "seed", Size: int64(len("cli-bytes-" + version)), Data: []byte("cli-bytes-" + version),
	}))
	time.Sleep(2 * time.Millisecond)
	return rel.ID.String()
}

// releaseVersions 经 GET /api/admin/releases 返回版本列表。
func releaseVersions(t *testing.T, srvURL, token string) []string {
	t.Helper()
	resp := doJSON(t, srvURL, "GET", "/api/admin/releases", token, "")
	require.Equal(t, 200, resp.StatusCode)
	defer resp.Body.Close()
	var list []struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	out := make([]string, 0, len(list))
	for _, r := range list {
		out = append(out, r.Version)
	}
	return out
}

// cliLatestVersion 经 GET /api/cli/latest?channel=stable 返回最新版本。
func cliLatestVersion(t *testing.T, srvURL, token string) string {
	t.Helper()
	resp := doJSON(t, srvURL, "GET", "/api/cli/latest?channel=stable", token, "")
	require.Equal(t, 200, resp.StatusCode)
	defer resp.Body.Close()
	var out struct {
		Version string `json:"version"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out.Version
}

// TestAdminDeleteRelease：删除语义——删最新 → list 无该版本、latest 回落
// 到剩余最新（GetLatestReleaseByChannel 的 created_at 语义）；制品级联删除；
// 不存在/非法 id → 404；非 admin → 403。
func TestAdminDeleteRelease(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)

	// 未认证 → 401
	assert.Equal(t, 401, doJSON(t, srv.URL, "DELETE", "/api/admin/releases/"+strings.Repeat("0", 32), "", "").StatusCode)

	id040 := seedRelease(t, env, "0.4.0")
	id042 := seedRelease(t, env, "0.4.2")
	id045 := seedRelease(t, env, "0.4.5")

	assert.Equal(t, "0.4.5", cliLatestVersion(t, srv.URL, admin))
	assert.Equal(t, []string{"0.4.5", "0.4.2", "0.4.0"}, releaseVersions(t, srv.URL, admin))

	// 删除中间版本：latest 不受影响（仍 0.4.5）。
	resp := doJSON(t, srv.URL, "DELETE", "/api/admin/releases/"+id042, admin, "")
	require.Equal(t, 204, resp.StatusCode)
	assert.Equal(t, []string{"0.4.5", "0.4.0"}, releaseVersions(t, srv.URL, admin))
	assert.Equal(t, "0.4.5", cliLatestVersion(t, srv.URL, admin))

	// 删除最新：list 无 0.4.5，latest 自动回落到 0.4.0。
	resp = doJSON(t, srv.URL, "DELETE", "/api/admin/releases/"+id045, admin, "")
	require.Equal(t, 204, resp.StatusCode)
	assert.Equal(t, []string{"0.4.0"}, releaseVersions(t, srv.URL, admin))
	assert.Equal(t, "0.4.0", cliLatestVersion(t, srv.URL, admin))

	// 制品级联删除：被删 release 的 release_artifacts 无残留。
	var artCount int
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT COUNT(*) FROM release_artifacts WHERE release_id = $1`, id045).Scan(&artCount))
	assert.Equal(t, 0, artCount)

	// 不存在 → 404；非法 UUID → 404。
	resp = doJSON(t, srv.URL, "DELETE", "/api/admin/releases/"+strings.Repeat("0", 8)+"-0000-4000-8000-000000000000", admin, "")
	require.Equal(t, 404, resp.StatusCode)
	resp = doJSON(t, srv.URL, "DELETE", "/api/admin/releases/not-a-uuid", admin, "")
	require.Equal(t, 404, resp.StatusCode)

	// 删光后 latest → 404（无 release）。
	resp = doJSON(t, srv.URL, "DELETE", "/api/admin/releases/"+id040, admin, "")
	require.Equal(t, 204, resp.StatusCode)
	resp = doJSON(t, srv.URL, "GET", "/api/cli/latest?channel=stable", admin, "")
	require.Equal(t, 404, resp.StatusCode)
}

// TestAdminUploadRelease：终态上传形态——setup（必填，MZ 校验）+ cli
// （可选）。bundle 部件已随遗留更新通道退役（未上线直采终态）：仍传入 →
// 400 明确报错；缺 setup → 400；非 PE setup → 400 且 release 不落库。
func TestAdminUploadRelease(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)

	setup := append([]byte("MZ"), []byte("fake-inno-setup-0.10.0-payload")...)

	// setup-only 上传 → 201。
	resp := uploadRelease(t, srv.URL, admin, "0.10.0", map[string][]byte{"setup": setup})
	require.Equal(t, 201, resp.StatusCode)

	// /setup.exe 直流刚上传的安装器，sha256 头与制品一致。
	got, body := getSetup(t, srv.URL+"/installer?channel=stable")
	require.Equal(t, 200, got.StatusCode)
	assert.Equal(t, setup, body)
	assert.Equal(t, sha256Hex(body), got.Header.Get("X-Xnc-Sha256"))

	// /setup.json 清单字段与制品一致。
	mf := getSetupManifest(t, srv.URL+"/installer.json?channel=stable")
	assert.Equal(t, "0.10.0", mf.Version)
	assert.Equal(t, sha256Hex(setup), mf.SHA256)
	assert.Equal(t, int64(len(setup)), mf.Size)

	// setup + cli 同 release → 201，cli 制品入库（CLI 自更新通道可用）。
	time.Sleep(2 * time.Millisecond) // created_at 严格递增（同 seedRelease）
	cli := []byte("fake-cli-0.10.1")
	resp = uploadRelease(t, srv.URL, admin, "0.10.1", map[string][]byte{"setup": setup, "cli": cli})
	require.Equal(t, 201, resp.StatusCode)
	rel, err := env.Store.Q().GetReleaseByVersion(t.Context(), "0.10.1")
	require.NoError(t, err)
	ca, err := env.Store.Q().GetArtifact(t.Context(), sqlc.GetArtifactParams{ReleaseID: rel.ID, Name: cliArtifactName})
	require.NoError(t, err)
	assert.Equal(t, sha256Hex(cli), ca.Sha256)

	// bundle 部件仍传入 → 400 明确报"bundle channel retired"，release 不落库。
	resp = uploadRelease(t, srv.URL, admin, "0.10.2", map[string][]byte{
		"setup":  setup,
		"bundle": []byte("legacy-bundle"),
	})
	require.Equal(t, 400, resp.StatusCode)
	var e struct {
		Error proto.APIError `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&e))
	assert.Contains(t, e.Error.Message, "bundle channel retired")
	assert.NotContains(t, releaseVersions(t, srv.URL, admin), "0.10.2")

	// 缺 setup → 400（setup 是必填部件）。
	resp = uploadRelease(t, srv.URL, admin, "0.10.3", map[string][]byte{"cli": cli})
	require.Equal(t, 400, resp.StatusCode)
	assert.NotContains(t, releaseVersions(t, srv.URL, admin), "0.10.3")

	// setup 非 PE（无 MZ 头）→ 400，release 不落库。
	resp = uploadRelease(t, srv.URL, admin, "0.10.4", map[string][]byte{"setup": []byte("not-an-exe")})
	require.Equal(t, 400, resp.StatusCode)
	assert.NotContains(t, releaseVersions(t, srv.URL, admin), "0.10.4")
}

// TestAdminDeleteReleaseForbidden：非 admin（无任何 cluster owner 身份）→ 403，
// 与 release 管理面其余端点一致。
func TestAdminDeleteReleaseForbidden(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)

	_, userTok := createUserViaAPI(t, srv.URL, admin, "plain@t.local", "Plain")
	id := seedRelease(t, env, "0.4.5")

	resp := doJSON(t, srv.URL, "DELETE", "/api/admin/releases/"+id, userTok, "")
	require.Equal(t, 403, resp.StatusCode)
	var e struct {
		Error proto.APIError `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&e))
	assert.Equal(t, proto.CodeForbidden, e.Error.Code)

	// release 未被删除。
	assert.Equal(t, []string{"0.4.5"}, releaseVersions(t, srv.URL, admin))
}
