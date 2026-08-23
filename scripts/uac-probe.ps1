# uac-probe.ps1 - M2-Slice1 Task 1 evidence probe (run in SESSION 1 via
# schtasks /RU <console user> /IT; created by the orchestrator, args baked
# into /TR - see scripts/e2e-slice3.sh start_probe for the pattern).
#
# Modes (the desktop-side observer is xnc-desktop --console-diag with the
# DesktopWatch: desktop_transition lines + diag_pipeline ... desktop=<name>;
# this script only TRIGGERS the transitions and timestamps them):
#   uac     Start-Process -Verb RunAs cmd '/c exit' -> consent.exe raises the
#           secure desktop. MUST run non-elevated (an elevated caller would
#           launch the child directly with NO prompt) - create the task
#           WITHOUT /RL HIGHEST. With -AutoDismiss: after DismissDelaySec,
#           SendKeys {ESC} from this same session-1 script (cancels the
#           prompt); if consent.exe survives +4s, a second ESC + a
#           Stop-Process attempt (succeeds only when elevated; the
#           orchestrator's SYSTEM exec is the real backstop). The prompt
#           lifetime is logged so the desktop log and this log align.
#   lock    rundll32 user32.dll,LockWorkStation (lock screen -> input desktop
#           leaves winsta0\Default).
#   unlock  after UnlockDelaySec send N x {ENTER} spaced UnlockEnterSpacingSec
#           (XIAOXIN user 'labs' has an EMPTY password: Enter dismisses the
#           clock and signs in). SendKeys may not reach the secure desktop
#           from a non-SYSTEM process - the outcome is recorded either way,
#           exactly what Task 1 is here to measure.
#   status  no-op heartbeat: session/elevation/consent snapshot (debugging).
#
# Log: one line per step, machine-correlatable with the desktop log:
#   [uac-prope <epoch_ms> local=<iso>] msg
# (epoch ms aligns with the orchestrator's clock-skew measurement; local= is
# the same wall clock the desktop XNC_LOG lines use.)
#
# Exit 0 = script completed (outcome details are in the log); 2 = bad usage.

param(
  [ValidateSet('uac', 'lock', 'unlock', 'status')]
  [string]$Mode = 'status',
  [switch]$AutoDismiss,
  [int]$DismissDelaySec = 3,
  [int]$ConsentAppearTimeoutSec = 10,
  [int]$DismissWaitTimeoutSec = 30,
  [int]$UnlockDelaySec = 2,
  [int]$UnlockEnterCount = 3,
  [int]$UnlockEnterSpacingSec = 2,
  [string]$LogPath = 'C:\xnc-diag\uac-probe.log'
)

$ErrorActionPreference = 'Continue'
if ($DismissDelaySec -lt 0 -or $UnlockEnterCount -lt 1 -or $UnlockEnterSpacingSec -lt 1) {
  Write-Output "uac-probe: invalid parameters"; exit 2
}

$dir = Split-Path -Parent $LogPath
if ($dir -and -not (Test-Path $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }

function Log([string]$msg) {
  $ms = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
  $local = Get-Date -Format 'yyyy-MM-ddTHH:mm:ss.fff'
  $line = "[uac-probe $ms local=$local] $msg"
  Add-Content -Path $LogPath -Value $line
  Write-Output $line
}

function Send-Key([string]$key) {
  try {
    $ws = New-Object -ComObject WScript.Shell
    $ws.SendKeys($key)
    Log "sendkeys ok key=$key"
  } catch {
    Log "sendkeys FAILED key=$key err=$($_.Exception.Message)"
  }
}

function Get-ConsentCount { @(Get-Process -Name consent -ErrorAction SilentlyContinue).Count }

$elevated = $false
try {
  $id = [Security.Principal.WindowsIdentity]::GetCurrent()
  $elevated = (New-Object Security.Principal.WindowsPrincipal($id)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
} catch {}

Log "start mode=$Mode autoDismiss=$AutoDismiss user=$([Environment]::UserName) elevated=$elevated session=$([Environment]::SessionId)"

switch ($Mode) {

  'status' {
    Log "status consent_procs=$(Get-ConsentCount) interactive=$([Environment]::UserInteractive)"
    Log "done mode=status"
    break
  }

  'uac' {
    if ($elevated) {
      Log "WARN elevated caller: -Verb RunAs would NOT prompt (secure desktop never appears)"
    }
    Log "uac trigger Start-Process -Verb RunAs cmd /c exit (background job: the call blocks until the prompt is answered)"
    # Start-Process -Verb RunAs BLOCKS until the prompt is answered - run it
    # in a background job so this script can watch consent.exe appear/die in
    # real time (run-1 finding: a foreground call missed the whole window).
    $job = Start-Job -ScriptBlock {
      try {
        Start-Process -Verb RunAs -FilePath cmd.exe -ArgumentList '/c', 'exit' -ErrorAction Stop
        'spawned'
      } catch {
        'threw: ' + $_.Exception.Message
      }
    }
    # Wait for the secure desktop to actually go up (consent.exe appears).
    $appeared = $false
    $dl = (Get-Date).AddSeconds($ConsentAppearTimeoutSec)
    while ((Get-Date) -lt $dl) {
      if (Get-ConsentCount -gt 0) { $appeared = $true; break }
      Start-Sleep -Milliseconds 100
    }
    Log "uac consent_appeared=$appeared procs=$(Get-ConsentCount)"
    if (-not $appeared) {
      $r = Receive-Job -Job $job -Wait -AutoRemoveJob
      Log "uac job result (no prompt raised): $r"
      Log "done mode=uac appeared=False"
      break
    }
    if ($AutoDismiss) {
      Log "autodismiss waiting ${DismissDelaySec}s before ESC"
      Start-Sleep -Seconds $DismissDelaySec
      Send-Key '{ESC}'
      # ESC may not reach the secure desktop from a non-SYSTEM caller: give
      # the prompt 4s, then a second ESC and a kill attempt (kill succeeds
      # only when elevated; outcome is logged either way - evidence, not
      # assumption).
      Start-Sleep -Seconds 4
      if (Get-ConsentCount -gt 0) {
        Log "autodismiss consent still up after first ESC (procs=$(Get-ConsentCount))"
        Send-Key '{ESC}'
        Start-Sleep -Seconds 2
        if (Get-ConsentCount -gt 0) {
          Log "autodismiss trying Stop-Process consent (works only elevated/SYSTEM)"
          try {
            Stop-Process -Name consent -Force -ErrorAction Stop
            Log "autodismiss Stop-Process issued"
          } catch {
            Log "autodismiss Stop-Process failed: $($_.Exception.Message)"
          }
        }
      }
    }
    # Track the prompt lifetime end-to-end (the secure-desktop window the
    # desktop watch observes).
    $gone = $false
    $dl = (Get-Date).AddSeconds($DismissWaitTimeoutSec)
    while ((Get-Date) -lt $dl) {
      if (Get-ConsentCount -eq 0) { $gone = $true; break }
      Start-Sleep -Milliseconds 200
    }
    Log "uac consent_gone=$gone procs=$(Get-ConsentCount)"
    $r = Receive-Job -Job $job -Wait -AutoRemoveJob
    Log "uac job decision: $r"
    Log "done mode=uac appeared=$appeared gone=$gone"
    break
  }

  'lock' {
    Log "lock trigger rundll32 user32.dll,LockWorkStation"
    rundll32.exe user32.dll,LockWorkStation
    Log "lock LockWorkStation returned"
    Log "done mode=lock"
    break
  }

  'unlock' {
    Log "unlock waiting ${UnlockDelaySec}s for the lock screen to settle"
    Start-Sleep -Seconds $UnlockDelaySec
    for ($i = 1; $i -le $UnlockEnterCount; $i++) {
      Send-Key '{ENTER}'
      Log "unlock enter $i/$UnlockEnterCount sent (empty-password sign-in)"
      if ($i -lt $UnlockEnterCount) { Start-Sleep -Seconds $UnlockEnterSpacingSec }
    }
    Start-Sleep -Seconds 2
    Log "unlock consent_procs=$(Get-ConsentCount) logonui_procs=$(@(Get-Process -Name LogonUI -ErrorAction SilentlyContinue).Count) (LogonUI>0 => still locked)"
    Log "done mode=unlock"
    break
  }
}
exit 0
