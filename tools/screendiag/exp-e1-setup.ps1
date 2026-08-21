[Console]::OutputEncoding = [Text.Encoding]::UTF8
$a = New-ScheduledTaskAction -Execute 'cmd.exe' -Argument '/c C:\xnc\dxgiprobe.exe -seconds 6 -dump 1 -recreate -json > C:\xnc\e1.json 2> C:\xnc\e1.log'
$p = New-ScheduledTaskPrincipal -UserId 'LABS' -LogonType Interactive
Register-ScheduledTask -TaskName xncprobe-e1 -Action $a -Principal $p -Force | Out-Null
Start-ScheduledTask -TaskName xncprobe-e1
'e1 task registered and started'
