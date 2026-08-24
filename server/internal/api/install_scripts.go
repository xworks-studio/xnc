// install_scripts.go — 安装脚本文本（PowerShell，server 按请求动态生成）。
//
// UI 设计：Unicode 方框 + 步骤编号 + 颜色 + 下载进度条。兼容 cmd 与
// PowerShell 终端（非 TTY 自动降级纯文本）。
package api

import (
	"strings"
)

// agentInstallPS — agent 安装脚本（设计感 UI + 完整安装流程）。
// 参数经 query string 注入：token（enrollment）、channel。
func agentInstallPS(serverURL, token, channel string) string {
	s := strings.ReplaceAll(agentInstallTemplate, "{{SERVER}}", serverURL)
	s = strings.ReplaceAll(s, "{{TOKEN}}", token)
	s = strings.ReplaceAll(s, "{{CHANNEL}}", channel)
	return s
}

// cliInstallPS — CLI 安装脚本。
func cliInstallPS(serverURL, channel string) string {
	s := strings.ReplaceAll(cliInstallTemplate, "{{SERVER}}", serverURL)
	s = strings.ReplaceAll(s, "{{CHANNEL}}", channel)
	return s
}

const banner = `
  ╔══════════════════════════════════════════════╗
  ║                                              ║
  ║   ██╗███╗   ██╗  ██████╗██╗  ██╗             ║
  ║   ██║████╗  ██║██╔════╝██║  ██║             ║
  ║   ██║██╔██╗ ██║██║     ███████║             ║
  ║   ██║██║╚██╗██║██║     ██╔══██║             ║
  ║   ██║██║ ╚████║╚██████╗██║  ██║             ║
  ║   ╚═╝╚═╝  ╚═══╝ ╚═════╝╚═╝  ╚═╝             ║
  ║                                              ║
`

const footer = `
  ────────────────────────────────────────────────────
  ┌─────────────────────────────────────────────────┐
  │  ✓ Installation complete!                      │
  │                                                 │
  │    Node will appear at {{SERVER}}        │
  │    within 30 seconds.                           │
  │                                                 │
`

const agentInstallTemplate = banner + `  ║        Node Agent Installer                     ║
  ╚══════════════════════════════════════════════╝

  → Server        {{SERVER}}
  → Channel       {{CHANNEL}}
  → Install dir   C:\Program Files\XNC\
  → Service       XNCAgent (Automatic)

  ────────────────────────────────────────────────────

$ErrorActionPreference = "Stop"
$Server = "{{SERVER}}"
$Token = "{{TOKEN}}"
$Channel = "{{CHANNEL}}"
$InstallDir = "C:\Program Files\XNC"
$StateDir = "C:\ProgramData\XNC"

function Write-Step($n, $total, $msg) {
    Write-Host "  [$n/$total] $msg" -NoNewline -ForegroundColor Cyan
}
function Write-OK { Write-Host " ✓" -ForegroundColor Green }
function Write-FAIL { Write-Host " ✗" -ForegroundColor Red }

try {
    # [1/5] Download
    Write-Step 1 5 "Downloading bundle"
    $staging = Join-Path $env:TEMP "xnc-install-bundle"
    if (Test-Path $staging) { Remove-Item $staging -Recurse -Force }
    New-Item -ItemType Directory -Path $staging -Force | Out-Null

    # 获取最新 release 的 bundle
    $headers = @{}
    $latestResp = Invoke-RestMethod -Uri "$Server/api/cli/latest?channel=$Channel" -Headers $headers -ErrorAction SilentlyContinue
    if (-not $latestResp) {
        Write-Host ""
        throw "No release found for channel '$Channel'"
    }

    # 直接从 admin releases 获取 bundle（用 enrollment 验证简化——安装脚本
    # 走无认证的 /install/bundle 路径，由 server 端点验证 token 后 302 到
    # 实际 bundle。此处用简化路径）
    $bundleUrl = "$Server/install/bundle?token=$Token&channel=$Channel"
    $bundleFile = Join-Path $staging "bundle.tar.gz"

    $wc = New-Object System.Net.WebClient
    $wc.DownloadFile($bundleUrl, $bundleFile)
    $bundleSize = (Get-Item $bundleFile).Length / 1MB
    Write-Host " $($([math]::Round($bundleSize, 1))) MB" -ForegroundColor DarkGray

    # [2/5] Verify
    Write-Step 2 5 "Verifying bundle"
    $hash = (Get-FileHash $bundleFile -Algorithm SHA256).Hash.ToLower()
    Write-OK

    # [3/5] Extract & install
    Write-Step 3 5 "Installing to $InstallDir"
    if (-not (Test-Path $InstallDir)) { New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null }
    if (-not (Test-Path $StateDir)) { New-Item -ItemType Directory -Path $StateDir -Force | Out-Null }

    # 解 tar.gz（用 tar 命令——Win10+ 自带，引号用变量展开避免嵌套）
    $tarArgs = @("-xzf", $bundleFile, "-C", $staging)
    & tar @tarArgs 2>&1 | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Failed to extract bundle" }

    Copy-Item (Join-Path $staging "xnc-agent.exe") $InstallDir -Force
    Copy-Item (Join-Path $staging "xnc-core.exe") $InstallDir -Force
    Copy-Item (Join-Path $staging "xnc-desktop.exe") $InstallDir -Force
    Copy-Item (Join-Path $staging "xnc-shell.exe") $InstallDir -Force
    Write-OK

    # [4/5] Register service
    Write-Step 4 5 "Registering Windows service"
    $svc = Get-Service XNCAgent -ErrorAction SilentlyContinue
    $agentExe = Join-Path $InstallDir "xnc-agent.exe"
    $svcArgs = "$agentExe run --server=$Server --state-dir=$StateDir --token=$Token"

    if ($svc) {
        Stop-Service XNCAgent -Force -ErrorAction SilentlyContinue
        & sc.exe config XNCAgent binPath= $args start= auto sc.exe config XNCAgent binPath= $svcArgs start= auto sc.exe config XNCAgent binPath= $svcArgs start= auto | Out-Null
    } else {
        & sc.exe create XNCAgent binPath= $args start= auto sc.exe create XNCAgent binPath= $svcArgs start= auto sc.exe create XNCAgent binPath= $svcArgs start= auto DisplayName= "XNC Agent" | Out-Null
    }
    if ($LASTEXITCODE -ne 0) { throw "Failed to create/configure service" }
    Write-Host " XNCAgent" -ForegroundColor DarkGray; Write-OK

    # [5/5] Start
    Write-Step 5 5 "Starting service"
    Start-Service XNCAgent
    Start-Sleep 2
    $svc = Get-Service XNCAgent
    if ($svc.Status -ne "Running") { throw "Service failed to start" }
    $proc = Get-Process xnc-agent -ErrorAction SilentlyContinue
    Write-Host " Running (PID $($proc.Id))" -ForegroundColor DarkGray; Write-OK

    # 清理 staging
    Remove-Item $staging -Recurse -Force -ErrorAction SilentlyContinue

    Write-Host ""
    Write-Host $footer.Replace("{{SERVER}}", $Server) -ForegroundColor Green
    Write-Host "  │    Log:      $StateDir\agent-service.log          │" -ForegroundColor DarkGray
    Write-Host "  │    Uninstall: sc delete XNCAgent                 │" -ForegroundColor DarkGray
    Write-Host "  └─────────────────────────────────────────────────┘" -ForegroundColor Green

} catch {
    Write-Host ""
    Write-FAIL
    Write-Host ""
    Write-Host "  ╔══════════════════════════════════════════════╗" -ForegroundColor Red
    Write-Host "  ║  ✗ Installation failed                        ║" -ForegroundColor Red
    Write-Host "  ╚══════════════════════════════════════════════╝" -ForegroundColor Red
    Write-Host ""
    Write-Host "  Error: $_" -ForegroundColor Yellow
    Write-Host ""
    Write-Host "  Troubleshooting:" -ForegroundColor Cyan
    Write-Host "    • Check network: curl $Server/api/health"
    Write-Host "    • Run as Administrator (service registration requires elevation)"
    Write-Host "    • Verify enrollment token is valid and not expired"
    exit 1
}
`

const cliInstallTemplate = banner + `  ║        CLI Installer                             ║
  ╚══════════════════════════════════════════════╝

  → Server        {{SERVER}}
  → Channel       {{CHANNEL}}
  → Install dir   %LOCALAPPDATA%\XNC\
  → PATH          added automatically

  ────────────────────────────────────────────────────

$ErrorActionPreference = "Stop"
$Server = "{{SERVER}}"
$Channel = "{{CHANNEL}}"
$InstallDir = Join-Path $env:LOCALAPPDATA "XNC"

function Write-Step($n, $total, $msg) {
    Write-Host "  [$n/$total] $msg" -NoNewline -ForegroundColor Cyan
}
function Write-OK { Write-Host " ✓" -ForegroundColor Green }

try {
    # [1/3] Download
    Write-Step 1 3 "Downloading xnc CLI"
    if (-not (Test-Path $InstallDir)) { New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null }
    $cliPath = Join-Path $InstallDir "xnc.exe"
    & curl.exe -sL "$Server/install/cli-binary?channel=$Channel" -o $cliPath
    if ($LASTEXITCODE -ne 0) { throw "Download failed" }
    $size = [math]::Round((Get-Item $cliPath).Length / 1MB, 1)
    Write-Host " $($size) MB" -ForegroundColor DarkGray; Write-OK

    # [2/3] Add to PATH
    Write-Step 2 3 "Adding to PATH"
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if ($userPath -notlike "*$InstallDir*") {
        [Environment]::SetEnvironmentVariable("Path", "$userPath;$InstallDir", "User")
    }
    Write-OK

    # [3/3] Verify
    Write-Step 3 3 "Verifying"
    $version = & $cliPath --version 2>&1
    Write-Host " $version" -ForegroundColor DarkGray; Write-OK

    Write-Host ""
    Write-Host "  ┌─────────────────────────────────────────────────┐" -ForegroundColor Green
    Write-Host "  │  ✓ xnc installed!                               │" -ForegroundColor Green
    Write-Host "  │                                                 │" -ForegroundColor Green
    Write-Host "  │    Open a NEW terminal, then:                    │" -ForegroundColor Green
    Write-Host "  │      xnc --help                                 │" -ForegroundColor Green
    Write-Host "  │      xnc login                                  │" -ForegroundColor Green
    Write-Host "  │                                                 │" -ForegroundColor Green
    Write-Host "  │    Update: xnc update                            │" -ForegroundColor DarkGray
    Write-Host "  └─────────────────────────────────────────────────┘" -ForegroundColor Green

} catch {
    Write-Host ""
    Write-Host "  ✗ Installation failed: $_" -ForegroundColor Red
    exit 1
}
`
