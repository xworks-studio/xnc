# static-overlay.ps1 - M1-Slice2 E2E helper: a fullscreen TopMost form that
# repaints a large counter every -TickMs, making the captured desktop
# DETERMINISTICALLY QUIET (no change-driver storms, no reliance on whatever
# happens to animate on the console today).
#
# Why a slow tick and not a solid still frame: a perfectly static screen
# produces ZERO encoder output by design (pipeline §7.4 static = no encode,
# and the real Mf encoder skips identical input even with ForceIDR armed —
# T6 run-6 finding), so an armed on-demand IDR (sub_join / connect / PLI)
# only surfaces on the next REAL frame. A 3s tick gives scenario ② a sparse
# stream (~15 AU / 45s) and scenarios ③④ a bounded IDR latency (≤ tick);
# scenario ④ reruns the overlay with -TickMs 1000 for the ≤2s PLI budget.
#
# Run via schtasks /RU <console user> /IT in session 1; schtasks /End
# force-closes it early.
param(
  [int]$Seconds = 110,
  [int]$TickMs = 3000   # 0 = solid still frame (perfectly static: zero
                        # encoder output by design §7.4; only for quietness
                        # gates — armed on-demand IDRs CANNOT surface)
)
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$f = New-Object System.Windows.Forms.Form
$f.BackColor = [System.Drawing.Color]::FromArgb(32, 40, 48)
$f.FormBorderStyle = [System.Windows.Forms.FormBorderStyle]::None
# Manual full-SCREEN bounds, NOT Maximized: a maximized borderless window
# still respects the work area and leaves the taskbar visible — its tray
# (WiFi icon animating from our own stream traffic) + clock kept producing
# ~1.2fps of changes through every quiet run. TopMost + full screen bounds
# covers the taskbar too (run-9 finding).
$f.StartPosition = 'Manual'
$b = [System.Windows.Forms.Screen]::PrimaryScreen.Bounds
$f.Location = $b.Location
$f.Size = $b.Size
$f.TopMost = $true
$f.KeyPreview = $true
$f.Add_KeyDown({ $f.Close() })
$f.Add_Click({ $f.Close() })
$font = New-Object System.Drawing.Font('Consolas', 160)
$brush = [System.Drawing.Brushes]::Orange
$script:tick = 0
$f.Add_Paint({
  param($s, $e)
  $e.Graphics.DrawString("xnc-quiet $script:tick", $font, $brush, 80, 80)
})
$f.Show()
$f.Activate()
$deadline = (Get-Date).AddSeconds($Seconds)
while ((Get-Date) -lt $deadline -and -not $f.IsDisposed) {
  [System.Windows.Forms.Application]::DoEvents()
  if ($TickMs -gt 0) {
    Start-Sleep -Milliseconds $TickMs
    $script:tick++
    $f.Invalidate()
    [System.Windows.Forms.Application]::DoEvents()
  } else {
    Start-Sleep -Milliseconds 500
  }
}
$f.Close()
