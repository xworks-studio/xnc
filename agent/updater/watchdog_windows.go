//go:build windows

// watchdog_windows.go — 回滚看门狗（spec §9.4 兜底 1/3）：
//
// agent 在执行安装器前写 StateDir\watchdog.ps1 并注册一次性计划任务
// XNCRollbackWatchdog（SYSTEM，触发 = pending.deadline + 宽限）。触发时：
// pending 已清（新 agent 自检通过）→ 无操作；仍在 → 重启 XNCAgent 给最后
// 一次自检机会；宽限后仍在 → 原地静默重跑 installer-cache 旧版安装器
// （T6 机验契约），并落延迟审计标记（看门狗无控制连接，agent 下次上线
// 补报 UPDATE_AUDIT(update_rollback)）。
//
// 工具选择（记录）：注册用 PowerShell Register-ScheduledTask（DateTime
// 原生、秒级精度、-Force 幂等覆盖；schtasks /SD 的日期格式随区域设置
// 漂移）；删除按控制器裁定用 schtasks /Delete /F（任务不存在视同成功）。
package updater

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// watchdogRestartWaitSeconds 看门狗脚本重启 agent 后的等待秒数（与
// WatchdogRestartGrace 同量级；脚本常量独立于 Go 侧，改动需两处同步）。
const watchdogRestartWaitSeconds = 120

// watchdogScriptPath StateDir 内看门狗脚本路径。
func watchdogScriptPath(stateDir string) string {
	return filepath.Join(stateDir, watchdogFile)
}

// registerWatchdog 写脚本 + 注册一次性任务（runAt 取整到下一秒，避免
// 注册时刻与触发时刻同秒竞态）。
func (u *Updater) registerWatchdog(runAt time.Time) error {
	if err := writeWatchdogScript(u.StateDir, u.installDir()); err != nil {
		return err
	}
	script := watchdogScriptPath(u.StateDir)
	// ISO 8601 字面量 + [datetime] 转换：区域设置无关。必须用 LOCAL 时间——
	// PS 5.1 的 [datetime]'…' 按本机时区解析（真机发现：喂 UTC 墙钟在
	// UTC+8 机器上触发时刻是 8 小时前，一次性任务永不再触发）。
	ps := fmt.Sprintf(
		`$a = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument '-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File "%s"'; `+
			`$t = New-ScheduledTaskTrigger -Once -At ([datetime]'%s'); `+
			`Register-ScheduledTask -TaskName '%s' -Action $a -Trigger $t -User 'SYSTEM' -RunLevel Highest -Force | Out-Null`,
		script, runAt.Local().Add(time.Second).Format("2006-01-02T15:04:05"), WatchdogTask)
	out, err := exec.Command("powershell.exe", "-NoProfile", "-Command", ps).CombinedOutput()
	if err != nil {
		return fmt.Errorf("register watchdog task: %w: %s", err, strings.TrimSpace(string(out)))
	}
	u.log().Info("update: rollback watchdog registered", "task", WatchdogTask,
		"triggerAt", runAt.Format(time.RFC3339))
	return nil
}

// deleteWatchdog 删除看门狗任务（void：不存在/删除失败均视同成功——
// 任务是一次性的，残留不重触发，下一次注册 -Force 覆盖；失败仅记日志）。
func deleteWatchdog() {
	out, err := exec.Command("schtasks", "/Delete", "/TN", WatchdogTask, "/F").CombinedOutput()
	if err != nil {
		slog.Default().Warn("update: watchdog task delete failed (non-fatal)",
			"err", err, "out", strings.TrimSpace(string(out)))
	}
}

// writeWatchdogScript 生成 watchdog.ps1（ASCII，PS 5.1 兼容；参数经烘焙
// 默认值传入——与 xnc.iss 的脚本生成约定一致，绝不 Mandatory 以防隐藏
// 窗口挂起）。
func writeWatchdogScript(stateDir, installDir string) error {
	var b strings.Builder
	b.WriteString("param(\r\n")
	b.WriteString("    [string]$StateDir = '" + escapePSSingleQuoted(stateDir) + "',\r\n")
	b.WriteString("    [string]$InstallDir = '" + escapePSSingleQuoted(installDir) + "'\r\n")
	b.WriteString(")\r\n")
	b.WriteString("$ErrorActionPreference = 'SilentlyContinue'\r\n")
	b.WriteString("function Log([string]$msg) {\r\n")
	b.WriteString("    $log = Join-Path $StateDir 'watchdog.log'\r\n")
	b.WriteString("    \"$(Get-Date -Format s) $msg\" | Out-File -FilePath $log -Append -Encoding ascii\r\n")
	b.WriteString("}\r\n")
	b.WriteString("$pending = Join-Path $StateDir 'update-pending.json'\r\n")
	b.WriteString("if (-not (Test-Path $pending)) { Log 'watchdog: no pending marker; nothing to do'; exit 0 }\r\n")
	b.WriteString("Log 'watchdog: pending marker present; restarting XNCAgent for one last self-check'\r\n")
	b.WriteString("$svc = Get-Service -Name 'XNCAgent' -ErrorAction SilentlyContinue\r\n")
	b.WriteString("if ($svc -and $svc.Status -ne 'Running') { Start-Service -Name 'XNCAgent' -ErrorAction SilentlyContinue }\r\n")
	b.WriteString("Start-Sleep -Seconds " + fmt.Sprint(watchdogRestartWaitSeconds) + "\r\n")
	b.WriteString("if (-not (Test-Path $pending)) { Log 'watchdog: pending cleared after restart; update healthy'; exit 0 }\r\n")
	// —— 仍 pending：更新已死。原地静默重跑缓存旧版安装器（spec 9.4）。
	b.WriteString("$p = Get-Content -Path $pending -Raw | ConvertFrom-Json\r\n")
	b.WriteString("$from = [string]$p.from\r\n")
	b.WriteString("$to = [string]$p.to\r\n")
	b.WriteString("Log \"watchdog: pending survived; rolling back $to -> $from\"\r\n")
	// 延迟审计标记（agent 无连接的失败上报——bundle 期 failed-marker 模式）。
	b.WriteString("$audit = Join-Path $StateDir 'update-audit.json'\r\n")
	b.WriteString("@{ event = 'update_rollback'; from = $from; to = $to; reason = 'watchdog deadline exceeded' } | ConvertTo-Json -Compress | Set-Content -Path $audit -Encoding ascii\r\n")
	b.WriteString("$cache = Join-Path $StateDir 'installer-cache'\r\n")
	// 回滚源查找：顶层（更新从未发生的形态）→ rollback 子目录 stash
	// （新安装器已把顶层修剪为新版本、agent 又没活到自检的形态）。
	// 注意元素必须加括号：@('a'+$v+'b', ...) 不加括号时 PS 的逗号/加号
	// 优先级会把表达式拼坏（真机发现：Test-Path 恒假 → 回滚源丢失）。
	// 源找到才删 pending：三处皆缺时保留标记退出（非 0），24h 启动兜底
	// 会在源重新出现后重试；先删标记则坏版本永远无人收拾。
	b.WriteString("$exe = $null\r\n")
	// 候选含旧命名（xnc-setup-*，0.7.2 及之前）：升级后回滚源可能是
	// 旧 agent 按旧命名缓存的文件，过渡期必须互认（与 Go 侧 findInstaller
	// 的后缀匹配同一语义）。
	b.WriteString("$cands = @(('XNC-Setup-' + $from + '.exe'), ('XNC-Setup-dev-' + $from + '.exe'), ('xnc-setup-' + $from + '.exe'), ('xnc-setup-dev-' + $from + '.exe'))\r\n")
	b.WriteString("foreach ($leaf in $cands) {\r\n")
	b.WriteString("    $t = Join-Path $cache $leaf\r\n")
	b.WriteString("    if (Test-Path $t) { $exe = $t; break }\r\n")
	b.WriteString("}\r\n")
	b.WriteString("if (-not $exe) {\r\n")
	b.WriteString("    $stash = Join-Path $cache 'rollback'\r\n")
	b.WriteString("    foreach ($leaf in $cands) {\r\n")
	b.WriteString("        $t = Join-Path $stash $leaf\r\n")
	b.WriteString("        if (Test-Path $t) { $exe = $t; break }\r\n")
	b.WriteString("    }\r\n")
	b.WriteString("}\r\n")
	b.WriteString("if (-not $exe) { Log \"watchdog: rollback source missing for $from; keeping pending for backstop retry\"; exit 1 }\r\n")
	// 执行前删 pending：回滚后启动的旧 agent 不得再次触发回滚（否则
	// from==to 死循环）。
	b.WriteString("Remove-Item -Path $pending -Force\r\n")
	b.WriteString("Log \"watchdog: executing rollback installer in place: $exe\"\r\n")
	b.WriteString("$proc = Start-Process -FilePath $exe -ArgumentList '/VERYSILENT','/SUPPRESSMSGBOXES','/NORESTART',('/DIR=\"' + $InstallDir + '\"') -WindowStyle Hidden -Wait -PassThru\r\n")
	b.WriteString("Log ('watchdog: rollback installer exit code ' + $proc.ExitCode)\r\n")
	b.WriteString("exit $proc.ExitCode\r\n")
	return os.WriteFile(watchdogScriptPath(stateDir), []byte(b.String()), 0o755)
}

// escapePSSingleQuoted PS 单引号字符串转义（单引号翻倍）。
func escapePSSingleQuoted(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
