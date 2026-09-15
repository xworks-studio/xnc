# build.ps1 - one-shot installer build (spec section 3): build the five
# binaries via the existing build paths, then compile installer/xnc.iss with
# ISCC (version + channel injection), and emit the setup exe sha256 to
# stdout plus a bin\<setup>.sha256 sidecar.
#
#   powershell -File installer\build.ps1 -Version 0.6.1 [-Channel stable|dev]
#
# Binary paths mirror the repo's existing build entries: agent = Makefile
# build-agent flags (version single source), cli/shellhost = the go build
# commands used by the Makefile load path, core/desktop = native/*/build.bat
# (self-locating vcvars64; artifacts land in ..\..\bin). make itself is NOT
# invoked (not assumed on PATH on Windows builders); flag drift between the
# agent build here and Makefile AGENT_LDFLAGS is caught by the --version
# self-check below. ISCC defaults to the per-user Inno Setup 6 install (not
# on PATH); override with -ISCC.
param(
    [string]$Version = "0.0.0-dev",
    [ValidateSet("stable", "dev")][string]$Channel = "stable",
    [string]$ISCC = "$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe",
    [switch]$ReuseNative,
    [switch]$SkipIdd
)

$ErrorActionPreference = "Stop"
# 脚本位于 <repo>/src/installer：$src = <repo>/src（各模块目录），
# $root = 仓库根（bin/ 产物池在此）。
$src = Split-Path -Parent $PSScriptRoot
$root = Split-Path -Parent $src
$bin = Join-Path $root "bin"
New-Item -ItemType Directory -Force -Path $bin | Out-Null

function Invoke-Step([string]$name, [scriptblock]$body) {
    Write-Output "build.ps1: $name"
    & $body
    if ($LASTEXITCODE -ne 0) { throw "$name failed (exit $LASTEXITCODE)" }
}

# 1) five binaries (five-binary version single-source, 2026-09-15 spec:
#    agent via ldflags as before; CLI/shellhost via -X main.<name>Version;
#    host via XNC_HOST_VERSION env + build.rs rerun-if-env-changed; core via
#    XNC_VERSION env -> build.bat /DXNC_VERSION).
Invoke-Step "build xnc-agent.exe" {
    Push-Location (Join-Path $src "agent")
    try { go build -ldflags "-X xnc/agent/machineinfo.Version=$Version" -o (Join-Path $bin "xnc-agent.exe") ./cmd/xnc-agent }
    finally { Pop-Location }
}
Invoke-Step "build xnc.exe (cli)" {
    Push-Location (Join-Path $src "cli")
    try { go build -ldflags "-X main.cliVersion=$Version" -o (Join-Path $bin "xnc.exe") . }
    finally { Pop-Location }
}
Invoke-Step "build xnc-shell.exe (shellhost)" {
    Push-Location (Join-Path $src "shellhost")
    try { go build -ldflags "-X main.shellVersion=$Version" -o (Join-Path $bin "xnc-shell.exe") . }
    finally { Pop-Location }
}
# Native build.bat scripts must run via cmd from their own directory (they
# cd /d %~dp0 themselves, but Start-Process needs a sane working dir anyway).
# -ReuseNative：bin 里已有 core/desktop 产物时跳过 MSVC（CI 缓存命中路径；
# 缓存键 = native/** 哈希，未变即有效）。release 构建不传此开关——发版
# 永远全量重建。
if (-not ($ReuseNative -and (Test-Path (Join-Path $bin "xnc-core.exe")))) {
    Invoke-Step "build xnc-core.exe" {
        # 五进制版本同源：build.bat 读 XNC_VERSION 编进 /DXNC_VERSION 宏
        #（common/version.h）。作用域内设置，构建后还原。
        $env:XNC_VERSION = $Version
        try {
            $p = Start-Process -FilePath "cmd.exe" -ArgumentList "/c build.bat" -WorkingDirectory (Join-Path $src "native\core") -NoNewWindow -Wait -PassThru
            $global:LASTEXITCODE = $p.ExitCode
        } finally {
            Remove-Item Env:XNC_VERSION -ErrorAction SilentlyContinue
        }
    }
} else { Write-Output "build.ps1: reuse cached xnc-core.exe" }
# RTV host（Rust，2026-09-08 重构替代 C++ xnc-desktop）：cargo release；
# 需 VCPKG_ROOT（x64-windows-static：ffmpeg[amf,nvcodec,qsv] + libyuv）与
# LIBCLANG_PATH（bindgen）。产物 crt-static 单文件，复制进 bin。
# -ReuseNative：bin 里已有产物时跳过 cargo（CI 缓存命中路径；缓存键 =
# host/** + third_party/scrap/** 哈希）。release 构建不传此开关——发版
# 永远全量重建。
if (-not ($ReuseNative -and (Test-Path (Join-Path $bin "xnc-host.exe")))) {
    if (-not $env:VCPKG_ROOT) { throw "xnc-host build needs VCPKG_ROOT (x64-windows-static with ffmpeg[amf,nvcodec,qsv] + libyuv)" }
    if (-not $env:LIBCLANG_PATH) { throw "xnc-host build needs LIBCLANG_PATH (bindgen)" }
    Invoke-Step "build xnc-host.exe (cargo release)" {
        Push-Location (Join-Path $src "host")
        try {
            # 五进制版本同源：option_env! 编译期取值，host/build.rs 的
            # rerun-if-env-changed 保证版本变更不命中陈旧缓存。
            $env:XNC_HOST_VERSION = $Version
            cargo build --release
            if ($LASTEXITCODE -ne 0) { throw "cargo build failed" }
            Copy-Item (Join-Path $PWD "target\release\xnc-host.exe") (Join-Path $bin "xnc-host.exe") -Force
        } finally {
            Remove-Item Env:XNC_HOST_VERSION -ErrorAction SilentlyContinue
            Pop-Location
        }
    }
} else { Write-Output "build.ps1: reuse cached xnc-host.exe" }

# IDD 虚拟显示器驱动（安装器可选组件 idd，src/third_party/xncidd，vendored
# RustDeskIddDriver + 微软 IddSample 基底）：WDK 工具集 msbuild（自定位
# vcvars64，同 native build.bat 模式），SignMode=off 后手工签名 dll（内嵌）
# + cat（目录）。UMDF 为用户态驱动，自签 + 安装器信任分发即可加载（无
# testmode，XIAOXIN 真机验证过）。WDK 构建目标缺失（CI dryrun / 无 WDK
# 机器）时告警跳过，绝不阻塞 exe/installer 构建；-SkipIdd 显式跳过。
$iddBuilt = $false
if (-not $SkipIdd) {
    $iddOut = Join-Path $bin "driver\xncidd"
    if (-not ($ReuseNative -and (Test-Path (Join-Path $iddOut "XncIdd.dll")))) {
        $iddSrc = Join-Path $src "third_party\xncidd"
        $wdkBuild = "${env:ProgramFiles(x86)}\Windows Kits\10\build"
        $vswhere = "${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe"
        $vcvars = ""
        if (Test-Path $vswhere) {
            $vcvars = (& $vswhere -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath 2>$null | Out-String).Trim()
        }
        if (-not $vcvars -and (Test-Path "C:\Program Files\Microsoft Visual Studio\2022\Community\VC\Auxiliary\Build\vcvars64.bat")) {
            $vcvars = "C:\Program Files\Microsoft Visual Studio\2022\Community"
        }
        if (-not (Test-Path $wdkBuild)) {
            Write-Warning "build.ps1: WDK build targets not found - skipping idd driver build (release builds need WDK)"
        } elseif (-not $vcvars -or -not (Test-Path (Join-Path $vcvars "VC\Auxiliary\Build\vcvars64.bat"))) {
            Write-Warning "build.ps1: VS2022 vcvars64 not found - skipping idd driver build"
        } else {
            Invoke-Step "build xncidd driver (msbuild+WDK)" {
                $sln = Join-Path $iddSrc "XncIdd.sln"
                $cmd = "call `"$(Join-Path $vcvars 'VC\Auxiliary\Build\vcvars64.bat')`" >nul 2>&1 && msbuild `"$sln`" /p:Configuration=Release /p:Platform=x64 /p:TargetVersion=Windows10 /p:SignMode=off /p:SpectreMitigation=false /m /v:minimal /nologo"
                & cmd.exe /c $cmd
                if ($LASTEXITCODE -ne 0) { throw "msbuild XncIdd.sln failed (exit $LASTEXITCODE)" }
            }
            # 驱动包 = 打过戳的 INF + dll + cat（同目录、INF CatalogFile 同名单）
            New-Item -ItemType Directory -Force -Path $iddOut | Out-Null
            $iddRel = Join-Path $iddSrc "x64\Release"
            Copy-Item (Join-Path $iddRel "XncIdd.dll") (Join-Path $iddOut "XncIdd.dll") -Force
            Copy-Item (Join-Path $iddRel "XncIddDriver.inf") (Join-Path $iddOut "XncIdd.inf") -Force
            Copy-Item (Join-Path $iddRel "XncIddDriver\xncidd.cat") (Join-Path $iddOut "XncIdd.cat") -Force
            $iddBuilt = $true
            Write-Output "build.ps1: idd driver staged to bin\driver\xncidd"
        }
    } else { Write-Output "build.ps1: reuse cached idd driver"; $iddBuilt = $true }
}

# Version single-source check (five-binary, 2026-09-15 spec): every packaged
# binary must self-report the version being packaged. CLI/shell/host/core
# speak `--version` (agent: existing flag; CLI: cobra; shellhost: stdlib
# flag; host: clap; core: wmain branch). Any mismatch fails the build —
# catches a missing ldflags/env injection AND stale cargo caches.
$versionTargets = @(
    @{ Exe = "xnc-agent.exe";  Flag = "--version" },
    @{ Exe = "xnc.exe";         Flag = "--version" },
    @{ Exe = "xnc-shell.exe";   Flag = "--version" },
    @{ Exe = "xnc-host.exe";    Flag = "--version" },
    @{ Exe = "xnc-core.exe";    Flag = "--version" }
)
foreach ($vt in $versionTargets) {
    # 取输出末段 token：agent/host/core/shell 输出裸版本号，CLI 的 cobra
    # --version 带 "xnc version " 前缀——统一按末段比对。
    $reported = (& (Join-Path $bin $vt.Exe) $vt.Flag | Out-String).Trim()
    if ($reported) { $reported = ($reported -split '\s+')[-1] }
    if ($reported -ne $Version) {
        throw "$($vt.Exe) self-reported version '$reported' != packaging version '$Version' (rebuild with matching version injection)"
    }
}

# Authenticode signing (self-signed interim, spec §13): sign the five exes
# BEFORE ISCC (the installer embeds them - installs land signed) and the
# setup exe AFTER ISCC; sha256 sidecar is computed last so it covers the
# signed artifact. Key material lives outside git: installer\codesign.pfx
# (gitignored) + password in env XNC_CODESIGN_PASSWORD (deploy/.env on dev
# machines). No pfx -> warn + unsigned build (never block keyless builders).
$signCert = $null
$pfxPath = Join-Path $PSScriptRoot "codesign.pfx"
if (Test-Path $pfxPath) {
    if (-not $env:XNC_CODESIGN_PASSWORD) { throw "codesign.pfx found but XNC_CODESIGN_PASSWORD is not set" }
    $signCert = [System.Security.Cryptography.X509Certificates.X509Certificate2]::new(
        $pfxPath, $env:XNC_CODESIGN_PASSWORD)
    Write-Output "build.ps1: signing with cert $($signCert.Subject)"
} else {
    Write-Warning "build.ps1: installer\codesign.pfx not found - building UNSIGNED"
}
# 时间戳服务器：构建前用系统 curl.exe（独立于 WinHTTP 栈）3 秒探测选定一
# 个可达者；全不可达则免时间戳（自签过渡期可接受，正式 CA 后改强制）。
# 注：Set-AuthenticodeSignature 走 WinHTTP，本机曾对其偶发无限挂起（curl
# 同址正常）——故签名包在 60 秒硬超时作业里，超时降级免时间戳，绝不卡死构建。
$tsaUrl = $null
if ($signCert) {
    foreach ($ts in @("http://timestamp.digicert.com",
                      "http://timestamp.sectigo.com",
                      "http://timestamp.globalsign.com/tsa/r6/advanced")) {
        $host2 = ([uri]$ts).Host
        $null = & curl.exe -s -m 3 -o NUL "http://$host2/" 2>$null
        if ($LASTEXITCODE -eq 0) { $tsaUrl = $ts; break }
    }
    if (-not $tsaUrl) { Write-Warning "build.ps1: no timestamp server reachable - signing WITHOUT timestamps" }
}
function Sign-Artifact([string]$file) {
    if (-not $signCert) { return }
    $sig = $null
    if ($tsaUrl) {
        $job = Start-Job -ScriptBlock {
            param($f, $pfx, $pw, $ts)
            $c = [System.Security.Cryptography.X509Certificates.X509Certificate2]::new($pfx, $pw)
            Set-AuthenticodeSignature -FilePath $f -Certificate $c -TimestampServer $ts
        } -ArgumentList $file, $pfxPath, $env:XNC_CODESIGN_PASSWORD, $tsaUrl
        if (Wait-Job $job -Timeout 60) {
            $sig = Receive-Job $job
        } else {
            Write-Warning "build.ps1: timestamp signing timed out (60s) - plain signature; disabling timestamps for this build"
            $script:tsaUrl = $null
        }
        Remove-Job $job -Force -ErrorAction SilentlyContinue
    }
    if (-not $sig -or $sig.Status -ne "Valid") {
        $sig = Set-AuthenticodeSignature -FilePath $file -Certificate $signCert
        if ($sig.Status -ne "Valid") { throw "signing $file failed: $($sig.Status) $($sig.StatusMessage)" }
        Write-Warning "build.ps1: $(Split-Path -Leaf $file) signed WITHOUT timestamp"
    } else {
        Write-Output "build.ps1: signed $(Split-Path -Leaf $file)"
    }
}
foreach ($exe in @("xnc-agent.exe", "xnc.exe", "xnc-shell.exe", "xnc-core.exe", "xnc-host.exe")) {
    Sign-Artifact (Join-Path $bin $exe)
}
# 驱动签名走 signtool（Set-AuthenticodeSignature 不产目录签名所需的
# 完整 cat 语义）：dll 内嵌签名 + cat 目录签名，同一 pfx/时间戳策略。
if ($iddBuilt) {
    function Sign-DriverArtifact([string]$file) {
        if (-not $signCert) { return }
        $signtool = Get-ChildItem "${env:ProgramFiles(x86)}\Windows Kits\10\bin" -Recurse -Filter "signtool.exe" -ErrorAction SilentlyContinue |
            Sort-Object FullName -Descending | Select-Object -First 1
        if (-not $signtool) { throw "signtool.exe not found in Windows Kits (cannot sign driver)" }
        $args = @("sign", "/fd", "SHA256", "/f", $pfxPath, "/p", $env:XNC_CODESIGN_PASSWORD)
        if ($tsaUrl) { $args += @("/tr", $tsaUrl, "/td", "SHA256") }
        $args += $file
        & $signtool.FullName $args
        if ($LASTEXITCODE -ne 0) { throw "signtool signing $file failed (exit $LASTEXITCODE)" }
        Write-Output "build.ps1: signed $(Split-Path -Leaf $file) (driver)"
    }
    Sign-DriverArtifact (Join-Path $iddOut "XncIdd.dll")
    Sign-DriverArtifact (Join-Path $iddOut "XncIdd.cat")
}

# VersionInfoVersion must be strictly numeric w.x.y.z ("0.6.1" -> "0.6.1.0",
# "0.0.0-dev" -> "0.0.0.0").
$numeric = ($Version -split "-")[0]
try { $parts = @($numeric -split "\." | ForEach-Object { [int]$_ }) } catch {
    throw "cannot derive a numeric version from '$Version'"
}
if ($parts.Count -gt 4) { $parts = $parts[0..3] }
while ($parts.Count -lt 4) { $parts += 0 }
$setupVersion = $parts -join "."

# 2) ISCC (relative paths in xnc.iss resolve from the installer dir).
# CertThumb: 签名构建时传指纹，安装器据此装卸本机证书信任（xnc.iss）。
if (-not (Test-Path $ISCC)) { throw "ISCC.exe not found at $ISCC (pass -ISCC <path>)" }
$thumb = if ($signCert) { $signCert.Thumbprint } else { "" }
Push-Location $PSScriptRoot
try {
    & $ISCC "/DVersion=$Version" "/DChannel=$Channel" "/DSetupVersion=$setupVersion" "/DCertThumb=$thumb" "xnc.iss"
    if ($LASTEXITCODE -ne 0) { throw "ISCC failed (exit $LASTEXITCODE)" }
} finally { Pop-Location }

# 3) artifact + sha256 (stdout for CI; sidecar for release metadata).
# 版本号已带 -dev 后缀（新版本规则：dev 迭代走 PATCH+-dev）时不再重复
# 频道后缀——否则产物名出现 XNC-Installer-dev-<v>-dev.exe。
$suffix = ""
if ($Channel -eq "dev" -and -not $Version.EndsWith("-dev")) { $suffix = "-dev" }
$name = "XNC-Installer$suffix-$Version.exe"
$setup = Join-Path $bin $name
if (-not (Test-Path $setup)) { throw "setup exe not found after ISCC: $setup" }
Sign-Artifact $setup
$hash = (Get-FileHash -Algorithm SHA256 $setup).Hash.ToLowerInvariant()
"$hash  $name" | Set-Content -Path "$setup.sha256" -Encoding ascii
Write-Output "installer: $setup"
Write-Output "sha256: $hash"
