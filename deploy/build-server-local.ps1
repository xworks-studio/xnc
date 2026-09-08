# build-server-local.ps1 - 本地构建 server 镜像并直推 SRV（不过 CI/GHCR）。
# 用法: powershell -File deploy\build-server-local.ps1 -Version 0.10.9
# 前提: web/dist 已构建（npm run build）；SRV 凭据在 deploy/.env 的 SRV_*。
# 流程: docker build（deploy/Dockerfile，仓库根上下文）→ docker save →
#   SFTP 上传 → docker load + 打 compose 引用 tag → compose up -d --no-pull。
# 注意: SRV 侧 watchtower 已停用（本地镜像会被它的 GHCR 轮询降级）。
param([Parameter(Mandatory)][string]$Version)
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$image = "ghcr.io/xworks-studio/xnc-server:latest"

Write-Output "== docker build (local) =="
docker build -f "$root\deploy\Dockerfile" -t $image --build-arg XNC_VERSION=$Version "$root"
if ($LASTEXITCODE -ne 0) { throw "docker build failed" }

Write-Output "== docker save =="
$tar = Join-Path $env:TEMP "xnc-server-$Version.tar"
docker save -o $tar $image
if ($LASTEXITCODE -ne 0) { throw "docker save failed" }
Write-Output "saved: $tar ($([math]::Round((Get-Item $tar).Length/1MB,1)) MB)"

Write-Output "== upload + load on SRV =="
$envFile = Join-Path $PSScriptRoot "..\.env"
Get-Content $envFile | Where-Object { $_ -match '^SRV_' } | ForEach-Object {
  $k, $v = $_.split('=', 2); Set-Item -Path "Env:$k" -Value $v
}
$sftp = Join-Path $PSScriptRoot "push-image.sftp"
@"
open $env:SRV_SSH_USER@$env:SRV_HOST
put "$($tar -replace '\','/')" /tmp/xnc-server.tar
"@ | Set-Content $sftp -Encoding ASCII
& sftp -P $env:SRV_SSH_PORT -b $sftp
if ($LASTEXITCODE -ne 0) { throw "sftp upload failed" }
Remove-Item $sftp, $tar -Force -ErrorAction SilentlyContinue

ssh -p $env:SRV_SSH_PORT "$env:SRV_SSH_USER@$env:SRV_HOST" `
  "docker load -i /tmp/xnc-server.tar && docker tag $image $image && cd /opt/xnc/deploy && docker compose up -d --no-pull xnc-server && sleep 4 && curl -sk https://127.0.0.1/api/health -H 'Host: xnc.app'"
if ($LASTEXITCODE -ne 0) { throw "remote load/up failed" }
