//go:build linux

// orchestrate_linux.go — Linux 平台实现：tar.gz 制品的在线升级编排。
// 与 install.sh 同构的版本目录模型：解包校验 → 落位 /opt/xnc/versions/<ver>
// → 原子切 current 符号链接 → systemctl restart（systemd 停本服务 = 旧
// agent 进程终结，execInstaller 不再返回——语义对齐 Windows 安装器停服）。
// 回滚双源：缓存/staging 的 from 版 tarball（Rollback 通用路径重装），
// 或 install.sh 保留的版本目录（rollbackFallback 切链即回滚，秒级）。
// 看门狗等价物：systemd Restart=always（进程级）+ StartupPendingCheck/
// SelfCheck（版本级：pending 在而自报不符 → 回滚）。已知缺口（相对
// Windows schtask）：新 agent 在 OnReady 前崩溃循环时依赖 pending 的
// Deadline 过期语义，无独立的外部看门狗强杀——真机验证关注此形态。
package updater

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 布局常量（与 deploy/linux/install.sh 共同契约）。var 形态供测试注入。
var (
	xncInstallRoot = "/opt/xnc"
	xncVersionsDir = xncInstallRoot + "/versions"
	xncCurrentLink = xncInstallRoot + "/current"
	xncServiceUnit = "xnc-agent"
)

// systemctl 动作缝（测试注入；生产走 systemctl 二进制）。
var systemctlRestart = func() error {
	out, err := exec.Command("systemctl", "restart", xncServiceUnit).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart %s: %v: %s", xncServiceUnit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func systemctlActive() error {
	if err := exec.Command("systemctl", "is-active", "--quiet", xncServiceUnit).Run(); err != nil {
		return fmt.Errorf("service %s not active", xncServiceUnit)
	}
	return nil
}

// cacheInstallerName staging/缓存制品文件名（deploy/build-linux.ps1 产物名；
// dev 频道镜像 Windows 的 -dev 后缀约定）。
func cacheInstallerName(version, channel string) string {
	if channel == "dev" {
		return "xnc-linux-amd64-dev-" + version + ".tar.gz"
	}
	return "xnc-linux-amd64-" + version + ".tar.gz"
}

func installerSuffix() string       { return ".tar.gz" }
func manifestPlatformParam() string { return "&platform=linux" }

// samePath Linux：路径语义大小写敏感。
func samePath(a, b string) (bool, error) {
	return filepath.Clean(a) == filepath.Clean(b), nil
}

// platform 装配平台缝。看门狗 seam 为 no-op 成功：Linux 无 schtask 等价物，
// 回滚兜底 = systemd Restart + StartupPendingCheck（见文件头注释）。
func (u *Updater) platform() {
	if u.execInstaller == nil {
		u.execInstaller = u.execTarballInstaller
	}
	if u.serviceHealthy == nil {
		u.serviceHealthy = systemctlActive
	}
	if u.registerWatchdogTask == nil {
		u.registerWatchdogTask = func(time.Time) error { return nil }
	}
	if u.deleteWatchdogTask == nil {
		u.deleteWatchdogTask = func() {}
	}
	if u.rollbackReady == nil {
		u.rollbackReady = versionDirRollbackReady
	}
	if u.rollbackFallback == nil {
		u.rollbackFallback = rollbackToVersionDir
	}
}

// versionFromInstallerName 从缓存文件名提取版本（xnc-linux-amd64[-dev]-<ver>.tar.gz）。
func versionFromInstallerName(name string) string {
	s := strings.TrimSuffix(name, ".tar.gz")
	for _, p := range []string{"xnc-linux-amd64-dev-", "xnc-linux-amd64-"} {
		if v, ok := strings.CutPrefix(s, p); ok {
			return v
		}
	}
	return s
}

// execTarballInstaller 解包 staged tar.gz → 校验 agent 二进制自报版本 →
// 落位版本目录 → 原子切 current → systemctl restart。
func (u *Updater) execTarballInstaller(staged string) error {
	version := versionFromInstallerName(filepath.Base(staged))
	if version == "" {
		return fmt.Errorf("cannot derive version from %q", filepath.Base(staged))
	}
	if err := os.MkdirAll(xncVersionsDir, 0o755); err != nil {
		return err
	}
	// 解包目标在 versions 同层（同文件系统保证 rename 原子性）。
	unpack, err := os.MkdirTemp(xncVersionsDir, ".unpack-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(unpack)
	if err := extractTarGz(staged, unpack); err != nil {
		return err
	}
	// 制品校验：bin/xnc-agent 存在、可执行、自报版本与文件名一致
	// （sha256 已在 applyUpdate 校验，这里挡"包对了内容错"的发布事故）。
	agentBin := filepath.Join(unpack, "bin", "xnc-agent")
	out, err := exec.Command(agentBin, "--version").Output()
	if err != nil {
		return fmt.Errorf("verify %s --version: %w", agentBin, err)
	}
	if got := strings.TrimSpace(string(out)); got != version {
		return fmt.Errorf("payload version mismatch: name=%s binary reports %q", version, got)
	}
	// 原子落位 + 切链（临时链接 rename 覆盖，current 永不悬空）。
	dest := filepath.Join(xncVersionsDir, version)
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.Rename(unpack, dest); err != nil {
		return err
	}
	tmpLink := xncCurrentLink + ".new"
	_ = os.Remove(tmpLink)
	if err := os.Symlink(dest, tmpLink); err != nil {
		return err
	}
	if err := os.Rename(tmpLink, xncCurrentLink); err != nil {
		return err
	}
	u.log().Info("update: switched current symlink", "version", version, "dest", dest)
	// systemd 重启 = 本进程被停（不返回）；无 systemd/测试注入时正常返回。
	return systemctlRestart()
}

// extractTarGz 平铺解包（路径穿越防护：拒绝绝对路径与 ".." 段；保留可执行位）。
func extractTarGz(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		clean := filepath.Clean(hdr.Name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("tar entry escapes target dir: %q", hdr.Name)
		}
		target := filepath.Join(dst, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode) & 0o777
			if mode == 0 {
				mode = 0o644
			}
			if err := writeTarFile(target, tr, mode); err != nil {
				return err
			}
		default:
			// 符号链接等：制品只含常规文件与目录，其余拒绝（防逃逸面）。
			return fmt.Errorf("tar entry type unsupported: %q (type %c)", hdr.Name, hdr.Typeflag)
		}
	}
}

func writeTarFile(target string, r io.Reader, mode os.FileMode) error {
	tmp := target + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// versionDirRollbackReady：from 版目录在且含可用 agent 即就绪——升级后
// current 切向新版本，from 目录天然成为回退源（升级前 current 指向 from
// 属正常形态，不得据此排除）；from 目录已被修剪时退化为"存在其它可用
// 版本目录"。
func versionDirRollbackReady(from string) bool {
	if _, err := os.Stat(filepath.Join(xncVersionsDir, from, "bin", "xnc-agent")); err == nil {
		return true
	}
	_, err := candidateVersionDir(from)
	return err == nil
}

// candidateVersionDir 选回退目录：优先 from 版目录（上次升级的来源），
// 否则版本序最新的其它目录。
func candidateVersionDir(from string) (string, error) {
	entries, err := os.ReadDir(xncVersionsDir)
	if err != nil {
		return "", err
	}
	cur, _ := filepath.EvalSymlinks(xncCurrentLink)
	var names []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(xncVersionsDir, e.Name())
		if cur != "" && p == cur {
			continue // current 指向的不算回退源
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no previous version dir under %s", xncVersionsDir)
	}
	if _, err := os.Stat(filepath.Join(xncVersionsDir, from, "bin", "xnc-agent")); err == nil {
		return filepath.Join(xncVersionsDir, from), nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return filepath.Join(xncVersionsDir, names[0]), nil
}

// rollbackToVersionDir 回滚 = current 切回旧版本目录 + 重启（秒级，
// 与 install.sh 手工回滚同一动作）。
func rollbackToVersionDir(from string) error {
	dir, err := candidateVersionDir(from)
	if err != nil {
		return err
	}
	tmpLink := xncCurrentLink + ".rollback"
	_ = os.Remove(tmpLink)
	if err := os.Symlink(dir, tmpLink); err != nil {
		return err
	}
	if err := os.Rename(tmpLink, xncCurrentLink); err != nil {
		return err
	}
	return systemctlRestart()
}
