# setres.ps1 - M2-Slice1 Task 2 live lever: change the PRIMARY display mode
# (Set-DisplayResolution does not exist on this Win11 client SKU - probed
# 2026-08-23). Run INTERACTIVE in session 1 (schtasks /RU LABS /IT), see
# e2e-slice3.sh start_probe for the task pattern. EnumDisplaySettings gives
# the REAL mode (a non-DPI-aware Screen query reports 1024x768 on the
# 2880x1800@200% node - T1 finding).
param(
  [int]$Width = 0,        # 0 = query only
  [int]$Height = 0,
  [string]$LogPath = 'C:\xnc-diag\setres.log'
)
$dir = Split-Path -Parent $LogPath
if ($dir -and -not (Test-Path $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
function Log($s) {
  $ms = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
  Add-Content -Path $LogPath -Value "[setres $ms local=$(Get-Date -Format 'yyyy-MM-ddTHH:mm:ss.fff')] $s"
}
Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
namespace XNC {
public static class Res {
  [StructLayout(LayoutKind.Sequential, CharSet=CharSet.Unicode)]
  public struct DEVMODE {
    [MarshalAs(UnmanagedType.ByValTStr, SizeConst=32)] public string dmDeviceName;
    public ushort dmSpecVersion, dmDriverVersion, dmSize, dmDriverExtra;
    public uint dmFields;
    public int dmPositionX, dmPositionY;
    public uint dmDisplayOrientation, dmDisplayFixedOutput;
    public short dmColor, dmDuplex, dmYResolution, dmTTOption, dmCollate;
    [MarshalAs(UnmanagedType.ByValTStr, SizeConst=32)] public string dmFormName;
    public ushort dmLogPixels;
    public uint dmBitsPerPel, dmPelsWidth, dmPelsHeight, dmDisplayFlags, dmDisplayFrequency;
    public uint dmICMMethod, dmICMIntent, dmMediaType, dmDitherType;
    public uint dmReserved1, dmReserved2, dmPanningWidth, dmPanningHeight;
  }
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern bool EnumDisplaySettingsW(string dn, int mode, ref DEVMODE dm);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int ChangeDisplaySettingsW(ref DEVMODE dm, int flags);
  public static DEVMODE Current() {
    DEVMODE dm = new DEVMODE(); dm.dmSize = (ushort)Marshal.SizeOf(typeof(DEVMODE));
    EnumDisplaySettingsW(null, -1 /*ENUM_CURRENT_SETTINGS*/, ref dm);
    return dm;
  }
  public static int Set(uint w, uint h) {
    DEVMODE dm = Current();
    dm.dmFields = 0x80000 /*DM_PELSWIDTH*/ | 0x100000 /*DM_PELSHEIGHT*/;
    dm.dmPelsWidth = w; dm.dmPelsHeight = h;
    return ChangeDisplaySettingsW(ref dm, 0x1 /*CDS_UPDATEREGISTRY*/);
  }
}
}
"@
$cur = [XNC.Res]::Current()
Log ("query w=" + $cur.dmPelsWidth + " h=" + $cur.dmPelsHeight + " bpp=" + $cur.dmBitsPerPel + " freq=" + $cur.dmDisplayFrequency + " as=" + [Environment]::UserName + " sess=" + [Environment]::SessionId)
"CUR {0} {1}" -f $cur.dmPelsWidth, $cur.dmPelsHeight
if ($Width -gt 0 -and $Height -gt 0) {
  $rc = [XNC.Res]::Set([uint32]$Width, [uint32]$Height)
  Log ("set w=$Width h=$Height rc=$rc (0=DISP_CHANGE_SUCCESSFUL)")
  Start-Sleep -Milliseconds 1500
  $after = [XNC.Res]::Current()
  Log ("after w=" + $after.dmPelsWidth + " h=" + $after.dmPelsHeight)
  "SET rc=$rc now=$($after.dmPelsWidth)x$($after.dmPelsHeight)"
}
exit 0
