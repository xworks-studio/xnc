Add-Type -MemberDefinition '[DllImport("user32.dll")] public static extern bool SetCursorPos(int x, int y); [DllImport("user32.dll")] public static extern void mouse_event(uint f, uint dx, uint dy, uint data, UIntPtr extra);' -Name U -Namespace W
[W.U]::SetCursorPos(800, 600) | Out-Null
[W.U]::mouse_event(1, 5, 5, 0, [UIntPtr]::Zero)  # MOUSEEVENTF_MOVE relative
Start-Sleep -Milliseconds 200
[W.U]::mouse_event(1, -5, -5, 0, [UIntPtr]::Zero)
"wake injected"
