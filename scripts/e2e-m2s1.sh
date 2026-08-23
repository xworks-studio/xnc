#!/usr/bin/env bash
# e2e-m2s1.sh - M2-Slice1 desktop & session reliability acceptance gate on
# LABS-XIAOXIN (unified CaptureReset recovery, GDI ladder, gated SAS, input
# continuity across desktop switches).
#
# Topology: identical to scripts/e2e-slice3.sh (dev-topology.md authoritative):
#   LABS-DEV (docker): dev xnc-server + coturn, LAN http://$LAN_IP:18080
#   LABS-XIAOXIN: dev core (SYSTEM console pipe server, up to 3 generations
#                below), dev agent (run-dev-console, session 1), xnc-desktop
#                spawned via the 0x0100 StartCapture RPC.
#   this machine: e2eviewer (server mode) + session-1 lever tasks on XIAOXIN.
#
# Gates (all live, numbers honest, failures recorded with conclusions):
#   1 UAC        : session1 non-elevated trigger -> consent.exe up (secure
#                  desktop observed) -> viewer rides the T2 reset path
#                  (STATE recovering -> capture_rebuilt + IDR + frames resume
#                  <=2s) -> input KEY Enter approves the default-focused
#                  "Yes" -> elevated xnc-uac-child.exe observed via tasklist
#                  + elevation marker (net session). Enter relies on UAC
#                  default focus; if it does not land, consent-kill (cancel)
#                  backstop + gate marked partial with conclusion.
#   2 lock + SAS : SoftwareSASGeneration=1 + core --allow-sas; viewer sas op
#                  -> security screen up (LogonUI process evidence, T4: hr
#                  is NOT delivery proof) -> stream stays connected ->
#                  sysenter unlock (SYSTEM sessrun scan-code Enter, empty
#                  password) -> desktop restored, frames resume, generation
#                  bumps recorded node-side.
#   3 resolution : 2880x1800 <-> 1920x1080 via setres.ps1 -> DISPLAY_CHANGED
#                  x2, generation strictly increasing, new frame <= 2s after
#                  each event (T2 regression, scripted now).
#   4 downgrade  : core generation with XNC_FORCE_DXGI_HEALTH=50 ->
#                  STATE backend_changed(gdi), frames continue <= 15 fps
#                  (tick overlay drives change), ~30s DXGI probe ->
#                  backend_changed(dxgi) + IDR <= 2s (T3 regression).
#   5 SAS gate   : core generation WITHOUT --allow-sas -> secure_attention
#                  ok=false SAS_DENIED + node sas_audit line.
#   6 input reg  : after gates 1+2 cycles: slice3 sample (3 moves + keyA +
#                  text->notepad save) still passes = input continuity
#                  across desktop switches.
#
# Core generations: gen1 clean (gate 5) -> gen2 --allow-sas + SAS policy
# (gates 1/2/6/3) -> gen3 XNC_FORCE_DXGI_HEALTH=50 (gate 4) -> gen4 clean
# (final state for the owner). Registry SoftwareSASGeneration is restored to
# 0 in the teardown trap EVEN ON FAILURE (red line).
#
# Usage: scripts/e2e-m2s1.sh [node] [artifacts-dir]
#   node            default LABS-XIAOXIN (prod name for xnc exec/put/get)
#   artifacts-dir   default .superpowers/sdd/2026-08-23-m2-slice1-desktop-reliability/e2e
#   env CONSOLE_USER  console-session user for session-1 tasks (default LABS)
#
# The dev core + dev agent + dev stack stay running at exit for the owner
# browser check; teardown lives in scripts/dev-topology.md §6.
#
# Exit 0 = all gates pass; 1 = any gate failed (numbers recorded regardless).

set -euo pipefail

NODE="${1:-LABS-XIAOXIN}"
CONSOLE_USER="${CONSOLE_USER:-LABS}"
DEV_NODE_NAME="XIAOXIN-DEV"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
ROOT_W="$(cd "$ROOT" && pwd -W)"
ART="${2:-$ROOT_W/.superpowers/sdd/2026-08-23-m2-slice1-desktop-reliability/e2e}"
XNC="$ROOT/bin/xnc.exe"
E2E="$ROOT/bin/e2eviewer.exe"
SESSRUN="$ROOT/bin/sessrun.exe"
PROBE_TASK="xnc-m2s1-probe"
NOTEPAD_TASK="xnc-m2s1-notepad"
UAC_TASK="xnc-m2s1-uac"
SETRES_TASK="xnc-m2s1-setres"
OVERLAY_TASK="xnc-m2s1-overlay"
CORE_TASK="xnc-m2s1-core"
AGENT_TASK="xnc-m2s1-agent"
REMOTE_DIR='C:\xnc-dev'
CORE_PIPE='\\.\pipe\xnc-core-m2s1'
DIAG='C:\xnc-diag'
SAS_REG='HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System'

say() { printf '\n=== %s ===\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
gate() { # gate <name> <0|1 pass> <detail>
  if [ "$2" -eq 1 ]; then
    printf 'GATE PASS  %-34s %s\n' "$1" "$3"
  else
    printf 'GATE FAIL  %-34s %s\n' "$1" "$3"
    GATE_FAIL=1
  fi
}
json_num()  { { grep -oE "\"$2\": *[0-9]+"  "$1" | head -1 | sed 's/.*: *//'; } || true; }
json_bool() { { grep -oE "\"$2\": *(true|false)" "$1" | head -1 | sed 's/.*: *//'; } || true; }

dev_node_id() { # id of the ONLINE node named <name>(-N)?, else nothing
  { curl -sf -H "Authorization: Bearer $JWT" "$LAN/api/nodes/" 2>/dev/null \
    | grep -oE '\{[^}]*\}' | grep -E "\"name\":\"$1(-[0-9]+)?\"" | grep -F '"status":"online"' \
    | grep -oE '"id": *"[^"]*"' | head -1 | sed 's/.*: *"//;s/"$//'; } || true
}
dev_node_online_count() {
  { curl -sf -H "Authorization: Bearer $JWT" "$LAN/api/nodes/" 2>/dev/null \
    | grep -oE '\{[^}]*\}' | grep -E "\"name\":\"$1(-[0-9]+)?\"" | grep -cF '"status":"online"'; } || true
}

# Session-1 interactive task helpers (slice3 pattern).
run_task() { # run_task <TN> <TR-powershell-args>
  "$XNC" exec "$NODE" "schtasks /Create /F /TN $1 /TR 'powershell -ExecutionPolicy Bypass $2' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT" >/dev/null
  "$XNC" exec "$NODE" "schtasks /Run /TN $1" >/dev/null
}
fetch() { # fetch <remote-under-C:\xnc-diag> -> $ART/<name>
  "$XNC" get "$NODE" "$DIAG\\$1" "$ART/$1" >/dev/null 2>&1
}

GATE_FAIL=0
mkdir -p "$ART"
ORIG_W=""; ORIG_H=""; RES_CHANGED=0

cleanup() {
  # 1. registry red line FIRST: SoftwareSASGeneration back to 0 even on failure.
  "$XNC" exec "$NODE" "reg add \"$SAS_REG\" /v SoftwareSASGeneration /t REG_DWORD /d 0 /f | Out-Null; exit 0" >/dev/null 2>&1 || true
  # 2. resolution restore if a switch landed mid-run.
  if [ -n "$ORIG_W" ] && [ "$RES_CHANGED" = "1" ]; then
    run_task "$SETRES_TASK" "-File C:\xnc-dev\setres.ps1 -Width $ORIG_W -Height $ORIG_H" || true
  fi
  # 3. scoped kills ONLY (ledger rule: image+session/path scoped, never bare):
  #    hanging UAC prompt (consent-kill = cancel, sanctioned), our elevated
  #    child, notepad. The dev core/agent stay for the owner.
  "$XNC" exec "$NODE" 'Get-Process consent -ErrorAction SilentlyContinue | Stop-Process -Force; taskkill /IM xnc-uac-child.exe /F >$null 2>&1; taskkill /IM notepad.exe /F >$null 2>&1; exit 0' >/dev/null 2>&1 || true
  # 4. never leave the console locked (gate-2 mid-death).
  "$XNC" exec "$NODE" 'if (@(Get-Process -Name LogonUI -ErrorAction SilentlyContinue).Count -gt 0) { C:\xnc-diag\sessrun.exe -session 1 -- powershell -NoProfile -ExecutionPolicy Bypass -File C:\xnc-dev\sysenter.ps1 -Count 4 -SpacingSec 2 -LogPath C:\xnc-diag\m2s1-sysenter.log }; exit 0' >/dev/null 2>&1 || true
  # 5. task cleanup.
  for t in "$PROBE_TASK" "$NOTEPAD_TASK" "$UAC_TASK" "$SETRES_TASK" "$OVERLAY_TASK" xnc-dismiss-popup; do
    "$XNC" exec "$NODE" "schtasks /End /TN $t; schtasks /Delete /F /TN $t; exit 0" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT INT TERM

# --- 0. Preflight -----------------------------------------------------------

say "Preflight"
[ -f "$ROOT/deploy/.env" ] || fail "deploy/.env missing (gitignored; see dev-topology.md §1)"
set -a; . "$ROOT/deploy/.env"; set +a
LAN_IP="${XNC_DEV_EXTERNAL_IP:-192.168.1.12}"
LAN="http://$LAN_IP:18080"
echo "dev LAN URL: $LAN  (node=$NODE dev-node=$DEV_NODE_NAME)"

# --- 1. Build everything fresh ----------------------------------------------

say "Build: cli + agent + e2eviewer + sessrun + native (core/desktop, selftests)"
(cd "$ROOT/cli" && go build -o "../bin/xnc.exe" .)
(cd "$ROOT/agent" && go build -o "../bin/xnc-agent.exe" ./cmd/xnc-agent)
go build -C "$ROOT" -o "$E2E" ./tools/e2eviewer
(cd "$ROOT/tools/screendiag" && go build -o "../../bin/sessrun.exe" ./cmd/sessrun)
[ -x "$E2E" ] || fail "e2eviewer build"
[ -x "$SESSRUN" ] || fail "sessrun build"
(cd "$ROOT/native/core" && cmd //c build.bat selftest)
(cd "$ROOT/native/desktop" && cmd //c build.bat selftest)

# --- 2. Dev stack rebuild ----------------------------------------------------

say "Dev stack up (docker, slice2 overlay)"
COMPOSE="docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.dev.yml -f deploy/docker-compose.slice2.yml"
(cd "$ROOT" && $COMPOSE up -d --build)
for i in $(seq 1 60); do curl -sf "$LAN/api/health" >/dev/null && break; sleep 2; done
curl -sf "$LAN/api/health" >/dev/null || fail "dev server not healthy at $LAN"

say "Dev admin login + enrollment token"
JWT=$(curl -sf -X POST "$LAN/api/auth/login" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$XNC_ADMIN_EMAIL\",\"password\":\"$XNC_ADMIN_PASSWORD\"}" \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$JWT" ] || fail "admin login failed (check XNC_ADMIN_* in deploy/.env)"
ETOK=$(curl -sf -X POST "$LAN/api/clusters/default/enrollment-tokens" \
  -H "Authorization: Bearer $JWT" -H 'Content-Type: application/json' \
  -d '{"ttl":"2h","maxUses":5}' | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$ETOK" ] || fail "enrollment token create failed"

# --- 3. Stop previous dev agent/core BEFORE overwriting their exes -----------

say "Stop any previous dev agent and wait for it to drop offline"
"$XNC" exec "$NODE" "schtasks /End /TN $AGENT_TASK; schtasks /Delete /F /TN $AGENT_TASK; exit 0" >/dev/null 2>&1 || true
"$XNC" exec "$NODE" 'Get-Process xnc-agent -ErrorAction SilentlyContinue | Where-Object {$_.Path -like "C:\xnc-dev*"} | Stop-Process -Force; exit 0' >/dev/null 2>&1 || true
for i in $(seq 1 45); do
  [ "$(dev_node_online_count "$DEV_NODE_NAME")" -eq 0 ] && break
  sleep 2
done

say "Stop any previous dev core (path-scoped) + session-1 desktop child (image+session scoped)"
stop_core() {
  "$XNC" exec "$NODE" "schtasks /End /TN $CORE_TASK; schtasks /Delete /F /TN $CORE_TASK; exit 0" >/dev/null 2>&1 || true
  "$XNC" exec "$NODE" 'Get-Process xnc-core -ErrorAction SilentlyContinue | Where-Object {$_.Path -like "C:\xnc-dev*"} | Stop-Process -Force; taskkill /IM xnc-desktop.exe /FI "SESSION eq 1" /F >$null 2>&1; exit 0' >/dev/null 2>&1 || true
  sleep 2
}
stop_core

# --- 4. Deploy dev binaries + levers, start core gen1 + agent -----------------

say "Deploy agent + core + desktop + levers to $NODE ($REMOTE_DIR)"
"$XNC" exec "$NODE" "New-Item -ItemType Directory -Force -Path C:\xnc-dev,$DIAG | Out-Null; Copy-Item C:\Windows\System32\cmd.exe C:\xnc-dev\xnc-uac-child.exe -Force; reg add \"$SAS_REG\" /v SoftwareSASGeneration /t REG_DWORD /d 0 /f | Out-Null; exit 0" >/dev/null
"$XNC" put "$NODE" "$ROOT_W/bin/xnc-agent.exe"    "$REMOTE_DIR\\xnc-agent.exe"
"$XNC" put "$NODE" "$ROOT_W/bin/xnc-core.exe"     "$REMOTE_DIR\\xnc-core.exe"
"$XNC" put "$NODE" "$ROOT_W/bin/xnc-desktop.exe"  "$REMOTE_DIR\\xnc-desktop.exe"
"$XNC" put "$NODE" "$ROOT_W/bin/sessrun.exe"      "$DIAG\\sessrun.exe"
"$XNC" put "$NODE" "$ROOT_W/scripts/sysenter.ps1"        "$REMOTE_DIR\\sysenter.ps1"
"$XNC" put "$NODE" "$ROOT_W/scripts/setres.ps1"          "$REMOTE_DIR\\setres.ps1"
"$XNC" put "$NODE" "$ROOT_W/scripts/m2s1-uac.ps1"        "$REMOTE_DIR\\m2s1-uac.ps1"
"$XNC" put "$NODE" "$ROOT_W/scripts/input-probe.ps1"     "$REMOTE_DIR\\input-probe.ps1"
"$XNC" put "$NODE" "$ROOT_W/scripts/notepad-type-test.ps1" "$REMOTE_DIR\\notepad-type-test.ps1"
"$XNC" put "$NODE" "$ROOT_W/scripts/static-overlay.ps1"  "$REMOTE_DIR\\static-overlay.ps1"
"$XNC" put "$NODE" "$ROOT_W/scripts/dismiss-popup.ps1"   "$REMOTE_DIR\\dismiss-popup.ps1"

CORE_SECRET_HEX=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
start_core() { # start_core <gen-tag> [XNC_VAR=value | flag]...
  # .cmd body: env `set` lines first, then the exe line (env inherited by the
  # xnc-desktop the core spawns - the forced-health lever rides this).
  local tag="$1" a; shift
  {
    printf '@echo off\r\n'
    for a in "$@"; do case "$a" in XNC_*=*) printf 'set %s\r\n' "$a";; esac; done
    printf 'C:\\xnc-dev\\xnc-core.exe --console --smoke-secret %s --pipe-name %s' "$CORE_SECRET_HEX" "$CORE_PIPE"
    for a in "$@"; do case "$a" in XNC_*=*) ;; *) printf ' %s' "$a";; esac; done
    printf ' > C:\\xnc-dev\\core.log 2>&1\r\n'
  } > "$ART/core-$tag.cmd"
  "$XNC" put "$NODE" "$ART/core-$tag.cmd" "$REMOTE_DIR\\core-dev.cmd"
  "$XNC" exec "$NODE" "schtasks /Create /F /TN $CORE_TASK /TR 'C:\xnc-dev\core-dev.cmd' /SC ONCE /ST 23:59 /RU SYSTEM" >/dev/null
  "$XNC" exec "$NODE" "schtasks /Run /TN $CORE_TASK" >/dev/null
  sleep 2
}
start_agent() {
  printf '@echo off\r\nC:\\xnc-dev\\xnc-agent.exe run-dev-console --server %s --token %s --name %s --log-file C:\\xnc-dev\\dev-agent.log --desktop-core-pipe %s --desktop-core-secret-hex %s >> C:\\xnc-dev\\dev-agent-console.log 2>&1\r\n' \
    "$LAN" "$ETOK" "$DEV_NODE_NAME" "$CORE_PIPE" "$CORE_SECRET_HEX" > "$ART/agent-dev.cmd"
  "$XNC" put "$NODE" "$ART/agent-dev.cmd" "$REMOTE_DIR\\agent-dev.cmd"
  "$XNC" exec "$NODE" "schtasks /Create /F /TN $AGENT_TASK /TR 'C:\xnc-dev\agent-dev.cmd' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT /RL HIGHEST" >/dev/null
  "$XNC" exec "$NODE" "schtasks /Run /TN $AGENT_TASK" >/dev/null
}

say "Start dev core GEN1 (clean, NO --allow-sas) + dev agent on $NODE"
start_core gen1
start_agent
"$XNC" exec "$NODE" 'netsh advfirewall firewall delete rule name="xnc-dev-agent-dev" >$null; netsh advfirewall firewall add rule name="xnc-dev-agent-dev" dir=in program="C:\xnc-dev\xnc-agent.exe" action=allow >$null; netsh advfirewall firewall add rule name="xnc-dev-agent-dev-out" dir=out program="C:\xnc-dev\xnc-agent.exe" action=allow >$null; exit 0' >/dev/null 2>&1 || true
"$XNC" exec "$NODE" "schtasks /Create /F /TN xnc-dismiss-popup /TR 'powershell -ExecutionPolicy Bypass -File C:\xnc-dev\dismiss-popup.ps1' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT" >/dev/null 2>&1 || true

say "Wait for $DEV_NODE_NAME online on the dev server"
NODE_ID=""
for i in $(seq 1 90); do
  NODE_ID=$(dev_node_id "$DEV_NODE_NAME")
  [ -n "$NODE_ID" ] && break
  sleep 2
done
if [ -z "$NODE_ID" ]; then
  "$XNC" exec "$NODE" 'Get-Content C:\xnc-dev\dev-agent.log -Tail 25; exit 0' > "$ART/agent-start-failure.log" 2>&1 || true
  fail "$DEV_NODE_NAME never came online (agent log tail captured)"
fi
echo "dev node: $DEV_NODE_NAME ($NODE_ID)"

# Geometry (moves are stream-logical = physical px, DPI-aware query).
GEOMPS='Add-Type -AssemblyName System.Windows.Forms; Add-Type -Namespace X -Name D -MemberDefinition "[DllImport(\"user32.dll\")] public static extern bool SetProcessDPIAware();"; [void][X.D]::SetProcessDPIAware(); $b=[System.Windows.Forms.Screen]::PrimaryScreen.Bounds; "GEOM {0} {1}" -f $b.Width,$b.Height; exit 0'
SCREEN=$("$XNC" exec "$NODE" "$GEOMPS" 2>/dev/null | tr -d '\r' || true)
SW=$(printf '%s' "$SCREEN" | grep -oE 'GEOM [0-9]+ [0-9]+' | grep -oE '[0-9]+' | head -1)
SH=$(printf '%s' "$SCREEN" | grep -oE 'GEOM [0-9]+ [0-9]+' | grep -oE '[0-9]+' | tail -1)
SW="${SW:-1920}"; SH="${SH:-1080}"
ORIG_W="$SW"; ORIG_H="$SH"
echo "primary screen: $SCREEN (moves in stream-logical px)"

# Clock skew XIAOXIN vs THIS box (probe CSV alignment, gate 6).
L1=$(date +%s%3N)
R=$("$XNC" exec "$NODE" '[DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds(); exit 0' 2>/dev/null | tr -d '\r' | grep -E '^[0-9]+$' | head -1)
L2=$(date +%s%3N)
if [ -n "$R" ]; then SKEW=$(( R - (L1 + L2) / 2 )); else SKEW=0; fi
echo "clock skew (remote-local) = ${SKEW}ms"

viewer() { # viewer <out.json> <out.log> <duration> <extra args...>
  local outj="$1" outl="$2" dur="$3"; shift 3
  "$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
    --duration "$dur" --expect-first-frame-ms 8000 --keyframe-retry-after 1.5s \
    --json "$@" > "$outj" 2> "$outl" || true
}

# --- 5. GATE 5: SAS gate WITHOUT --allow-sas (core gen1) ----------------------

say "Gate 5: secure_attention denied without --allow-sas (core gen1, default)"
G5_RC=0
"$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
  --duration 12s --expect-first-frame-ms 8000 --sas --json \
  > "$ART/g5-sas.json" 2> "$ART/g5-sas.log" || G5_RC=$?
cat "$ART/g5-sas.json"
G5_OK=$(node -e 'const j=require(process.argv[1]);console.log(j.sas&&j.sas.sent&&!j.sas.ok&&j.sas.code==="SAS_DENIED"?1:0)' "$ART/g5-sas.json" 2>/dev/null || echo 0)
G5_DET=$(node -e 'const j=require(process.argv[1]);console.log(j.sas?`ok=${j.sas.ok} code=${j.sas.code} rtt=${j.sas.rttMs}ms`:"no sas field")' "$ART/g5-sas.json" 2>/dev/null || echo "?")
"$XNC" exec "$NODE" 'Get-Content C:\xnc-dev\core.log; exit 0' > "$ART/core-gen1.log" 2>&1 || true
G5_AUDIT=$(grep -c 'sas_audit action=sas_denied' "$ART/core-gen1.log" || true)
gate "5-sas-denied" "$G5_OK" "viewer: $G5_DET (ok=false + SAS_DENIED is the observation, not a failure)"
gate "5-sas-audit-logged" "$([ "${G5_AUDIT:-0}" -ge 1 ] && echo 1 || echo 0)" "node core.log sas_audit action=sas_denied x${G5_AUDIT:-0}"

# --- 6. Core gen2: --allow-sas + SoftwareSASGeneration=1 ----------------------

say "Core GEN2: --allow-sas + SoftwareSASGeneration=1 (registry restored to 0 in trap)"
stop_core
"$XNC" exec "$NODE" "reg add \"$SAS_REG\" /v SoftwareSASGeneration /t REG_DWORD /d 1 /f | Out-Null; reg query \"$SAS_REG\" /v SoftwareSASGeneration; exit 0" > "$ART/sas-policy.log" 2>&1 || true
cat "$ART/sas-policy.log"
start_core gen2 --allow-sas

# --- 7. GATE 1: UAC secure desktop + Enter approval --------------------------

run_gate1() { # sets G1_* vars; one attempt
  local TS=$(date +%s)
  "$XNC" exec "$NODE" "schtasks /Create /F /TN $UAC_TASK /TR 'powershell -ExecutionPolicy Bypass -File C:\xnc-dev\m2s1-uac.ps1' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT" >/dev/null
  cat > "$ART/g1-steps.json" <<EOF
[ {"op":"lease"},
  {"op":"wait","ms":8000},
  {"op":"key","code":"AltLeft","down":true},
  {"op":"key","code":"KeyY","down":true},
  {"op":"key","code":"KeyY","down":false},
  {"op":"key","code":"AltLeft","down":false},
  {"op":"wait","ms":2500} ]
EOF
  "$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
    --duration 34s --expect-first-frame-ms 8000 --keyframe-retry-after 1.5s \
    --input-script "$ART/g1-steps.json" --json \
    > "$ART/g1-uac.json" 2> "$ART/g1-uac.log" &
  local VPID=$!
  sleep 4
  echo "[$(date +%s%3N)] trigger UAC (session 1, non-elevated)"
  "$XNC" exec "$NODE" "schtasks /Run /TN $UAC_TASK" >/dev/null
  sleep 5    # t+9: prompt must be up (Enter fires ~t+13..17, after this check)
  G1_CONSENT_UP=$("$XNC" exec "$NODE" '@(Get-Process -Name consent -ErrorAction SilentlyContinue).Count; exit 0' 2>/dev/null | tr -d '\r' | grep -E '^[0-9]+$' | head -1)
  echo "consent procs at t+9: ${G1_CONSENT_UP:-?} (prompt up; viewer Enter pending)"
  sleep 19   # t+28: approval (Enter ~t+13..17) should have landed
  G1_CONSENT_DOWN=$("$XNC" exec "$NODE" '@(Get-Process -Name consent -ErrorAction SilentlyContinue).Count; exit 0' 2>/dev/null | tr -d '\r' | grep -E '^[0-9]+$' | head -1)
  G1_TASKLIST=$("$XNC" exec "$NODE" 'tasklist /FI "IMAGENAME eq xnc-uac-child.exe"; exit 0' 2>/dev/null | tr -d '\r' | grep -c 'xnc-uac-child.exe' || true)
  G1_MARKER=$("$XNC" exec "$NODE" 'if (Test-Path C:\xnc-diag\uac-elev.txt) { (Get-Content C:\xnc-diag\uac-elev.txt -Raw).Trim() } else { "NO-MARKER" }; exit 0' 2>/dev/null | tr -d '\r\n' | head -1)
  echo "consent at t+28: ${G1_CONSENT_DOWN:-?} child rows: ${G1_TASKLIST:-0} marker: $G1_MARKER"
  wait "$VPID" || true
  cat "$ART/g1-uac.json"
  # Backstop: if the prompt is still up, cancel it (consent-kill) so the run
  # does not leave a standing prompt.
  G1_CONSENT_END=$("$XNC" exec "$NODE" '@(Get-Process -Name consent -ErrorAction SilentlyContinue).Count; exit 0' 2>/dev/null | tr -d '\r' | grep -E '^[0-9]+$' | head -1)
  if [ "${G1_CONSENT_END:-0}" -gt 0 ]; then
    echo "NOTE: prompt still up after Enter — consent-kill backstop (cancel)"
    "$XNC" exec "$NODE" 'Get-Process consent -ErrorAction SilentlyContinue | Stop-Process -Force; exit 0' >/dev/null 2>&1 || true
  fi
  fetch m2s1-uac.log || true
}

say "Gate 1: UAC secure desktop -> viewer reset path -> Alt+Y approves"
# Run-1 evidence (kept in the results doc): plain Enter approved the
# DEFAULT-FOCUSED control, which on this prompt class (unsigned renamed
# binary) is "No" - the prompt cancelled, no elevation. Alt+Y is the UAC
# "Yes" keyboard accelerator, independent of which button holds focus.
run_gate1
G1_RETRY_NOTE=""
if ! node -e 'const j=require(process.argv[1]);console.log((j.input&&j.input.steps||[]).every(s=>s.ok)?1:0)' "$ART/g1-uac.json" 2>/dev/null | grep -q 1; then
  echo "attempt-1 script incomplete — one retry with a fresh prompt"
  G1_RETRY_NOTE="(attempt-2)"
  run_gate1
fi
G1_STEPS=$(node -e 'const j=require(process.argv[1]);console.log((j.input&&j.input.steps||[]).every(s=>s.ok)?1:0)' "$ART/g1-uac.json" 2>/dev/null || echo 0)
gate "1-input-steps-ok" "$G1_STEPS" "lease+Enter steps ok${G1_RETRY_NOTE} err=$(node -e 'const j=require(process.argv[1]);console.log(j.input&&j.input.err?j.input.err:"none")' "$ART/g1-uac.json" 2>/dev/null)"
G1_PROMPT=$([ "${G1_CONSENT_UP:-0}" -ge 1 ] && echo 1 || echo 0)
gate "1-prompt-observed" "$G1_PROMPT" "consent.exe procs at t+9: ${G1_CONSENT_UP:-?} (secure desktop up, before Enter)"
node -e '
const j = require(process.argv[1]);
const states = j.stateSamples || [];
const rec = states.filter(s => s.code === "recovering");
const reb = states.filter(s => s.code === "capture_rebuilt");
const au = j.auTimesMs || [], keys = j.keyTimesMs || [];
let resume = -1, idr = -1;
if (reb.length) {
  const first = au.find(t => t >= reb[reb.length-1].tMs);
  if (first !== undefined) resume = first - reb[reb.length-1].tMs;
  const k = keys.find(t => t >= reb[reb.length-1].tMs);
  if (k !== undefined) idr = k - reb[reb.length-1].tMs;
}
console.log(JSON.stringify({recovering: rec.length, rebuilt: reb.length, resumeMs: resume, idrMs: idr, frames: j.frames, keyframes: j.keyframes, connected: j.connected, durationMs: j.durationMs}));
' "$ART/g1-uac.json" > "$ART/g1-analysis.json"
cat "$ART/g1-analysis.json"
G1_RESET=$(node -e 'const a=require(process.argv[1]);console.log(a.recovering>=1&&a.rebuilt>=1?1:0)' "$ART/g1-analysis.json" 2>/dev/null || echo 0)
gate "1-viewer-reset-path" "$G1_RESET" "recovering+capture_rebuilt states observed (T2 recovery), detail: $(node -e 'const a=require(process.argv[1]);console.log("resume="+a.resumeMs+"ms idr="+a.idrMs+"ms")' "$ART/g1-analysis.json" 2>/dev/null)"
G1_ELEV=0
[ "${G1_MARKER:-}" = "ELEVATED-OK" ] && [ "${G1_TASKLIST:-0}" -ge 1 ] && G1_ELEV=1
G1_ELEV_NOTE=""
if [ "$G1_ELEV" -eq 0 ] && [ "${G1_CONSENT_DOWN:-0}" -gt 0 ]; then
  G1_ELEV_NOTE=" — Enter did not approve (input into secure desktop failed); consent-kill fallback used; click-test partial"
fi
gate "1-elevated-process" "$G1_ELEV" "marker=${G1_MARKER:-none} tasklist-rows=${G1_TASKLIST:-0}${G1_ELEV_NOTE}"

# --- 8. GATE 2: SAS -> security screen -> unlock, stream survives -------------

say "Gate 2: SAS -> security screen (LogonUI) -> sysenter unlock -> stream survives"
: > "$ART/g2-lockui.txt"
cat > "$ART/g2-steps.json" <<'EOF'
[ {"op":"lease"},
  {"op":"wait","ms":9000},
  {"op":"sas"},
  {"op":"wait","ms":22000} ]
EOF
"$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
  --duration 80s --expect-first-frame-ms 8000 --keyframe-retry-after 1.5s \
  --input-script "$ART/g2-steps.json" --json \
  > "$ART/g2-sas.json" 2> "$ART/g2-sas.log" &
G2_PID=$!
sleep 25   # sas fires ~t+15
G2_LOCKED=$("$XNC" exec "$NODE" '@(Get-Process -Name LogonUI -ErrorAction SilentlyContinue).Count; exit 0' 2>/dev/null | tr -d '\r' | grep -E '^[0-9]+$' | head -1)
echo "[$(date +%s%3N)] LogonUI procs (locked?): ${G2_LOCKED:-?}"
echo "logonui@t+25 ${G2_LOCKED:-?}" >> "$ART/g2-lockui.txt"
sleep 18   # t+43
say "Gate 2: unlock via SYSTEM sessrun sysenter (empty password)"
"$XNC" exec "$NODE" 'C:\xnc-diag\sessrun.exe -session 1 -- powershell -NoProfile -ExecutionPolicy Bypass -File C:\xnc-dev\sysenter.ps1 -Count 3 -SpacingSec 2 -LogPath C:\xnc-diag\m2s1-sysenter.log; exit 0' >/dev/null 2>&1 || true
sleep 14   # t+57
G2_UNLOCKED=$("$XNC" exec "$NODE" '@(Get-Process -Name LogonUI -ErrorAction SilentlyContinue).Count; exit 0' 2>/dev/null | tr -d '\r' | grep -E '^[0-9]+$' | head -1)
echo "LogonUI procs after unlock (t+57): ${G2_UNLOCKED:-?}"
echo "logonui@t+57 ${G2_UNLOCKED:-?}" >> "$ART/g2-lockui.txt"
wait "$G2_PID" || true
cat "$ART/g2-sas.json"
fetch m2s1-sysenter.log || true
node -e '
const j = require(process.argv[1]);
const states = j.stateSamples || [];
const reb = states.filter(s => s.code === "capture_rebuilt");
const au = j.auTimesMs || [];
let lastAuAfterRebuild = -1;
if (reb.length && au.length) {
  const t = reb[reb.length-1].tMs;
  for (const a of au) if (a >= t) lastAuAfterRebuild = a;
}
const sasStep = ((j.input && j.input.steps) || []).find(s => s.op === "sas") || {};
console.log(JSON.stringify({connected: j.connected, durationMs: j.durationMs, frames: j.frames, keyframes: j.keyframes,
  recovering: states.filter(s=>s.code==="recovering").length, rebuilt: reb.length,
  lastAuAfterRebuild, sasDetail: sasStep.detail || "none", sasOk: sasStep.ok === true}));
' "$ART/g2-sas.json" > "$ART/g2-analysis.json"
cat "$ART/g2-analysis.json"
G2_LOCKOK=$([ "${G2_LOCKED:-0}" -ge 1 ] && echo 1 || echo 0)
gate "2-secure-screen-up" "$G2_LOCKOK" "LogonUI procs ${G2_LOCKED:-?} after SAS (delivery observed, hr is not proof - T4)"
G2_UNLOCKOK=$([ "${G2_UNLOCKED:-99}" -eq 0 ] && echo 1 || echo 0)
gate "2-unlocked" "$G2_UNLOCKOK" "LogonUI procs ${G2_UNLOCKED:-?} after sysenter (see m2s1-sysenter.log)"
G2_STREAM=$(node -e 'const a=require(process.argv[1]);console.log(a.connected&&a.durationMs>=75000&&a.rebuilt>=1&&a.lastAuAfterRebuild>0?1:0)' "$ART/g2-analysis.json" 2>/dev/null || echo 0)
gate "2-stream-survives-cycle" "$G2_STREAM" "$(node -e 'const a=require(process.argv[1]);console.log(`connected=${a.connected} duration=${a.durationMs}ms frames=${a.frames} keys=${a.keyframes} recovering=${a.recovering} rebuilt=${a.rebuilt} lastAU+${a.lastAuAfterRebuild}ms`)' "$ART/g2-analysis.json" 2>/dev/null)"
G2_SASSTEP=$(node -e 'const a=require(process.argv[1]);console.log(a.sasOk?1:0)' "$ART/g2-analysis.json" 2>/dev/null || echo 0)
gate "2-sas-step-ok" "$G2_SASSTEP" "input sas step: $(node -e 'const a=require(process.argv[1]);console.log(a.sasDetail)' "$ART/g2-analysis.json" 2>/dev/null) (hr=0 = call returned, delivery proven by LogonUI)"

# --- 9. GATE 6: input continuity regression after 1+2 cycles ------------------

say "Gate 6: slice3 input sample after the 1+2 desktop cycles (moves + key + text)"
start_probe() {
  "$XNC" exec "$NODE" "schtasks /End /TN $PROBE_TASK; exit 0" >/dev/null 2>&1 || true
  sleep 1
  "$XNC" exec "$NODE" "schtasks /Create /F /TN $PROBE_TASK /TR 'powershell -ExecutionPolicy Bypass -File C:\xnc-dev\input-probe.ps1 -DurationSec $2 -OutFile $DIAG\\$1' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT" >/dev/null
  "$XNC" exec "$NODE" "schtasks /Run /TN $PROBE_TASK" >/dev/null
}
TS=$(date +%s)
TYPED="XNC-M2S1-OK-$TS"
"$XNC" exec "$NODE" "schtasks /Create /F /TN $NOTEPAD_TASK /TR 'powershell -ExecutionPolicy Bypass -File C:\xnc-dev\notepad-type-test.ps1 -PrepareOnly -FilePath C:\xnc-diag\typed-$TS.txt' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT" >/dev/null
"$XNC" exec "$NODE" "schtasks /Run /TN $NOTEPAD_TASK" >/dev/null
sleep 4
"$XNC" exec "$NODE" "schtasks /Run /TN xnc-dismiss-popup; exit 0" >/dev/null 2>&1 || true
start_probe probe-g6.csv 45
M1X=$((SW/4));   M1Y=$((SH/4))
M2X=$((SW*3/4)); M2Y=$((SH/3))
M3X=$((SW/2));   M3Y=$((SH*3/4))
cat > "$ART/g6-steps.json" <<EOF
[ {"op":"lease"},
  {"op":"wait","ms":1500},
  {"op":"move","x":$M1X,"y":$M1Y},
  {"op":"wait","ms":800},
  {"op":"move","x":$M2X,"y":$M2Y},
  {"op":"wait","ms":800},
  {"op":"move","x":$M3X,"y":$M3Y},
  {"op":"wait","ms":800},
  {"op":"key","code":"KeyA","down":true},
  {"op":"wait","ms":2000},
  {"op":"key","code":"KeyA","down":false},
  {"op":"wait","ms":500},
  {"op":"text","s":"$TYPED"},
  {"op":"key","code":"ControlLeft","down":true},
  {"op":"key","code":"KeyS","down":true},
  {"op":"key","code":"KeyS","down":false},
  {"op":"key","code":"ControlLeft","down":false},
  {"op":"wait","ms":1500} ]
EOF
viewer "$ART/g6-input.json" "$ART/g6-input.log" 16s --input-script "$ART/g6-steps.json"
cat "$ART/g6-input.json"
sleep 18; fetch probe-g6.csv || true
G6_START=$(json_num "$ART/g6-input.json" startUnixMs)
G6_FF=$(json_num "$ART/g6-input.json" firstFrameMs)
if [ -f "$ART/probe-g6.csv" ] && [ -n "$G6_START" ] && [ -n "$G6_FF" ] && [ "$G6_FF" -gt 0 ]; then
  STEP0=$((G6_START + G6_FF))
  T_MOVE1=$((STEP0 + 1500 + 150))
  T_MOVE2=$((STEP0 + 1500 + 800 + 300))
  T_MOVE3=$((STEP0 + 1500 + 1600 + 450))
  node -e '
  const fs = require("fs");
  // argv: 1=csv 2..4=(xA,yA,tA) 5..7=(xB,yB,tB) 8..10=(xC,yC,tC) 11=skew 12=out
  const skew = +process.argv[11];
  const csv = fs.readFileSync(process.argv[1], "utf8").trim().split(/\r?\n/).slice(1)
    .map(l => l.split(",")).filter(p => p.length === 8)
    .map(p => ({t: +p[0] - skew, x: +p[1], y: +p[2], a: +p[3]}));
  const targets = [[+process.argv[2], +process.argv[3], +process.argv[4]], [+process.argv[5], +process.argv[6], +process.argv[7]], [+process.argv[8], +process.argv[9], +process.argv[10]]];
  const moves = targets.map(([x, y, et]) => {
    const hit = csv.find(r => Math.abs(r.x - x) <= 5 && Math.abs(r.y - y) <= 5 && Math.abs(r.t - et) <= 500);
    return {want: [x, y], hit: hit ? {dt: hit.t - et} : null};
  });
  const a1 = csv.find(r => r.a === 1);
  let key = null;
  if (a1) { let i = csv.indexOf(a1); while (i + 1 < csv.length && csv[i + 1].a === 1) i++; key = {downAt: a1.t, upAt: csv[i + 1] ? csv[i + 1].t : null}; }
  fs.writeFileSync(process.argv[12], JSON.stringify({moves, key}, null, 1));
  ' "$ART/probe-g6.csv" "$M1X" "$M1Y" "$T_MOVE1" "$M2X" "$M2Y" "$T_MOVE2" "$M3X" "$M3Y" "$T_MOVE3" "$SKEW" "$ART/g6-analysis.json"
  cat "$ART/g6-analysis.json"
  G6_MOVES=$(node -e 'const a=require(process.argv[1]);console.log(a.moves.every(m=>m.hit)?1:0)' "$ART/g6-analysis.json" 2>/dev/null || echo 0)
  gate "6-moves-land" "$G6_MOVES" "probe rows at 3 coords ±5px/500ms: $(node -e 'const a=require(process.argv[1]);console.log(a.moves.map(m=>m.hit?("dt="+m.hit.dt+"ms"):"MISS").join(","))' "$ART/g6-analysis.json" 2>/dev/null)"
  G6_KEY=$(node -e 'const a=require(process.argv[1]);console.log(a.key&&a.key.upAt?1:0)' "$ART/g6-analysis.json" 2>/dev/null || echo 0)
  gate "6-keyA-down-up" "$G6_KEY" "$(node -e 'const a=require(process.argv[1]);console.log(a.key?"down@"+a.key.downAt+" up@"+a.key.upAt:"keyA never down")' "$ART/g6-analysis.json" 2>/dev/null)"
else
  gate "6-moves-land" 0 "probe CSV or viewer timestamps missing"
  gate "6-keyA-down-up" 0 "probe CSV or viewer timestamps missing"
fi
g6_typed_fetch() { # <ts>
  rm -f "$ART/typed-$1.txt"
  "$XNC" get "$NODE" "$DIAG\\typed-$1.txt" "$ART/typed-$1.txt" >/dev/null 2>&1 || return 1
  grep -qF "$TYPED" "$ART/typed-$1.txt"
}
g6_notepad_retry() { # <ts> — slice3 pattern: fresh notepad + save-only script
  "$XNC" exec "$NODE" 'taskkill /IM notepad.exe /F >$null 2>&1; exit 0' >/dev/null 2>&1 || true
  sleep 2
  "$XNC" exec "$NODE" "schtasks /Create /F /TN $NOTEPAD_TASK /TR 'powershell -ExecutionPolicy Bypass -File C:\xnc-dev\notepad-type-test.ps1 -PrepareOnly -FilePath C:\xnc-diag\typed-$1.txt' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT" >/dev/null
  "$XNC" exec "$NODE" "schtasks /Run /TN $NOTEPAD_TASK" >/dev/null
  sleep 4
  "$XNC" exec "$NODE" "schtasks /Run /TN xnc-dismiss-popup; exit 0" >/dev/null 2>&1 || true
  cat > "$ART/g6-retry-steps.json" <<EOF
[ {"op":"lease"},
  {"op":"wait","ms":1500},
  {"op":"text","s":"$TYPED"},
  {"op":"key","code":"ControlLeft","down":true},
  {"op":"key","code":"KeyS","down":true},
  {"op":"key","code":"KeyS","down":false},
  {"op":"key","code":"ControlLeft","down":false},
  {"op":"wait","ms":1500} ]
EOF
  viewer "$ART/g6-retry.json" "$ART/g6-retry.log" 8s --input-script "$ART/g6-retry-steps.json"
  cat "$ART/g6-retry.json" || true
  sleep 3
}
rm -f "$ART/typed-$TS.txt"
"$XNC" get "$NODE" "$DIAG\\typed-$TS.txt" "$ART/typed-$TS.txt" >/dev/null 2>&1 || true
G6_TEXT=0
[ -f "$ART/typed-$TS.txt" ] && grep -qF "$TYPED" "$ART/typed-$TS.txt" && G6_TEXT=1
G6_NOTE=""
if [ "$G6_TEXT" -eq 0 ]; then
  echo "typed-file attempt 1 failed (slice3 known flake: notepad focus/dialog) — retrying with fresh notepad"
  TS2=$((TS + 1))
  g6_notepad_retry "$TS2"
  if g6_typed_fetch "$TS2"; then G6_TEXT=1; TS="$TS2"; G6_NOTE="(attempt-2, fresh notepad + save-only script)"; fi
fi
gate "6-text-to-notepad" "$G6_TEXT" "typed file contains '$TYPED' (input continuity across 1+2 cycles)${G6_NOTE}"

# --- 10. GATE 3: resolution switch regression ---------------------------------

say "Gate 3: resolution $ORIG_W<->1920x1080 (setres lever, session 1)"
switch_res() { # switch_res <w> <h>
  RES_CHANGED=1
  "$XNC" exec "$NODE" "schtasks /Create /F /TN $SETRES_TASK /TR 'powershell -ExecutionPolicy Bypass -File C:\xnc-dev\setres.ps1 -Width $1 -Height $2' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT" >/dev/null
  "$XNC" exec "$NODE" "schtasks /Run /TN $SETRES_TASK" >/dev/null
}
"$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
  --duration 55s --expect-first-frame-ms 8000 --keyframe-retry-after 1.5s \
  --json > "$ART/g3-res.json" 2> "$ART/g3-res.log" &
G3_PID=$!
sleep 12
echo "[$(date +%s%3N)] switch to 1920x1080"
switch_res 1920 1080
sleep 20   # t+32
echo "[$(date +%s%3N)] restore $ORIG_W x $ORIG_H"
switch_res "$ORIG_W" "$ORIG_H"
sleep 4; RES_CHANGED=0   # restore issued; viewer observes the event
wait "$G3_PID" || true
cat "$ART/g3-res.json"
node -e '
const j = require(process.argv[1]);
const d = j.displaySamples || [], r = j.displayResumeMs || [];
let genOk = d.length >= 2, resumeOk = r.length >= 2;
for (let i = 1; i < d.length; i++) if (d[i].Gen <= d[i-1].Gen) genOk = false;
for (const ms of r) if (ms < 0 || ms > 2000) resumeOk = false;
console.log(JSON.stringify({events: j.displayEvents, samples: d, resumeMs: r, genOk: genOk?1:0, resumeOk: resumeOk?1:0, frames: j.frames, keyframes: j.keyframes}));
' "$ART/g3-res.json" > "$ART/g3-analysis.json"
cat "$ART/g3-analysis.json"
G3_EVENTS=$(node -e 'const a=require(process.argv[1]);console.log(a.events>=2?1:0)' "$ART/g3-analysis.json" 2>/dev/null || echo 0)
gate "3-display-changed-events" "$G3_EVENTS" "displayEvents=$(node -e 'const a=require(process.argv[1]);console.log(a.events)' "$ART/g3-analysis.json" 2>/dev/null) samples=$(node -e 'const a=require(process.argv[1]);console.log(JSON.stringify(a.samples))' "$ART/g3-analysis.json" 2>/dev/null)"
G3_GEN=$(node -e 'const a=require(process.argv[1]);console.log(a.genOk)' "$ART/g3-analysis.json" 2>/dev/null || echo 0)
gate "3-generation-increases" "$G3_GEN" "gen sequence: $(node -e 'const a=require(process.argv[1]);console.log(a.samples.map(s=>s.gen).join("->"))' "$ART/g3-analysis.json" 2>/dev/null)"
G3_RESUME=$(node -e 'const a=require(process.argv[1]);console.log(a.resumeOk&&a.events>=2?1:0)' "$ART/g3-analysis.json" 2>/dev/null || echo 0)
gate "3-new-frame-le-2000ms" "$G3_RESUME" "resume after each event: $(node -e 'const a=require(process.argv[1]);console.log(a.resumeMs.join(",")+"ms")' "$ART/g3-analysis.json" 2>/dev/null)"

# Geometry sanity: back at the original mode (else restore again before gen3).
SCREEN2=$("$XNC" exec "$NODE" "$GEOMPS" 2>/dev/null | tr -d '\r' || true)
SW2=$(printf '%s' "$SCREEN2" | grep -oE 'GEOM [0-9]+ [0-9]+' | grep -oE '[0-9]+' | head -1)
SH2=$(printf '%s' "$SCREEN2" | grep -oE 'GEOM [0-9]+ [0-9]+' | grep -oE '[0-9]+' | tail -1)
if [ -n "$SW2" ] && { [ "$SW2" != "$ORIG_W" ] || [ "$SH2" != "$ORIG_H" ]; }; then
  echo "NOTE: geometry ${SW2}x${SH2} != original ${ORIG_W}x${ORIG_H} — restoring again"
  switch_res "$ORIG_W" "$ORIG_H"; sleep 4
fi
RES_CHANGED=0

# --- 11. GATE 4: forced GDI downgrade + probe recovery ------------------------

say "Core GEN3: XNC_FORCE_DXGI_HEALTH=50 (ladder downgrade regression)"
"$XNC" exec "$NODE" 'Get-Content C:\xnc-dev\core.log; exit 0' > "$ART/core-gen2.log" 2>&1 || true
stop_core
start_core gen3 XNC_FORCE_DXGI_HEALTH=50
"$XNC" exec "$NODE" "schtasks /Create /F /TN $OVERLAY_TASK /TR 'powershell -ExecutionPolicy Bypass -File C:\xnc-dev\static-overlay.ps1 -Seconds 50 -TickMs 250' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT" >/dev/null
"$XNC" exec "$NODE" "schtasks /Run /TN $OVERLAY_TASK" >/dev/null
sleep 3
say "Gate 4: viewer under forced health=50 -> GDI -> 30s probe -> DXGI"
"$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
  --duration 75s --expect-first-frame-ms 15000 --keyframe-retry-after 1.5s \
  --json > "$ART/g4-gdi.json" 2> "$ART/g4-gdi.log" || true
cat "$ART/g4-gdi.json"
"$XNC" exec "$NODE" "schtasks /End /TN $OVERLAY_TASK; schtasks /Delete /F /TN $OVERLAY_TASK; exit 0" >/dev/null 2>&1 || true
node -e '
const j = require(process.argv[1]);
const st = (j.stateSamples || []).filter(s => s.code === "backend_changed");
const au = j.auTimesMs || [], keys = j.keyTimesMs || [];
let fps = null, idr = -1, framesInWindow = 0;
if (st.length >= 2) {
  const t1 = st[0].tMs, t2 = st[st.length-1].tMs;
  framesInWindow = au.filter(t => t >= t1 && t < t2).length;
  if (t2 > t1) fps = framesInWindow / ((t2 - t1) / 1000);
  const k = keys.find(t => t >= t2);
  if (k !== undefined) idr = k - t2;
}
console.log(JSON.stringify({backendChanged: st.length, times: st.map(s=>s.tMs), framesInWindow, gdiFps: fps===null?null:+fps.toFixed(1), idrAfterDxgiMs: idr, frames: j.frames, keyframes: j.keyframes, connected: j.connected}));
' "$ART/g4-gdi.json" > "$ART/g4-analysis.json"
cat "$ART/g4-analysis.json"
"$XNC" exec "$NODE" 'Get-Content C:\xnc-dev\core.log; exit 0' > "$ART/core-gen3.log" 2>&1 || true
G4_NODE_GDI=$(grep -c 'backend_changed backend=gdi' "$ART/core-gen3.log" || true)
G4_NODE_DXGI=$(grep -c 'backend_changed backend=dxgi' "$ART/core-gen3.log" || true)
G4_NODE_PROBE=$(grep -c 'dxgi_probe ok=1' "$ART/core-gen3.log" || true)
gate "4-node-gdi-switch" "$([ "${G4_NODE_GDI:-0}" -ge 1 ] && echo 1 || echo 0)" "node log backend_changed backend=gdi x${G4_NODE_GDI:-0}"
gate "4-node-dxgi-probe-recovery" "$([ "${G4_NODE_DXGI:-0}" -ge 1 ] && [ "${G4_NODE_PROBE:-0}" -ge 1 ] && echo 1 || echo 0)" "node log dxgi_probe ok=1 x${G4_NODE_PROBE:-0}, backend_changed backend=dxgi x${G4_NODE_DXGI:-0}"
G4_STATES=$(node -e 'const a=require(process.argv[1]);console.log(a.backendChanged>=2?1:0)' "$ART/g4-analysis.json" 2>/dev/null || echo 0)
gate "4-viewer-backend-states" "$G4_STATES" "backend_changed states x$(node -e 'const a=require(process.argv[1]);console.log(a.backendChanged)' "$ART/g4-analysis.json" 2>/dev/null) at $(node -e 'const a=require(process.argv[1]);console.log(a.times.join(","))' "$ART/g4-analysis.json" 2>/dev/null)ms"
G4_FPS=$(node -e 'const a=require(process.argv[1]);console.log(a.gdiFps!==null&&a.gdiFps<=15.5?1:0)' "$ART/g4-analysis.json" 2>/dev/null || echo 0)
gate "4-gdi-fps-le-15" "$G4_FPS" "GDI window fps=$(node -e 'const a=require(process.argv[1]);console.log(a.gdiFps)' "$ART/g4-analysis.json" 2>/dev/null) over $(node -e 'const a=require(process.argv[1]);console.log(a.framesInWindow)' "$ART/g4-analysis.json" 2>/dev/null) frames (cap 15, 0.5 slack)"
G4_IDR=$(node -e 'const a=require(process.argv[1]);console.log(a.idrAfterDxgiMs>=0&&a.idrAfterDxgiMs<=2000?1:0)' "$ART/g4-analysis.json" 2>/dev/null || echo 0)
gate "4-idr-after-dxgi-le-2000ms" "$G4_IDR" "IDR after dxgi recovery: $(node -e 'const a=require(process.argv[1]);console.log(a.idrAfterDxgiMs)+"ms"' "$ART/g4-analysis.json" 2>/dev/null)"

# --- 12. Final clean core + summary -------------------------------------------

say "Final: restart clean core (no env, no --allow-sas) + registry back to 0"
G2_GENS=$(grep -c 'rt generation++' "$ART/core-gen2.log" 2>/dev/null || true)
stop_core
start_core gen4
"$XNC" exec "$NODE" "reg add \"$SAS_REG\" /v SoftwareSASGeneration /t REG_DWORD /d 0 /f | Out-Null; exit 0" >/dev/null 2>&1 || true
fetch setres.log || true

say "Gate summary (node=$NODE dev=$DEV_NODE_NAME)"
printf '1  UAC         : prompt=%s steps=%s reset-path=%s elevated=%s%s\n' \
  "${G1_PROMPT:-?}" "${G1_STEPS:-?}" "${G1_RESET:-?}" "${G1_ELEV:-?}" "${G1_ELEV_NOTE}"
printf '2  lock+SAS    : up=%s unlocked=%s stream=%s sas-step=%s (gen bumps node-side: %s)\n' \
  "${G2_LOCKOK:-?}" "${G2_UNLOCKOK:-?}" "${G2_STREAM:-?}" "${G2_SASSTEP:-?}" "${G2_GENS:-0}"
printf '3  resolution  : events=%s gen=%s resume2s=%s\n' \
  "${G3_EVENTS:-?}" "${G3_GEN:-?}" "${G3_RESUME:-?}"
printf '4  downgrade   : node-gdi=%s node-dxgi=%s states=%s fps15=%s idr=%s\n' \
  "$([ "${G4_NODE_GDI:-0}" -ge 1 ] && echo 1 || echo 0)" "$([ "${G4_NODE_DXGI:-0}" -ge 1 ] && echo 1 || echo 0)" "${G4_STATES:-?}" "${G4_FPS:-?}" "${G4_IDR:-?}"
printf '5  SAS gate    : denied=%s audit=%s\n' "$G5_OK" "$([ "${G5_AUDIT:-0}" -ge 1 ] && echo 1 || echo 0)"
printf '6  input reg   : moves=%s keyA=%s text=%s\n' "${G6_MOVES:-?}" "${G6_KEY:-?}" "${G6_TEXT:-?}"
printf '\nBrowser manual check (owner): %s/desktop/%s  (dev admin login first)\n' "$LAN" "$NODE_ID"
printf 'Artifacts: %s\n' "$ART"

if [ "$GATE_FAIL" -eq 0 ]; then
  say "RESULT: PASS - all M2-Slice1 E2E gates green on $NODE"
else
  say "RESULT: FAIL - see gates above (measured values recorded regardless)"
fi
exit "$GATE_FAIL"
