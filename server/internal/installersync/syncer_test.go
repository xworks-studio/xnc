// syncer_test.go — installersync 真 PG 测试：GitHub API 用 httptest 假服务器
// 模拟（列表端点 + 资产下载 + ETag 304），入库走真 PG（testcontainers），
// 验证 /installer 消费路径所依赖的 latest/artifact 语义。
package installersync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"xnc/server/internal/db"
	"xnc/server/internal/db/sqlc"
)

// fakeGH — GitHub Releases API 最小仿真。tags 新→旧；"draft:" 前缀标记
// draft release。资产内容经 addInstaller 注入，browser_download_url 指回
// 本服务器 /dl/<name>；downloads 记录资产下载计数（幂等/304 断言用）。
type fakeGH struct {
	mu        sync.Mutex
	srv       *httptest.Server
	tags      []string
	contents  map[string][]byte
	downloads map[string]int
}

func newFakeGH(t *testing.T, tagsNewestFirst []string) *fakeGH {
	f := &fakeGH{
		tags:      tagsNewestFirst,
		contents:  map[string][]byte{},
		downloads: map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		// ETag 由内容派生：tags 变化 ⇒ 新 ETag（对齐 GitHub 语义，否则
		// 测试无法驱动"远端出新版本"场景）。
		etag := `W/"list-` + strings.Join(f.tags, "|") + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		type asset struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		}
		var out []map[string]any
		for _, tag := range f.tags {
			draft := strings.HasPrefix(tag, "draft:")
			tn := strings.TrimPrefix(tag, "draft:")
			name := "XNC-Installer" + channelSuffixOf(tn) + "-" + versionOf(tn) + ".exe"
			as := []asset{
				{Name: name, URL: f.srv.URL + "/dl/" + name, Size: int64(len(f.contents[name]))},
				{Name: name + ".sha256", URL: f.srv.URL + "/dl/" + name + ".sha256", Size: 80},
			}
			out = append(out, map[string]any{
				"tag_name": tn, "body": "release " + tn, "draft": draft, "assets": as,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		name := r.URL.Path[len("/dl/"):]
		f.downloads[name]++
		data, ok := f.contents[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// versionOf — "v1.2.3-dev" → "1.2.3-dev"（去 v 前缀，保留 dev 后缀）。
func versionOf(tag string) string { return tag[1:] }

// channelSuffixOf — tag 尾部 -dev → 制品名中缀 "-dev"。
func channelSuffixOf(tag string) string {
	if strings.HasSuffix(tag, "-dev") {
		return "-dev"
	}
	return ""
}

// addInstaller — 注入某 tag 的安装器资产 + 边车（mangle=true 注入错哈希）。
func (f *fakeGH) addInstaller(tag string, mangle bool) string {
	name := "XNC-Installer" + channelSuffixOf(tag) + "-" + versionOf(tag) + ".exe"
	data := append([]byte("MZ"), []byte("fake installer bytes for "+tag)...)
	f.contents[name] = data
	sum := sha256.Sum256(data)
	sumHex := hex.EncodeToString(sum[:])
	if mangle {
		sumHex = "ff" + sumHex[2:]
	}
	f.contents[name+".sha256"] = []byte(sumHex + "  " + name)
	return name
}

func (f *fakeGH) snapshotDownloads() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make(map[string]int, len(f.downloads))
	for k, v := range f.downloads {
		cp[k] = v
	}
	return cp
}

func newTestSyncer(t *testing.T, st *db.Store, f *fakeGH) *Syncer {
	s := New(st, "o/r", "", 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.apiBase = f.srv.URL
	return s
}

func TestSyncOnceFillsBothChannels(t *testing.T) {
	st := db.OpenTestStore(t)
	f := newFakeGH(t, []string{"v1.1.0-dev", "v1.0.0", "v0.9.0"})
	f.addInstaller("v1.1.0-dev", false)
	stableName := f.addInstaller("v1.0.0", false)
	f.addInstaller("v0.9.0", false) // 非频道最新：不应入库

	s := newTestSyncer(t, st, f)
	require.NoError(t, s.syncOnce(context.Background()))

	rel, err := st.Q().GetLatestReleaseByChannel(t.Context(), "stable")
	require.NoError(t, err)
	require.Equal(t, "1.0.0", rel.Version)
	art, err := st.Q().GetArtifact(t.Context(), sqlc.GetArtifactParams{ReleaseID: rel.ID, Name: "setup.exe"})
	require.NoError(t, err)
	require.Equal(t, f.contents[stableName], art.Data)
	want := sha256.Sum256(f.contents[stableName])
	require.Equal(t, hex.EncodeToString(want[:]), art.Sha256)

	rel, err = st.Q().GetLatestReleaseByChannel(t.Context(), "dev")
	require.NoError(t, err)
	require.Equal(t, "1.1.0-dev", rel.Version)
	_, err = st.Q().GetArtifact(t.Context(), sqlc.GetArtifactParams{ReleaseID: rel.ID, Name: "setup.exe"})
	require.NoError(t, err)

	// 非频道最新（0.9.0）不拉取不入库：latest 语义只同步各频道最新。
	_, err = st.Q().GetReleaseByVersion(t.Context(), "0.9.0")
	require.Error(t, err)
	dl := f.snapshotDownloads()
	require.Equal(t, 1, dl["XNC-Installer-1.0.0.exe"])
	require.Equal(t, 1, dl["XNC-Installer-dev-1.1.0-dev.exe"])
	require.NotContains(t, dl, "XNC-Installer-0.9.0.exe")
}

func TestSyncIdempotentWithETag(t *testing.T) {
	st := db.OpenTestStore(t)
	f := newFakeGH(t, []string{"v1.0.0"})
	name := f.addInstaller("v1.0.0", false)

	s := newTestSyncer(t, st, f)
	require.NoError(t, s.syncOnce(context.Background()))
	first := f.snapshotDownloads()

	// 第二轮：ETag 304 短路——资产零下载。
	require.NoError(t, s.syncOnce(context.Background()))
	require.Equal(t, first, f.snapshotDownloads(), "304 路径不应触发任何资产下载")

	// 第三轮（远端出新版本）：etag 失效 → 仅新版本被拉取，旧资产不重下。
	f.mu.Lock()
	f.tags = []string{"v1.0.1"}
	f.mu.Unlock()
	f.addInstaller("v1.0.1", false)
	require.NoError(t, s.syncOnce(context.Background()))
	rel, err := st.Q().GetLatestReleaseByChannel(t.Context(), "stable")
	require.NoError(t, err)
	require.Equal(t, "1.0.1", rel.Version)
	dl := f.snapshotDownloads()
	require.Equal(t, first[name], dl[name], "旧版本资产不重下")
	require.Equal(t, 1, dl["XNC-Installer-1.0.1.exe"])
}

func TestSyncSkipsExistingReleaseWithArtifact(t *testing.T) {
	st := db.OpenTestStore(t)
	f := newFakeGH(t, []string{"v1.0.0"})
	f.addInstaller("v1.0.0", false)

	// 预置完整 release（模拟直传时代的存量行）。
	rel, err := st.Q().CreateRelease(t.Context(), sqlc.CreateReleaseParams{Version: "1.0.0", Channel: "stable"})
	require.NoError(t, err)
	require.NoError(t, st.Q().PutArtifact(t.Context(), sqlc.PutArtifactParams{
		ReleaseID: rel.ID, Name: "setup.exe", Sha256: "00", Size: 1, Data: []byte("MZx"),
	}))

	s := newTestSyncer(t, st, f)
	require.NoError(t, s.syncOnce(context.Background()))
	// 快路径跳过：exe 资产零下载，存量数据不被覆盖。
	require.NotContains(t, f.snapshotDownloads(), "XNC-Installer-1.0.0.exe")
	art, err := st.Q().GetArtifact(t.Context(), sqlc.GetArtifactParams{ReleaseID: rel.ID, Name: "setup.exe"})
	require.NoError(t, err)
	require.Equal(t, []byte("MZx"), art.Data)
}

func TestSyncHealsHalfRow(t *testing.T) {
	st := db.OpenTestStore(t)
	f := newFakeGH(t, []string{"v1.0.0"})
	name := f.addInstaller("v1.0.0", false)

	// 预置半截行：release 在、制品缺（直传路径中断的形态）。
	rel, err := st.Q().CreateRelease(t.Context(), sqlc.CreateReleaseParams{Version: "1.0.0", Channel: "stable"})
	require.NoError(t, err)

	s := newTestSyncer(t, st, f)
	require.NoError(t, s.syncOnce(context.Background()))
	art, err := st.Q().GetArtifact(t.Context(), sqlc.GetArtifactParams{ReleaseID: rel.ID, Name: "setup.exe"})
	require.NoError(t, err)
	require.Equal(t, f.contents[name], art.Data)
}

func TestSyncRejectsShaMismatch(t *testing.T) {
	st := db.OpenTestStore(t)
	f := newFakeGH(t, []string{"v1.0.0"})
	f.addInstaller("v1.0.0", true) // 边车哈希被篡改

	s := newTestSyncer(t, st, f)
	require.NoError(t, s.syncOnce(context.Background())) // 单频道失败只记日志
	_, err := st.Q().GetReleaseByVersion(t.Context(), "1.0.0")
	require.Error(t, err, "校验失败的版本不得入库")
}

func TestSyncIgnoresForeignTagsAndDrafts(t *testing.T) {
	st := db.OpenTestStore(t)
	// draft 无资产内容：若被误选为频道最新，该频道将无入库，断言即失败。
	f := newFakeGH(t, []string{"draft:v2.0.0", "experiment-2026", "v1.0.0"})
	f.addInstaller("v1.0.0", false)

	s := newTestSyncer(t, st, f)
	require.NoError(t, s.syncOnce(context.Background()))
	rel, err := st.Q().GetLatestReleaseByChannel(t.Context(), "stable")
	require.NoError(t, err)
	require.Equal(t, "1.0.0", rel.Version)
}
