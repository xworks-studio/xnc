// Package updater — agent 自更新编排（spec §9，安装器即更新器）。
//
// agent 不再解包搬文件：发现（/setup.json）→ 下载 sha256 校验（staging）
// → 确认 installer-cache 回滚源 → 写 update-pending.json + 注册一次性
// 看门狗计划任务 → SYSTEM 静默执行 setup.exe；新 agent 启动自检收尾
// （成功：删 pending/看门狗、刷新缓存、audit update_ok；失败：主动回滚
// = 原地静默重跑缓存旧版安装器，audit update_rollback）。
//
// 三触发（§9.1）：WS 推送 UPDATE_AVAILABLE（HandlePush）、6h 轮询
// （CheckNow，agent 侧装配）、agentctl 管道手动（ForceCheck，Task 8）。
// 推送携带 {version,url,sha256}（已认证控制通道，哈希即真相）；轮询以
// setup.json 为准。
package updater

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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"xnc/proto"
)

// SetupManifest — /setup.json?channel= 动态清单（T1 契约）。
type SetupManifest struct {
	Version    string `json:"version"`
	URL        string `json:"url"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	ReleasedAt string `json:"releasedAt"`
}

// PendingUpdate — update-pending.json（§9.3）：更新进行中标记，看门狗与
// 新 agent 自检的共同判据。
type PendingUpdate struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	StartedAt time.Time `json:"startedAt"`
	Deadline  time.Time `json:"deadline"`
}

// StateDir 内固定文件名（安装器 T6 / 看门狗脚本按同一约定读写）。
const (
	pendingName   = "update-pending.json"
	throttleName  = "update-throttle.json"
	auditName     = "update-audit.json" // 离线回滚的延迟审计标记
	watchdogFile  = "watchdog.ps1"
	stagingName   = "staging"
	cacheName     = "installer-cache"
	rollbackName  = "rollback" // installer-cache 的回滚源子目录（见 stashRollbackSource）
	WatchdogTask  = "XNCRollbackWatchdog" // 与 xnc.iss 共享的计划任务名（T6 契约）
	coreServiceNm = "XNCCore"
)

// 编排参数（spec §9；测试经 Updater 字段缩短）。
const (
	// DefaultPendingWindow pending 截止窗（§9.3 deadline = startedAt+15min）。
	DefaultPendingWindow = 15 * time.Minute
	// WatchdogRestartGrace 看门狗触发后重启 agent 的等待窗（deadline 后的
	// 小额宽限，§9.3"trigger = deadline+small grace"）。
	WatchdogRestartGrace = 2 * time.Minute
	// WatchdogRestartWaitSec 看门狗脚本内重启 agent 后的等待秒数（须与
	// WatchdogRestartGrace 同量级；脚本常量，独立于 Go 侧）。
	// PendingMaxAge 过期 pending 兜底（§9.4：deadline 24h 未清 → 任意一次
	// 成功启动时回滚）。
	PendingMaxAge = 24 * time.Hour
	// DefaultInstallDir 安装器 /DIR 目标（与 xnc.iss DefaultDirName 一致）。
	DefaultInstallDir = `C:\Program Files\XNC`
	// DefaultHealthWindow 自检等服务健康的轮询窗。
	DefaultHealthWindow = 60 * time.Second
	// PollInterval 6h 周期轮询（§9.1；agent 侧装配，测试可缩短）。
	PollInterval = 6 * time.Hour

	// downloadLimit 单次下载上限（安装器数十 MB，512MB 兜底）。
	downloadLimit = 512 << 20
)

// backoffSchedule 同版本失败退避（§9.1：1h → 4h → 24h 封顶）。
var backoffSchedule = []time.Duration{time.Hour, 4 * time.Hour, 24 * time.Hour}

// Updater 持有更新编排状态。单实例随 agent 连接周期存活；节流/黑名单/
// pending 持久化在 StateDir，实例重建无损失。
type Updater struct {
	ServerURL  string // 控制面基址（setup.json/setup.exe 相对路径的拼接根）
	StateDir   string // %ProgramData%\XNC
	Version    string // 自报版本（machineinfo.Version，单一来源）
	Channel    string // 绑定频道（stable|dev；空 = stable）
	InstallDir string // 安装器 /DIR 目标；空 = DefaultInstallDir
	Log        *slog.Logger

	// PendingWindow / HealthWindow：测试缩短用；零值取默认。
	PendingWindow time.Duration
	HealthWindow  time.Duration

	// Report audit（UPDATE_AUDIT）发送闭包（OnReady 注入）；nil 或发送
	// 失败时落 update-audit.json，agent 下次连接补报。
	Report func(m proto.Message) error

	now func() time.Time // 时钟缝（退避/过期测试注入）

	// 平台缝（orchestrate_windows.go 提供真实现；orchestrate_other.go 桩；
	// 单测注入假实现——假安装器即普通 .cmd）。
	execInstaller        func(exePath string) error
	serviceHealthy       func() error
	registerWatchdogTask func(runAt time.Time) error
	deleteWatchdogTask   func() // 删除失败仅记日志（任务一次性，残留不重触发）
}

// New 构造编排器并装配平台实现（Windows 真实现，其他平台报错桩）。
func New(serverURL, stateDir, version, channel string, log *slog.Logger) *Updater {
	if log == nil {
		log = slog.Default()
	}
	u := &Updater{ServerURL: serverURL, StateDir: stateDir, Version: version,
		Channel: channel, Log: log}
	u.platform()
	return u
}

func (u *Updater) log() *slog.Logger {
	if u.Log != nil {
		return u.Log
	}
	return slog.Default()
}

func (u *Updater) installDir() string {
	if u.InstallDir != "" {
		return u.InstallDir
	}
	return DefaultInstallDir
}

func (u *Updater) pendingWindow() time.Duration {
	if u.PendingWindow > 0 {
		return u.PendingWindow
	}
	return DefaultPendingWindow
}

func (u *Updater) healthWindow() time.Duration {
	if u.HealthWindow > 0 {
		return u.HealthWindow
	}
	return DefaultHealthWindow
}

func (u *Updater) clock() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

// installerTimeout 执行安装器的等待上限：pending 截止 + 看门狗宽限后，
// agent 侧不再等（看门狗兜底；超时按失败直接回滚）。
func (u *Updater) installerTimeout() time.Duration {
	return u.pendingWindow() + WatchdogRestartGrace + time.Minute
}

// ---- 触发入口（§9.1）----

// CheckNow 立即拉取 setup.json 并编排一轮更新（周期轮询/目标版本信号）。
func (u *Updater) CheckNow(ctx context.Context) error {
	return u.check(ctx, false)
}

// ForceCheck 手动立即检查（Task 8 agentctl upgrade op 内核）：跳过退避
// （人工决策），黑名单/降级/pending 串行化照常。
func (u *Updater) ForceCheck(ctx context.Context) error {
	return u.check(ctx, true)
}

// HandlePush 处理 WS 推送 UPDATE_AVAILABLE：推送已是完整目标清单
// {version,url,sha256}（已认证控制通道），直接编排。
func (u *Updater) HandlePush(ctx context.Context, push proto.UpdateAvailable) error {
	return u.orchestrateTrigger(ctx, false, &SetupManifest{
		Version: push.Version, URL: push.URL, SHA256: push.SHA256,
	})
}

// check 轮询路径：setup.json 为清单权威（pending 串行化先于网络请求）。
func (u *Updater) check(ctx context.Context, force bool) error {
	if p, ok := LoadPending(u.StateDir); ok {
		u.log().Info("update: pending exists; refusing new trigger",
			"from", p.From, "to", p.To)
		return nil
	}
	m, err := u.FetchManifest(ctx)
	if err != nil {
		u.log().Warn("update: setup.json fetch failed", "err", err)
		return err
	}
	return u.orchestrateTrigger(ctx, force, m)
}

// 进程级编排互斥（§9.3 串行化）：同一 agent 进程只有一个安装目标，任何
// 时刻至多一轮编排在跑。跨 Updater 实例生效——连接周期的轮询/推送编排器
// 与 agentctl 手动触发（Task 8）自建的实例并存时仍互斥，否则两实例可
// 并行下载并各跑一次安装器（pending 落盘前的窗口）。
var (
	orchestrateMu         sync.Mutex
	orchestrateInProgress bool
)

func beginOrchestrate() bool {
	orchestrateMu.Lock()
	defer orchestrateMu.Unlock()
	if orchestrateInProgress {
		return false
	}
	orchestrateInProgress = true
	return true
}

func endOrchestrate() {
	orchestrateMu.Lock()
	orchestrateInProgress = false
	orchestrateMu.Unlock()
}

// orchestrateTrigger 节流闸门 + 编排主体（推送/轮询/手动共用）。
func (u *Updater) orchestrateTrigger(ctx context.Context, force bool, m *SetupManifest) error {
	u.platform()
	if !beginOrchestrate() {
		u.log().Info("update: orchestration already in progress; skipping trigger")
		return nil
	}
	defer endOrchestrate()

	// 串行化（§9.3）：pending 存在时拒绝再次触发。
	if p, ok := LoadPending(u.StateDir); ok {
		u.log().Info("update: pending exists; refusing new trigger",
			"from", p.From, "to", p.To)
		return nil
	}
	switch {
	case m.Version == "" || m.Version == u.Version:
		return nil
	case CompareVersions(m.Version, u.Version) <= 0:
		// 降级保护（§9.5）：常规路径拒绝旧版本（看门狗/回滚直执不受限）。
		u.log().Info("update: target not newer; skipping",
			"target", m.Version, "self", u.Version)
		return nil
	}
	if reason, bad := Blacklisted(u.StateDir, m.Version); bad {
		u.log().Info("update: version blacklisted until a new version appears",
			"version", m.Version, "reason", reason)
		return nil
	}
	if !force {
		if wait, ok := Throttled(u.StateDir, m.Version, u.clock()); ok {
			u.log().Info("update: failure backoff", "version", m.Version,
				"retryIn", wait.Round(time.Second))
			return nil
		}
	}
	return u.applyUpdate(ctx, m)
}

// applyUpdate §9.2/§9.3 主体：下载校验 → 回滚源确认 → pending+看门狗 →
// 执行安装器。
func (u *Updater) applyUpdate(ctx context.Context, m *SetupManifest) error {
	// 1. 下载到 staging（原子：tmp+rename；重复下载幂等覆盖）。
	staged, err := u.download(ctx, m)
	if err != nil {
		u.recordFailure(m.Version, err)
		return err
	}
	// 2. sha256 校验（信任根；失败入黑名单，§9.1）。
	sum, err := fileSHA256(staged)
	if err != nil {
		u.recordFailure(m.Version, err)
		return err
	}
	if !hashEqual(sum, m.SHA256) {
		_ = os.Remove(staged)
		reason := fmt.Sprintf("sha256 mismatch (manifest=%s got=%s)", m.SHA256, sum)
		BlacklistVersion(u.StateDir, m.Version, reason)
		u.recordFailure(m.Version, errors.New(reason))
		u.log().Warn("update: "+reason, "version", m.Version)
		return errors.New(reason)
	}
	// 3. 回滚源确认（§9.2 步骤 3）：installer-cache 必须有当前版本安装器。
	if err := u.ensureRollbackSource(ctx, m); err != nil {
		u.log().Warn("update: rollback source unavailable; skipping update", "err", err)
		u.recordFailure(m.Version, err)
		return err
	}
	// 3b. 回滚源入 stash：新安装器的 post 脚本会把 installer-cache 顶层
	// 修剪成仅剩新版本（T6 keep-1），自检失败时 from 版安装器已不在顶层。
	// 执行前把 from 安装器备份到 installer-cache\rollback\（安装器的修剪
	// 只扫顶层 *.exe，子目录不受影响——真机发现，2026-09-03）。
	if err := u.stashRollbackSource(); err != nil {
		u.log().Warn("update: rollback stash copy failed; continuing", "err", err)
	}
	// 4. pending 标记 + 一次性看门狗（先于执行，§9.3 步骤 1）。
	p := &PendingUpdate{From: u.Version, To: m.Version,
		StartedAt: u.clock().UTC(),
		Deadline:  u.clock().Add(u.pendingWindow()).UTC()}
	if err := SavePending(u.StateDir, p); err != nil {
		return err
	}
	runAt := p.Deadline.Add(WatchdogRestartGrace)
	if err := u.registerWatchdogTask(runAt); err != nil {
		// 无看门狗不执行更新（回滚兜底缺失）。
		_ = DeletePending(u.StateDir)
		u.recordFailure(m.Version, err)
		return fmt.Errorf("register watchdog: %w", err)
	}
	// 5. SYSTEM 执行（§9.3 步骤 2）。真安装器会在 PrepareToInstall 停
	// XNCAgent——本进程随之消亡，代码不再返回；安装器失败/挂起而 agent
	// 存活时在此收尾（直接回滚，与看门狗等效且更快）。
	u.log().Info("update: executing installer", "installer", staged,
		"from", p.From, "to", p.To, "dir", u.installDir())
	err = u.execInstaller(staged)
	if err == nil {
		// 未停 agent 的路径（假安装器/异常）：真流程由新 agent 自检收尾。
		u.log().Info("update: installer exited 0; awaiting new-agent self-check",
			"to", p.To)
		return nil
	}
	u.log().Warn("update: installer failed", "err", err)
	BlacklistVersion(u.StateDir, m.Version, "installer failed: "+err.Error())
	if rbErr := u.Rollback(p, "installer failed: "+err.Error()); rbErr != nil {
		return fmt.Errorf("%w (rollback incomplete: %v)", err, rbErr)
	}
	// 回滚成功也要向触发方报告更新失败（手动升级路径需感知）。
	return fmt.Errorf("update failed and rolled back: %w", err)
}

// Rollback §9.4 回滚：静默原地重跑 installer-cache 中的 from 版本安装器
// （顶层 → rollback 子目录 stash → staging 兜底）。
//
// 顺序（评审修复，round 1）：先做回滚源查找——三处皆缺时**保留 pending
// 与看门狗**（只 audit + 黑名单 + 报错）：pending 在，StartupPendingCheck
// 的 24h 兜底会在之后的每次成功启动重试回滚，一旦源重新出现（人工修复
// 安装/操作员补拷缓存）即恢复；先删标记则节点永远停在坏版本且无人知晓。
// 源找到才删 pending+看门狗（防回滚后的旧 agent 再次触发回滚），再执行；
// audit 尽力在线发送，离线落标记由下次连接补报；版本入黑名单。
func (u *Updater) Rollback(p *PendingUpdate, reason string) error {
	u.audit(&proto.UpdateAudit{Event: proto.UpdateEventRollback,
		From: p.From, To: p.To, Reason: reason})
	BlacklistVersion(u.StateDir, p.To, "rolled back: "+reason)
	src := CachedInstaller(u.StateDir, p.From)
	if src == "" {
		src = CachedRollbackInstaller(u.StateDir, p.From)
	}
	if src == "" {
		src = StagedInstaller(u.StateDir, p.From)
	}
	if src == "" {
		u.log().Error("update: rollback source missing; keeping pending for backstop retry",
			"from", p.From, "to", p.To)
		return fmt.Errorf("rollback: no cached installer for %s", p.From)
	}
	_ = DeletePending(u.StateDir)
	u.deleteWatchdogTask()
	u.log().Warn("update: rolling back by re-running cached installer",
		"from", p.From, "brokenTo", p.To, "installer", src, "reason", reason)
	return u.execInstaller(src)
}

// audit 发送 UPDATE_AUDIT；不在线（Report nil/失败）则写延迟标记。
func (u *Updater) audit(a *proto.UpdateAudit) {
	m, err := proto.NewMsg(proto.TypeUpdateAudit, a)
	if err != nil {
		return
	}
	if u.Report != nil && u.Report(m) == nil {
		return
	}
	b, _ := json.Marshal(a)
	_ = writeAtomic(filepath.Join(u.StateDir, auditName), b)
}

// ConsumeAuditMarker 读取并删除延迟审计标记（无则 ok=false；一次性，
// 避免重连重复上报——吸收自 bundle 期 failed-marker 模式）。
func ConsumeAuditMarker(stateDir string) (proto.UpdateAudit, bool) {
	var a proto.UpdateAudit
	b, err := os.ReadFile(filepath.Join(stateDir, auditName))
	if err != nil {
		return a, false
	}
	_ = os.Remove(filepath.Join(stateDir, auditName))
	if json.Unmarshal(b, &a) != nil {
		return proto.UpdateAudit{}, false
	}
	return a, true
}

// SelfCheck 新 agent 启动自检收尾（§9.4；agent 在控制连接 OnReady 调用
// ——WS 可达已由 OnReady 证明）。仅当 pending.to == 自报版本时有动作：
// 服务健康（XNCCore Running/StartPending 容忍）→ 删 pending+看门狗、
// installer-cache 刷新为 to 版本、退避清零、audit update_ok；不健康 →
// 主动回滚（不等看门狗）。
func (u *Updater) SelfCheck(ctx context.Context) {
	u.platform()
	p, ok := LoadPending(u.StateDir)
	if !ok || p.To != u.Version {
		return // 非"待收尾的新版本"形态：StartupPendingCheck 已处理
	}
	u.log().Info("update: self-check starting", "from", p.From, "to", p.To)
	deadline := u.clock().Add(u.healthWindow())
	for {
		if err := u.serviceHealthy(); err == nil {
			u.finalize(p)
			return
		}
		if u.clock().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
			return // 周期被打断（rebind）：下一周期 OnReady 重检
		case <-time.After(2 * time.Second):
		}
	}
	if ctx.Err() != nil {
		return
	}
	u.log().Warn("update: self-check failed (services unhealthy); rolling back")
	u.Rollback(p, "self-check: services unhealthy")
}

// finalize 成功收尾。缓存刷新尽力而为：安装器 post 脚本本就会把执行中
// 的 setup.exe 写入 installer-cache（T6 契约），这里再从 staging 兜底拷
// 一次；失败仅记日志（下一次更新的回滚源确认会再尝试补）。
func (u *Updater) finalize(p *PendingUpdate) {
	if staged := StagedInstaller(u.StateDir, p.To); staged != "" {
		if err := CopyToCache(u.StateDir, staged); err != nil {
			u.log().Warn("update: cache refresh from staging failed", "err", err)
		}
	}
	if _, err := os.Stat(filepath.Join(u.StateDir, cacheName, cacheInstallerName(p.To, u.Channel))); err != nil {
		u.log().Warn("update: installer-cache missing new version after finalize",
			"version", p.To, "err", err)
	}
	_ = DeletePending(u.StateDir)
	u.deleteWatchdogTask()
	clearRollbackStash(u.StateDir)
	ClearThrottle(u.StateDir, p.To)
	u.log().Info("update: complete", "from", p.From, "to", p.To)
	u.audit(&proto.UpdateAudit{Event: proto.UpdateEventOK, From: p.From, To: p.To})
}

// StartupPendingCheck agent 启动时的 pending 巡检（§9.4 看门狗不可靠的
// 兜底 + 版本一致性）：pending 超 24h 未清 → 回滚；自报既非 to 也非
// from → 版本一致性失败，回滚（§9.6）；from==自报（新 agent 未上线）且
// 未过期 → 交给看门狗；to==自报 → 交给 OnReady 自检。
func (u *Updater) StartupPendingCheck(ctx context.Context) {
	u.platform()
	p, ok := LoadPending(u.StateDir)
	if !ok {
		return
	}
	if u.clock().Sub(p.StartedAt) > PendingMaxAge {
		u.log().Warn("update: pending expired (>24h); rolling back (watchdog backstop)",
			"from", p.From, "to", p.To,
			"age", u.clock().Sub(p.StartedAt).Round(time.Minute))
		u.Rollback(p, "pending expired >24h (watchdog unavailable)")
		return
	}
	if p.To != u.Version && p.From != u.Version {
		u.log().Warn("update: self version matches neither pending.from nor pending.to; rolling back",
			"self", u.Version, "from", p.From, "to", p.To)
		u.Rollback(p, "version mismatch: self is neither pending.from nor pending.to")
	}
}

// ---- 下载与清单（§9.2）----

// FetchManifest 拉取 /setup.json?channel=<channel>。
func (u *Updater) FetchManifest(ctx context.Context) (*SetupManifest, error) {
	uurl := strings.TrimRight(u.ServerURL, "/") + "/setup.json?channel=" + u.channel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uurl, nil)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("setup.json: HTTP %d", resp.StatusCode)
	}
	var m SetupManifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("setup.json: %w", err)
	}
	return &m, nil
}

func (u *Updater) channel() string {
	if u.Channel == "" {
		return "stable"
	}
	return u.Channel
}

// download 按 manifest 下载安装器到 staging\<名字>（原子覆盖）。URL 相对
// 路径拼 ServerURL；绝对路径必须与 ServerURL 同源（T1 契约）。
func (u *Updater) download(ctx context.Context, m *SetupManifest) (string, error) {
	uurl, err := u.resolveURL(m.URL)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uurl, nil)
	if err != nil {
		return "", err
	}
	hc := &http.Client{Timeout: 10 * time.Minute}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	dst := filepath.Join(u.StateDir, stagingName, cacheInstallerName(m.Version, u.channel()))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := writeAtomicFromReader(dst, io.LimitReader(resp.Body, downloadLimit)); err != nil {
		return "", err
	}
	return dst, nil
}

// resolveURL 相对路径 → ServerURL 拼接；绝对路径须同源。
func (u *Updater) resolveURL(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("empty download url")
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return strings.TrimRight(u.ServerURL, "/") + raw, nil
	}
	srv, err := url.Parse(u.ServerURL)
	if err != nil {
		return "", err
	}
	got, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(got.Host, srv.Host) {
		return "", fmt.Errorf("download url %q is not same-origin with server %q", raw, u.ServerURL)
	}
	return raw, nil
}

// ensureRollbackSource §9.2 步骤 3：installer-cache 必须有当前版本的
// setup.exe。缺失则依次：staging 里找同版本 → 同频道 setup.json 仍供应
// 当前版本则下载补拷；server 不再供应（已是更新版本）→ 报错（调用方
// 放弃本次更新）。
func (u *Updater) ensureRollbackSource(ctx context.Context, target *SetupManifest) error {
	if CachedInstaller(u.StateDir, u.Version) != "" {
		return nil
	}
	if staged := StagedInstaller(u.StateDir, u.Version); staged != "" {
		return CopyToCache(u.StateDir, staged)
	}
	m, err := u.FetchManifest(ctx)
	if err != nil {
		return err
	}
	if m.Version != u.Version {
		return fmt.Errorf("rollback source missing: server no longer serves installer for current version %s (latest=%s)",
			u.Version, m.Version)
	}
	staged, err := u.download(ctx, m)
	if err != nil {
		return err
	}
	sum, err := fileSHA256(staged)
	if err != nil || !hashEqual(sum, m.SHA256) {
		return fmt.Errorf("rollback source download sha256 mismatch")
	}
	return CopyToCache(u.StateDir, staged)
}

// ---- 安装器文件定位（T6 命名契约：xnc-setup[-dev]-<version>.exe）----

// cacheInstallerName staging/缓存安装器文件名（与安装器
// OutputBaseFilename 一致：dev 频道带 -dev 后缀）。
func cacheInstallerName(version, channel string) string {
	if channel == "dev" {
		return "xnc-setup-dev-" + version + ".exe"
	}
	return "xnc-setup-" + version + ".exe"
}

// findInstaller 在 dir 中查找 version 的安装器（精确候选 + "-<version>.exe"
// 后缀匹配，容忍未来频道后缀）。
func findInstaller(dir, version string) string {
	cands := []string{
		cacheInstallerName(version, "stable"),
		cacheInstallerName(version, "dev"),
	}
	for _, c := range cands {
		if _, err := os.Stat(filepath.Join(dir, c)); err == nil {
			return filepath.Join(dir, c)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	suffix := "-" + version + ".exe"
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), strings.ToLower(suffix)) {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

// CachedInstaller installer-cache 顶层中 version 的安装器路径（无则空串）。
func CachedInstaller(stateDir, version string) string {
	return findInstaller(filepath.Join(stateDir, cacheName), version)
}

// CachedRollbackInstaller installer-cache\rollback\ stash 中 version 的
// 安装器路径（无则空串）。
func CachedRollbackInstaller(stateDir, version string) string {
	return findInstaller(filepath.Join(stateDir, cacheName, rollbackName), version)
}

// stashRollbackSource 把 from（当前）版本的安装器备份进
// installer-cache\rollback\（执行新安装器前调用；ensureRollbackSource 已
// 保证顶层存在）。只保留 stash 中这一份。
func (u *Updater) stashRollbackSource() error {
	src := CachedInstaller(u.StateDir, u.Version)
	if src == "" {
		return fmt.Errorf("no top-level installer for %s to stash", u.Version)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	dir := filepath.Join(u.StateDir, cacheName, rollbackName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, filepath.Base(src)), b)
}

// clearRollbackStash 自检成功后清除 stash（下次更新重新入栈）。
func clearRollbackStash(stateDir string) {
	_ = os.RemoveAll(filepath.Join(stateDir, cacheName, rollbackName))
}

// StagedInstaller staging 中 version 的安装器路径（无则空串）。
func StagedInstaller(stateDir, version string) string {
	return findInstaller(filepath.Join(stateDir, stagingName), version)
}

// CopyToCache 把安装器拷入 installer-cache 并只保留这一份（§9.2 仅 1 份；
// 同路径拷贝幂等跳过——回滚原地执行时源即目的）。
func CopyToCache(stateDir, src string) error {
	cache := filepath.Join(stateDir, cacheName)
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(cache, filepath.Base(src))
	if same, _ := samePath(src, dst); !same {
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := writeAtomic(dst, b); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		return nil
	}
	keep := strings.ToLower(filepath.Base(src))
	for _, e := range entries {
		if !e.IsDir() && strings.ToLower(e.Name()) != keep {
			_ = os.Remove(filepath.Join(cache, e.Name()))
		}
	}
	return nil
}

// ---- pending / 原子写 / 哈希 ----

// PendingPath update-pending.json 完整路径。
func PendingPath(stateDir string) string { return filepath.Join(stateDir, pendingName) }

// LoadPending 读取 pending 标记（不存在返回 false）。
func LoadPending(stateDir string) (*PendingUpdate, bool) {
	b, err := os.ReadFile(PendingPath(stateDir))
	if err != nil {
		return nil, false
	}
	var p PendingUpdate
	if json.Unmarshal(b, &p) != nil || p.To == "" {
		return nil, false
	}
	return &p, true
}

// SavePending 原子写 pending（tmp+rename）。
func SavePending(stateDir string, p *PendingUpdate) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(PendingPath(stateDir), b)
}

// DeletePending 删除 pending 标记（不存在视为成功）。
func DeletePending(stateDir string) error {
	err := os.Remove(PendingPath(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// writeAtomic 同目录 tmp + rename 顶替（binding.Save 同一约定）。
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

// writeAtomicFromReader 流式原子写（下载大文件不驻留内存两份）。
func writeAtomicFromReader(path string, r io.Reader) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashEqual 大小写无关的哈希比较（setup.json 为小写，防御大写）。
func hashEqual(got, want string) bool {
	return strings.EqualFold(got, strings.TrimSpace(want))
}

// CleanupStale 启动清扫：bundle 期 update-staging-* 目录与 staging 下载
// tmp 残留。staging 中已下载的安装器保留——自检收尾还要用它刷新
// installer-cache。
func CleanupStale(stateDir string) {
	matches, _ := filepath.Glob(filepath.Join(stateDir, "update-staging-*"))
	for _, m := range matches {
		_ = os.RemoveAll(m)
	}
	tmps, _ := filepath.Glob(filepath.Join(stateDir, stagingName, "*.tmp-*"))
	for _, m := range tmps {
		_ = os.Remove(m)
	}
}

// ---- 节流与黑名单（§9.1）----

type throttleFailure struct {
	Fails   int       `json:"fails"`
	NextTry time.Time `json:"nextTry"`
}

type throttleState struct {
	Failures  map[string]throttleFailure `json:"failures"`
	Blacklist map[string]string          `json:"blacklist"`
}

func loadThrottle(stateDir string) *throttleState {
	st := &throttleState{Failures: map[string]throttleFailure{}, Blacklist: map[string]string{}}
	b, err := os.ReadFile(filepath.Join(stateDir, throttleName))
	if err == nil {
		_ = json.Unmarshal(b, st)
	}
	if st.Failures == nil {
		st.Failures = map[string]throttleFailure{}
	}
	if st.Blacklist == nil {
		st.Blacklist = map[string]string{}
	}
	return st
}

func saveThrottle(stateDir string, st *throttleState) {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	_ = writeAtomic(filepath.Join(stateDir, throttleName), b)
}

// recordFailure 记一次失败：退避 1h→4h→24h（成功清零见 ClearThrottle）。
func (u *Updater) recordFailure(version string, err error) {
	if version == "" {
		return // 清单拉取失败等无版本场景：不节流（无键可节流）
	}
	st := loadThrottle(u.StateDir)
	f := st.Failures[version]
	f.Fails++
	step := backoffSchedule[len(backoffSchedule)-1]
	if f.Fails <= len(backoffSchedule) {
		step = backoffSchedule[f.Fails-1]
	}
	f.NextTry = u.clock().Add(step)
	st.Failures[version] = f
	// 只留最近 8 个版本的失败记录（防长期膨胀）。
	if len(st.Failures) > 8 {
		var oldest string
		var oldestT time.Time
		for v, ff := range st.Failures {
			if oldest == "" || ff.NextTry.Before(oldestT) {
				oldest, oldestT = v, ff.NextTry
			}
		}
		delete(st.Failures, oldest)
	}
	saveThrottle(u.StateDir, st)
	u.log().Info("update: failure recorded", "version", version,
		"fails", f.Fails, "retryIn", step)
}

// Throttled 版本是否在退避期内（true = 跳过，wait = 剩余等待）。
func Throttled(stateDir, version string, now time.Time) (time.Duration, bool) {
	f, ok := loadThrottle(stateDir).Failures[version]
	if !ok {
		return 0, false
	}
	if wait := f.NextTry.Sub(now); wait > 0 {
		return wait, true
	}
	return 0, false
}

// ClearThrottle 成功后清零该版本的退避记录。
func ClearThrottle(stateDir, version string) {
	st := loadThrottle(stateDir)
	if _, ok := st.Failures[version]; !ok {
		return
	}
	delete(st.Failures, version)
	saveThrottle(stateDir, st)
}

// Blacklisted 版本是否在黑名单（sha256 不符/安装器退出码非 0/回滚，
// §9.1——直至 setup.json 出现新 version，新版本天然不在表内）。
func Blacklisted(stateDir, version string) (string, bool) {
	reason, ok := loadThrottle(stateDir).Blacklist[version]
	return reason, ok
}

// BlacklistVersion 加入黑名单。
func BlacklistVersion(stateDir, version, reason string) {
	st := loadThrottle(stateDir)
	st.Blacklist[version] = reason
	saveThrottle(stateDir, st)
}

// ---- 版本比较（§9.5/§9.6）----

// CompareVersions 点分数字版本比较（无第三方依赖的最小实现）：
//   - 逐段数值比较，缺段补零（"0.6" == "0.6.0"）；
//   - 破折号后缀 = 预发布，同数字时更低（"0.6.2-dev" < "0.6.2"）；
//   - 双方都有后缀时后缀按字典序；
//   - 非数字段按字典序兜底。
//
// 返回 -1/0/1。
func CompareVersions(a, b string) int {
	an, as := splitVersion(a)
	bn, bs := splitVersion(b)
	n := max(len(an), len(bn))
	for i := 0; i < n; i++ {
		// 缺段补零（"0.6" == "0.6.0"）。
		x, y := "0", "0"
		if i < len(an) {
			x = an[i]
		}
		if i < len(bn) {
			y = bn[i]
		}
		if c := compareNumericOrLex(x, y); c != 0 {
			return c
		}
	}
	// 数字相同：无后缀 > 有后缀；双后缀字典序。
	switch {
	case as == "" && bs == "":
		return 0
	case as == "":
		return 1
	case bs == "":
		return -1
	default:
		return strings.Compare(as, bs)
	}
}

func splitVersion(v string) (nums []string, suffix string) {
	if i := strings.IndexByte(v, '-'); i >= 0 {
		return strings.Split(v[:i], "."), v[i+1:]
	}
	return strings.Split(v, "."), ""
}

func compareNumericOrLex(a, b string) int {
	if isNumeric(a) && isNumeric(b) {
		return compareIntStrings(a, b)
	}
	if isNumeric(a) {
		return -1 // 数字段 < 非数字段（确定性兜底）
	}
	if isNumeric(b) {
		return 1
	}
	return strings.Compare(a, b)
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func compareIntStrings(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
