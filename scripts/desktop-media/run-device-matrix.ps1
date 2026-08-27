# run-device-matrix.ps1 - M4 Task 1: the device matrix runner.
#
# Enumerates machine rows from a -Machines JSON (or a built-in default) and
# produces one RunResult per row under artifacts\desktop-media\<ts>\<row>\:
#
#   mode "local"       always executed: delegates to run-soak.ps1 (bounded
#                      diag + viewer + metrics + from-diag + verify), so the
#                      localhost row carries FULL evidence, not a stub.
#   mode "winrm"       documented only unless -ExecuteRemote; with it, a
#                      best-effort Invoke-Command runs the console-diag on
#                      the remote checkout and fetches the stats.json sidecar
#                      back for conversion (diag-level stub: no receive-side
#                      evidence - counters/percentiles only, never pixels).
#   mode "unavailable" recorded as a stub row with Verdict=UNAVAILABLE.
#
# Rows merge into one Markdown table (desktopreport merge). Stub rows carry
# NO measurements (only Host + Verdict) and are never verified - a gate can
# only pass on real evidence.
#
# Windows PowerShell 5.1 compatible (no ternary / ?? / &&).
#
# Usage:
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\desktop-media\run-device-matrix.ps1 -Plan
#   powershell ... -Machines C:\machines.json              # custom rows
#   powershell ... -ExecuteRemote                          # also run winrm rows
#
# Exit codes: 0 = every EXECUTED row verified PASS; 1 = an executed row
# failed verify (stub rows never fail the matrix); 2 = harness error.

param(
    [string]$Machines = "",
    [switch]$ExecuteRemote,
    [switch]$Plan,
    [string]$RunRoot = "",
    [int]$DiagSec = 60,
    [int]$Fps = 30,
    [ValidateSet("v1", "v2")]
    [string]$Pipeline = "v2"
)

$ErrorActionPreference = "Continue"

function Fail([string]$msg) {
    Write-Host "run-device-matrix: $msg" -ForegroundColor Red
    exit 2
}

function Quote-Arg([string]$a) {
    if ($a -match " ") { return '"' + $a + '"' }
    return $a
}

function Render-Cmd([string]$exe, [string[]]$argList) {
    $s = $exe
    foreach ($a in $argList) { $s = $s + " " + (Quote-Arg $a) }
    return $s
}

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$repoRoot = (Resolve-Path (Join-Path $scriptDir "..\..")).Path
$soakScript = Join-Path $scriptDir "run-soak.ps1"
$artifactsRoot = Join-Path $repoRoot "artifacts\desktop-media"

# Built-in default rows (ruling 3: localhost always; labs-xiaoxin documented;
# NVIDIA/AMD rows recorded UNAVAILABLE - no such machines exist here).
$defaultMachinesJson = @'
{
  "machines": [
    { "name": "localhost", "mode": "local", "diagSec": 60, "fps": 30, "pipeline": "v2",
      "note": "Intel hybrid; RDP session -> software rung (M2 finding)" },
    { "name": "labs-xiaoxin", "mode": "winrm", "user": "labs",
      "repoPath": "C:\\Users\\labs\\Desktop\\XNC", "diagSec": 60, "fps": 30, "pipeline": "v2",
      "note": "console session; real QSV attempt; WinRM, run with -ExecuteRemote" },
    { "name": "nvidia-row", "mode": "unavailable",
      "note": "no NVIDIA machine in this environment" },
    { "name": "amd-row", "mode": "unavailable",
      "note": "no AMD machine in this environment" }
  ]
}
'@

# ---- resolve every output path BEFORE starting anything ----------------------

$ts = Get-Date -Format "yyyyMMdd-HHmmss"
if ($RunRoot -eq "") { $RunRoot = Join-Path $artifactsRoot ("device-matrix-" + $ts) }
$runFull = [System.IO.Path]::GetFullPath($RunRoot)
if (-not $runFull.StartsWith($artifactsRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
    Fail "RunRoot must live under artifacts\desktop-media (got $runFull)"
}
if ($runFull -match " ") {
    Fail "run root contains spaces; the bounded runners pass bare argv ($runFull)"
}
$tablePath = Join-Path $runFull "table.md"
$reportExe = Join-Path $runFull "desktopreport.exe"

# ---- machine rows -------------------------------------------------------------

if ($Machines -ne "") {
    if (-not (Test-Path -LiteralPath $Machines)) { Fail "Machines file not found: $Machines" }
    $machinesJson = Get-Content -Raw -LiteralPath $Machines
} else {
    $machinesJson = $defaultMachinesJson
}
$rows = @()
try {
    $parsed = $machinesJson | ConvertFrom-Json
    foreach ($m in @($parsed.machines)) {
        # PS 5.1: no ?? operator - explicit null fallbacks per field.
        $rowDiag = $DiagSec
        if ($null -ne $m.diagSec) { $rowDiag = [int]$m.diagSec }
        $rowFps = $Fps
        if ($null -ne $m.fps) { $rowFps = [int]$m.fps }
        $rowPipeline = $Pipeline
        if ($null -ne $m.pipeline -and $m.pipeline -ne "") { $rowPipeline = [string]$m.pipeline }
        $rows += @{
            name = [string]$m.name; mode = [string]$m.mode
            user = [string]$m.user; repoPath = [string]$m.repoPath
            diagSec = $rowDiag; fps = $rowFps
            pipeline = $rowPipeline; note = [string]$m.note
        }
    }
} catch {
    Fail "machines JSON invalid: $($_.Exception.Message)"
}

if ($rows.Count -eq 0) { Fail "machines JSON has no rows" }
$seenLocal = $false
foreach ($r in $rows) {
    if ($r.mode -eq "local") { $seenLocal = $true }
}
if (-not $seenLocal) { Fail "machines JSON must include exactly one local row (the mandatory row)" }

# ---- PLAN ----------------------------------------------------------------------

if ($Plan) {
    Write-Host "=== run-device-matrix PLAN (nothing starts, nothing is created) ==="
    Write-Host ("run root : {0}" -f $runFull)
    Write-Host ("table    : {0}" -f $tablePath)
    Write-Host ("remote   : {0}" -f $(if ($ExecuteRemote) { "EXECUTE winrm rows" } else { "winrm rows documented only (add -ExecuteRemote to run)" }))
    Write-Host ""
    $i = 0
    foreach ($r in $rows) {
        $i++
        Write-Host ("--- row {0}: {1} [{2}] {3}" -f $i, $r.name, $r.mode, $r.note)
        switch ($r.mode) {
            "local" {
                Write-Host ("  [execute] powershell -File {0} -DurationSec {1} -Pipeline {2} -Fps {3} -RunDir {4}" -f `
                    $soakScript, $r.diagSec, $r.pipeline, $r.fps, (Join-Path $runFull $r.name))
            }
            "winrm" {
                if ($ExecuteRemote) {
                    Write-Host ("  [execute] Invoke-Command -ComputerName {0} (build.bat + console-diag {1}s, fetch stats.json -> from-diag stub)" -f $r.name, $r.diagSec)
                } else {
                    Write-Host ("  [skip   ] documented only; would Invoke-Command -ComputerName {0} -ScriptBlock {{ build.bat; xnc-desktop.exe --console-diag --fps {1} --duration {2} ... }}" -f $r.name, $r.fps, $r.diagSec)
                }
            }
            "unavailable" {
                Write-Host "  [stub   ] RunResult { Host: $($r.name), Verdict: UNAVAILABLE } (no measurements)"
            }
            default {
                Write-Host ("  [error  ] unknown mode {0}" -f $r.mode) -ForegroundColor Red
            }
        }
    }
    Write-Host ""
    Write-Host ("[build ] go build -o {0} .   (in tools\desktopreport)" -f $reportExe)
    Write-Host ("[run   ] {0}" -f (Render-Cmd $reportExe @("merge", "-out", $tablePath, "<row>\runresult.json ...")))
    exit 0
}

# ---- EXECUTE --------------------------------------------------------------------

if (-not (Get-Command go -ErrorAction SilentlyContinue)) { Fail "go not found on PATH" }
New-Item -ItemType Directory -Force -Path $runFull | Out-Null
$runFull = (Resolve-Path $runFull).Path

# The matrix needs desktopreport itself (remote rows, stub merges) BEFORE
# the local row runs - build it once into the run root.
Push-Location (Join-Path $repoRoot "tools\desktopreport")
try { go build -o $reportExe . } finally { Pop-Location }
if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $reportExe)) { Fail "go build desktopreport failed" }

$rowResults = @()   # runresult/stub JSON paths, rows in order
$failedRows = @()

foreach ($r in $rows) {
    $rowDir = Join-Path $runFull $r.name
    $rowResult = Join-Path $rowDir "runresult.json"
    Write-Host ""
    Write-Host ("--- row {0} [{1}] ---" -f $r.name, $r.mode) -ForegroundColor Cyan

    switch ($r.mode) {
        "local" {
            # Full evidence: the bounded soak runner (diag + viewer events +
            # metrics + from-diag + verify). It manages its own tracked PIDs.
            $soakArgs = @("-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $soakScript,
                          "-DurationSec", "$($r.diagSec)", "-Pipeline", $r.pipeline,
                          "-Fps", "$($r.fps)", "-RunDir", $rowDir)
            Write-Host ("[execute] powershell {0}" -f (Render-Cmd "powershell.exe" $soakArgs))
            & powershell.exe @soakArgs
            $code = $LASTEXITCODE
            if ($code -eq 2) { Fail "local row harness error (run-soak exit 2; see its output above)" }
            if ($code -ne 0) { $failedRows += $r.name }
            if (Test-Path -LiteralPath $rowResult) { $rowResults += $rowResult }
        }
        "winrm" {
            if (-not $ExecuteRemote) {
                Write-Host ("[skip] remote row documented only (add -ExecuteRemote); stub with Verdict=SKIPPED-REMOTE" -f $r.name) -ForegroundColor Yellow
                New-Item -ItemType Directory -Force -Path $rowDir | Out-Null
                $stub = '{{ "Host": "{0}", "Verdict": "SKIPPED-REMOTE", "Browser": "n/a (see row note)" }}' -f $r.name
                [System.IO.File]::WriteAllText($rowResult, $stub)
                $rowResults += $rowResult
            } else {
                # Best-effort remote diag (counters only: the stats.json
                # sidecar travels back as text; the remote h264 stays remote).
                New-Item -ItemType Directory -Force -Path $rowDir | Out-Null
                $remoteScript = {
                    param($repo, $durSec, $fps, $v2)
                    & (Join-Path $repo "native\desktop\build.bat") | Out-Null
                    $out = Join-Path $env:TEMP "xnc-m4-diag.h264"
                    $dargs = @("--console-diag", "--fps", "$fps", "--duration", "$durSec", "--out", $out)
                    if ($v2) { $dargs = @("--desktop-pipeline-v2") + $dargs }
                    $p = Start-Process -FilePath (Join-Path $repo "bin\xnc-desktop.exe") `
                        -ArgumentList $dargs -WindowStyle Hidden -PassThru -Wait
                    $stats = Join-Path $env:TEMP "stats.json"
                    if (Test-Path $stats) { return (Get-Content -Raw $stats) }
                    return "MISSING_STATS"
                }
                try {
                    $statsJson = Invoke-Command -ComputerName $r.name -ScriptBlock $remoteScript `
                        -ArgumentList $r.repoPath, $r.diagSec, $r.fps, ($r.pipeline -eq "v2") -ErrorAction Stop
                    if ($statsJson -eq "MISSING_STATS" -or [string]::IsNullOrWhiteSpace($statsJson)) {
                        throw "remote diag produced no stats.json sidecar"
                    }
                    $statsLocal = Join-Path $rowDir "stats.json"
                    [System.IO.File]::WriteAllText($statsLocal, $statsJson)
                    if (-not (Test-Path -LiteralPath $reportExe)) { Fail "desktopreport.exe missing (build step failed earlier)" }
                    $fromArgs = @("from-diag", "-stats", $statsLocal, "-host", $r.name,
                                  "-browser", "none (remote diag stub)", "-out", $rowResult)
                    Write-Host ("[run] {0}" -f (Render-Cmd $reportExe $fromArgs))
                    & $reportExe @fromArgs
                    if ($LASTEXITCODE -ne 0) { throw "from-diag failed for remote row" }
                    & $reportExe verify $rowResult
                    if ($LASTEXITCODE -ne 0) { $failedRows += $r.name }
                    $rowResults += $rowResult
                } catch {
                    Write-Host ("[unavailable] remote row {0}: {1}" -f $r.name, $_.Exception.Message) -ForegroundColor Yellow
                    $stub = '{{ "Host": "{0}", "Verdict": "UNAVAILABLE" }}' -f $r.name
                    [System.IO.File]::WriteAllText($rowResult, $stub)
                    $rowResults += $rowResult
                }
            }
        }
        "unavailable" {
            New-Item -ItemType Directory -Force -Path $rowDir | Out-Null
            $stub = '{{ "Host": "{0}", "Verdict": "UNAVAILABLE" }}' -f $r.name
            [System.IO.File]::WriteAllText($rowResult, $stub)
            $rowResults += $rowResult
            Write-Host ("[stub] {0}: {1}" -f $r.name, $r.note) -ForegroundColor Yellow
        }
        default {
            Fail "unknown machine mode '$($r.mode)' for row $($r.name)"
        }
    }
}

# ---- merged table ----------------------------------------------------------------

if (-not (Test-Path -LiteralPath $reportExe)) { Fail "desktopreport.exe missing (expected from the local row)" }
& $reportExe merge -out $tablePath @rowResults | Out-Host
Write-Host ""
Write-Host ("merged table : {0}" -f $tablePath)

if ($failedRows.Count -gt 0) {
    Write-Host ("device matrix: FAIL ({0})" -f ($failedRows -join ", ")) -ForegroundColor Red
    exit 1
}
Write-Host "device matrix: executed rows PASS (stub rows recorded, not verified)" -ForegroundColor Green
exit 0
