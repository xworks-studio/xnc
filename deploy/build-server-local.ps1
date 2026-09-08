# build-server-local.ps1 - 本地构建 server 镜像并直推 SRV（不过 CI/GHCR）。
# 用法: powershell -File deploy\build-server-local.ps1 -ServerVersion 0.10.9
# 注意参数名不能用 -Version：powershell.exe -File 会把它吞作引擎参数
# （脚本收到空值，tar 名缺版本即此症状）。
# 前提: web/dist 已构建（npm run build）；SRV 凭据在 deploy/.env 的 SRV_*
# （远端步骤经 deploy/push_server_image.py 走 paramiko 密码认证——OpenSSH
# CLI 无法非交互输密码）。
# 流程: docker build（deploy/Dockerfile，仓库根上下文）→ docker save →
#   push_server_image.py（SFTP 上传 → docker load + tag → compose up）。
# 注意: SRV 侧 watchtower 已停用（本地镜像会被它的 GHCR 轮询降级）。
param(
  [Parameter(Mandatory)][string]$ServerVersion,
  # 跳过构建只推送（tar 已在时调试用）
  [switch]$PushOnly
)
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$image = "ghcr.io/xworks-studio/xnc-server:latest"
$tar = Join-Path $env:TEMP "xnc-server-push.tar"

if (-not $PushOnly) {
  Write-Output "== docker build (local) =="
  docker build -f "$root\deploy\Dockerfile" -t $image --build-arg XNC_VERSION=$ServerVersion "$root"
  if ($LASTEXITCODE -ne 0) { throw "docker build failed" }

  Write-Output "== docker save =="
  docker save -o $tar $image
  if ($LASTEXITCODE -ne 0) { throw "docker save failed" }
}
Write-Output ("tar: " + $tar + " (" + [math]::Round((Get-Item $tar).Length / 1MB, 1) + " MB)")

Write-Output "== push to SRV (paramiko) =="
py -3 (Join-Path $PSScriptRoot "push_server_image.py") $tar
if ($LASTEXITCODE -ne 0) { throw "push failed" }
