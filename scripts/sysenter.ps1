# sysenter.ps1 - M2-Slice1 Task 1 unlock lever: Enter press from THIS process
# running as SYSTEM in the interactive session (tools/screendiag sessrun
# -session 1 -- powershell -File sysenter.ps1). Probe evidence 2026-08-22
# (XIAOXIN, Win11, labs empty password):
#   - LABS SendKeys (default AND /RL HIGHEST elevated) never reaches the
#     lock/logon UI (UIPI: System-integrity target);
#   - SYSTEM keybd_event/VK SendInput: the clock ignores it;
#   - SYSTEM scan-code SendInput (KEYEVENTF_SCANCODE) IS accepted when the
#     thread is bound to the current desktop, and the full down+up must ride
#     ONE SendInput batch (a split press loses the UP once the desktop
#     switches mid-press: injected=(1,0));
#   - press #1 (Default) switches to the secure logon UI and every further
#     SendInput from a Default-bound thread is refused (injected=0) - the
#     thread must REBIND via OpenInputDesktop(GENERIC_ALL)+SetThreadDesktop
#     first (SetThreadDesktop fails ERROR_BUSY on the PS main thread, so the
#     bind+inject runs on a fresh C# thread that never touched user32).
# This is exactly the desktop-switch input design the M2-Slice1 plan
# prescribes (spec: 输入线程主动 SetThreadDesktop 至当前 input desktop).
param(
  [int]$Count = 3,
  [int]$SpacingSec = 2,
  [string]$LogPath = 'C:\xnc-diag\uac-probe.log'
)
$dir = Split-Path -Parent $LogPath
if ($dir -and -not (Test-Path $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
namespace XNC {
public static class EnterInj {
  public static string LogPath = "";
  [DllImport("user32.dll", SetLastError=true)] static extern uint SendInput(uint n, byte[] p, int cb);
  [DllImport("user32.dll", SetLastError=true)] static extern IntPtr OpenInputDesktop(uint f, bool i, uint a);
  [DllImport("user32.dll", SetLastError=true)] static extern bool SetThreadDesktop(IntPtr h);
  [DllImport("user32.dll")] static extern bool CloseDesktop(IntPtr h);
  static void L(string s) {
    try { System.IO.File.AppendAllText(LogPath, "[sysenter " +
      System.DateTimeOffset.UtcNow.ToUnixTimeMilliseconds() + " local=" +
      System.DateTime.Now.ToString("yyyy-MM-ddTHH:mm:ss.fff") + "] " + s + "\r\n"); } catch {}
  }
  // One full Enter press: rebind to the CURRENT input desktop, then the
  // down+up pair in ONE SendInput batch. Returns injected event count.
  static int PressOnce() {
    IntPtr h = OpenInputDesktop(0, false, 0x10000000);  // GENERIC_ALL
    if (h == IntPtr.Zero) {
      L("bind open_failed err=" + System.Runtime.InteropServices.Marshal.GetLastWin32Error());
    } else {
      bool ok = SetThreadDesktop(h);
      int e = System.Runtime.InteropServices.Marshal.GetLastWin32Error();
      CloseDesktop(h);
      if (!ok) { L("bind set_failed err=" + e); return -2; }
      L("bind rebound");
    }
    byte[] b = new byte[80];  // two x64 INPUT structs
    WriteU32(b, 0, 1);  WriteU16(b, 10, 0x1C); WriteU32(b, 12, 8);        // down (SCANCODE)
    WriteU32(b, 40, 1); WriteU16(b, 50, 0x1C); WriteU32(b, 52, 8 | 2);    // up (SCANCODE|KEYUP)
    uint n = SendInput(2, b, 40);
    L("inject n=" + n);
    return (int)n;
  }
  static void WriteU32(byte[] a, int o, uint v) { a[o]=(byte)v; a[o+1]=(byte)(v>>8); a[o+2]=(byte)(v>>16); a[o+3]=(byte)(v>>24); }
  static void WriteU16(byte[] a, int o, ushort v) { a[o]=(byte)v; a[o+1]=(byte)(v>>8); }
  public static int Press() {
    int r = -99;
    System.Threading.Thread t = new System.Threading.Thread(
      new System.Threading.ThreadStart(delegate { r = PressOnce(); }));  // fresh thread: no user32 state
    t.Start(); t.Join(5000);
    return r;
  }
}
}
"@
[XNC.EnterInj]::LogPath = $LogPath
for ($i = 1; $i -le $Count; $i++) {
  $n = [XNC.EnterInj]::Press()
  $ms = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
  Add-Content -Path $LogPath -Value "[sysenter $ms local=$(Get-Date -Format 'yyyy-MM-ddTHH:mm:ss.fff')] enter $i/$Count rc=$n as=$([Environment]::UserName)"
  if ($i -lt $Count) { Start-Sleep -Seconds $SpacingSec }
}
exit 0
