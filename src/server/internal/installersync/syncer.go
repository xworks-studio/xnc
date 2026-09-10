// Package installersync — GitHub Releases → 本地 release store 的定时拉取。
//
// 分发架构（docs/ci-release-and-deploy.md §3）：CI 发布的终点是 GitHub
// Release（唯一事实源）；server 以分钟级轮询把各频道最新安装器拉回本地
// releases/release_artifacts，/installer 与 /installer.json 的服务路径与
// agent 更新契约零改动（agent 本就轮询 installer.json，server 无需推送）。
// 频道判定以 tag 命名（vX.Y.Z → stable，vX.Y.Z-dev → dev）为准，不依赖
// release 的 prerelease 标记——标记配错不影响分发正确性。
package installersync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"xnc/server/internal/db"
	"xnc/server/internal/db/sqlc"
)

const (
	defAPIBase       = "https://api.github.com"
	maxInstallerByte = 128 << 20 // 与 admin 上传路径同限（api.maxUploadBytes）
	// setupArtifactName 入库制品名，与 api.setupArtifactName 一致——
	// /installer、/installer.json 按此名取制品。
	setupArtifactName = "setup.exe"
	listPageSize      = 30
	apiTimeout        = 20 * time.Second
	downloadTimeout   = 10 * time.Minute
)

// installerTag 与 release.yml 的 tag 格式校验同一规则（段 ≤4 位，dev 仅
// 裸后缀）。不匹配的 tag 不是本产品的发版，跳过。
var installerTag = regexp.MustCompile(`^v(\d{1,4}\.\d{1,4}\.\d{1,4})(-dev)?$`)

// ghRelease / ghAsset — GitHub Releases API 的最小子集（列表端点）。
type ghRelease struct {
	TagName string    `json:"tag_name"`
	Body    string    `json:"body"`
	Draft   bool      `json:"draft"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// Syncer 周期拉取器：每轮取 releases 列表（ETag 条件请求，304 不占 API
// 限额），对 stable/dev 各自的最新一条做"本地无则拉取入库"。单轮失败只
// 记日志，下轮重试（interval 即退避，不放大请求频率）。
type Syncer struct {
	st       *db.Store
	repo     string // "owner/name"
	token    string // 可选 Bearer：匿名 60 req/h 对分钟级轮询已足够
	interval time.Duration
	log      *slog.Logger
	hc       *http.Client
	apiBase  string // 生产 defAPIBase；测试注入 httptest 基址
	etag     string
}

func New(st *db.Store, repo, token string, interval time.Duration, log *slog.Logger) *Syncer {
	s := &Syncer{
		st:       st,
		repo:     repo,
		token:    token,
		interval: interval,
		log:      log,
		apiBase:  defAPIBase,
	}
	// Authorization 只随 *.github.com 重定向携带：资产下载 302 到签名
	// S3 URL 时多余 Authorization 会破坏其查询串签名。
	s.hc = &http.Client{CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if s.token != "" && (r.URL.Host == "github.com" || strings.HasSuffix(r.URL.Host, ".github.com")) {
			r.Header.Set("Authorization", "Bearer "+s.token)
		}
		return nil
	}}
	return s
}

// Run 阻塞运行至 ctx 取消：启动即同步一次（空库冷启动自填充），此后按
// interval 轮询。main.go 以信号 ctx 治理生命周期。
func (s *Syncer) Run(ctx context.Context) {
	s.tick(ctx)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tick(ctx)
		}
	}
}

func (s *Syncer) tick(ctx context.Context) {
	if err := s.syncOnce(ctx); err != nil {
		s.log.Warn("installer sync failed", "repo", s.repo, "err", err)
	}
}

func (s *Syncer) syncOnce(ctx context.Context) error {
	rels, changed, err := s.listReleases(ctx)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	// 列表按创建时间新→旧：每频道首条即该频道最新。
	latest := map[string]ghRelease{}
	for _, r := range rels {
		if r.Draft {
			continue // 资产可能仍在上传，不可信
		}
		m := installerTag.FindStringSubmatch(r.TagName)
		if m == nil {
			continue
		}
		ch := "stable"
		if m[2] == "-dev" {
			ch = "dev"
		}
		if _, seen := latest[ch]; !seen {
			latest[ch] = r
		}
	}
	for _, ch := range []string{"stable", "dev"} { // 固定顺序，日志稳定
		r, ok := latest[ch]
		if !ok {
			continue
		}
		if err := s.syncRelease(ctx, ch, r); err != nil {
			// 单频道失败不拖累另一频道；下一轮重试。
			s.log.Error("installer sync release", "channel", ch, "tag", r.TagName, "err", err)
		}
	}
	return nil
}

// listReleases — GET /repos/{repo}/releases（覆盖 stable+prerelease，一次
// 调用两频道共用）。携带 If-None-Match：304 时 GitHub 不计限额。
func (s *Syncer) listReleases(ctx context.Context) ([]ghRelease, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.apiBase+"/repos/"+s.repo+"/releases?per_page="+fmt.Sprint(listPageSize), nil)
	if err != nil {
		return nil, false, err
	}
	s.prepare(req)
	if s.etag != "" {
		req.Header.Set("If-None-Match", s.etag)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return nil, false, nil
	case resp.StatusCode != http.StatusOK:
		return nil, false, fmt.Errorf("list releases: %s", resp.Status)
	}
	var rels []ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rels); err != nil {
		return nil, false, fmt.Errorf("decode releases: %w", err)
	}
	s.etag = resp.Header.Get("ETag")
	return rels, true, nil
}

// syncRelease — 单频道最新一条：本地已有（release 行 + setup 制品俱在）
// 则跳过（幂等 + 免下载快路径）；否则下载校验后事务入库。release 行在
// 而制品缺席（历史直传路径可能留下的半截行）会触发重新拉取补全（自愈）。
func (s *Syncer) syncRelease(ctx context.Context, channel string, r ghRelease) error {
	version := strings.TrimPrefix(r.TagName, "v")
	if existing, err := s.st.Q().GetReleaseByVersion(ctx, version); err == nil {
		if _, aerr := s.st.Q().GetArtifact(ctx, sqlc.GetArtifactParams{
			ReleaseID: existing.ID, Name: setupArtifactName,
		}); aerr == nil {
			return nil
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lookup release %s: %w", version, err)
	}

	exe := findInstallerAsset(r.Assets, version)
	if exe == nil {
		return fmt.Errorf("no XNC-Installer*-%s.exe asset", version)
	}
	sidecar := findAsset(r.Assets, exe.Name+".sha256")
	if sidecar == nil {
		return fmt.Errorf("no sha256 sidecar for %s", exe.Name)
	}
	want, err := s.fetchSum(ctx, sidecar.BrowserDownloadURL)
	if err != nil {
		return err
	}
	data, err := s.fetchInstaller(ctx, exe)
	if err != nil {
		return err
	}
	if len(data) < 2 || data[0] != 'M' || data[1] != 'Z' {
		return fmt.Errorf("downloaded %s is not a Windows executable (MZ)", exe.Name)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("sha256 mismatch for %s: got %s want %s", exe.Name, got, want)
	}

	// 事务落地：release 行与制品原子可见——半截提交会让
	// GetLatestReleaseByChannel 命中无制品行，/installer 404。
	tx, err := s.st.Pool().Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // 提交后为 no-op
	q := s.st.Q().WithTx(tx)
	rel, err := q.CreateRelease(ctx, sqlc.CreateReleaseParams{
		Version: version, Notes: r.Body, Channel: channel,
	})
	if err != nil {
		return fmt.Errorf("create release: %w", err)
	}
	if err := q.PutArtifact(ctx, sqlc.PutArtifactParams{
		ReleaseID: rel.ID, Name: setupArtifactName,
		Sha256: want, Size: int64(len(data)), Data: data,
	}); err != nil {
		return fmt.Errorf("store setup: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	s.log.Info("installer synced", "channel", channel, "version", version,
		"sha256", want[:12], "size", len(data))
	return nil
}

// findInstallerAsset — 制品名形如 XNC-Installer[-dev]-<version>.exe
// （build.ps1 的 OutputBaseFilename 约定）；按前缀+版本后缀匹配，杜绝
// 误取其他附件（如未来的符号包）。
func findInstallerAsset(assets []ghAsset, version string) *ghAsset {
	for i := range assets {
		n := assets[i].Name
		if strings.HasPrefix(n, "XNC-Installer") && strings.HasSuffix(n, "-"+version+".exe") {
			return &assets[i]
		}
	}
	return nil
}

func findAsset(assets []ghAsset, name string) *ghAsset {
	for i := range assets {
		if assets[i].Name == name {
			return &assets[i]
		}
	}
	return nil
}

// fetchSum — 拉 .sha256 边车（build.ps1 产出格式 "<hash>  <name>"，与
// sha256sum 一致）并取首字段。上限 4KB：正常 64 hex + 文件名。
func (s *Syncer) fetchSum(ctx context.Context, url string) (string, error) {
	body, err := s.fetch(ctx, url, apiTimeout, 4<<10)
	if err != nil {
		return "", fmt.Errorf("fetch sha256 sidecar: %w", err)
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 || len(fields[0]) != 64 {
		return "", fmt.Errorf("malformed sha256 sidecar")
	}
	if _, err := hex.DecodeString(fields[0]); err != nil {
		return "", fmt.Errorf("malformed sha256 sidecar: %w", err)
	}
	return strings.ToLower(fields[0]), nil
}

func (s *Syncer) fetchInstaller(ctx context.Context, a *ghAsset) ([]byte, error) {
	if a.Size > maxInstallerByte {
		return nil, fmt.Errorf("installer %s too large: %d bytes", a.Name, a.Size)
	}
	data, err := s.fetch(ctx, a.BrowserDownloadURL, downloadTimeout, maxInstallerByte)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", a.Name, err)
	}
	return data, nil
}

// fetch — 带超时与硬上限的 GET（LimitReader+1 读法：超限即拒绝，防恶意
// 资产耗尽内存）。
func (s *Syncer) fetch(ctx context.Context, url string, timeout time.Duration, max int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	s.prepare(req)
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("exceeds %d byte limit", max)
	}
	return data, nil
}

// prepare — GitHub API 必要头：User-Agent 强制、JSON Accept、可选 Bearer。
func (s *Syncer) prepare(req *http.Request) {
	req.Header.Set("User-Agent", "xnc-server-installer-sync")
	req.Header.Set("Accept", "application/vnd.github+json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
}
