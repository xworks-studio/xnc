# install-dev-agent.ps1 - XNCAgentDev service install/uninstall (M2-Slice3
# Task 2). Creates the DEV-ONLY agent service via the agent's own `install`
# command (mgr API; per-word argv quoting handled in Go). Isolated from
# production XNCAgent by NAME (XNCAgentDev), STATE DIR
# (C:\ProgramData\XNCAgentDev) and binary path (C:\xnc-dev) - prod identity/
# state are never touched. Requires an elevated PowerShell.
#
#   powershell -File scripts\install-dev-agent.ps1 -Install -Server http://<dev-server>:8080 -Token <enroll-token>
#   powershell -File scripts\install-dev-agent.ps1 -Install -Server ... -PipeName \\.\pipe\xnc-core-dev -SecretHex 746573742d706970652d736563726574
#   powershell -File scripts\install-dev-agent.ps1 -Uninstall
#
# Desktop creds (-PipeName/-SecretHex) ride the service argv
# (--desktop-core-pipe/--desktop-core-secret-hex) - the same dev-only
# plaintext binPath precedent as XNCCoreDev's --smoke-secret (see
# scripts/dev-topology.md warning; production credential channel = later
# slice). Secret is never logged.
param(
  [switch]$Install,
  [switch]$Uninstall,
  [string]$ServiceName = 'XNCAgentDev',
  [string]$BinPath = 'C:\xnc-dev\xnc-agent.exe',
  [Parameter(Mandatory=$false)][string]$Server = '',
  [Parameter(Mandatory=$false)][string]$Token = '',
  [string]$StateDir = 'C:\ProgramData\XNCAgentDev',
  [string]$PipeName = '\\.\pipe\xnc-core-dev',
  # hex("test-pipe-secret") - the e2e smoke vector; must match XNCCoreDev's
  # --smoke-secret for core StartCapture to authorize.
  [string]$SecretHex = '746573742d706970652d736563726574'
)

$ErrorActionPreference = 'Stop'

if (-not ($Install -xor $Uninstall)) {
  Write-Error "specify exactly one of -Install / -Uninstall"
  exit 2
}
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
  ).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $admin) { Write-Error "must run elevated (service create/delete)"; exit 2 }

if (-not (Test-Path $BinPath)) { Write-Error "agent exe not found: $BinPath"; exit 2 }

if ($Install) {
  if ($Server -eq '') { Write-Error "-Server is required for -Install"; exit 2 }
  New-Item -ItemType Directory -Force -Path $StateDir | Out-Null
  $args = @('install',
    "--server=$Server",
    "--state-dir=$StateDir",
    "--service-name=$ServiceName",
    "--desktop-core-pipe=$PipeName",
    "--desktop-core-secret-hex=$SecretHex")
  if ($Token -ne '') { $args += "--token=$Token" }
  & $BinPath @args
  if ($LASTEXITCODE -ne 0) { Write-Error "agent install failed ($LASTEXITCODE)"; exit 1 }
  # LocalSystem + Automatic come from the Go installer; add the reverse of the
  # core's dependency note: core should start after the agent.
  $coreDev = Get-Service XNCCoreDev -ErrorAction SilentlyContinue
  if ($coreDev) { sc.exe config XNCCoreDev depend= $ServiceName | Out-Null }
  Get-Service $ServiceName | Format-Table Name, Status, StartType
  Write-Host "state: $StateDir (isolated from C:\ProgramData\XNCAgent)"
  Write-Host "desktop core pipe: $PipeName (secret in service argv - dev-only plaintext precedent)"
} else {
  & $BinPath uninstall "--service-name=$ServiceName"
  if ($LASTEXITCODE -ne 0) { Write-Error "agent uninstall failed ($LASTEXITCODE)"; exit 1 }
  Write-Host "uninstalled $ServiceName (state dir $StateDir left in place for re-enroll reuse)"
}
