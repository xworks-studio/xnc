# collect-metrics.ps1 - M4 Task 1: sample CPU% + working set of ONE pid
# into JSON lines (desktopreport from-diag -metrics consumes this shape).
#
#   {"ts":"2026-08-26T10:00:00Z","pid":4242,"cpuPercent":4.2,"workingSetMB":210.5}
#
# cpuPercent is normalized to TOTAL machine capacity: the process's
# TotalProcessorTime delta over wall-clock elapsed, divided by the logical
# processor count (0-100 = whole-box scale, same semantics as
# \Processor(_Total)\% Processor Time). The first sample is the baseline and
# carries cpuPercent 0. workingSetMB is the instantaneous WorkingSet64.
#
# Bounded + safe by construction (harness rules):
#   - stops at -DurationSec or when the target pid exits, whichever first;
#   - it only OBSERVES the process (Get-Process -Id) - it never starts or
#     kills anything;
#   - counters only: no pixel data of any kind.
#
# Windows PowerShell 5.1 compatible (no ternary / ?? / &&).
#
# Usage:
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\desktop-media\collect-metrics.ps1 -TargetPid 4242 -DurationSec 300 -OutFile artifacts\...\metrics.jsonl
#   ... -Plan    # print the exact sampling plan, run nothing

param(
    [Parameter(Mandatory = $true)]
    [int]$TargetPid,
    [int]$IntervalMs = 1000,
    [int]$DurationSec = 300,
    [Parameter(Mandatory = $true)]
    [string]$OutFile,
    [switch]$Plan
)

$ErrorActionPreference = "Stop"

function FailUsage([string]$msg) {
    Write-Host "collect-metrics: $msg" -ForegroundColor Red
    exit 2
}

if ($TargetPid -le 0) { FailUsage "TargetPid must be a positive pid (got $TargetPid)" }
if ($IntervalMs -lt 100) { FailUsage "IntervalMs must be >= 100ms (got $IntervalMs)" }
if ($DurationSec -le 0) { FailUsage "DurationSec must be > 0 (got $DurationSec)" }
if ($OutFile -eq "") { FailUsage "OutFile is required" }

# Resolve the output path BEFORE any sampling: in EXECUTE mode the file's
# directory must already exist (the runner pre-creates the run dir) - no
# New-Item here, so a typo cannot scatter files anywhere unexpected. Plan
# mode only prints, so a not-yet-created directory is fine there.
$outFull = [System.IO.Path]::GetFullPath($OutFile)
$outDir = Split-Path -Parent $outFull
if (-not $Plan -and -not (Test-Path -LiteralPath $outDir)) { FailUsage "output directory does not exist: $outDir (the runner must pre-create the run dir)" }

$cores = [int]$env:NUMBER_OF_PROCESSORS
if ($cores -lt 1) { $cores = 1 }

if ($Plan) {
    Write-Host "=== collect-metrics PLAN (nothing runs) ==="
    Write-Host ("target pid     : {0}" -f $TargetPid)
    Write-Host ("interval       : {0} ms" -f $IntervalMs)
    Write-Host ("duration       : {0} s (bounded; also stops when pid exits)" -f $DurationSec)
    Write-Host ("output         : {0}" -f $outFull)
    Write-Host ("cpu normalized : /{0} logical processors (whole-box scale)" -f $cores)
    Write-Host ("sample line    : {""ts"":<iso8601>,""pid"":$TargetPid,""cpuPercent"":<0-100>,""workingSetMB"":<mb>}")
    exit 0
}

# Baseline: fails fast when the pid is already gone.
$proc = Get-Process -Id $TargetPid -ErrorAction SilentlyContinue
if ($null -eq $proc) { FailUsage "pid $TargetPid not found" }

# WorkingSet64/TotalProcessorTime can throw while the process is exiting;
# observe-only, never fatal.
function Sample-WorkingSetMB($p) {
    try { return [Math]::Round($p.WorkingSet64 / 1MB, 3) } catch { return -1 }
}

$sw = [System.Diagnostics.Stopwatch]::StartNew()
$lastCpuSec = $proc.TotalProcessorTime.TotalSeconds
$lastElapsedSec = 0.0
$samples = 0

Write-Host ("collect-metrics: pid={0} interval={1}ms duration={2}s out={3}" -f $TargetPid, $IntervalMs, $DurationSec, $outFull)

while ($sw.Elapsed.TotalSeconds -lt $DurationSec) {
    $p = Get-Process -Id $TargetPid -ErrorAction SilentlyContinue
    if ($null -eq $p) {
        Write-Host ("collect-metrics: pid {0} exited after {1:F1}s ({2} samples)" -f $TargetPid, $sw.Elapsed.TotalSeconds, $samples)
        break
    }
    $elapsed = $sw.Elapsed.TotalSeconds
    $cpuSec = 0.0
    try { $cpuSec = $p.TotalProcessorTime.TotalSeconds } catch { $cpuSec = $lastCpuSec }
    $cpuPercent = 0.0
    $dt = $elapsed - $lastElapsedSec
    if ($samples -gt 0 -and $dt -gt 0.05) {
        $cpuPercent = [Math]::Round((($cpuSec - $lastCpuSec) / $dt) / $cores * 100.0, 3)
        if ($cpuPercent -lt 0) { $cpuPercent = 0.0 }
        $lastCpuSec = $cpuSec
        $lastElapsedSec = $elapsed
    }
    $ws = Sample-WorkingSetMB $p
    $line = '{{"ts":"{0}","pid":{1},"cpuPercent":{2},"workingSetMB":{3}}}' -f `
        ([DateTime]::UtcNow.ToString("yyyy-MM-ddTHH:mm:ssZ", [System.Globalization.CultureInfo]::InvariantCulture)), `
        $TargetPid, $cpuPercent, $ws
    [System.IO.File]::AppendAllText($outFull, $line + "`n")
    $samples++
    # Bounded sleep: never past the duration deadline.
    $remainMs = [int](($DurationSec - $sw.Elapsed.TotalSeconds) * 1000)
    if ($remainMs -le 0) { break }
    if ($remainMs -gt $IntervalMs) { $remainMs = $IntervalMs }
    Start-Sleep -Milliseconds $remainMs
}

$sw.Stop()
Write-Host ("collect-metrics: done samples={0} elapsed={1:F1}s -> {2}" -f $samples, $sw.Elapsed.TotalSeconds, $outFull)
exit 0
