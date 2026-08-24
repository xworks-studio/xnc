# install-xnccore.ps1 - prod bootstrap: create/start (or just ensure running)
# the XNCCore Windows service for a deployed xnc-core.exe.
#
# Params:
#   -InstallDir   directory containing xnc-core.exe (default: script location)
#   -StateDir     REQUIRED - the xnc agent's state dir; the service's
#                 --secret-file (<StateDir>\core-secret.hex) is created and
#                 DACL-locked by xnc-core itself on first start (SYSTEM+Admins
#                 only). The agent reads the same file for pipe auth.
#   -ServiceName  service name (default XNCCore; must match xnc-core
#                 --service <name>)
#   -Uninstall    stop + delete the service instead
#
# Idempotent: existing service -> just ensure running.
param(
    [string]$InstallDir = $PSScriptRoot,
    [Parameter(Mandatory = $true)]
    [string]$StateDir,
    [string]$ServiceName = "XNCCore",
    [switch]$Uninstall
)

$ErrorActionPreference = "Stop"

if ($Uninstall) {
    $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if (-not $svc) { Write-Output "install-xnccore: service $ServiceName not present (nothing to do)"; exit 0 }
    if ($svc.Status -ne 'Stopped') {
        Stop-Service -Name $ServiceName -Force
        Start-Sleep -Seconds 2
    }
    sc.exe delete $ServiceName | Out-Null
    if ($LASTEXITCODE -ne 0) { Write-Error "install-xnccore: sc.exe delete failed (exit $LASTEXITCODE)"; exit 1 }
    Write-Output "install-xnccore: service $ServiceName deleted"
    exit 0
}

$coreExe = Join-Path $InstallDir "xnc-core.exe"
if (-not (Test-Path $coreExe)) { Write-Error "install-xnccore: $coreExe not found"; exit 1 }
if (-not (Test-Path $StateDir)) { Write-Error "install-xnccore: StateDir $StateDir does not exist"; exit 1 }
$secretFile = Join-Path $StateDir "core-secret.hex"

$existing = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($existing) {
    Write-Output "install-xnccore: service $ServiceName already exists (binPath unchanged; delete with -Uninstall to re-create)"
    if ($existing.Status -ne 'Running') { Start-Service -Name $ServiceName }
} else {
    # binPath uses sc.exe quoting (space-tolerant) + SYSTEM + auto start.
    $binPath = "`"$coreExe`" --service $ServiceName --secret-file `"$secretFile`""
    sc.exe create $ServiceName binPath= $binPath start= auto obj= LocalSystem | Out-Null
    if ($LASTEXITCODE -ne 0) { Write-Error "install-xnccore: sc.exe create failed (exit $LASTEXITCODE)"; exit 1 }
    sc.exe description $ServiceName "XNC node core (XNIP pipe server; secret persisted at $secretFile)" | Out-Null
    Start-Service -Name $ServiceName
}

$svc = Get-Service -Name $ServiceName
Write-Output ("install-xnccore: service {0} status={1} exe={2} secretFile={3}" -f $ServiceName, $svc.Status, $coreExe, $secretFile)
