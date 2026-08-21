[Console]::OutputEncoding = [Text.Encoding]::UTF8
# E2: SYSTEM 令牌 + 交互会话（schtasks /RU SYSTEM /IT → 落在 console 会话）
schtasks /create /tn xncprobe-e2 /tr "cmd /c C:\xnc\dxgiprobe.exe -seconds 6 -dump 1 -recreate -json > C:\xnc\e2.json 2> C:\xnc\e2.log" /sc once /st 23:59 /ru SYSTEM /it /f | Out-Null
schtasks /run /tn xncprobe-e2 | Out-Null
'e2 task registered and started'
