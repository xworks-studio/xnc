# Dev services (M2-Slice3): XNCCoreDev / XNCAgentDev on XIAOXIN

Dev topology service install for the M2-Slice3 double-service gate. Isolated
from production by service NAME (`XNCCore*Dev` vs `XNCAgent`) and path
(`C:\xnc-dev\` vs `C:\xnc\`); the dev core keeps no state directory (its
"state" is the live capture children, scoped by handle) and logs to
`C:\xnc-dev\xnc-core-service.log` (service mode reopens stderr there).

## Core: XNCCoreDev (this slice, Task 1)

```powershell
# on XIAOXIN, elevated (xnc exec runs SYSTEM):
powershell -ExecutionPolicy Bypass -File C:\xnc-dev\install-dev-services.ps1 -Install
#   -> sc create XNCCoreDev binPath= "C:\xnc-dev\xnc-core.exe --service XNCCoreDev
#        --pipe-name \\.\pipe\xnc-core-dev --smoke-secret <hex>"
#      LocalSystem, Automatic, depends on XNCAgentDev when present; auto-started.
powershell -File C:\xnc-dev\install-dev-services.ps1 -Uninstall   # stop + sc delete
```

- Credentials: dev-only plaintext binPath (`--smoke-secret`), the documented
  dev-topology precedent; the production SCM credential channel is a later
  slice. The service-mode env fallback (`XNC_CORE_PIPE_NAME` /
  `XNC_CORE_SECRET_HEX`) avoids the plaintext when preferred.
- Service mode opens the SAS gate (M2-Slice3 ruling; server capability is
  the first gate). Console mode keeps `--allow-sas` default-deny.
- SCM stop drains exactly like console Ctrl+C: accept loop exits, capture
  and shell children are TerminateProcess'd scoped by stored handle, WTS
  monitor stops, `STOPPED` is reported. Known limitation: no core->desktop
  DRAIN handshake exists, so the desktop child's input ReleaseAll-at-exit
  does not run on a service stop (Slice-3 follow-up ledger note).

## Agent: XNCAgentDev (Task 2)

```powershell
# on XIAOXIN, elevated (xnc exec runs SYSTEM):
# 1) put the new build at C:\xnc-dev\xnc-agent.exe (xnc put)
# 2) install (server/token/enrollment flags ride the service argv):
powershell -File C:\xnc-dev\install-dev-agent.ps1 -Install -Server http://<dev-server>:8080 -Token <enroll-token>
#   -> xnc-agent.exe install --server … --state-dir C:\ProgramData\XNCAgentDev
#        --service-name XNCAgentDev --desktop-core-pipe \\.\pipe\xnc-core-dev
#        --desktop-core-secret-hex <dev hex> [--token …]
#      LocalSystem, Automatic (Go mgr installer), auto-started; also re-points
#      XNCCoreDev's depend= at XNCAgentDev.
powershell -File C:\xnc-dev\install-dev-agent.ps1 -Uninstall   # stop + delete
```

- Isolation: distinct service NAME (`XNCAgentDev`), distinct STATE DIR
  (`C:\ProgramData\XNCAgentDev` — own identity.json/agent-service.log), dev
  binary path `C:\xnc-dev\`. Production `XNCAgent` service, `C:\xnc\` and
  `C:\ProgramData\XNCAgent` are never touched (verify block below).
- Desktop credentials: `--desktop-core-pipe`/`--desktop-core-secret-hex` in
  the service argv map to `XNC_DESKTOP_CORE_PIPE`/`…_SECRET_HEX` inside the
  service process (SCM has no interactive env) — the same dev-only plaintext
  binPath precedent as XNCCoreDev's `--smoke-secret`. Secret never logged.
- Session-intent self-healing (Task 2): while a desktop session's server-side
  logical session is alive, capture loss (desktoppipe Done, core RPC failure,
  logoff killing the desktop child) is auto-healed — re-StartCapture with
  backoff 1s/2s/4s…cap 30s within a 90s window; viewer gets
  `{"type":"state","code":"reattached"}` and the stream resumes on the same
  WebRTC connection (no renegotiation). Give-up: session closed or window
  expired → `{"type":"state","code":"capture_lost"}`.
- Default install (no `--service-name`) remains `XNCAgent`/`ProgramData\XNCAgent`
  — production behavior unchanged.

## Verify / isolate (XIAOXIN, via prod xnc)

```bash
XNC=./bin/xnc.exe; NODE=LABS-XIAOXIN
$XNC exec $NODE 'Get-Service XNCCoreDev'                 # Running
$XNC exec $NODE 'C:\xnc-dev\xnc-shell-probe.exe --core-pipe xnc-core-dev --secret 746573742d706970652d736563726574 --token user --profile CMD --command "echo svc-ok"'   # pipe connectable
$XNC exec $NODE 'Stop-Service XNCCoreDev; Start-Sleep 3; Get-Service XNCCoreDev'  # Stopped; drain lines in xnc-core-service.log
$XNC exec $NODE 'Get-Service XNCAgent'                  # prod untouched, Running
$XNC exec $NODE 'Get-Service XNCAgentDev'               # Task 2: Running (dev agent)
$XNC exec $NODE 'Stop-Service XNCAgentDev; Start-Sleep 5; Start-Service XNCAgentDev; Get-Service XNCAgentDev'  # survives Stop/Start; node back online
$XNC exec $NODE 'Get-Item C:\ProgramData\XNCAgent\identity.json | Select LastWriteTime'  # prod state untouched
$XNC exec $NODE 'Get-ChildItem C:\ProgramData\XNCAgentDev'  # dev state isolated
```
