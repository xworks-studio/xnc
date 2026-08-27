# run-soak.ps1 - M4 Task 1: the bounded, reviewable soak runner.
#
# One soak = one run directory under artifacts\desktop-media\<timestamp>\
# holding EVERY output (bitstream + stats sidecar + viewer reports + metrics
# JSONL + RunResult + merged table). All paths are resolved BEFORE anything
# starts. The harness moves counters/percentiles only; the H.264 dump exists
# because the console-diag contract requires --out (the stats sidecar is
# written next to it) - it stays under git-ignored artifacts and is never
# read, copied, or committed by this harness.
#
# Topology (all processes Start-Process -WindowStyle Hidden -PassThru; the
# tracked PID list is the ONLY thing ever killed - never by exe name):
#
#   xnc-desktop.exe --console-diag [--desktop-pipeline-v2] --fps F
#                   --duration D --out <run>\capture.h264
#                   --pipe <run pipe> --secret <run secret> --log-file <run>\native.log
#     = capture pipeline + stats.json sidecar + live rt subscribers
#   collect-metrics.ps1 -TargetPid <native pid>   (CPU%/working-set JSONL)
#   e2eviewer.exe -direct-pipe ... per event      (viewer/pli/overflow/reconnect)
#   desktopreport.exe from-diag|verify|merge      (post-processing, in-shell)
#
# Event schedule: -EventSchedule <file.json>
#   {"events":[{"type":"viewer|pli|overflow|reconnect","atSec":N,"durationSec":N},
#              {"type":"overflow","atSec":N,"durationSec":N,"fbQueueMs":250}]}
# or the built-in default (fractions of -DurationSec): a standing viewer for
# the whole run, one PLI event at 20%, one feedback-overflow event at 40%,
# one reconnect (two short viewers with a gap) at 60%. A reconnect event
# runs as TWO sequential viewer processes (connect, drop, reconnect).
# Validation: every event must end inside the soak window and at most 4
# viewers may overlap (the native --max-subs default).
#
# -Pipeline v1 fails fast when XNC_DESKTOP_PIPELINE_V2 is set in the parent
# shell (children inherit it and would silently flip the wire to v2).
#
# Windows PowerShell 5.1 compatible (no ternary / ?? / &&).
#
# Usage:
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\desktop-media\run-soak.ps1 -Plan
#   powershell ... -DurationSec 5400 -Pipeline v2 -Fps 30            # 90min bounded soak
#   powershell ... -DurationSec 600 -EventSchedule C:\sched.json     # custom schedule
#
# Exit codes: 0 = soak verified PASS; 1 = verify FAIL (P0/gate); 2 = harness
# error (missing binary, bad schedule, native failed to start...).

param(
    [int]$DurationSec = 300,
    [ValidateSet("v1", "v2")]
    [string]$Pipeline = "v2",
    [int]$Fps = 30,
    [string]$EventSchedule = "",
    [string]$RunDir = "",
    [switch]$Plan
)

$ErrorActionPreference = "Continue"

# ---- helpers ---------------------------------------------------------------

function Fail([string]$msg) {
    Write-Host "run-soak: $msg" -ForegroundColor Red
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

# ---- locate repo + tools ---------------------------------------------------

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$repoRoot = (Resolve-Path (Join-Path $scriptDir "..\..")).Path
$nativeExe = Join-Path $repoRoot "bin\xnc-desktop.exe"
$e2eDir = Join-Path $repoRoot "tools\e2eviewer"
$reportDir = Join-Path $repoRoot "tools\desktopreport"
$collectScript = Join-Path $scriptDir "collect-metrics.ps1"
$artifactsRoot = Join-Path $repoRoot "artifacts\desktop-media"

if ($DurationSec -le 0) { Fail "DurationSec must be > 0 (got $DurationSec)" }
if ($Fps -le 0) { Fail "Fps must be > 0 (got $Fps)" }
if ($Pipeline -eq "v1" -and $env:XNC_DESKTOP_PIPELINE_V2) {
    Fail "XNC_DESKTOP_PIPELINE_V2 is set in this shell; children would silently run the v2 wire. Clear it for a v1 soak."
}

# ---- resolve EVERY output path BEFORE starting anything ---------------------

$ts = Get-Date -Format "yyyyMMdd-HHmmss"
if ($RunDir -eq "") { $RunDir = Join-Path $artifactsRoot ("soak-" + $ts) }
$runFull = [System.IO.Path]::GetFullPath($RunDir)
if (-not $runFull.StartsWith($artifactsRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
    Fail "RunDir must live under artifacts\desktop-media (got $runFull)"
}
if ($runFull -match " ") {
    Fail "run dir contains spaces; the bounded runners pass bare argv to the native exe ($runFull)"
}

$h264Path      = Join-Path $runFull "capture.h264"
$statsPath     = Join-Path $runFull "stats.json"      # sidecar: written next to --out by the native
$nativeLog     = Join-Path $runFull "native.log"
$nativeStderr  = Join-Path $runFull "native.stderr.log"
$metricsPath   = Join-Path $runFull "metrics.jsonl"
$resultPath    = Join-Path $runFull "runresult.json"
$tablePath     = Join-Path $runFull "table.md"
$e2eExe        = Join-Path $runFull "e2eviewer.exe"
$reportExe     = Join-Path $runFull "desktopreport.exe"
$pipeName      = "xnc-desktop-soak-" + $ts

# ---- machine metadata (read-only CIM; fills the RunResult identity fields) --

$hostName = $env:COMPUTERNAME
$gpuName = ""
$driverVer = ""
$osLabel = ""
try {
    $vc = Get-CimInstance Win32_VideoController | Select-Object -First 1
    if ($null -ne $vc) {
        $gpuName = [string]$vc.Name
        $driverVer = [string]$vc.DriverVersion
    }
    $os = Get-CimInstance Win32_OperatingSystem
    if ($null -ne $os) { $osLabel = ("{0} {1}" -f [string]$os.Caption, [string]$os.Version) }
} catch {
    Write-Host "run-soak: CIM metadata unavailable ($($_.Exception.Message)); RunResult identity fields stay empty"
}

# ---- event schedule ---------------------------------------------------------

$defaultSchedule = {
    # Fractions of DurationSec, all bounded inside the soak window.
    $d = [double]$DurationSec
    $pliDur = [Math]::Max(1, [Math]::Min(30, [int][Math]::Floor($d * 0.10)))
    $ovfDur = [Math]::Max(1, [Math]::Min(30, [int][Math]::Floor($d * 0.10)))
    $recDur = [Math]::Max(2, [Math]::Min(40, [int][Math]::Floor($d * 0.15)))
    @(
        @{ type = "viewer";     atSec = 0;                durationSec = $DurationSec },
        @{ type = "pli";        atSec = [int][Math]::Floor($d * 0.20); durationSec = $pliDur },
        @{ type = "overflow";   atSec = [int][Math]::Floor($d * 0.40); durationSec = $ovfDur; fbQueueMs = 250 },
        @{ type = "reconnect";  atSec = [int][Math]::Floor($d * 0.60); durationSec = $recDur }
    )
}

$events = @()
if ($EventSchedule -ne "") {
    if (-not (Test-Path -LiteralPath $EventSchedule)) { Fail "EventSchedule not found: $EventSchedule" }
    try {
        $sched = Get-Content -Raw -LiteralPath $EventSchedule | ConvertFrom-Json
        foreach ($e in @($sched.events)) { $events += @{ type = [string]$e.type; atSec = [int]$e.atSec; durationSec = [int]$e.durationSec; fbQueueMs = [int]$e.fbQueueMs } }
    } catch {
        Fail "EventSchedule JSON invalid: $($_.Exception.Message)"
    }
} else {
    $events = & $defaultSchedule
}

if ($events.Count -eq 0) { Fail "event schedule is empty" }
$knownTypes = @("viewer", "pli", "overflow", "reconnect")
foreach ($e in $events) {
    if ($knownTypes -notcontains $e.type) { Fail "unknown event type '$($e.type)'" }
    if ($e.atSec -lt 0) { Fail "event atSec must be >= 0" }
    if ($e.durationSec -le 0) { Fail "event durationSec must be > 0" }
    if (($e.atSec + $e.durationSec) -gt $DurationSec) {
        Fail "event $($e.type) at $($e.atSec)s + $($e.durationSec)s ends after the soak window ($DurationSec s)"
    }
}
# Overlap check: reconnect runs two sequential halves; others one process.
$timeline = @()
foreach ($e in $events) {
    if ($e.type -eq "reconnect") {
        $half = [int][Math]::Floor($e.durationSec / 2)
        if ($half -lt 1) { Fail "reconnect durationSec must be >= 2" }
        $gap = $e.durationSec - (2 * $half)
        $timeline += @{ start = $e.atSec; end = $e.atSec + $half }
        $timeline += @{ start = $e.atSec + $half + $gap; end = $e.atSec + $e.durationSec }
    } else {
        $timeline += @{ start = $e.atSec; end = $e.atSec + $e.durationSec }
    }
}
$maxOverlap = 0
for ($t = 0; $t -lt $DurationSec; $t++) {
    $n = 0
    foreach ($iv in $timeline) { if ($t -ge $iv.start -and $t -lt $iv.end) { $n++ } }
    if ($n -gt $maxOverlap) { $maxOverlap = $n }
}
if ($maxOverlap -gt 4) { Fail "schedule has $maxOverlap concurrent viewers; the rt server caps at 4 (--max-subs)" }

# ---- per-run secret (32 bytes hex; children get it via argv, diag-only path) --

$secretBytes = New-Object byte[] 32
$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
$rng.GetBytes($secretBytes)
$rng.Dispose()
$secret = ($secretBytes | ForEach-Object { $_.ToString("x2") }) -join ""

# ---- the exact commands ------------------------------------------------------

$nativeArgs = @("--console-diag", "--fps", "$Fps", "--duration", "$DurationSec",
                "--out", $h264Path, "--pipe", $pipeName, "--secret", $secret,
                "--log-file", $nativeLog)
if ($Pipeline -eq "v2") { $nativeArgs = @("--desktop-pipeline-v2") + $nativeArgs }

$collectArgs = @("-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $collectScript,
                 "-TargetPid", "<NATIVE_PID>", "-IntervalMs", "1000",
                 "-DurationSec", "$DurationSec", "-OutFile", $metricsPath)

function Viewer-Args([int]$durationSec, [hashtable]$e) {
    # One e2eviewer process against the run pipe; --json prints the summary
    # line to stdout (redirected per event).
    $a = @("-direct-pipe", $pipeName, "-direct-secret", $secret,
           "-duration", ("{0}s" -f $durationSec), "-json",
           "-expect-frames-max", "0")
    if ($e.type -eq "pli") { $a += @("-pli-at", "5s") }
    if ($e.type -eq "overflow") {
        $q = 250
        if ($e.fbQueueMs -gt 0) { $q = $e.fbQueueMs }
        $a += @("-fb-queue-ms", "$q")
    }
    return ,$a
}

# ---- PLAN --------------------------------------------------------------------

if ($Plan) {
    Write-Host "=== run-soak PLAN (nothing starts, nothing is created) ==="
    Write-Host ("pipeline       : {0} (v1 notes: XNC_DESKTOP_PIPELINE_V2 must be unset)" -f $Pipeline)
    Write-Host ("duration/fps   : {0}s @ {1}fps (bounded: native self-exits at --duration)" -f $DurationSec, $Fps)
    Write-Host ("run dir        : {0}" -f $runFull)
    Write-Host ("  capture.h264 : {0}  (diag contract; git-ignored, never read by the harness)" -f $h264Path)
    Write-Host ("  stats.json   : {0}" -f $statsPath)
    Write-Host ("  metrics      : {0}" -f $metricsPath)
    Write-Host ("  runresult    : {0}" -f $resultPath)
    Write-Host ("  table        : {0}" -f $tablePath)
    Write-Host ("native exe     : {0}" -f $nativeExe)
    if (-not (Test-Path -LiteralPath $nativeExe)) {
        Write-Host "  WARNING      : native exe MISSING - build first: native\desktop\build.bat" -ForegroundColor Yellow
    }
    Write-Host ("machine        : host={0} gpu={1} driver={2} os={3}" -f $hostName, $gpuName, $driverVer, $osLabel)
    Write-Host ""
    Write-Host "--- bounded commands (all tracked PIDs; try/finally kills ONLY these PIDs) ---"
    Write-Host ("[hidden start] {0}" -f (Render-Cmd $nativeExe $nativeArgs))
    Write-Host ("[hidden start] {0}" -f (Render-Cmd "powershell.exe" $collectArgs))
    $i = 0
    foreach ($e in $events) {
        $i++
        if ($e.type -eq "reconnect") {
            $half = [int][Math]::Floor($e.durationSec / 2)
            $gap = $e.durationSec - (2 * $half)
            Write-Host ("[hidden start +{0,4}s] e2eviewer event#{1} reconnect/1 ({2}s): {3}" -f $e.atSec, $i, $half, (Render-Cmd $e2eExe (Viewer-Args $half $e)))
            Write-Host ("[hidden start +{0,4}s] e2eviewer event#{1} reconnect/2 ({2}s): {3}" -f ($e.atSec + $half + $gap), $i, $half, (Render-Cmd $e2eExe (Viewer-Args $half $e)))
        } else {
            Write-Host ("[hidden start +{0,4}s] e2eviewer event#{1} {2} ({3}s): {4}" -f $e.atSec, $i, $e.type, $e.durationSec, (Render-Cmd $e2eExe (Viewer-Args $e.durationSec $e)))
        }
    }
    Write-Host ""
    Write-Host "--- post-processing (in-shell, after the finally cleanup) ---"
    Write-Host ("[build ] go build -o {0} .   (in tools\e2eviewer)" -f $e2eExe)
    Write-Host ("[build ] go build -o {0} .   (in tools\desktopreport)" -f $reportExe)
    Write-Host ("[run   ] {0}" -f (Render-Cmd $reportExe (@("from-diag", "-stats", $statsPath, "-e2e", "<viewer*.json>", "-metrics", $metricsPath, "-host", $hostName, "-gpu", $gpuName, "-driver", $driverVer, "-os", $osLabel, "-browser", "e2eviewer", "-out", $resultPath))))
    Write-Host ("[run   ] {0}" -f (Render-Cmd $reportExe (@("verify", $resultPath))))
    Write-Host ("[run   ] {0}" -f (Render-Cmd $reportExe (@("merge", "-out", $tablePath, $resultPath))))
    Write-Host ""
    Write-Host ("max concurrent viewers: {0} (rt server cap 4)" -f $maxOverlap)
    exit 0
}

# ---- EXECUTE -----------------------------------------------------------------

if (-not (Get-Command go -ErrorAction SilentlyContinue)) { Fail "go not found on PATH" }
if (-not (Test-Path -LiteralPath $nativeExe)) { Fail "native exe missing: $nativeExe (build: native\desktop\build.bat)" }

New-Item -ItemType Directory -Force -Path $runFull | Out-Null
$runFull = (Resolve-Path $runFull).Path
# The collector appends on first sample; pre-create so from-diag always has
# a (possibly empty) metrics file even when the native dies instantly.
if (-not (Test-Path -LiteralPath $metricsPath)) {
    [System.IO.File]::WriteAllText($metricsPath, "")
}

Write-Host "=== run-soak ==="
Write-Host ("run dir : {0}" -f $runFull)
Write-Host ("events  : {0}" -f (($events | ForEach-Object { "{0}@{1}s/{2}s" -f $_.type, $_.atSec, $_.durationSec }) -join ", "))

# Build the Go tools into the run dir BEFORE any process starts.
Push-Location $e2eDir
try { go build -o $e2eExe . } finally { Pop-Location }
if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $e2eExe)) { Fail "go build e2eviewer failed" }
Push-Location $reportDir
try { go build -o $reportExe . } finally { Pop-Location }
if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $reportExe)) { Fail "go build desktopreport failed" }

$trackedPids = @()   # the ONLY processes this script will ever kill
$viewerReports = New-Object System.Collections.ArrayList

function Kill-Tracked {
    foreach ($p in $script:trackedPids) {
        $alive = Get-Process -Id $p -ErrorAction SilentlyContinue
        if ($null -ne $alive) {
            Write-Host ("run-soak: cleanup pid {0} ({1})" -f $p, $alive.ProcessName)
            Stop-Process -Id $p -Force -ErrorAction SilentlyContinue
        }
    }
}

$harnessError = $false
try {
    # 1. Native pipeline (hidden, tracked).
    Write-Host ("[start] {0}" -f (Render-Cmd $nativeExe $nativeArgs))
    $nativeProc = Start-Process -FilePath $nativeExe -ArgumentList $nativeArgs `
        -WindowStyle Hidden -PassThru `
        -RedirectStandardError $nativeStderr
    $trackedPids += $nativeProc.Id
    $nativePid = $nativeProc.Id
    Write-Host ("native pid {0}" -f $nativePid)

    # 2. Wait for the rt pipe (bounded 15s; no pipe = no viewers).
    $pipePath = "\\.\pipe\" + $pipeName
    $pipeReady = $false
    $deadline = [DateTime]::UtcNow.AddSeconds(15)
    while ([DateTime]::UtcNow -lt $deadline) {
        if ($nativeProc.HasExited) { break }
        if (Test-Path -LiteralPath $pipePath) { $pipeReady = $true; break }
        Start-Sleep -Milliseconds 200
    }
    if (-not $pipeReady) {
        Write-Host "run-soak: rt pipe never appeared (native exited or slow start); continuing with the sidecar only" -ForegroundColor Yellow
    }

    # 3. Metrics sampler (hidden, tracked).
    $collectArgsRun = @("-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $collectScript,
                        "-TargetPid", "$nativePid", "-IntervalMs", "1000",
                        "-DurationSec", "$DurationSec", "-OutFile", $metricsPath)
    Write-Host ("[start] {0}" -f (Render-Cmd "powershell.exe" $collectArgsRun))
    $metricsProc = Start-Process -FilePath "powershell.exe" -ArgumentList $collectArgsRun `
        -WindowStyle Hidden -PassThru
    $trackedPids += $metricsProc.Id

    # 4. Schedule the viewer events.
    if ($pipeReady) {
        $startUtc = [DateTime]::UtcNow
        $pending = New-Object System.Collections.ArrayList
        $i = 0
        foreach ($e in $events) {
            $i++
            if ($e.type -eq "reconnect") {
                $half = [int][Math]::Floor($e.durationSec / 2)
                $gap = $e.durationSec - (2 * $half)
                [void]$pending.Add(@{ at = $startUtc.AddSeconds($e.atSec); dur = $half; e = $e; name = ("viewer-{0:d2}-reconnect-1" -f $i) })
                [void]$pending.Add(@{ at = $startUtc.AddSeconds($e.atSec + $half + $gap); dur = $half; $e = $e; name = ("viewer-{0:d2}-reconnect-2" -f $i) })
            } else {
                [void]$pending.Add(@{ at = $startUtc.AddSeconds($e.atSec); dur = $e.durationSec; e = $e; name = ("viewer-{0:d2}-{1}" -f $i, $e.type) })
            }
        }
        $pending = @($pending | Sort-Object { $_.at })
        $nextIx = 0
        while ($nextIx -lt $pending.Count) {
            if ($nativeProc.HasExited) {
                Write-Host "run-soak: native exited early; skipping remaining viewer events" -ForegroundColor Yellow
                break
            }
            $now = [DateTime]::UtcNow
            if ($now -lt $pending[$nextIx].at) {
                Start-Sleep -Milliseconds 200
                continue
            }
            $job = $pending[$nextIx]
            $nextIx++
            $reportFile = Join-Path $runFull ($job.name + ".json")
            $vArgs = Viewer-Args $job.dur $job.e
            Write-Host ("[start +{0,5:F1}s] {1}" -f ($now - $startUtc).TotalSeconds, (Render-Cmd $e2eExe $vArgs))
            $vp = Start-Process -FilePath $e2eExe -ArgumentList $vArgs `
                -WindowStyle Hidden -PassThru `
                -RedirectStandardOutput $reportFile `
                -RedirectStandardError (Join-Path $runFull ($job.name + ".stderr.log"))
            $trackedPids += $vp.Id
            [void]$viewerReports.Add($reportFile)
        }
    }

    # 5. Bounded wait for the native (self-exits at --duration) + grace.
    $graceSec = 60
    if (-not $nativeProc.HasExited) {
        try {
            Wait-Process -Id $nativePid -Timeout ($DurationSec + $graceSec) -ErrorAction Stop
        } catch {
            if (-not $nativeProc.HasExited) {
                Write-Host "run-soak: native overran its bound; killing tracked pid" -ForegroundColor Yellow
            }
        }
    }
    # 6. Bounded wait for lingering viewers, then let the finally kill them.
    foreach ($p in $trackedPids) {
        if ($p -eq $nativePid) { continue }
        $alive = Get-Process -Id $p -ErrorAction SilentlyContinue
        if ($null -ne $alive) {
            try { Wait-Process -Id $p -Timeout 30 -ErrorAction Stop } catch { }
        }
    }
} catch {
    # Any harness-level failure (start failure, IO error...): the finally
    # below still kills ONLY the tracked pids, then we exit 2.
    Write-Host ("run-soak: harness error: {0}" -f $_.Exception.Message) -ForegroundColor Red
    $harnessError = $true
} finally {
    Kill-Tracked
}

if ($harnessError) { exit 2 }

# ---- convert + verify + merge (in-shell; counters/percentiles only) ----------

if (-not (Test-Path -LiteralPath $statsPath)) {
    Fail "no stats sidecar at $statsPath (native failed before writing it; see $nativeLog)"
}

$e2eFlags = @()
$emptyReports = 0
foreach ($r in $viewerReports) {
    if (-not (Test-Path -LiteralPath $r)) { continue }
    $item = Get-Item -LiteralPath $r
    if ($item.Length -eq 0) {
        $emptyReports++
        Write-Host ("run-soak: WARNING empty viewer report (dial failure?): {0}" -f $r) -ForegroundColor Yellow
        continue
    }
    $e2eFlags += @("-e2e", $r)
}
if ($viewerReports.Count -eq 0) {
    Write-Host "run-soak: WARNING no viewer evidence in this run (rt pipe or events missing); receive-side gates read 0" -ForegroundColor Yellow
}
if ($emptyReports -gt 0) {
    Write-Host ("run-soak: WARNING {0} viewer report(s) empty and excluded from the RunResult (see run log)" -f $emptyReports) -ForegroundColor Yellow
}

$fromArgs = @("from-diag", "-stats", $statsPath) + $e2eFlags +
    @("-metrics", $metricsPath, "-host", $hostName, "-gpu", $gpuName,
      "-driver", $driverVer, "-os", $osLabel, "-browser", "e2eviewer",
      "-out", $resultPath)
Write-Host ("[run] {0}" -f (Render-Cmd $reportExe $fromArgs))
& $reportExe @fromArgs
if ($LASTEXITCODE -ne 0) { Fail "from-diag failed" }

Write-Host ("[run] {0}" -f (Render-Cmd $reportExe @("verify", $resultPath)))
& $reportExe verify $resultPath
$verifyCode = $LASTEXITCODE

& $reportExe merge -out $tablePath $resultPath | Out-Host

if ($verifyCode -eq 0) {
    Write-Host "run-soak: PASS" -ForegroundColor Green
    exit 0
}
Write-Host "run-soak: FAIL (see violations above)" -ForegroundColor Red
exit 1
