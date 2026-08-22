# dismiss-popup.ps1 - M1-Slice2 E2E helper: close a standing Windows
# Security / firewall prompt on the interactive console (session 1). The dev
# agent (fresh exe per run) triggers one; it floats ABOVE even TopMost
# overlays and its animated content pollutes the quiet scenarios (~1.2fps
# measured, runs 7/8). Process-MainWindowTitle matching does NOT see it
# (service-hosted window) — FindWindow by exact title + WM_CLOSE does.
# WM_CLOSE = the cancel/deny choice: harmless here because the agent is
# outbound-only and scripts/e2e-slice2.sh adds explicit allow rules anyway.
# Run via schtasks /RU <console user> /IT in session 1.
Add-Type -AssemblyName System.Windows.Forms
Add-Type @"
using System;
using System.Runtime.InteropServices;
public class Win32Popup {
  [DllImport("user32.dll", CharSet = CharSet.Unicode)]
  public static extern IntPtr FindWindow(string cls, string title);
  [DllImport("user32.dll")]
  public static extern bool PostMessage(IntPtr hWnd, uint msg, IntPtr wp, IntPtr lp);
  [DllImport("user32.dll")]
  public static extern bool IsWindow(IntPtr hWnd);
}
"@
$titles = @('Windows Security', 'Windows Defender Firewall', 'Windows Firewall')
$deadline = (Get-Date).AddSeconds(15)
do {
  $hit = $false
  foreach ($t in $titles) {
    for ($i = 0; $i -lt 30; $i++) {
      $h = [Win32Popup]::FindWindow($null, $t)
      if ($h -eq [IntPtr]::Zero) { break }
      $hit = $true
      [void][Win32Popup]::PostMessage($h, 0x0010, [IntPtr]::Zero, [IntPtr]::Zero)
      Start-Sleep -Milliseconds 150
    }
  }
  if (-not $hit) { break }
  Start-Sleep -Milliseconds 500
} while ((Get-Date) -lt $deadline)
