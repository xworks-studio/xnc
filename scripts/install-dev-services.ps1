# install-dev-services.ps1 - XNCCoreDev service install/uninstall (M2-Slice3
# Task 1). Creates the DEV-ONLY core service via sc.exe (no listeners, so no
# firewall rule). Isolated from production XNCAgent by name (XNCCoreDev) and
# path (C:\xnc-dev); the dev core has no state directory and logs to its own
# dir. Requires an elevated PowerShell.
#
#   powershell -File scripts\install-dev-services.ps1 -Install
#   powershell -File scripts\install-dev-services.ps1 -Install -SecretHex <64hex> -PipeName <name>
#   powershell -File scripts\install-dev-services.ps1 -Uninstall
#
# Defaults match the dev topology/e2e vectors: pipe \\.\pipe\xnc-core-dev,
# secret = hex("test-pipe-secret") (dev-only plaintext binPath precedent -
# see scripts/dev-topology.md warning; production uses the SCM credential
# channel, a later slice).
param(
  [switch]$Install,
  [switch]$Uninstall,
  [string]$ServiceName = 'XNCCoreDev',
  [string]$BinPath = 'C:\xnc-dev\xnc-core.exe',
  [string]$PipeName = '\\.\pipe\xnc-core-dev',
  # hex("test-pipe-secret") - the e2e smoke vector (16B/32 chars)
  [string]$SecretHex = '746573742d706970652d736563726574'
)

$ErrorActionPreference = 'Stop'

if (-not ($Install -xor $Uninstall)) {
  Write-Error "specify exactly one of -Install / -Uninstall"
  exit 2
}
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
  ).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $admin) { Write-Error "must run elevated (sc.exe create/delete)"; exit 2 }

if ($Install) {
  if (-not (Test-Path $BinPath)) { Write-Error "core exe not found: $BinPath"; exit 2 }
  $bin = "$BinPath --service $ServiceName --pipe-name $PipeName --smoke-secret $SecretHex"
  sc.exe create $ServiceName binPath= $bin start= auto obj= LocalSystem
  if ($LASTEXITCODE -ne 0) { Write-Error "sc create failed ($LASTEXITCODE)"; exit 1 }
  sc.exe description $ServiceName "XNC dev node core (M2-Slice3 dev topology; isolated from prod XNCAgent)"
  # Dependency: XNCAgentDev (Task 2) if it is present; harmless to re-run.
  $agentDev = Get-Service XNCAgentDev -ErrorAction SilentlyContinue
  if ($agentDev) { sc.exe config $ServiceName depend= XNCAgentDev | Out-Null }
  Write-Host "installed $ServiceName (binPath: $bin)"
  if (-not $agentDev) { Write-Host "note: XNCAgentDev not present - dependency skipped (install agent first to get it)" }
  Write-Host "pipe: $PipeName  secret-hex: $SecretHex (dev-only plaintext binPath precedent)"
  Start-Service $ServiceName
  Get-Service $ServiceName | Format-Table Name, Status, StartType
} else {
  $svc = Get-Service $ServiceName -ErrorAction SilentlyContinue
  if (-not $svc) { Write-Host "$ServiceName not present - nothing to do"; exit 0 }
  if ($svc.Status -ne 'Stopped') { Stop-Service $ServiceName -Force }
  sc.exe delete $ServiceName
  if ($LASTEXITCODE -ne 0) { Write-Error "sc delete failed ($LASTEXITCODE)"; exit 1 }
  Write-Host "uninstalled $ServiceName"
}
