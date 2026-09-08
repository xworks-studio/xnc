# build-relay.ps1 — 交叉编译 xnc-relay 并部署到中继机(relay-plane T9)。
# 用法:
#   powershell deploy/build-relay.ps1 -PublicHost 47.96.83.132
#   powershell deploy/build-relay.ps1 -PublicHost 47.96.83.132 -SkipDeploy   # 只构建
# 流程:GOOS=linux 交叉编译(静态单文件) → deploy/push_relay.py(paramiko,
# 凭据读 deploy/.env 的 RELAY1_* 键)。服务器侧由 main 站先合入并部署
# (build-server-local.ps1)——relay 注册需要 /api/relay/connect 已存在。
param(
    [string]$Server = "wss://xnc.app/api/relay/connect",
    [Parameter(Mandatory = $true)][string]$PublicHost,
    [string]$Region = "cn-hangzhou",
    [int]$MaxSessions = 100,
    [int]$MaxMbpsOut = 500,
    [switch]$SkipDeploy
)
$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot

Write-Host "== build xnc-relay (linux/amd64 static) =="
Push-Location "$repo\relay"
try {
    $env:CGO_ENABLED = "0"; $env:GOOS = "linux"; $env:GOARCH = "amd64"
    go build -trimpath -ldflags "-s -w" -o "..\bin\xnc-relay-linux" ./cmd/xnc-relay
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
} finally {
    Remove-Item Env:CGO_ENABLED, Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
    Pop-Location
}
$out = "$repo\bin\xnc-relay-linux"
$hash = (Get-FileHash $out -Algorithm SHA256).Hash.ToLower()
Write-Host "built: $out sha256=$hash"

if ($SkipDeploy) { Write-Host "-SkipDeploy: done."; exit 0 }

Write-Host "== deploy to relay host =="
py -3 "$repo\deploy\push_relay.py" $out --server $Server --public-host $PublicHost `
    --region $Region --max-sessions $MaxSessions --max-mbps-out $MaxMbpsOut
if ($LASTEXITCODE -ne 0) { throw "push_relay.py failed" }
Write-Host "relay deployed. Approve at server if pending: PATCH /api/admin/relays/{id}"
