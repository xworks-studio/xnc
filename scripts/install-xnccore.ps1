# install-xnccore.ps1 - prod bootstrap: create/start (or just ensure running)
# the XNCCore Windows service for a deployed xnc-core.exe.
#
# Params:
#   -InstallDir   directory containing xnc-core.exe (default: script location;
#                 must exist)
#   -StateDir     REQUIRED - must exist - the xnc agent's state dir; the
#                 service's --secret-file (<StateDir>\core-secret.hex) is
#                 created and DACL-locked by xnc-core itself on first start
#                 (SYSTEM+Admins only). The agent reads the same file for
#                 pipe auth.
#   -ServiceName  service name (default XNCCore; must match xnc-core
#                 --service <name>; must match ^[A-Za-z0-9-]+$)
#   -Uninstall    stop + delete the service instead
#
# Service creation uses the New-Service cmdlet (not sc.exe): PowerShell 5.1
# mangles embedded quotes in arguments passed to native sc.exe, breaking
# binPath values that quote spaced paths. New-Service -BinaryPathName passes
# the string (including its embedded quotes) verbatim to the Win32 service
# API.
#
# NOTE: 安装器升级/回滚路径（installer/xnc.iss）负责服务的停止→删除→重建，
# 不再经本脚本；本脚本保留为无安装器场景（如 dev 裸部署 xnc-core.exe）的
# 手动 bootstrap/修复工具。幂等：服务已存在 -> 仅确保运行。
param(
    [string]$InstallDir = $PSScriptRoot,
    [Parameter(Mandatory = $true)]
    [string]$StateDir,
    [string]$ServiceName = "XNCCore",
    [switch]$Uninstall
)

$ErrorActionPreference = "Stop"

if ($ServiceName -notmatch '^[A-Za-z0-9-]+$') {
    Write-Error "install-xnccore: -ServiceName '$ServiceName' is invalid (allowed: A-Z a-z 0-9 hyphen)"
    exit 1
}

if ($Uninstall) {
    $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if (-not $svc) { Write-Output "install-xnccore: service $ServiceName not present (nothing to do)"; exit 0 }
    if ($svc.Status -ne 'Stopped') {
        Stop-Service -Name $ServiceName -Force
        Start-Sleep -Seconds 2
    }
    # sc.exe delete only takes the bare service name - no quoting hazard.
    sc.exe delete $ServiceName | Out-Null
    if ($LASTEXITCODE -ne 0) { Write-Error "install-xnccore: sc.exe delete failed (exit $LASTEXITCODE)"; exit 1 }
    Write-Output "install-xnccore: service $ServiceName deleted"
    exit 0
}

if (-not (Test-Path $InstallDir -PathType Container)) {
    Write-Error "install-xnccore: InstallDir $InstallDir does not exist"; exit 1
}
if (-not (Test-Path $StateDir -PathType Container)) {
    Write-Error "install-xnccore: StateDir $StateDir does not exist"; exit 1
}
$coreExe = Join-Path $InstallDir "xnc-core.exe"
if (-not (Test-Path $coreExe)) { Write-Error "install-xnccore: $coreExe not found"; exit 1 }
$secretFile = Join-Path $StateDir "core-secret.hex"

$existing = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($existing) {
    Write-Output "install-xnccore: service $ServiceName already exists (binPath unchanged; delete with -Uninstall to re-create)"
    if ($existing.Status -ne 'Running') { Start-Service -Name $ServiceName }
} else {
    # binPath quotes the spaced paths; New-Service passes embedded quotes
    # through verbatim (PS 5.1-safe, unlike sc.exe argument passing).
    $binPath = '"' + $coreExe + '" --service ' + $ServiceName + ' --secret-file "' + $secretFile + '"'
    New-Service -Name $ServiceName -BinaryPathName $binPath -StartupType Automatic |
        Out-Null
    Set-Service -Name $ServiceName -Description "XNC node core (XNIP pipe server; secret persisted at $secretFile)"
    Start-Service -Name $ServiceName
}

$svc = Get-Service -Name $ServiceName
Write-Output ("install-xnccore: service {0} status={1} exe={2} secretFile={3}" -f $ServiceName, $svc.Status, $coreExe, $secretFile)
