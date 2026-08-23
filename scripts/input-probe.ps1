# input-probe.ps1 - M1-Slice3 Task 5: session-1 input injection probe.
#
# Samples every ~100ms and appends one CSV row per sample:
#
#   t,cursorX,cursorY,keyA,keyCtrl,keyShift,numlk,capslk
#
#   t         Unix epoch ms ([DateTimeOffset]::UtcNow) - correlates with
#             viewer-side timestamps and the e2e orchestrator clock
#   cursorX/Y GetCursorPos screen pixels (the coordinates SendInput
#             MOUSEEVENTF_ABSOLUTE|VIRTUALDESK maps into)
#   keyA/keyCtrl/keyShift  0/1 = GetAsyncKeyState(VK) & 0x8001:
#             high bit (key down right now) OR low bit (pressed since the
#             previous probe call). The low bit catches taps shorter than
#             one sample period; the high bit catches held keys (stuck-key
#             cleanup gate). Note TEXT injection is KEYEVENTF_UNICODE and
#             intentionally does NOT flip the A key state - only scancode
#             injection ({"op":"key","code":"KeyA",...}) does.
#   numlk/capslk  0/1 = GetKeyState(VK_NUMLOCK/VK_CAPITAL) & 1 toggle
#             state - LOCK op (type 6) evidence: the T6 NumLock ruling
#             reads these columns (E0-prefixed vs plain 0x45 injection).
#
# Writes to C:\xnc-diag\input-probe.csv (overwrites per run), flushed per
# line so a hard task kill still leaves every sampled row on disk.
#
# Run in session 1 (interactive desktop - SendInput lands there):
#   schtasks /Create /F /TN xnc-input-probe /TR 'powershell -ExecutionPolicy
#     Bypass -File C:\xnc-dev\input-probe.ps1 -DurationSec 40' /SC ONCE
#     /ST 23:59 /RU <console user> /IT ; schtasks /Run /TN xnc-input-probe
# (same pattern as scripts/e2e-slice2.sh overlay tasks; /End force-stops).
param(
  [int]$DurationSec = 30,
  [string]$OutFile = 'C:\xnc-diag\input-probe.csv',
  [int]$SampleMs = 100
)

$ErrorActionPreference = 'Stop'
$dir = Split-Path -Parent $OutFile
if ($dir -and -not (Test-Path $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }

Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
public static class XncInputProbe {
  [StructLayout(LayoutKind.Sequential)]
  public struct POINT { public int X; public int Y; }
  [DllImport("user32.dll")] public static extern bool SetProcessDPIAware();
  [DllImport("user32.dll")] public static extern bool GetCursorPos(out POINT p);
  [DllImport("user32.dll")] public static extern short GetAsyncKeyState(int vKey);
  [DllImport("user32.dll")] public static extern short GetKeyState(int vKey);
}
"@

# Physical px (run-1 T6 finding): with display scaling != 100% a non-DPI-aware
# GetCursorPos virtualizes coordinates (XIAOXIN: 200% -> probe saw 384 for a
# 768-px stream move, half of every injected coordinate). The capture/stream
# space is physical; opt this process in so GetCursorPos matches it.
[void][XncInputProbe]::SetProcessDPIAware()

# VK codes: 0x41 'A', 0x11 VK_CONTROL, 0x10 VK_SHIFT.
function KeyBit([int]$vk) {
  $s = [XncInputProbe]::GetAsyncKeyState($vk)
  if ((-not $s) -or (($s -band 0x8001) -eq 0)) { return 0 }
  return 1
}
# Lock toggle state (low bit of GetKeyState). 0x90 VK_NUMLOCK, 0x14 VK_CAPITAL.
function LockBit([int]$vk) {
  $s = [XncInputProbe]::GetKeyState($vk)
  if ((-not $s) -or (($s -band 1) -eq 0)) { return 0 }
  return 1
}

$writer = New-Object System.IO.StreamWriter($OutFile, $false)
$writer.AutoFlush = $true  # flush per line: rows survive a hard /End kill
try {
  $writer.WriteLine('t,cursorX,cursorY,keyA,keyCtrl,keyShift,numlk,capslk')
  $deadline = [DateTimeOffset]::UtcNow.AddSeconds($DurationSec)
  $samples = 0
  while ([DateTimeOffset]::UtcNow -lt $deadline) {
    $iterStart = [DateTimeOffset]::UtcNow
    $p = New-Object XncInputProbe+POINT
    [void][XncInputProbe]::GetCursorPos([ref]$p)
    $row = '{0},{1},{2},{3},{4},{5},{6},{7}' -f $iterStart.ToUnixTimeMilliseconds(), $p.X, $p.Y, (KeyBit 0x41), (KeyBit 0x11), (KeyBit 0x10), (LockBit 0x90), (LockBit 0x14)
    $writer.WriteLine($row)
    $samples++
    # hold the ~100ms cadence: sleep the unspent remainder of the period
    $spent = [int]([DateTimeOffset]::UtcNow - $iterStart).TotalMilliseconds
    $rest = $SampleMs - $spent
    if ($rest -gt 0) { Start-Sleep -Milliseconds $rest }
  }
  Write-Output ("input-probe: {0} samples over {1}s -> {2}" -f $samples, $DurationSec, $OutFile)
}
finally {
  $writer.Close()
}
