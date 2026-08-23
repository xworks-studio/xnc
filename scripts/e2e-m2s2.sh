#!/usr/bin/env bash
# e2e-m2s2.sh - M2-Slice2 XIAOXIN E2E acceptance gate: shell-host token
# semantics + supervision + stability polish + exec truncation honesty.
#
# Topology: scripts/dev-topology.md (authoritative).
#   LABS-DEV (docker): dev xnc-server + coturn, LAN http://$LAN_IP:18080
#   LABS-XIAOXIN: dev core (SYSTEM console pipe server), dev agent
#                 (run-dev-console, session 1, node name XIAOXIN-DEV),
#                 xnc-desktop / xnc-shell spawned via core RPCs.
#   this machine: xnc CLI (dev server profile) + e2eviewer (server mode).
#
# Gates (numbers honest; failures recorded with conclusions):
#   1 exec user token  : xnc exec XIAOXIN-DEV whoami -> console user (LABS)
#   2 exec --system    : operator 403 / owner 202 + whoami = nt authority\
#                        system + audit system=true
#   3 interactive shell: CLI leg (winpty: SHELL_BEGIN + echo roundtrip)
#                        + xnc-shell-probe resize smoke (core->shell pipe:
#                        BEGIN geometry, stdin roundtrip, RESIZE, alive)
#   4 crash supervision: viewer running -> scoped taskkill xnc-desktop ->
#                        core backoff restart (1s) -> viewer recovers
#                        (generation++ and frames keep flowing)
#   5 crash-loop       : 5x kill (wait for each restart) -> core logs
#                        crash_loop_degraded + restart child cmdline carries
#                        backend/encoder degradation flags -> frames flow
#   6 stability        : forced health drop -> GDI -> probe recovery ->
#                        health=100 in backend_changed line; SAS busy
#                        (concurrent secure_attention -> busy); storm
#                        backoff = unit-test covered (no env hook) - note
#   7 truncation       : >4MB exec output -> EXEC_RESULT.truncated=true +
#                        stderr marker line '[xnc] output truncated: N...'
#
# Core generations: gen1 --allow-sas (gates 1-5,7 + sas busy) ->
# gen2 XNC_FORCE_DXGI_HEALTH=50 (gate 6 health).
#
# Usage: scripts/e2e-m2s2.sh [node] [artifacts-dir]
#   node            default LABS-XIAOXIN (prod name for xnc exec/put/get)
#   artifacts-dir   default
#     .superpowers/sdd/2026-08-23-m2-slice2-stability-shellhost/e2e
#   env CONSOLE_USER  console-session user (default LABS)
#
# All kills are image+session/path scoped (incident rules). Registry is not
# touched by this gate (no SAS-policy change needed: gen1 runs --allow-sas
# with SoftwareSASGeneration untouched; the SAS busy path is agent-side).
#
# Exit 0 = all gates pass; 1 = any gate failed (numbers recorded regardless).

set -euo pipefail

NODE="${1:-LABS-XIAOXIN}"
CONSOLE_USER="${CONSOLE_USER:-LABS}"
DEV_NODE_NAME="XIAOXIN-DEV"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
ROOT_W="$(cd "$ROOT" && pwd -W)"
ART="${2:-$ROOT_W/.superpowers/sdd/2026-08-23-m2-slice2-stability-shellhost/e2e}"
XNC="$ROOT/bin/xnc.exe"
E2E="$ROOT/bin/e2eviewer.exe"
REMOTE_DIR='C:\xnc-dev'
CORE_PIPE='\\.\pipe\xnc-core-m2s2'
CORE_TASK="xnc-m2s2-core"
AGENT_TASK="xnc-m2s2-agent"

say() { printf '\n=== %s ===\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
gate() { # gate <name> <0|1 pass> <detail>
  if [ "$2" -eq 1 ]; then
    printf 'GATE PASS  %-30s %s\n' "$1" "$3"
  else
    printf 'GATE FAIL  %-30s %s\n' "$1" "$3"
    GATE_FAIL=1
  fi
}

dev_node_id() { # id of the ONLINE node named <name>(-N)?, else nothing
  { curl -sf -H "Authorization: Bearer $JWT" "$LAN/api/nodes/" 2>/dev/null \
    | grep -oE '\{[^}]*\}' | grep -E "\"name\":\"$1(-[0-9]+)?\"" | grep -F '"status":"online"' \
    | grep -oE '"id": *"[^"]*"' | head -1 | sed 's/.*: *"//;s/"$//'; } || true
}

core_log() { "$XNC" exec "$NODE" 'Get-Content C:\xnc-dev\core.log; exit 0' 2>/dev/null || true; }
fetch() { "$XNC" get "$NODE" "$REMOTE_DIR\\$1" "$ART/$1" >/dev/null 2>&1 || true; }
nodejs() { node -e "$1" "${@:2}"; }

GATE_FAIL=0
mkdir -p "$ART"

cleanup() {
  # Scoped kills only: our dev tasks + dev-path processes. Prod XNCAgent
  # service and C:\xnc are never touched. The FINAL dev core gen + agent are
  # left running for the owner (m2s1 pattern; dev-topology.md §6 owns full
  # teardown) - reruns stop stale generations in stop_core (verified).
  "$XNC" exec "$NODE" "schtasks /End /TN $AGENT_TASK; schtasks /Delete /F /TN $AGENT_TASK; exit 0" >/dev/null 2>&1 || true
  "$XNC" exec "$NODE" "schtasks /End /TN $CORE_TASK; schtasks /Delete /F /TN $CORE_TASK; exit 0" >/dev/null 2>&1 || true
  "$XNC" exec "$NODE" 'taskkill /IM xnc-desktop.exe /FI "SESSION eq 1" /F >$null 2>&1; taskkill /IM xnc-shell.exe /F >$null 2>&1; exit 0' >/dev/null 2>&1 || true
  core_log > "$ART/core-final.log" || true
}
trap cleanup EXIT INT TERM

# --- 0. Preflight -----------------------------------------------------------

say "Preflight"
[ -f "$ROOT/deploy/.env" ] || fail "deploy/.env missing (see dev-topology.md §1)"
set -a; . "$ROOT/deploy/.env"; set +a
LAN_IP="${XNC_DEV_EXTERNAL_IP:-192.168.1.12}"
LAN="http://$LAN_IP:18080"
echo "dev LAN URL: $LAN  (node=$NODE dev-node=$DEV_NODE_NAME)"

# --- 1. Build everything fresh ----------------------------------------------

say "Build: cli + agent + shellhost + probe + viewer (native selftests ran in CI/task)"
(cd "$ROOT/cli" && go build -o "../bin/xnc.exe" .)
(cd "$ROOT/agent" && go build -o "../bin/xnc-agent.exe" ./cmd/xnc-agent)
(cd "$ROOT/shellhost" && go build -o "../bin/xnc-shell.exe" .)
go build -C "$ROOT/shellhost" -o "$ROOT/bin/xnc-shell-probe.exe" ./cmd/xnc-shell-probe
go build -C "$ROOT" -o "$E2E" ./tools/e2eviewer
[ -x "$E2E" ] || fail "e2eviewer build"

# --- 2. Dev stack up --------------------------------------------------------

say "Dev stack up (docker, slice2 overlay)"
COMPOSE="docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.dev.yml -f deploy/docker-compose.slice2.yml"
(cd "$ROOT" && $COMPOSE up -d --build >/dev/null)
for i in $(seq 1 60); do curl -sf "$LAN/api/health" >/dev/null && break; sleep 2; done
curl -sf "$LAN/api/health" >/dev/null || fail "dev server not healthy at $LAN"

say "Dev admin login + enrollment token"
JWT=$(curl -sf -X POST "$LAN/api/auth/login" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$XNC_ADMIN_EMAIL\",\"password\":\"$XNC_ADMIN_PASSWORD\"}" \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$JWT" ] || fail "admin login failed"
ETOK=$(curl -sf -X POST "$LAN/api/clusters/default/enrollment-tokens" \
  -H "Authorization: Bearer $JWT" -H 'Content-Type: application/json' \
  -d '{"ttl":"2h","maxUses":5}' | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$ETOK" ] || fail "enrollment token create failed"

# Operator user for gate 2 RBAC (idempotent: reruns reuse the existing user).
OP_EMAIL="m2s2-op-$(date +%s)@t.local"
OP_ID=$(curl -sf -X POST "$LAN/api/users" -H "Authorization: Bearer $JWT" \
  -H 'Content-Type: application/json' -d "{\"email\":\"$OP_EMAIL\",\"display_name\":\"M2S2 Op\",\"password\":\"OpPass123!\"}" \
  | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' || true)
OP_TOKEN=$(curl -sf -X POST "$LAN/api/auth/login" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$OP_EMAIL\",\"password\":\"OpPass123!\"}" \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p' || true)
[ -n "$OP_TOKEN" ] || fail "operator login failed"
if [ -n "$OP_ID" ]; then
  CLUSTER_ID=$(curl -sf -H "Authorization: Bearer $JWT" "$LAN/api/clusters/" | grep -oE '"id": *"[^"]*"' | head -1 | sed 's/.*: *"//;s/"$//')
  curl -sf -X POST "$LAN/api/clusters/$CLUSTER_ID/members" -H "Authorization: Bearer $JWT" \
    -H 'Content-Type: application/json' -d "{\"user_id\":\"$OP_ID\",\"role\":\"operator\"}" >/dev/null \
    || echo "operator membership add: already member (rerun)"
fi
echo "operator ready (new id: ${OP_ID:-existing})"

# --- 3. Stop previous dev agent/core, deploy binaries -----------------------

say "Stop any previous dev agent + core (scoped)"
"$XNC" exec "$NODE" "schtasks /End /TN $AGENT_TASK; schtasks /Delete /F /TN $AGENT_TASK; schtasks /End /TN xnc-dev-agent; schtasks /Delete /F /TN xnc-dev-agent; exit 0" >/dev/null 2>&1 || true
"$XNC" exec "$NODE" 'Get-Process xnc-agent -ErrorAction SilentlyContinue | Where-Object {$_.Path -like "C:\xnc-dev*"} | Stop-Process -Force; exit 0' >/dev/null 2>&1 || true
for t in $(seq 1 15); do
  N=$("$XNC" exec "$NODE" '@(Get-Process xnc-agent -ErrorAction SilentlyContinue | Where-Object {$_.Path -like "C:\xnc-dev*"}).Count; exit 0' 2>/dev/null | tr -d '\r' | grep -E '^[0-9]+$' | head -1)
  [ "${N:-1}" -eq 0 ] && break
  sleep 2
done
[ "${N:-1}" -eq 0 ] || fail "stale dev agent still alive (exe overwrite would fail)"
stop_core() {
  "$XNC" exec "$NODE" "schtasks /End /TN $CORE_TASK; schtasks /Delete /F /TN $CORE_TASK; schtasks /End /TN xnc-dev-core; schtasks /Delete /F /TN xnc-dev-core; exit 0" >/dev/null 2>&1 || true
  "$XNC" exec "$NODE" 'Get-Process xnc-core -ErrorAction SilentlyContinue | Where-Object {$_.Path -like "C:\xnc-dev*"} | Stop-Process -Force; taskkill /IM xnc-desktop.exe /FI "SESSION eq 1" /F >$null 2>&1; taskkill /IM xnc-shell.exe /F >$null 2>&1; exit 0' >/dev/null 2>&1 || true
  # VERIFY death (run-4 lesson: a surviving stale core keeps owning the pipe
  # name and the next generation silently never starts).
  for t in $(seq 1 15); do
    N=$("$XNC" exec "$NODE" '@(Get-Process xnc-core -ErrorAction SilentlyContinue | Where-Object {$_.Path -like "C:\xnc-dev*"}).Count; exit 0' 2>/dev/null | tr -d '\r' | grep -E '^[0-9]+$' | head -1)
    [ "${N:-1}" -eq 0 ] && return 0
    sleep 2
  done
  fail "stale dev xnc-core still alive after stop_core (${N:-?} procs)"
}
stop_core

say "Deploy dev binaries to $NODE"
"$XNC" exec "$NODE" "New-Item -ItemType Directory -Force -Path $REMOTE_DIR | Out-Null; exit 0" >/dev/null
"$XNC" put "$NODE" "$ROOT_W/bin/xnc-agent.exe"  "$REMOTE_DIR\\xnc-agent.exe"
"$XNC" put "$NODE" "$ROOT_W/bin/xnc-core.exe"   "$REMOTE_DIR\\xnc-core.exe"
"$XNC" put "$NODE" "$ROOT_W/bin/xnc-desktop.exe" "$REMOTE_DIR\\xnc-desktop.exe"
"$XNC" put "$NODE" "$ROOT_W/bin/xnc-shell.exe"  "$REMOTE_DIR\\xnc-shell.exe"
"$XNC" put "$NODE" "$ROOT_W/bin/xnc-shell-probe.exe" "$REMOTE_DIR\\xnc-shell-probe.exe"

CORE_SECRET_HEX=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
start_core() { # start_core <gen-tag> [flag...]
  local tag="$1"; shift
  {
    printf '@echo off\r\n'
    printf 'C:\\xnc-dev\\xnc-core.exe --console --smoke-secret %s --pipe-name %s' "$CORE_SECRET_HEX" "$CORE_PIPE"
    for a in "$@"; do printf ' %s' "$a"; done
    printf ' > C:\\xnc-dev\\core.log 2>&1\r\n'
  } > "$ART/core-$tag.cmd"
  "$XNC" put "$NODE" "$ART/core-$tag.cmd" "$REMOTE_DIR\\core-dev.cmd"
  "$XNC" exec "$NODE" "schtasks /Create /F /TN $CORE_TASK /TR 'C:\xnc-dev\core-dev.cmd' /SC ONCE /ST 23:59 /RU SYSTEM" >/dev/null
  "$XNC" exec "$NODE" "schtasks /Run /TN $CORE_TASK" >/dev/null
  # VERIFY start (run-4 lesson: a stale core owning the pipe name makes the
  # fresh generation silently never start): core.log must show "listening on".
  local ok=0 t
  for t in $(seq 1 15); do
    if "$XNC" exec "$NODE" "if (Select-String -Path C:\\xnc-dev\\core.log -Pattern 'listening on' -SimpleMatch) { exit 0 } else { exit 1 }" >/dev/null 2>&1; then ok=1; break; fi
    sleep 2
  done
  [ "$ok" = "1" ] || { core_log > "$ART/core-start-failure-$tag.log" || true; fail "core gen '$tag' did not reach listening state"; }
}
start_agent() {
  printf '@echo off\r\nC:\\xnc-dev\\xnc-agent.exe run-dev-console --server %s --token %s --name %s --log-file C:\\xnc-dev\\dev-agent.log --desktop-core-pipe %s --desktop-core-secret-hex %s >> C:\\xnc-dev\\dev-agent-console.log 2>&1\r\n' \
    "$LAN" "$ETOK" "$DEV_NODE_NAME" "$CORE_PIPE" "$CORE_SECRET_HEX" > "$ART/agent-dev.cmd"
  "$XNC" put "$NODE" "$ART/agent-dev.cmd" "$REMOTE_DIR\\agent-dev.cmd"
  "$XNC" exec "$NODE" "schtasks /Create /F /TN $AGENT_TASK /TR 'C:\xnc-dev\agent-dev.cmd' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT /RL HIGHEST" >/dev/null
  "$XNC" exec "$NODE" "schtasks /Run /TN $AGENT_TASK" >/dev/null
}

say "Start dev core GEN1 (--allow-sas) + dev agent"
start_core gen1 --allow-sas
start_agent
"$XNC" exec "$NODE" 'netsh advfirewall firewall delete rule name="xnc-dev-agent-dev" >$null; netsh advfirewall firewall add rule name="xnc-dev-agent-dev" dir=in program="C:\xnc-dev\xnc-agent.exe" action=allow >$null; netsh advfirewall firewall add rule name="xnc-dev-agent-dev-out" dir=out program="C:\xnc-dev\xnc-agent.exe" action=allow >$null; exit 0' >/dev/null 2>&1 || true

say "Wait for $DEV_NODE_NAME online"
NODE_ID=""
for i in $(seq 1 90); do
  NODE_ID=$(dev_node_id "$DEV_NODE_NAME")
  [ -n "$NODE_ID" ] && break
  sleep 2
done
[ -n "$NODE_ID" ] || { "$XNC" exec "$NODE" 'Get-Content C:\xnc-dev\dev-agent.log -Tail 25; exit 0' > "$ART/agent-start-failure.log" 2>&1 || true; fail "$DEV_NODE_NAME never came online"; }
echo "dev node: $DEV_NODE_NAME ($NODE_ID)"

viewer() { # viewer <out.json> <out.log> <duration> <extra args...>
  local outj="$1" outl="$2" dur="$3"; shift 3
  "$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
    --duration "$dur" --expect-first-frame-ms 8000 --keyframe-retry-after 1.5s \
    --json "$@" > "$outj" 2> "$outl" || true
}

# dev-profile CLI helper (LAN URL + admin JWT).
xncdev() { "$XNC" --server "$LAN" --token "$JWT" "$@"; }

# --- GATE 1: exec user token ------------------------------------------------

say "Gate 1: exec with USER token -> whoami = console user"
G1_OUT=$(xncdev exec --json --shell cmd "$NODE_ID" "whoami" 2>/dev/null || true)
echo "$G1_OUT" > "$ART/g1-whoami.json"
G1_USER=$(printf '%s' "$G1_OUT" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{try{console.log(JSON.parse(s).data.stdout.trim())}catch(e){console.log("")}})')
gate "1-exec-user-token" "$([ -n "$G1_USER" ] && printf '%s' "$G1_USER" | grep -qiE "labs(\\labs)?$|\\\\labs$" && echo 1 || echo 0)" "whoami='$G1_USER' (expect console user LABS)"

# --- GATE 2: exec --system RBAC + SYSTEM whoami + audit ----------------------

say "Gate 2: exec --system (operator 403 / owner 202 + SYSTEM whoami + audit)"
G2_OP_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $OP_TOKEN" -H 'Content-Type: application/json' \
  -d '{"command":"whoami","system":true}' "$LAN/api/nodes/$NODE_ID/exec")
gate "2-system-operator-403" "$([ "$G2_OP_CODE" = "403" ] && echo 1 || echo 0)" "operator --system -> HTTP $G2_OP_CODE (expect 403)"
G2_OP_OK_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $OP_TOKEN" -H 'Content-Type: application/json' \
  -d '{"command":"whoami"}' "$LAN/api/nodes/$NODE_ID/exec")
echo "operator without system -> HTTP $G2_OP_OK_CODE (409 NODE_OFFLINE-able pass expected)"

G2_OUT=$(xncdev exec --json --shell cmd --system "$NODE_ID" "whoami" 2>/dev/null || true)
echo "$G2_OUT" > "$ART/g2-system-whoami.json"
G2_USER=$(printf '%s' "$G2_OUT" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{try{console.log(JSON.parse(s).data.stdout.trim())}catch(e){console.log("")}})')
gate "2-system-owner-exec" "$(printf '%s' "$G2_USER" | grep -qi "nt authority\\\\system" && echo 1 || echo 0)" "owner --system whoami='$G2_USER' (expect nt authority\\system)"

sleep 2
G2_AUDIT=$(curl -sf -H "Authorization: Bearer $JWT" "$LAN/api/audit?action=exec.start&limit=20" 2>/dev/null || true)
printf '%s' "$G2_AUDIT" > "$ART/g2-audit.json"
# NOTE: rows extraction must special-case a top-level array FIRST -
# `j.entries||...` matches the built-in Array.prototype.entries method
# (a truthy function) and the check silently reads 0 (run-9 lesson).
G2_AUDIT_OK=$(printf '%s' "$G2_AUDIT" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{try{const j=JSON.parse(s);const a=Array.isArray(j)?j:(Array.isArray(j.data)?j.data:[]);console.log(a.some(r=>JSON.stringify(r).includes("\"system\":true")||JSON.stringify(r).includes("\"system\": true")||JSON.stringify(r).includes("\"system\":\"true\""))?1:0)}catch(e){console.log(0)}})')
gate "2-system-audit" "$G2_AUDIT_OK" "audit exec.start entries carry system=true (raw in g2-audit.json)"

# --- GATE 3: interactive shell (CLI leg + probe resize smoke) -----------------

say "Gate 3: interactive shell"
# 3a CLI TTY leg: `xnc shell` refuses non-interactive stdin by design
# (failUsage; agent session leg covered by agent session loopback tests and
# the owner's interactive use). This unattended runner has no allocatable
# console (winpty asserts without one), so the sanctioned pipeprobe route
# below carries the gate; the CLI constraint is recorded, not a failure.
echo "note: xnc shell requires an interactive terminal; unattended gate uses xnc-shell-probe (plan-sanctioned)" | tee "$ART/g3-cli-note.txt"

# 3b full ConPTY path incl. resize via xnc-shell-probe ON the node.
"$XNC" exec "$NODE" "C:\\xnc-dev\\xnc-shell-probe.exe --core-pipe xnc-core-m2s2 --secret $CORE_SECRET_HEX --token user --profile CMD --interactive --cols 100 --rows 30 --resize 132x43 --send 'echo m2s2-interactive-ok' --expect m2s2-interactive-ok" > "$ART/g3-probe.log" 2>&1 || true
cat "$ART/g3-probe.log"
G3_PROBE=$(grep -c "interactive: PASS" "$ART/g3-probe.log" || true)
gate "3-shell-resize-smoke" "$([ "${G3_PROBE:-0}" -ge 1 ] && echo 1 || echo 0)" "probe: BEGIN+echo roundtrip+RESIZE alive ($(grep -o 'resize [0-9x]* accepted' "$ART/g3-probe.log" | head -1))"

# --- GATE 4: desktop crash supervision ---------------------------------------

say "Gate 4: viewer running -> scoped taskkill xnc-desktop -> backoff restart -> recovery"
rm -f "$ART/g4-viewer.json"
viewer "$ART/g4-viewer.json" "$ART/g4-viewer.log" 75s &
G4_VPID=$!
sleep 15   # first frame + steady stream established
echo "[$(date +%T)] scoped taskkill xnc-desktop (session 1)"
"$XNC" exec "$NODE" 'taskkill /IM xnc-desktop.exe /FI "SESSION eq 1" /F; exit 0' >/dev/null 2>&1 || true
wait $G4_VPID || true
core_log > "$ART/g4-core.log"
G4_RESTART=$(grep -c "desktop restart in" "$ART/g4-core.log" || true)
G4_BACKOFF1=$(grep -o "desktop restart in [0-9]*ms (crash #1" "$ART/g4-core.log" | head -1 || true)
G4_RESPAWN=$(grep -c "desktop restart spawned" "$ART/g4-core.log" || true)
G4_FRAMES=$(node -e 'try{const j=require(process.argv[1]);console.log(j.frames)}catch(e){console.log(0)}' "$ART/g4-viewer.json" 2>/dev/null || echo 0)
G4_GEN=$(node -e 'try{const j=require(process.argv[1]);const g=(j.displaySamples||[]).map(s=>s.gen);console.log(g.length?Math.max(...g):0)}catch(e){console.log(0)}' "$ART/g4-viewer.json" 2>/dev/null || echo 0)
G4_ALIVE=$("$XNC" exec "$NODE" '@(Get-Process xnc-desktop -ErrorAction SilentlyContinue | Where-Object {$_.Path -like "C:\xnc-dev*"}).Count; exit 0' 2>/dev/null | tr -d '\r' | grep -E '^[0-9]+$' | head -1)
# core-side generation after restart ("desktop restart spawned ... gen=N");
# viewer-side session teardown stops the capture afterwards, so alive=0 at
# check time is expected and NOT a failure.
G4_COREGEN=$(grep -o "desktop restart spawned pid=[0-9]* pipe=[^ ]* gen=[0-9]*" "$ART/g4-core.log" | tail -1 | grep -o "gen=[0-9]*" | grep -o "[0-9]*" || true)
gate "4-crash-backoff-restart" "$([ "${G4_RESTART:-0}" -ge 1 ] && [ "${G4_RESPAWN:-0}" -ge 1 ] && printf '%s' "$G4_BACKOFF1" | grep -q "in 1000ms" && echo 1 || echo 0)" "core: $G4_BACKOFF1) respawned=$G4_RESPAWN (first backoff 1s)"
gate "4-viewer-recovers" "$([ "${G4_FRAMES:-0}" -gt 5 ] && [ -n "$G4_COREGEN" ] && [ "${G4_COREGEN:-0}" -ge 2 ] && echo 1 || echo 0)" "frames=$G4_FRAMES over the 75s window (static desktop = sparse), core gen after restart=$G4_COREGEN (>=2), viewer display gen=$G4_GEN"

# --- GATE 5: crash-loop degraded restart -------------------------------------

say "Gate 5: crash-loop (5x kill, wait each restart) -> degraded gdi/software"
rm -f "$ART/g5-viewer.json"
viewer "$ART/g5-viewer.json" "$ART/g5-viewer.log" 260s &
G5_VPID=$!
sleep 10
for i in 1 2 3 4 5; do
  echo "[$(date +%T)] kill #$i"
  "$XNC" exec "$NODE" 'taskkill /IM xnc-desktop.exe /FI "SESSION eq 1" /F; exit 0' >/dev/null 2>&1 || true
  # wait for this kill's restart to spawn (patience 120s; backoff grows 1,2,4,8s)
  for t in $(seq 1 60); do
    NEW=$("$XNC" exec "$NODE" '@(Get-Process xnc-desktop -ErrorAction SilentlyContinue | Where-Object {$_.Path -like "C:\xnc-dev*"}).Count; exit 0' 2>/dev/null | tr -d '\r' | grep -E '^[0-9]+$' | head -1)
    [ "${NEW:-0}" -ge 1 ] && break
    sleep 2
  done
  echo "kill #$i: desktop respawned (alive=${NEW:-0})"
done
wait $G5_VPID || true
core_log > "$ART/g5-core.log"
G5_DEGRADED=$(grep -c "crash_loop_degraded kind=desktop" "$ART/g5-core.log" || true)
G5_DEGRADED_RESPAWN=$(grep "desktop restart spawned" "$ART/g5-core.log" | grep -c "degraded" || true)
G5_CMDLINE=$("$XNC" exec "$NODE" "Get-CimInstance Win32_Process -Filter \"Name='xnc-desktop.exe'\" | Select-Object -ExpandProperty CommandLine; exit 0" 2>/dev/null | tr -d '\r' || true)
echo "degraded child cmdline: $G5_CMDLINE" | tee "$ART/g5-cmdline.txt"
G5_ARGS_OK=$(printf '%s' "$G5_CMDLINE" | grep -qi "backend.*gdi" && printf '%s' "$G5_CMDLINE" | grep -qi "encoder.*software" && echo 1 || echo 0)
# run-9 lesson: WMI CommandLine of the SYSTEM-owned child comes back empty via
# the exec token; the mechanism is provable from logs instead - the core lock
# line names the degraded argv, the degraded respawn marks the spawn, and the
# child itself logs the gdi backend + software encoder actually in effect.
G5_ARGS_LOG=$(grep -q "crash_loop_degraded kind=desktop" "$ART/g5-core.log" \
  && grep -q "locked to --backend gdi --encoder software" "$ART/g5-core.log" \
  && grep -q "desktop restart spawned.*degraded" "$ART/g5-core.log" \
  && grep -q "encoder selector: software" "$ART/g5-core.log" \
  && grep -q "gdi init w=" "$ART/g5-core.log" && echo 1 || echo 0)
gate "5-degraded-child-args" "$([ "$G5_ARGS_OK" = "1" ] || [ "$G5_ARGS_LOG" = "1" ] && echo 1 || echo 0)" "child cmdline gdi/software (cmdline=$G5_ARGS_OK, log-evidence=$G5_ARGS_LOG: core lock line + degraded spawn + child 'gdi init' + 'encoder selector: software')"
G5_FRAMES=$(node -e 'try{const j=require(process.argv[1]);console.log(j.frames)}catch(e){console.log(0)}' "$ART/g5-viewer.json" 2>/dev/null || echo 0)
gate "5-crash-loop-degraded" "$([ "${G5_DEGRADED:-0}" -ge 1 ] && [ "${G5_DEGRADED_RESPAWN:-0}" -ge 1 ] && echo 1 || echo 0)" "crash_loop_degraded log x$G5_DEGRADED + degraded respawn x$G5_DEGRADED_RESPAWN"
gate "5-degraded-frames-flow" "$([ "${G5_FRAMES:-0}" -gt 10 ] && echo 1 || echo 0)" "viewer frames=$G5_FRAMES during/after crash-loop (gdi path)"

# --- Core GEN1b: fresh core to clear the crash-loop degraded lock ------------
# run-9 lesson: after gate 5 the core is degraded-locked (desktop restart in
# 32000ms, gdi) - the gate-6a viewer then never decodes a keyframe within 25s
# and aborts the input script before any SAS step runs. Restart a fresh gen1
# core (--allow-sas) and let the agent reconnect before SAS busy.
say "Core GEN1b: fresh core (clear crash-loop degraded lock) for gate 6a"
stop_core
start_core gen1b --allow-sas
sleep 20

# --- GATE 6a: SAS busy (agent-side, core gen1 --allow-sas) -------------------

say "Gate 6a: concurrent secure_attention -> busy"
cat > "$ART/g6a-steps.json" <<'EOF'
[ {"op":"sas_async"},
  {"op":"sas"},
  {"op":"wait","ms":2000} ]
EOF
G6A_BUSY=0; G6A_BUSY2=0
# The in-flight window is the core RPC (~ms); retry the back-to-back pair a
# few times until a busy reply lands (dedup is unit-tested; live evidence is
# best-effort observational).
for att in 1 2 3 4 5; do
  viewer "$ART/g6a-viewer-$att.json" "$ART/g6a-viewer-$att.log" 30s --input-script "$ART/g6a-steps.json"
  B1=$(grep -c "code=busy" "$ART/g6a-viewer-$att.log" || true)
  B2=$(node -e 'try{const j=require(process.argv[1]);const s=(j.input&&j.input.steps)||[];console.log(s.some(x=>(x.detail||"").includes("busy"))?1:0)}catch(e){console.log(0)}' "$ART/g6a-viewer-$att.json" 2>/dev/null || echo 0)
  echo "attempt $att: busy-in-log=$B1 busy-in-step=$B2"
  G6A_BUSY=$((G6A_BUSY + B1)); [ "$B2" -ge 1 ] && G6A_BUSY2=1
  [ $((G6A_BUSY + G6A_BUSY2)) -ge 1 ] && break
done
cp "$ART/g6a-viewer-1.json" "$ART/g6a-viewer.json" 2>/dev/null || true
gate "6-sas-busy" "$([ \( "${G6A_BUSY:-0}" -ge 1 \) -o \( "${G6A_BUSY2:-0}" -ge 1 \) ] && echo 1 || echo 0)" "concurrent SAS -> busy (log x$G6A_BUSY, step detail x$G6A_BUSY2, 5 attempts max)"

# --- GATE 7: exec output truncation honesty ----------------------------------

say "Gate 7: >4MB exec output -> truncated=true + marker"
# NOTE: $NODE_ID is REQUIRED before the command (run-8/9 lesson: without it
# the command string lands in the node-ref slot and the CLI fails USAGE).
G7_OUT=$(xncdev exec --json --shell cmd --timeout 240 "$NODE_ID" \
  "cmd /c for /L %i in (1,1,700000) do @echo 12345678901234567890123456789012345678901234567890" 2>/dev/null || true)
echo "$G7_OUT" > "$ART/g7-truncation.json"
G7_TRUNC=$(printf '%s' "$G7_OUT" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{try{console.log(JSON.parse(s).data.truncated?1:0)}catch(e){console.log(0)}})')
G7_MARKER=$(printf '%s' "$G7_OUT" | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{try{console.log((JSON.parse(s).data.stderr||"").includes("[xnc] output truncated:")?1:0)}catch(e){console.log(0)}})')
G7_BYTES=$(printf '%s' "$G7_OUT" | wc -c)
gate "7-truncated-flag" "$G7_TRUNC" "EXEC_RESULT.truncated=$G7_TRUNC (payload ${G7_BYTES}B captured in g7-truncation.json)"
gate "7-truncated-marker" "$G7_MARKER" "stderr marker line present=$G7_MARKER"

# --- Core GEN2: forced health drop for gate 6b ------------------------------

say "Core GEN2: XNC_FORCE_DXGI_HEALTH=50 (health-drop regression)"
stop_core
{
  printf '@echo off\r\nset XNC_FORCE_DXGI_HEALTH=50\r\n'
  printf 'C:\\xnc-dev\\xnc-core.exe --console --smoke-secret %s --pipe-name %s --allow-sas > C:\\xnc-dev\\core.log 2>&1\r\n' "$CORE_SECRET_HEX" "$CORE_PIPE"
} > "$ART/core-gen2.cmd"
"$XNC" put "$NODE" "$ART/core-gen2.cmd" "$REMOTE_DIR\\core-dev.cmd"
"$XNC" exec "$NODE" "schtasks /Create /F /TN $CORE_TASK /TR 'C:\xnc-dev\core-dev.cmd' /SC ONCE /ST 23:59 /RU SYSTEM" >/dev/null
"$XNC" exec "$NODE" "schtasks /Run /TN $CORE_TASK" >/dev/null
sleep 3

say "Gate 6b: health drop -> GDI -> probe recovery -> health=100"
viewer "$ART/g6b-viewer.json" "$ART/g6b-viewer.log" 100s
core_log > "$ART/g6b-core.log"
G6B_GDI=$(grep "backend_changed" "$ART/g6b-core.log" | grep -c "backend=gdi" || true)
G6B_RECOVER=$(grep "backend_changed" "$ART/g6b-core.log" | grep -c "backend=dxgi.*health=100" || true)
G6B_FRAMES=$(node -e 'try{const j=require(process.argv[1]);console.log(j.frames)}catch(e){console.log(0)}' "$ART/g6b-viewer.json" 2>/dev/null || echo 0)
gate "6-health-drop-gdi" "$([ "${G6B_GDI:-0}" -ge 1 ] && echo 1 || echo 0)" "forced health=50 -> backend_changed backend=gdi x$G6B_GDI"
gate "6-health-recover-100" "$([ "${G6B_RECOVER:-0}" -ge 1 ] && echo 1 || echo 0)" "probe recovery -> backend_changed backend=dxgi health=100 x$G6B_RECOVER (frames=$G6B_FRAMES)"
echo "NOTE gate 6 storm-backoff: no env injection hook on the live node; covered by native/desktop selftest + pipeline unit assertions (see results doc)."

# --- Summary -----------------------------------------------------------------

say "Summary"
core_log > "$ART/core-gen2-final.log" || true
"$XNC" exec "$NODE" 'Get-Content C:\xnc-dev\dev-agent.log -Tail 10; exit 0' > "$ART/agent-tail.log" 2>&1 || true
if [ "$GATE_FAIL" = "1" ]; then
  echo "RESULT: FAIL (see gates above; artifacts in $ART)"
  exit 1
fi
echo "RESULT: PASS (artifacts in $ART)"
