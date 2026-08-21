// Package updater — agent 自更新：OFFER 去重 → 下载校验 → staging →
// apply（Windows：杀 helper、删 DLL 缓存、spawn --apply-update 子进程、
// 退出由子进程重启服务）。
//
// 三入口（HELLO_ACK 回执 / 心跳 ACK 搭车 / UPDATE_OFFER）全部收敛到
// Handle；同版本幂等跳过，进行中版本重入忽略。
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"xnc/proto"
)

// Updater 持有自更新状态。单实例随 agent 进程存活。
type Updater struct {
	ServerURL string // 控制面 server 基址（offer URL 是相对路径）
	StateDir  string // staging 根目录
	Version   string // 当前 bundle 版本（machineinfo.Version）
	Log       *slog.Logger

	mu         sync.Mutex
	inProgress string                      // 正在更新的目标版本（重入忽略）
	Report     func(m proto.Message) error // 当前连接的 STATUS 发送闭包（OnReady 注入）
}

// bundleFile bundle 内固定文件名。
const (
	manifestName = "manifest.json"
	agentExe     = "xnc-agent.exe"
	helperExe    = "xnc-screen-helper.exe"
)

type manifest struct {
	Version string `json:"version"`
	Files   []struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

// Handle 处理一次目标版本信号（offer 或 targetVersion 通知）。
// send 用于 UPDATE_STATUS 上报（可为 nil：仅日志）。
func (u *Updater) Handle(ctx context.Context, offer proto.UpdateOffer, send func(proto.Message) error) {
	u.mu.Lock()
	if offer.Version == u.Version || offer.Version == u.inProgress {
		u.mu.Unlock()
		return
	}
	u.inProgress = offer.Version
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.inProgress = ""
		u.mu.Unlock()
	}()

	status := func(phase, errMsg string) {
		u.Log.Info("update", "phase", phase, "version", offer.Version, "err", errMsg)
		if send == nil && u.Report != nil {
			send = u.Report
		}
		if send != nil {
			m, _ := proto.NewMsg(proto.TypeUpdateStatus, proto.UpdateStatus{
				Version: offer.Version, Phase: phase, Error: errMsg})
			_ = send(m)
		}
	}
	fail := func(phase string, err error) {
		status(phase, err.Error())
	}

	// 1. 下载（offer.URL 相对路径 → server 基址拼接）。
	status(proto.UpdatePhaseDownloading, "")
	bundle, err := u.download(ctx, offer)
	if err != nil {
		fail(proto.UpdatePhaseDownloading, err)
		return
	}

	// 2. 校验（sha256 为信任根——控制通道已认证）。
	status(proto.UpdatePhaseVerifying, "")
	sum := sha256.Sum256(bundle)
	if hex.EncodeToString(sum[:]) != offer.SHA256 {
		fail(proto.UpdatePhaseVerifying, fmt.Errorf("sha256 mismatch"))
		return
	}

	// 3. staging：解包 + 逐文件校验 + manifest 版本一致性。
	status(proto.UpdatePhaseStaging, "")
	stageDir, err := u.stage(bundle, offer.Version)
	if err != nil {
		fail(proto.UpdatePhaseStaging, err)
		return
	}

	// 4. apply：平台特定之舞（Windows：子进程换文件重启服务）。
	status(proto.UpdatePhaseApplying, "")
	if err := u.apply(stageDir); err != nil {
		fail(proto.UpdatePhaseApplying, err)
		return
	}
	// applying 后进程退出，done 由新版 agent 的 HELLO 版本承担。
}

// download 拉取 bundle 全量字节（~15MB，agent bundle 单次下载）。
func (u *Updater) download(ctx context.Context, offer proto.UpdateOffer) ([]byte, error) {
	url := offer.URL
	if !strings_HasPrefix(url, "http") {
		url = u.ServerURL + url
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 128<<20))
}

// strings_HasPrefix 避免为单个前缀检查引入 strings 到本文件的额外行——
// 直接内联（保持文件聚焦）。
func strings_HasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

// stage 解包 bundle 到 stateDir/update-staging-<version>/，逐文件哈希
// 校验（manifest 为准），manifest 版本须与 offer 一致。返回 staging 目录。
func (u *Updater) stage(bundle []byte, version string) (string, error) {
	dir := filepath.Join(u.StateDir, "update-staging-"+version)
	_ = os.RemoveAll(dir) // 残留清扫（上次失败的半成品）
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := extractTarGz(bundle, dir); err != nil {
		return "", err
	}
	mfRaw, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return "", fmt.Errorf("manifest missing: %w", err)
	}
	var mf manifest
	if err := json.Unmarshal(mfRaw, &mf); err != nil {
		return "", fmt.Errorf("manifest invalid: %w", err)
	}
	if mf.Version != version {
		return "", fmt.Errorf("manifest version %q != offer %q", mf.Version, version)
	}
	for _, f := range mf.Files {
		b, err := os.ReadFile(filepath.Join(dir, f.Name))
		if err != nil {
			return "", fmt.Errorf("staged file missing %s: %w", f.Name, err)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			return "", fmt.Errorf("staged sha256 mismatch: %s", f.Name)
		}
	}
	// 必需文件齐全。
	for _, need := range []string{agentExe, helperExe} {
		if _, err := os.Stat(filepath.Join(dir, need)); err != nil {
			return "", fmt.Errorf("bundle missing %s", need)
		}
	}
	return dir, nil
}

// cleanupStale 清扫过期 staging 目录（agent 启动时调用）。
func CleanupStale(stateDir string) {
	matches, _ := filepath.Glob(filepath.Join(stateDir, "update-staging-*"))
	for _, m := range matches {
		_ = os.RemoveAll(m)
	}
}

// removeOld 备份当前文件为 .old（存在则覆盖旧备份——只保留一代）。
func removeOld(path string) error {
	old := path + ".old"
	_ = os.Remove(old)
	if _, err := os.Stat(path); err != nil {
		return nil // 不存在（如 helper 首装）视为成功
	}
	return os.Rename(path, old)
}

var _ = exec.Command // 供 apply_windows.go 使用（保持包引用稳定）
