# notepad-type-test.ps1 - M1-Slice3 Task 5: notepad typing surface for the
# e2eviewer input script (session 1).
#
# Division of labor: Notepad cannot be told a save path headlessly
# pre-SaveAs, so THIS script only launches Notepad and pulls it to the
# foreground; the typing is driven from the VIEWER side (--input-script):
# TEXT -> Ctrl+S (KEY ControlLeft/KeyS sequence) -> the SaveAs filename box
# receives the full path typed as TEXT -> Enter. The e2e orchestrator
# picks the timestamped path C:\xnc-diag\typed-<ts>.txt, bakes it into the
# input script, and later fetches + diffs the file content (`xnc get`).
#
# Params:
#   -PrepareOnly   just launch + foreground Notepad and exit 0 (the wait
#                  for the saved file happens elsewhere, e.g. bash-side)
#   -WaitSec       how long to wait for the typed-*.txt file (default 120)
#   -ExpectPath    exact file to wait for (default: any typed-*.txt that
#                  appeared after this script started)
#   -CloseNotepad  close the Notepad we launched once the file is seen
#
# Run in session 1 via schtasks /RU <console user> /IT (see
# scripts/input-probe.ps1 header for the pattern).
param(
  [switch]$PrepareOnly,
  [int]$WaitSec = 120,
  [string]$ExpectPath = '',
  [switch]$CloseNotepad,
  [string]$SaveDir = 'C:\xnc-diag',
  # T6 robustness: open notepad WITH this file (created empty) so the
  # viewer's Ctrl+S saves in place — no SaveAs dialog round-trip.
  [string]$FilePath = ''
)

$ErrorActionPreference = 'Stop'
if (-not (Test-Path $SaveDir)) { New-Item -ItemType Directory -Path $SaveDir -Force | Out-Null }
$startUtc = [DateTimeOffset]::UtcNow

Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
public static class XncNotepadFocus {
  [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr hWnd);
}
"@

# 1) Launch Notepad (works for both classic notepad.exe and the Win11
#    Store app resolution via App Paths) and wait for its main window.
#    With -FilePath the file is created first so Ctrl+S saves in place.
if ($FilePath -ne '' -and -not (Test-Path $FilePath)) {
  New-Item -ItemType File -Path $FilePath -Force | Out-Null
}
$proc = if ($FilePath -ne '') {
  Start-Process -FilePath 'notepad.exe' -ArgumentList $FilePath -PassThru
} else {
  Start-Process -FilePath 'notepad.exe' -PassThru
}
$mainHwnd = [IntPtr]::Zero
for ($i = 0; $i -lt 300; $i++) {   # up to ~30s for first-run app activation
  if ($proc.HasExited) { Write-Error "notepad exited immediately"; exit 2 }
  try { $proc.Refresh() } catch {}
  if ($proc.MainWindowHandle -ne 0) { $mainHwnd = $proc.MainWindowHandle; break }
  Start-Sleep -Milliseconds 100
}
if ($mainHwnd -eq [IntPtr]::Zero) { Write-Error "notepad main window never appeared"; exit 2 }

# 2) Foreground it so viewer-typed keys land in the document, not elsewhere.
#    Retry: a fresh console session may briefly deny the foreground switch.
$fg = $false
for ($i = 0; $i -lt 10; $i++) {
  if ([XncNotepadFocus]::SetForegroundWindow($mainHwnd)) { $fg = $true; break }
  Start-Sleep -Milliseconds 200
}
Write-Output ("notepad prepared pid={0} hwnd={1} foreground={2}" -f $proc.Id, $mainHwnd, $fg)

if ($PrepareOnly) { exit 0 }

# 3) Wait for the saved file (typed by the viewer script through SaveAs).
$deadline = [DateTimeOffset]::UtcNow.AddSeconds($WaitSec)
while ([DateTimeOffset]::UtcNow -lt $deadline) {
  $hit = $null
  if ($ExpectPath -ne '' ) {
    if ((Test-Path $ExpectPath)) { $hit = Get-Item $ExpectPath }
  } else {
    # newest typed-*.txt created/modified after this script started
    $hit = Get-ChildItem -Path $SaveDir -Filter 'typed-*.txt' -File -ErrorAction SilentlyContinue |
      Where-Object { $_.LastWriteTimeUtc -ge $startUtc } |
      Sort-Object LastWriteTimeUtc -Descending | Select-Object -First 1
  }
  if ($hit) {
    Write-Output ("typed-file {0} bytes={1}" -f $hit.FullName, $hit.Length)
    if ($CloseNotepad) {
      [void]$proc.CloseMainWindow()
      Start-Sleep -Seconds 2
      if (-not $proc.HasExited) { Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue }
    }
    exit 0
  }
  Start-Sleep -Milliseconds 500
}
Write-Output ("timeout: no typed-*.txt in {0} after {1}s" -f $SaveDir, $WaitSec)
exit 1
