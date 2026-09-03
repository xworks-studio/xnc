# Dev topology (M1-Slice2): dev server + coturn on LABS-DEV, console-run dev agent on XIAOXIN

One page, copy-paste ready. Fully isolated from production: the dev agent never
touches the XNCAgent service, its state lives only in `%TEMP%\xnc-dev-agent-<pid>`
(removed on exit), and it talks to a throwaway dev server — not control.xnc.app.

```
LABS-DEV (docker)                          LABS-XIAOXIN
┌─────────────────────────────┐            ┌────────────────────────────────┐
│ xnc-server  127.0.0.1:8080  │◄─ WS ──────│ xnc-agent.exe run-dev-console  │
│             0.0.0.0:18080   │  (LAN)     │  (schtasks, session 1, LABS)   │
│ coturn      :3478 tcp/udp   │◄─ TURN ────│  state: %TEMP%\xnc-dev-agent-* │
│             :49160-49200/udp│  (T4/T5)   │  prod XNCAgent service: untouched│
│ postgres    (internal)      │            └────────────────────────────────┘
└─────────────────────────────┘
```

## 1. Bring up the dev stack (LABS-DEV)

`deploy/.env` must contain the usual vars plus:

```
XNC_DEV_EXTERNAL_IP=<LABS-DEV LAN IP, e.g. 192.168.1.12>   # advertised by coturn
```

```bash
cd deploy && docker compose -f docker-compose.yml -f docker-compose.dev.yml \
    -f docker-compose.turn-dev.yml up -d --build
curl -s http://127.0.0.1:8080/api/health          # {"status":"ok",...}
curl -s http://<LAN-IP>:18080/api/health          # LAN-reachable check
```

`docker-compose.turn-dev.yml` adds: coturn (static lt-cred dev user
`xncdev` / `xncdev-secret`, realm `xnc.dev`, non-TLS dev TURN), server port
`0.0.0.0:18080 -> 8080` (LAN) alongside the dev overlay's `127.0.0.1:8080`, and
`XNC_HEARTBEAT_TIMEOUT=45s` (dev default 10s flaps a 30s-heartbeat agent offline).

## 2. Dev admin JWT + enrollment token (LABS-DEV)

Bootstrap admin comes from `XNC_ADMIN_EMAIL/XNC_ADMIN_PASSWORD` in `deploy/.env`
(created on first boot; `docker compose down -v` resets the whole dev DB).

```bash
cd deploy
JWT=$(curl -s -X POST http://127.0.0.1:8080/api/auth/login \
  -H 'Content-Type: application/json' \
  -d "{\"email\":\"$XNC_ADMIN_EMAIL\",\"password\":\"$XNC_ADMIN_PASSWORD\"}" \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
# enrollment token: reuse across runs (maxUses 50, 30d); each dev-agent run
# enrolls as a NEW node (fresh identity per run by design)
curl -s -X POST http://127.0.0.1:8080/api/clusters/default/enrollment-tokens \
  -H "Authorization: Bearer $JWT" -H 'Content-Type: application/json' \
  -d '{"ttl":"720h","maxUses":50}' | sed -n 's/.*"token":"\([^"]*\)".*/\1/p'
```

> ⚠ Secret visibility (dev-only): the enrollment token — and the e2e scripts'
> `--desktop-core-secret-hex` / core `--smoke-secret` — are stored in plaintext
> inside `schtasks /TR` task definitions and `C:\xnc-dev\*.cmd` scripts.
> Acceptable for this dev topology only; production paths use the SCM + stdin
> secret channel (M2).

## 3. Run the dev agent on XIAOXIN (via prod `xnc exec`)

Deploy the freshly built agent (build: `cd agent && go build -o ../bin/xnc-agent.exe ./cmd/xnc-agent`),
then run it as an interactive scheduled task in session 1 — survives the exec
return, lands on the console desktop (needed by later slice tasks), no SCM.

```bash
XNC=./bin/xnc.exe; NODE=LABS-XIAOXIN; LAN=http://192.168.1.12:18080; TOK=<enroll-token>

$XNC exec $NODE 'New-Item -ItemType Directory -Force -Path C:\xnc-dev | Out-Null'
$XNC put $NODE C:/Users/LABS/Desktop/XNC/bin/xnc-agent.exe C:\xnc-dev\xnc-agent.exe
$XNC exec $NODE "schtasks /Create /F /TN xnc-dev-agent /TR \"C:\xnc-dev\xnc-agent.exe run-dev-console --server $LAN --token $TOK --name XIAOXIN-DEV --log-file C:\xnc-dev\dev-agent.log\" /SC ONCE /ST 23:59 /RU LABS /IT"
$XNC exec $NODE 'schtasks /Run /TN xnc-dev-agent'
$XNC exec $NODE 'Get-Content C:\xnc-dev\dev-agent.log -Tail 5'   # expect "control connection ready"
```

`run-dev-console`: foreground process, Ctrl+C = clean exit (state dir removed),
no SCM interaction, DPAPI(LOCAL_MACHINE)-protected identity inside the temp dir.
Every run is a fresh identity — the enrollment token is always required.

## 4. Verify against the dev server

> Always target the **LAN URL** (`http://<LAN-IP>:18080`) for session-creating
> ops (exec/shell/screen/...): the server builds the agent-side session WS dial
> URL from the request's Host header — `127.0.0.1:8080` would send the agent
> dialing its own loopback.

```bash
$XNC --server $LAN --token $JWT node list            # XIAOXIN-DEV  online
$XNC --server $LAN --token $JWT exec XIAOXIN-DEV hostname   # -> LABS-XIAOXIN
```

(curl equivalent: `GET /api/nodes/` with `Authorization: Bearer $JWT`.)

## 5. Isolation proof (after dev agent is up)

```bash
$XNC node list                                   # prod list unchanged, LABS-XIAOXIN online
$XNC exec $NODE 'Get-Service XNCAgent'           # Status: Running (prod service untouched)
$XNC exec $NODE 'Get-ChildItem C:\Users\LABS\AppData\Local\Temp\xnc-dev-agent-*'  # dev state (only here)
$XNC exec $NODE 'Get-Item C:\xnc\identity.json'  # prod identity LastWriteTime predates the dev run
```

Two `xnc-agent.exe` processes coexist: prod service (SYSTEM, `--state-dir=C:\xnc`,
session 0) and dev agent (LABS, temp state, session 1).

## 6. Teardown

```bash
# scheduled tasks: dev agent, dev core (SYSTEM console pipe server, e2e-slice2.sh §5),
# and the one-shot popup-dismiss helper (left behind by any e2e-slice2.sh run;
# the script's own exit trap also deletes it — fix-round-1 residue sweep)
$XNC exec $NODE 'schtasks /End /TN xnc-dev-agent; schtasks /Delete /F /TN xnc-dev-agent'
$XNC exec $NODE 'schtasks /End /TN xnc-dev-core; schtasks /Delete /F /TN xnc-dev-core'
$XNC exec $NODE 'schtasks /Delete /F /TN xnc-dismiss-popup'
# firewall allow rules created per dev-agent exe (e2e-slice2.sh console hygiene;
# the exit trap deletes them too — listed here for hard-killed runs)
$XNC exec $NODE 'netsh advfirewall firewall delete rule name="xnc-dev-agent-dev"; netsh advfirewall firewall delete rule name="xnc-dev-agent-dev-out"'
# path-scoped sweep in case a task was hard-killed (never touches C:\xnc prod):
$XNC exec $NODE 'Get-Process xnc-agent,xnc-core,xnc-desktop -ErrorAction SilentlyContinue | Where-Object {$_.Path -like "C:\xnc-dev*"} | Stop-Process -Force; exit 0'
# hard-killed runs (task kill / reboot) leave temp dirs behind — sweep:
$XNC exec $NODE 'Remove-Item C:\Users\LABS\AppData\Local\Temp\xnc-dev-agent-* -Recurse -Force -ErrorAction SilentlyContinue; exit 0'
cd deploy && docker compose -f docker-compose.yml -f docker-compose.dev.yml \
    -f docker-compose.turn-dev.yml down          # add -v to also reset the dev DB/nodes
```

Notes:
- Dev-server nodes accumulate (one per dev-agent run): harmless; `down -v` resets.
- TURN credentials for T4/T5: `turn:<LAN-IP>:3478?transport=tcp`, user
  `xncdev` / `xncdev-secret` (static dev lt-cred; REST-cred is M2).
- Desktop sessions (T5): the turn-dev compose sets the server's
  `XNC_TURN_URLS/XNC_TURN_USERNAME/XNC_TURN_CREDENTIAL` from the same static
  user — `POST /api/nodes/{id}/desktop` returns the config in the `turn` field
  and relays it in SESSION_OPEN params. Without it the endpoint answers
  503 `TURN_UNCONFIGURED`. One desktop session per node; idle (no signaling
  frames) for 5 min auto-closes (viewer may simply re-POST).

## 生产端口清单(control.xnc.app / SRV,2026-08-24 retro P2#11 文档化)

安全组需开以下全部,否则对应功能失效(本次事故:3478 未开 → STUN/TURN
全盲;coturn 配置见 deploy/turnserver.conf,模板由 deploy_srv.py `up` 渲染):

| 端口 | 协议 | 用途 |
|---|---|---|
| 443 | TCP | HTTPS/WSS — Caddy 反代 xnc-server(web + /api) |
| 3478 | TCP+UDP | coturn STUN/TURN 监听(lt-cred 静态凭据,relay-only,无 TLS) |
| 49160-49200 | UDP | coturn TURN relay 分配段(min-port/max-port;不开则 relay 候选不可达) |

自检:`py deploy/deploy_srv.py verify` 含 STUN binding 探测(UDP+TCP
127.0.0.1:3478,期望 0x0101 应答)——本机端口通不代表安全组通,公网侧用
`curl https://<domain>/api/health`(verify 已含)。
