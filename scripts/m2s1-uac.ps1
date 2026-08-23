# m2s1-uac.ps1 - M2-Slice1 Task 6 gate-1 trigger: raise the UAC prompt from
# a NON-elevated session-1 process and, on approval, run a distinguishable
# elevated child that (a) proves elevation via `net session` (exits 5 when
# not elevated) and (b) stays alive ~$HoldSec for tasklist evidence.
# Run via schtasks /RU <console user> /IT WITHOUT /RL HIGHEST (an elevated
# caller would launch directly with NO prompt - see uac-probe.ps1).
# The child image is C:\xnc-dev\xnc-uac-child.exe (a copy of cmd.exe the
# orchestrator deploys) so every kill is image-scoped (ledger rule).
param(
  [int]$HoldSec = 40,
  [string]$Marker = 'C:\xnc-diag\uac-elev.txt',
  [string]$LogPath = 'C:\xnc-diag\m2s1-uac.log'
)
$ErrorActionPreference = 'Continue'
$dir = Split-Path -Parent $LogPath
if ($dir -and -not (Test-Path $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
function Log([string]$msg) {
  $ms = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
  $line = "[m2s1-uac $ms local=$(Get-Date -Format 'yyyy-MM-ddTHH:mm:ss.fff')] $msg"
  Add-Content -Path $LogPath -Value $line
  Write-Output $line
}
function Get-ConsentCount { @(Get-Process -Name consent -ErrorAction SilentlyContinue).Count }

$elevated = $false
try {
  $id = [Security.Principal.WindowsIdentity]::GetCurrent()
  $elevated = (New-Object Security.Principal.WindowsPrincipal($id)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
} catch {}
Log "start user=$([Environment]::UserName) elevated=$elevated session=$([Environment]::SessionId)"
if ($elevated) { Log "WARN elevated caller: -Verb RunAs would NOT prompt" }
Remove-Item -Force -ErrorAction SilentlyContinue $Marker

# Background job: Start-Process -Verb RunAs BLOCKS until the prompt is
# answered (uac-probe run-1 finding).
$job = Start-Job -ScriptBlock {
  try {
    Start-Process -Verb RunAs -FilePath 'C:\xnc-dev\xnc-uac-child.exe' `
      -ArgumentList '/c', "net session >nul 2>&1 && echo ELEVATED-OK> $using:Marker & ping -n $using:HoldSec 127.0.0.1 >nul" `
      -ErrorAction Stop
    'spawned'
  } catch {
    'threw: ' + $_.Exception.Message
  }
}
$appeared = $false
$dl = (Get-Date).AddSeconds(15)
while ((Get-Date) -lt $dl) {
  if (Get-ConsentCount -gt 0) { $appeared = $true; break }
  Start-Sleep -Milliseconds 100
}
Log "consent_appeared=$appeared procs=$(Get-ConsentCount)"
$gone = $false
$dl = (Get-Date).AddSeconds(90)
while ((Get-Date) -lt $dl) {
  if (Get-ConsentCount -eq 0) { $gone = $true; break }
  Start-Sleep -Milliseconds 200
}
Log "consent_gone=$gone marker=$(Test-Path $Marker) child_procs=$(@(Get-Process -Name xnc-uac-child -ErrorAction SilentlyContinue).Count)"
$r = Receive-Job -Job $job -Wait -AutoRemoveJob
Log "job decision: $r"
Log "done appeared=$appeared gone=$gone"
exit 0
