# run-network-matrix.ps1 - M3 Task 6: TURN/TCP congestion E2E network matrix.
#
# Runs the three relay-shaped constraint profiles (15Mbps/30ms, 5Mbps/100ms,
# 1Mbps/250ms) as deterministic loopback cases driven by SYNTHETIC feedback
# injection:
#
#   harness = tools/e2eviewer TestNetworkMatrixCase (a normal go test, so it
#   runs in CI too). Per case it wires the REAL desktop Handler (session-WS
#   signaling + streamQoS decision loop), REAL Publisher/ViewerSender (Pion
#   send side), and REAL viewer PeerConnections x2 (controller + spectator)
#   over loopback host candidates, then injects viewer_feedback frames shaped
#   to the profile (same 1s cadence / same field shape as web DesktopLive).
#
# WHY loopback/synthetic instead of OS-level shaping (netsh qos / throttling
# proxy), per the task ruling:
#   - This box is an RDP session; netsh qos requires the QoS Packet Scheduler
#     group policy and admin elevation, and its filters do not reliably apply
#     to loopback traffic at all - flaky by construction here.
#   - The synthetic mode exercises the QoS decision loop deterministically
#     (congestion ladders, spectator pause, IDR-first recovery) with zero OS
#     dependencies; per-profile assertions are identical to the real runs.
#   - Real TURN internet runs of this same matrix are M4's scope (per the
#     milestone ruling); the e2eviewer binary carries matching --fb-bps /
#     --fb-queue-ms / --fb-rtt-ms flags for those runs.
#
# Per-case assertions (from the ruling): queue age hard max 100ms; controller
# stays LIVE when a spectator is throttled (only the spectator pauses); every
# recovery begins with an IDR. Reports land in -ReportDir as
# report-<case>.json plus per-case go test logs.
#
# Windows PowerShell 5.1 compatible (no pwsh-only syntax).
#
# Usage:
#   powershell -ExecutionPolicy Bypass -File scripts\desktop-media\run-network-matrix.ps1
#   powershell ... -ReportDir D:\somewhere           # override report location
#   powershell ... -Cases "1mbps-250ms"              # run a single case

param(
    [string]$ReportDir = "",
    [string]$Cases = "",
    [int]$GoTestTimeoutSec = 180
)

$ErrorActionPreference = "Continue"

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$repoRoot = Resolve-Path (Join-Path $scriptDir "..\..")
$e2eDir = Join-Path $repoRoot "tools\e2eviewer"

if ($ReportDir -eq "") {
    $ReportDir = Join-Path $repoRoot ("network-matrix-reports\" + (Get-Date -Format "yyyyMMdd-HHmmss"))
}
if (-not (Test-Path $ReportDir)) {
    New-Item -ItemType Directory -Force -Path $ReportDir | Out-Null
}
$ReportDir = (Resolve-Path $ReportDir).Path

# The three relay-shaped profiles (mirrors networkMatrixCases in
# tools/e2eviewer/main_test.go).
$allCases = @("15mbps-30ms", "5mbps-100ms", "1mbps-250ms")
if ($Cases -ne "") {
    $caseList = $Cases -split "," | ForEach-Object { $_.Trim() } | Where-Object { $_ -ne "" }
} else {
    $caseList = $allCases
}

Write-Host "=== XNC desktop-media network matrix ==="
Write-Host ("mode      : loopback + synthetic viewer_feedback (deterministic; see script header)")
Write-Host ("repo      : {0}" -f $repoRoot)
Write-Host ("reports   : {0}" -f $ReportDir)
Write-Host ("cases     : {0}" -f ($caseList -join ", "))

# go must be on PATH.
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Host "FAIL: go not found on PATH" -ForegroundColor Red
    exit 1
}

$goTool = Get-Command go
Write-Host ("go        : {0} ({1})" -f $goTool.Source, (go version))

$env:XNC_NET_REPORT_DIR = $ReportDir
$failedCases = @()

foreach ($case in $caseList) {
    Write-Host ""
    Write-Host ("--- case {0} ---" -f $case) -ForegroundColor Cyan
    $env:XNC_NET_CASE = $case
    $logPath = Join-Path $ReportDir ("log-{0}.txt" -f $case)
    Push-Location $e2eDir
    try {
        # 2>&1 keeps build errors in the log; exit code surfaces via $LASTEXITCODE.
        go test ./... -run "TestNetworkMatrixCase" -count=1 -v -timeout ("{0}s" -f $GoTestTimeoutSec) 2>&1 |
            Tee-Object -FilePath $logPath | ForEach-Object { "$_" }
    } finally {
        Pop-Location
    }
    if ($LASTEXITCODE -ne 0) {
        $failedCases += $case
        Write-Host ("CASE {0}: FAIL (see {1})" -f $case, $logPath) -ForegroundColor Red
    } else {
        Write-Host ("CASE {0}: PASS" -f $case) -ForegroundColor Green
    }
}

Remove-Item Env:XNC_NET_CASE -ErrorAction SilentlyContinue
Remove-Item Env:XNC_NET_REPORT_DIR -ErrorAction SilentlyContinue

# ---- summary from the per-case report JSONs ----
Write-Host ""
Write-Host "=== matrix summary ==="
$summaryRows = @()
foreach ($case in $caseList) {
    $row = [ordered]@{
        case = $case; result = "MISSING"; ctrlFrames = 0; specFrames = 0
        queueAgeMaxMs = ""; pausedMs = ""; recoveryViolations = ""; configs = 0
    }
    $repPath = Join-Path $ReportDir ("report-{0}.json" -f $case)
    if (Test-Path $repPath) {
        try {
            $rep = Get-Content -Raw $repPath | ConvertFrom-Json
            $row.result = "FAIL"
            if ($rep.passed) { $row.result = "PASS" }
            if ($null -ne $rep.controller) { $row.ctrlFrames = $rep.controller.frames }
            if ($null -ne $rep.spectator) { $row.specFrames = $rep.spectator.frames }
            if ($null -ne $rep.controller.queueAgeMaxMs) {
                $row.queueAgeMaxMs = "{0:F1}" -f [double]$rep.controller.queueAgeMaxMs
            }
            if ($null -ne $rep.spectator.pausedMs) { $row.pausedMs = $rep.spectator.pausedMs }
            if ($null -ne $rep.controller.recoveryViolations) {
                $row.recoveryViolations = $rep.controller.recoveryViolations
            }
            if ($null -ne $rep.videoConfigs) { $row.configs = @($rep.videoConfigs).Count }
        } catch {
            $row.result = "BADJSON: $($_.Exception.Message)"
        }
    }
    $summaryRows += [pscustomobject]$row
}
$summaryRows | Format-Table case, result, ctrlFrames, specFrames, queueAgeMaxMs, pausedMs, recoveryViolations, configs -AutoSize

if ($failedCases.Count -gt 0) {
    Write-Host ("matrix result: FAIL ({0})" -f ($failedCases -join ", ")) -ForegroundColor Red
    exit 1
}
Write-Host "matrix result: PASS (all cases green)" -ForegroundColor Green
exit 0
