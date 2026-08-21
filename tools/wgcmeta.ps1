# wgcmeta.ps1 — 从 winmd 提取 WinRT 接口 GUID + 方法（vtable）顺序。
# 用法: powershell -NoProfile -File tools\wgcmeta.ps1
$ErrorActionPreference = "Stop"

$files = @(
    "C:\Windows\System32\WinMetadata\Windows.Graphics.winmd",
    "C:\Windows\System32\WinMetadata\Windows.Foundation.winmd"
)

foreach ($f in $files) {
    $asm = [Reflection.Assembly]::LoadFile($f)
    Write-Output ("### " + (Split-Path $f -Leaf))
    foreach ($t in $asm.GetTypes()) {
        if (-not $t.IsInterface) { continue }
        if ($t.Name -notmatch 'CaptureItem|CaptureFramePool|CaptureFrame|CaptureSession|DxgiInterfaceAccess|Direct3DSurface|Direct3DDevice$') { continue }
        Write-Output ("== " + $t.FullName + " [" + $t.GUID.ToString() + "]")
        $i = 3
        foreach ($m in $t.GetMethods()) {
            $ps = ($m.GetParameters() | ForEach-Object { $_.ParameterType.Name + " " + $_.Name }) -join ", "
            Write-Output ("  $i $($m.Name)($ps)")
            $i++
        }
    }
}
