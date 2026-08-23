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

## Agent: XNCAgentDev (Task 2, landed separately)

Not in this script; see Task 2. XNCCoreDev picks up the `depend=
XNCAgentDev` ordering automatically when the agent dev service exists at
install time (re-run -Install-time config, or `sc config XNCCoreDev
depend= XNCAgentDev` by hand).

## Verify / isolate (XIAOXIN, via prod xnc)

```bash
XNC=./bin/xnc.exe; NODE=LABS-XIAOXIN
$XNC exec $NODE 'Get-Service XNCCoreDev'                 # Running
$XNC exec $NODE 'C:\xnc-dev\xnc-shell-probe.exe --core-pipe xnc-core-dev --secret 746573742d706970652d736563726574 --token user --profile CMD --command "echo svc-ok"'   # pipe connectable
$XNC exec $NODE 'Stop-Service XNCCoreDev; Start-Sleep 3; Get-Service XNCCoreDev'  # Stopped; drain lines in xnc-core-service.log
$XNC exec $NODE 'Get-Service XNCAgent'                  # prod untouched, Running
```
