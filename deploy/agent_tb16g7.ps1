# Deploy xnc-agent to NODE_MAIN (LABS-TB16G7) and start it in background.
# Usage: powershell -NoProfile -File deploy/agent_tb16g7.ps1 -Server https://control.xnc.app -Token <enrollment-token>
param(
    [Parameter(Mandatory = $true)] [string] $Server,
    [Parameter(Mandatory = $true)] [string] $Token,
    [string] $LocalExe = "bin/xnc-agent.exe",  # run from repo root, or pass -LocalExe
    [string] $RemoteDir = "C:\xnc"
)

$ErrorActionPreference = "Stop"
if (-not (Test-Path $LocalExe)) { throw "agent exe not found: $LocalExe" }

$session = New-PSSession -ComputerName LABS-TB16G7
try {
    Invoke-Command -Session $session -ScriptBlock {
        param($dir)
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
    } -ArgumentList $RemoteDir

    Copy-Item -Path $LocalExe -Destination "$RemoteDir\xnc-agent.exe" -ToSession $session -Force

    # NOTE: processes started inside a PS-Remoting session die with the session
    # (WSMan job object). The durable path is the XNCAgent windows service.
    Invoke-Command -Session $session -ScriptBlock {
        param($dir, $server, $token)
        $svc = Get-Service XNCAgent -ErrorAction SilentlyContinue
        if ($svc) {
            # refresh binary then restart service
            Restart-Service XNCAgent -Force
            "service restarted"
        } else {
            $out = & "$dir\xnc-agent.exe" install "--server=$server" "--token=$token" "--state-dir=$dir" 2>&1
            "install output: $out"
        }
        (Get-Service XNCAgent).Status
    } -ArgumentList $RemoteDir, $Server, $Token

    Start-Sleep -Seconds 6
    Invoke-Command -Session $session -ScriptBlock {
        param($dir)
        "== service =="
        Get-Service XNCAgent | Select-Object Status, Name | Format-Table -AutoSize | Out-String
        "== process =="
        Get-Process xnc-agent -ErrorAction SilentlyContinue |
            Select-Object Id, ProcessName, StartTime | Format-Table -AutoSize | Out-String
    } -ArgumentList $RemoteDir
}
finally {
    Remove-PSSession $session
}
