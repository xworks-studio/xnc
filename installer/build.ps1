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
    [string]$ISCC = "$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe"
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$bin = Join-Path $root "bin"
New-Item -ItemType Directory -Force -Path $bin | Out-Null

function Invoke-Step([string]$name, [scriptblock]$body) {
    Write-Output "build.ps1: $name"
    & $body
    if ($LASTEXITCODE -ne 0) { throw "$name failed (exit $LASTEXITCODE)" }
}

# 1) five binaries (version injected into the agent; see Makefile AGENT_LDFLAGS).
Invoke-Step "build xnc-agent.exe" {
    Push-Location (Join-Path $root "agent")
    try { go build -ldflags "-X xnc/agent/machineinfo.Version=$Version" -o (Join-Path $bin "xnc-agent.exe") ./cmd/xnc-agent }
    finally { Pop-Location }
}
Invoke-Step "build xnc.exe (cli)" {
    Push-Location (Join-Path $root "cli")
    try { go build -o (Join-Path $bin "xnc.exe") . }
    finally { Pop-Location }
}
Invoke-Step "build xnc-shell.exe (shellhost)" {
    Push-Location (Join-Path $root "shellhost")
    try { go build -o (Join-Path $bin "xnc-shell.exe") . }
    finally { Pop-Location }
}
# Native build.bat scripts must run via cmd from their own directory (they
# cd /d %~dp0 themselves, but Start-Process needs a sane working dir anyway).
Invoke-Step "build xnc-core.exe" {
    $p = Start-Process -FilePath "cmd.exe" -ArgumentList "/c build.bat" -WorkingDirectory (Join-Path $root "native\core") -NoNewWindow -Wait -PassThru
    $global:LASTEXITCODE = $p.ExitCode
}
Invoke-Step "build xnc-desktop.exe" {
    $p = Start-Process -FilePath "cmd.exe" -ArgumentList "/c build.bat" -WorkingDirectory (Join-Path $root "native\desktop") -NoNewWindow -Wait -PassThru
    $global:LASTEXITCODE = $p.ExitCode
}

# Version single-source check: the agent's self-reported version must equal
# the version being packaged.
$reported = (& (Join-Path $bin "xnc-agent.exe") --version | Out-String).Trim()
if ($reported -ne $Version) {
    throw "agent self-reported version '$reported' != packaging version '$Version' (rebuild with matching -ldflags)"
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
function Sign-Artifact([string]$file) {
    if (-not $signCert) { return }
    # 时间戳服务器按序回退（境内网络对 digicert 常不可达，曾致构建挂死 20
    # 分钟）；全部失败则免时间戳签名并告警——自签过渡期可接受，正式 CA 落
    # 地后改为强制。
    $sig = $null
    foreach ($ts in @("http://timestamp.digicert.com",
                      "http://timestamp.sectigo.com",
                      "http://timestamp.globalsign.com/tsa/r6/advanced")) {
        $sig = Set-AuthenticodeSignature -FilePath $file -Certificate $signCert -TimestampServer $ts
        if ($sig.Status -eq "Valid") { break }
        Write-Warning "build.ps1: timestamp server $ts failed ($($sig.StatusMessage)); trying next"
    }
    if ($sig.Status -ne "Valid") {
        Write-Warning "build.ps1: all timestamp servers failed - signing WITHOUT timestamp"
        $sig = Set-AuthenticodeSignature -FilePath $file -Certificate $signCert
        if ($sig.Status -ne "Valid") { throw "signing $file failed: $($sig.Status) $($sig.StatusMessage)" }
    }
    Write-Output "build.ps1: signed $(Split-Path -Leaf $file)"
}
foreach ($exe in @("xnc-agent.exe", "xnc.exe", "xnc-shell.exe", "xnc-core.exe", "xnc-desktop.exe")) {
    Sign-Artifact (Join-Path $bin $exe)
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
$suffix = ""
if ($Channel -eq "dev") { $suffix = "-dev" }
$name = "XNC-Setup$suffix-$Version.exe"
$setup = Join-Path $bin $name
if (-not (Test-Path $setup)) { throw "setup exe not found after ISCC: $setup" }
Sign-Artifact $setup
$hash = (Get-FileHash -Algorithm SHA256 $setup).Hash.ToLowerInvariant()
"$hash  $name" | Set-Content -Path "$setup.sha256" -Encoding ascii
Write-Output "installer: $setup"
Write-Output "sha256: $hash"
