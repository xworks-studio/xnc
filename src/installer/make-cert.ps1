# make-cert.ps1 - 一次性生成自签代码签名证书（过渡方案，spec §13；正式
# Authenticode CA 落地后由其取代）。产物：codesign.cer（公钥，入库供机群
# 导入信任）+ codesign.pfx（私钥，gitignored，勿外传）；随机密码追加到
# deploy/.env 的 XNC_CODESIGN_PASSWORD。
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$cer = Join-Path $PSScriptRoot "codesign.cer"
$pfx = Join-Path $PSScriptRoot "codesign.pfx"

$cert = New-SelfSignedCertificate -Type CodeSigningCert `
    -Subject "CN=XNC Code Signing, OU=Release Engineering, O=XWorks Studio, C=CN" `
    -KeyUsage DigitalSignature -KeySpec Signature `
    -FriendlyName "XNC Code Signing (XWorks Studio)" `
    -CertStoreLocation "Cert:\CurrentUser\My" `
    -NotAfter (Get-Date).AddYears(3)
Export-Certificate -Cert $cert -FilePath $cer | Out-Null

$chars = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789".ToCharArray()
$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
$bytes = New-Object byte[] 32; $rng.GetBytes($bytes)
$pw = -join ($bytes | ForEach-Object { $chars[$_ % $chars.Length] })
$sec = ConvertTo-SecureString $pw -AsPlainText -Force
Export-PfxCertificate -Cert $cert -FilePath $pfx -Password $sec | Out-Null

$envFile = Join-Path $root "deploy\.env"
$line = "XNC_CODESIGN_PASSWORD=$pw"
if (Select-String -Path $envFile -Pattern "^XNC_CODESIGN_PASSWORD=" -Quiet) {
    (Get-Content $envFile) -replace "^XNC_CODESIGN_PASSWORD=.*", $line | Set-Content $envFile -Encoding utf8
} else {
    Add-Content $envFile $line
}
Write-Output "cert:   $cer (public, commit)"
Write-Output "key:    $pfx (PRIVATE, gitignored)"
Write-Output "passwd: XNC_CODESIGN_PASSWORD appended to deploy\.env"
Write-Output "thumb:  $($cert.Thumbprint)"
