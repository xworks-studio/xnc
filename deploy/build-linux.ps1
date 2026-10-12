# build-linux.ps1 — 交叉编译 Linux 节点制品并打 tar.gz（Linux 节点支持）。
# 用法:
#   powershell deploy/build-linux.ps1 -Version 0.10.36
# 产物: bin/xnc-linux-amd64-<Version>.tar.gz (+ .sha256 边车)
#   内容: bin/xnc-agent bin/xnc systemd/xnc-agent.service install.sh
# 版本单一来源契约:agent 经 -X xnc/agent/machineinfo.Version 注入(与
# Windows installer/build.ps1 同源);CLI 经 -X main.cliVersion。install.sh
# 在目标机从制品自读版本回填目录名——缺注入即装成 0.0.0-dev,一眼可见。
param(
    [Parameter(Mandatory = $true)][string]$Version,
    [ValidateSet("amd64", "arm64")][string]$Arch = "amd64"
)
$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot

$stage = Join-Path $env:TEMP "xnc-linux-stage-$Version-$Arch"
if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
New-Item -ItemType Directory -Force -Path "$stage\bin", "$stage\systemd" | Out-Null

function Invoke-GoBuild {
    param([string]$ModuleDir, [string]$Ldflags, [string]$Out, [string]$Pkg)
    Push-Location $ModuleDir
    try {
        $env:CGO_ENABLED = "0"; $env:GOOS = "linux"; $env:GOARCH = $Arch
        go build -trimpath -ldflags "-s -w $Ldflags" -o $Out $Pkg
        if ($LASTEXITCODE -ne 0) { throw "go build failed in $ModuleDir" }
    } finally {
        Remove-Item Env:CGO_ENABLED, Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
        Pop-Location
    }
}

Write-Host "== build xnc-agent + xnc (linux/$Arch, version $Version) =="
Invoke-GoBuild "$repo\src\agent" "-X xnc/agent/machineinfo.Version=$Version" (Join-Path $stage "bin\xnc-agent") "./cmd/xnc-agent"
Invoke-GoBuild "$repo\src\cli" "-X main.cliVersion=$Version" (Join-Path $stage "bin\xnc") "."

Copy-Item "$repo\deploy\linux\xnc-agent.service" "$stage\systemd\"
Copy-Item "$repo\deploy\linux\install.sh" "$stage\install.sh"

$out = "$repo\bin\xnc-linux-$Arch-$Version.tar.gz"
New-Item -ItemType Directory -Force -Path "$repo\bin" | Out-Null
if (Test-Path $out) { Remove-Item -Force $out }
# bsdtar（Windows 内置，绝对路径防被 Git Bash 的 GNU tar 抢占——后者把
# "C:" 解析成远程主机）：-C 进 staging 平铺打包，目标机解压即得
# bin/ systemd/ install.sh 三件。
$tarExe = "$env:SystemRoot\System32\tar.exe"
if (-not (Test-Path $tarExe)) { $tarExe = "tar" }
& $tarExe -czf $out -C $stage .
if ($LASTEXITCODE -ne 0) { throw "tar failed" }
Remove-Item -Recurse -Force $stage

$hash = (Get-FileHash $out -Algorithm SHA256).Hash.ToLower()
"$hash  $(Split-Path -Leaf $out)" | Out-File -Encoding ascii "$out.sha256"
Write-Host "built: $out"
Write-Host "sha256: $hash"
Write-Host "deploy: scp + tar xzf + sudo ./install.sh"
