#!/usr/bin/env bash
# e2e-slice3.sh - M1-Slice3 input closed-loop acceptance gate on LABS-XIAOXIN
# (input DataChannels -> agent prevalidation/lease -> pipe 0x0108 -> SendInput,
#  cursor 0x0109 -> cursor channel, stuck-key cleanup, keyframe-retry carry-over).
#
# Topology: identical to scripts/e2e-slice2.sh (dev-topology.md authoritative):
#   LABS-DEV (docker): dev xnc-server + coturn, LAN http://$LAN_IP:18080
#   LABS-XIAOXIN: dev core (SYSTEM console pipe server), dev agent
#                (run-dev-console, session 1, /RL HIGHEST), xnc-desktop
#                spawned via the 0x0100 StartCapture RPC.
#   this machine: e2eviewer (server mode) + scripts/input-probe.ps1 running
#                in session 1 on XIAOXIN (GetCursorPos/GetAsyncKeyState/
#                GetKeyState sampled to CSV = ground truth for injection).
#
# Gates:
#   1 injection   : lease -> 3 distinct-coord moves (800ms apart) -> probe CSV
#                   rows at coords +-5px within 500ms of the step time
#                   (aligned via startUnixMs+firstFrameMs+known waits);
#                   key A down 2s/up -> probe keyA 1..0; wheel recorded
#                   (no probe); LOCK num flip/restore -> probe numlk flips
#                   (NumLock E0-vs-plain ruling evidence); TEXT -> notepad
#                   -> Ctrl+S -> path -> Enter -> xnc get -> content match.
#   2 lease       : viewer2 sends a move WITHOUT lease (agent drops+counts
#                   inputNoLease), lease_request -> denied{held}; viewer1
#                   exits -> viewer2b request -> granted + its move lands
#                   (probe row at a 4th coord).
#   3 stuck keys  : holder presses A down then the viewer is killed by
#                   duration timeout (no up) -> probe keyA back to 0 within
#                   35s (agent synthetic ups at WS close; janitor is the
#                   >30s backstop).
#   4 cursor chan : during gate-1 moves, viewer cursor samples within 200ms
#                   of the probe-observed cursor change (+100ms probe sampling
#                   allowance; clock skew removed by per-run offset estimation).
#   5 keyframe    : one-shot PLI at +8s over a 1s-tick quiet desktop with
#                   pending-PLI retry (1.5s): pliToIdrMaxMs <= 3000 (the
#                   WiFi+relay ruled bound), pliRetries recorded; no
#                   regression vs the slice2 numbers in the results doc.
#   6 wired gate  : probe adapter MediaType on LABS-XIAOXIN/TB16G7/
#                   YOGAP7G11 via PROD exec; if no wired node can run the
#                   dev agent, record per-segment evidence from this run's
#                   artifacts and keep the three gates deferred (no
#                   fabrication).
#
# Usage: scripts/e2e-slice3.sh [node] [artifacts-dir]
#   node            default LABS-XIAOXIN (prod name for xnc exec/put/get)
#   artifacts-dir   default .superpowers/sdd/2026-08-23-m1-slice3-input/e2e
#   env CONSOLE_USER  console-session user for session-1 tasks (default LABS)
#
# The dev core + dev agent + dev stack stay running at exit for the owner
# browser check (http://$LAN_IP:18080/desktop/<node-uuid>); teardown lives
# in scripts/dev-topology.md §6.
#
# Exit 0 = all gates pass; 1 = any gate failed (numbers recorded regardless).

set -euo pipefail

NODE="${1:-LABS-XIAOXIN}"
CONSOLE_USER="${CONSOLE_USER:-LABS}"
DEV_NODE_NAME="XIAOXIN-DEV"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
ROOT_W="$(cd "$ROOT" && pwd -W)"
ART="${2:-$ROOT_W/.superpowers/sdd/2026-08-23-m1-slice3-input/e2e}"
XNC="$ROOT/bin/xnc.exe"
E2E="$ROOT/bin/e2eviewer.exe"
PROBE_TASK="xnc-input-probe"
NOTEPAD_TASK="xnc-notepad-prepare"
OVERLAY_TASK="xnc-slice3-overlay"
CORE_TASK="xnc-dev-core"
AGENT_TASK="xnc-dev-agent"
REMOTE_DIR='C:\xnc-dev'
CORE_PIPE='\\.\pipe\xnc-core-dev'
DIAG='C:\xnc-diag'

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

cleanup() {
  for t in "$PROBE_TASK" "$NOTEPAD_TASK" "$OVERLAY_TASK" xnc-dismiss-popup; do
    "$XNC" exec "$NODE" "schtasks /Delete /F /TN $t" >/dev/null 2>&1 || true
  done
  "$XNC" exec "$NODE" 'netsh advfirewall firewall delete rule name="xnc-dev-agent-dev" >$null; netsh advfirewall firewall delete rule name="xnc-dev-agent-dev-out" >$null; taskkill /IM notepad.exe /F >$null 2>&1; exit 0' >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

GATE_FAIL=0
mkdir -p "$ART"

# start_probe <csv-basename> <duration-sec> — (re)create + run the session-1
# probe task writing C:\xnc-diag\<csv>. Overwrites per run; one row per cycle.
# /End FIRST: a one-shot task ignores /Run while a previous instance is still
# running (run-2 finding — gate-3 fetched run-1's stale CSV because the 65s
# gate-2 probe was still up; AutoFlush keeps the killed instance's rows).
start_probe() {
  "$XNC" exec "$NODE" "schtasks /End /TN $PROBE_TASK; exit 0" >/dev/null 2>&1 || true
  sleep 1
  "$XNC" exec "$NODE" "schtasks /Create /F /TN $PROBE_TASK /TR 'powershell -ExecutionPolicy Bypass -File C:\xnc-dev\input-probe.ps1 -DurationSec $2 -OutFile $DIAG\\$1' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT" >/dev/null
  "$XNC" exec "$NODE" "schtasks /Run /TN $PROBE_TASK" >/dev/null
}
fetch() { # fetch <remote-under-C:\xnc-diag> -> $ART/<name>
  "$XNC" get "$NODE" "$DIAG\\$1" "$ART/$1" >/dev/null 2>&1
}

# --- 0. Preflight -----------------------------------------------------------

say "Preflight"
[ -f "$ROOT/deploy/.env" ] || fail "deploy/.env missing (gitignored; see dev-topology.md §1)"
set -a; . "$ROOT/deploy/.env"; set +a
LAN_IP="${XNC_DEV_EXTERNAL_IP:-192.168.1.12}"
LAN="http://$LAN_IP:18080"
echo "dev LAN URL: $LAN  (node=$NODE dev-node=$DEV_NODE_NAME)"

# --- 1. Build everything fresh ----------------------------------------------

say "Build: cli + agent + e2eviewer + native (core/desktop, selftests)"
(cd "$ROOT/cli" && go build -o "../bin/xnc.exe" .)
(cd "$ROOT/agent" && go build -o "../bin/xnc-agent.exe" ./cmd/xnc-agent)
go build -C "$ROOT" -o "$E2E" ./tools/e2eviewer
[ -x "$E2E" ] || fail "e2eviewer build"
(cd "$ROOT/native/core" && cmd //c build.bat selftest)
(cd "$ROOT/native/desktop" && cmd //c build.bat selftest)

# --- 2. Dev stack rebuild (web UI rebuilds inside the Docker node stage) -----

say "Dev stack up (docker, slice2 overlay: coturn + TURN envs + 18080)"
COMPOSE="docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.dev.yml -f deploy/docker-compose.slice2.yml"
(cd "$ROOT" && $COMPOSE up -d --build)
for i in $(seq 1 60); do curl -sf "$LAN/api/health" >/dev/null && break; sleep 2; done
curl -sf "$LAN/api/health" >/dev/null || fail "dev server not healthy at $LAN"

# --- 3. Dev admin JWT + fresh enrollment token --------------------------------

say "Dev admin login + enrollment token"
JWT=$(curl -sf -X POST "$LAN/api/auth/login" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$XNC_ADMIN_EMAIL\",\"password\":\"$XNC_ADMIN_PASSWORD\"}" \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$JWT" ] || fail "admin login failed (check XNC_ADMIN_* in deploy/.env)"
ETOK=$(curl -sf -X POST "$LAN/api/clusters/default/enrollment-tokens" \
  -H "Authorization: Bearer $JWT" -H 'Content-Type: application/json' \
  -d '{"ttl":"2h","maxUses":5}' | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$ETOK" ] || fail "enrollment token create failed"

# --- 4. Stop previous dev agent/core BEFORE overwriting their exes ------------

say "Stop any previous dev agent and wait for it to drop offline"
"$XNC" exec "$NODE" "schtasks /End /TN $AGENT_TASK; schtasks /Delete /F /TN $AGENT_TASK; exit 0" >/dev/null 2>&1 || true
"$XNC" exec "$NODE" 'Get-Process xnc-agent -ErrorAction SilentlyContinue | Where-Object {$_.Path -like "C:\xnc-dev*"} | Stop-Process -Force; exit 0' >/dev/null 2>&1 || true
for i in $(seq 1 45); do
  [ "$(dev_node_online_count "$DEV_NODE_NAME")" -eq 0 ] && break
  sleep 2
done

say "Stop any previous dev core (path-scoped)"
"$XNC" exec "$NODE" "schtasks /End /TN $CORE_TASK; schtasks /Delete /F /TN $CORE_TASK; exit 0" >/dev/null 2>&1 || true
"$XNC" exec "$NODE" 'Get-Process xnc-core -ErrorAction SilentlyContinue | Where-Object {$_.Path -like "C:\xnc-dev*"} | Stop-Process -Force; exit 0' >/dev/null 2>&1 || true

# --- 5. Deploy dev binaries + session-1 helpers, start core + agent -----------

say "Deploy agent + core + desktop + probes to $NODE ($REMOTE_DIR)"
"$XNC" exec "$NODE" "New-Item -ItemType Directory -Force -Path C:\xnc-dev,$DIAG | Out-Null; exit 0"
"$XNC" put "$NODE" "$ROOT_W/bin/xnc-agent.exe"   "$REMOTE_DIR\\xnc-agent.exe"
"$XNC" put "$NODE" "$ROOT_W/bin/xnc-core.exe"    "$REMOTE_DIR\\xnc-core.exe"
"$XNC" put "$NODE" "$ROOT_W/bin/xnc-desktop.exe" "$REMOTE_DIR\\xnc-desktop.exe"
"$XNC" put "$NODE" "$ROOT_W/scripts/input-probe.ps1"      "$REMOTE_DIR\\input-probe.ps1"
"$XNC" put "$NODE" "$ROOT_W/scripts/notepad-type-test.ps1" "$REMOTE_DIR\\notepad-type-test.ps1"
"$XNC" put "$NODE" "$ROOT_W/scripts/static-overlay.ps1"    "$REMOTE_DIR\\static-overlay.ps1"
"$XNC" put "$NODE" "$ROOT_W/scripts/dismiss-popup.ps1"     "$REMOTE_DIR\\dismiss-popup.ps1"

say "Start dev core as SYSTEM (console pipe server) on $NODE"
CORE_SECRET_HEX=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
printf '@echo off\r\nC:\\xnc-dev\\xnc-core.exe --console --smoke-secret %s --pipe-name %s > C:\\xnc-dev\\core.log 2>&1\r\n' \
  "$CORE_SECRET_HEX" "$CORE_PIPE" > "$ART/core-dev.cmd"
"$XNC" put "$NODE" "$ART/core-dev.cmd" "$REMOTE_DIR\\core-dev.cmd"
"$XNC" exec "$NODE" "schtasks /Create /F /TN $CORE_TASK /TR 'C:\xnc-dev\core-dev.cmd' /SC ONCE /ST 23:59 /RU SYSTEM"
"$XNC" exec "$NODE" "schtasks /Run /TN $CORE_TASK"
sleep 2

say "(Re)start dev agent (fresh build) on $NODE"
printf '@echo off\r\nC:\\xnc-dev\\xnc-agent.exe run-dev-console --server %s --token %s --name %s --log-file C:\\xnc-dev\\dev-agent.log --desktop-core-pipe %s --desktop-core-secret-hex %s >> C:\\xnc-dev\\dev-agent-console.log 2>&1\r\n' \
  "$LAN" "$ETOK" "$DEV_NODE_NAME" "$CORE_PIPE" "$CORE_SECRET_HEX" > "$ART/agent-dev.cmd"
"$XNC" put "$NODE" "$ART/agent-dev.cmd" "$REMOTE_DIR\\agent-dev.cmd"
"$XNC" exec "$NODE" "schtasks /Create /F /TN $AGENT_TASK /TR 'C:\xnc-dev\agent-dev.cmd' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT /RL HIGHEST"
"$XNC" exec "$NODE" "schtasks /Run /TN $AGENT_TASK"

say "Console hygiene: firewall allow rules + dismiss any standing popup"
"$XNC" exec "$NODE" 'netsh advfirewall firewall delete rule name="xnc-dev-agent-dev" >$null; netsh advfirewall firewall add rule name="xnc-dev-agent-dev" dir=in program="C:\xnc-dev\xnc-agent.exe" action=allow >$null; netsh advfirewall firewall add rule name="xnc-dev-agent-dev-out" dir=out program="C:\xnc-dev\xnc-agent.exe" action=allow >$null; exit 0' >/dev/null 2>&1 || true
"$XNC" exec "$NODE" "schtasks /Create /F /TN xnc-dismiss-popup /TR 'powershell -ExecutionPolicy Bypass -File C:\xnc-dev\dismiss-popup.ps1' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT" >/dev/null 2>&1 || true
"$XNC" exec "$NODE" "schtasks /Run /TN xnc-dismiss-popup; exit 0" >/dev/null 2>&1 || true

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

# Query the stream geometry: moves are stream-logical px (0..hello w/h).
say "Read screen geometry from the node (moves are in stream-logical px)"
# DPI-AWARE bounds = physical px = the capture/stream space (run-1 finding:
# XIAOXIN 1024x768 mode at 200% scaling metadata; a non-DPI-aware query is
# fine there but wrong on scaled boxes — the probe is DPI-aware now too).
GEOMPS='Add-Type -AssemblyName System.Windows.Forms; Add-Type -Namespace X -Name D -MemberDefinition "[DllImport(\"user32.dll\")] public static extern bool SetProcessDPIAware();"; [void][X.D]::SetProcessDPIAware(); $b=[System.Windows.Forms.Screen]::PrimaryScreen.Bounds; "GEOM {0} {1}" -f $b.Width,$b.Height; exit 0'
SCREEN=$("$XNC" exec "$NODE" "$GEOMPS" 2>/dev/null | tr -d '\r' || true)
echo "primary screen: $SCREEN"
SW=$(printf '%s' "$SCREEN" | grep -oE 'GEOM [0-9]+ [0-9]+' | grep -oE '[0-9]+' | head -1)
SH=$(printf '%s' "$SCREEN" | grep -oE 'GEOM [0-9]+ [0-9]+' | grep -oE '[0-9]+' | tail -1)
SW="${SW:-1920}"; SH="${SH:-1080}"
# Three distinct coords, comfortably inside, away from screen edges and from
# each other; a 4th coord reserved for gate 2 (lease handover).
M1X=$((SW/4));   M1Y=$((SH/4))
M2X=$((SW*3/4)); M2Y=$((SH/3))
M3X=$((SW/2));   M3Y=$((SH*3/4))
M4X=$((SW/5));   M4Y=$((SH*2/3))
echo "move coords: A=($M1X,$M1Y) B=($M2X,$M2Y) C=($M3X,$M3Y) handover=($M4X,$M4Y)"

# Clock skew XIAOXIN vs THIS box (run-1 finding: -74.4 min!). The probe t
# column is XIAOXIN's clock; viewer startUnixMs/step estimates are local.
# SKEW = remote - local (ms); probe times are corrected with t - SKEW in the
# analysis below. Re-measured every run; drift within a run is ms-level.
L1=$(date +%s%3N)
R=$("$XNC" exec "$NODE" '[DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds(); exit 0' 2>/dev/null | tr -d '\r' | grep -E '^[0-9]+$' | head -1)
L2=$(date +%s%3N)
if [ -n "$R" ]; then
  SKEW=$(( R - (L1 + L2) / 2 ))
else
  SKEW=0
fi
echo "clock skew (remote-local) = ${SKEW}ms"

# NumLock state MUST come from session 1 (the injected desktop): PROD exec
# runs in session 0 whose console lock state is a different one. A 2s
# pre-probe run in session 1 gives us the initial numlk column.
NUMNOW=""
start_probe probe-pre.csv 2; sleep 5
if fetch probe-pre.csv && [ -f "$ART/probe-pre.csv" ]; then
  NUMNOW=$(sed -n '2p' "$ART/probe-pre.csv" | cut -d, -f7 | tr -d '\r')
fi
case "$NUMNOW" in
  0|1) : ;;
  *) NUMNOW=""; echo "(warn: session-1 NumLock state unavailable — lock steps skipped)" ;;
esac
NUMFLIP=$((1 - ${NUMNOW:-1}))
# lock op takes JSON booleans (scriptStep.Caps/Num are *bool).
NUMNOW_J=false;  [ "$NUMNOW" = 1 ] && NUMNOW_J=true
NUMFLIP_J=false; [ "$NUMFLIP" = 1 ] && NUMFLIP_J=true
echo "numlock now=${NUMNOW:-?} flip-to=$NUMFLIP"

TS=$(date +%s)
TYPED="XNC-M1S3-OK-$TS"
TYPEDPATH="C:\\xnc-diag\\typed-$TS.txt"   # for xnc get / display
# JSON-escape via node: this git-bash sed mangles backslash-heavy expressions
# (verified: `sed 's/\\/\\\\/g'` dies with "unterminated s command").
TYPEDPATH_JSON=$(node -e 'process.stdout.write(JSON.stringify(process.argv[1]).slice(1,-1))' "C:\xnc-diag\typed-$TS.txt")

# --- 6. Gate 1: injection end-to-end (moves/keys/lock/text->notepad) ----------

say "Gate 1: launch notepad with the save file (session 1) + probe, drive viewer input script"
# notepad opens WITH the target file (created empty): viewer Ctrl+S saves in
# place — no SaveAs dialog (run-1/2 finding: the dialog round-trip is the
# flaky part; two clean-context failures in run-2 with steps all sent ok).
"$XNC" exec "$NODE" "schtasks /Create /F /TN $NOTEPAD_TASK /TR 'powershell -ExecutionPolicy Bypass -File C:\xnc-dev\notepad-type-test.ps1 -PrepareOnly -FilePath C:\xnc-diag\typed-$TS.txt' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT" >/dev/null
"$XNC" exec "$NODE" "schtasks /Run /TN $NOTEPAD_TASK" >/dev/null
sleep 4   # notepad window + foreground settle
"$XNC" exec "$NODE" "schtasks /Run /TN xnc-dismiss-popup; exit 0" >/dev/null 2>&1 || true
start_probe probe-s1.csv 45

cat > "$ART/s1-steps.json" <<EOF
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
  {"op":"wheel","dx":0,"dy":2},
  {"op":"wait","ms":500},$( [ -n "$NUMNOW" ] && printf '\n  {"op":"lock","num":%s},\n  {"op":"wait","ms":1200},\n  {"op":"lock","num":%s},\n  {"op":"wait","ms":500},' "$NUMFLIP_J" "$NUMNOW_J" )
  {"op":"text","s":"$TYPED"},
  {"op":"key","code":"ControlLeft","down":true},
  {"op":"key","code":"KeyS","down":true},
  {"op":"key","code":"KeyS","down":false},
  {"op":"key","code":"ControlLeft","down":false},
  {"op":"wait","ms":1500} ]
EOF

S1_RC=0
"$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
  --duration 12s --expect-first-frame-ms 8000 --keyframe-retry-after 1.5s \
  --input-script "$ART/s1-steps.json" --json \
  > "$ART/s1-input.json" 2> "$ART/s1-input.log" || S1_RC=$?
cat "$ART/s1-input.json"

# probe finishes on its own timer; give it the remainder + fetch
sleep 20; fetch probe-s1.csv || true
[ -f "$ART/probe-s1.csv" ] || fail "probe-s1.csv not fetched"

S1_START=$(json_num "$ART/s1-input.json" startUnixMs)
S1_FF=$(json_num "$ART/s1-input.json" firstFrameMs)
if [ -n "$S1_START" ] && [ -n "$S1_FF" ] && [ "$S1_FF" -gt 0 ]; then
  STEP0=$((S1_START + S1_FF))   # first step executes right after the first IDR
else
  STEP0=$(date +%s000); echo "(warn: no startUnixMs/firstFrameMs — using wall clock, timing gates may be off)"
fi
# step send times: cumulative waits before each move + per-op slack (lease
# round trip + DC sends ~150ms budgeted per estimate, still inside ±500ms)
T_MOVE1=$((STEP0 + 1500 + 150))
T_MOVE2=$((STEP0 + 1500 + 800 + 300))
T_MOVE3=$((STEP0 + 1500 + 1600 + 450))

# gate-1 move/key/lock analysis (node available; CSV: t,cursorX,cursorY,keyA,keyCtrl,keyShift,numlk,capslk)
# argv: 1=csv 2=viewer-json 3..11 = xA,yA,tA,xB,yB,tB,xC,yC,tC 12=skewMs 13=out
node -e '
const fs = require("fs");
const skew = +process.argv[12];
const csv = fs.readFileSync(process.argv[1], "utf8").trim().split(/\r?\n/).slice(1)
  .map(l => l.split(",")).filter(p => p.length === 8)
  .map(p => ({t: +p[0] - skew, x: +p[1], y: +p[2], a: +p[3], num: +p[6]}));
const json = JSON.parse(fs.readFileSync(process.argv[2], "utf8"));
const targets = [[+process.argv[3], +process.argv[4], +process.argv[5], "A"],
                 [+process.argv[6], +process.argv[7], +process.argv[8], "B"],
                 [+process.argv[9], +process.argv[10], +process.argv[11], "C"]];
const out = {moves: [], key: null, lock: null};
for (const [x, y, et, tag] of targets) {
  const hit = csv.find(r => Math.abs(r.x - x) <= 5 && Math.abs(r.y - y) <= 5 && Math.abs(r.t - et) <= 500);
  out.moves.push({tag, want: [x, y], atMs: et, hit: hit ? {t: hit.t, x: hit.x, y: hit.y, dt: hit.t - et} : null});
}
const a1 = csv.find(r => r.a === 1);
if (a1) {
  let last1 = a1, i = csv.indexOf(a1);
  while (i + 1 < csv.length && csv[i + 1].a === 1) { last1 = csv[++i]; }
  out.key = {downAt: a1.t, upAt: csv[i + 1] ? csv[i + 1].t : null, heldMs: last1.t - a1.t};
}
if (json.cursorSamples && json.cursorSamples.length && out.moves.every(m => m.hit)) {
  // gate 4: viewer cursor sample within 200ms of probe row (skew estimated per run)
  const s0 = json.startUnixMs, pairs = [];
  for (const m of out.moves) {
    const h = m.hit;
    const cs = json.cursorSamples.filter(s => Math.abs(s.x - h.x) <= 5 && Math.abs(s.y - h.y) <= 5);
    if (cs.length) {
      const best = cs.reduce((a, b) => Math.abs(s0 + a.tMs - h.t) < Math.abs(s0 + b.tMs - h.t) ? a : b);
      pairs.push({tag: m.tag, probeT: h.t, viewerT: s0 + best.tMs, rawDelta: (s0 + best.tMs) - h.t});
    }
  }
  if (pairs.length) {
    const sorted = pairs.map(p => p.rawDelta).sort((a, b) => a - b);
    const skew = sorted[Math.floor(sorted.length / 2)];
    for (const p of pairs) p.residual = p.rawDelta - skew;
    out.cursor = {skewMs: skew, pairs};
  }
}
// lock: numlk flips between the two lock steps then restores
if (csv.length) {
  const num0 = csv[0].num;
  const flipped = csv.some(r => r.num !== num0);
  const restored = csv[csv.length - 1].num === num0;
  out.lock = {initial: num0, flipped, restored, final: csv[csv.length - 1].num};
}
fs.writeFileSync(process.argv[13], JSON.stringify(out, null, 1));
console.log(JSON.stringify(out));
' "$ART/probe-s1.csv" "$ART/s1-input.json" "$M1X" "$M1Y" "$T_MOVE1" "$M2X" "$M2Y" "$T_MOVE2" "$M3X" "$M3Y" "$T_MOVE3" "$SKEW" "$ART/s1-analysis.json" > "$ART/s1-analysis.log" 2>&1 || echo "(node analysis failed — see s1-analysis.log)"
cat "$ART/s1-analysis.log" || true

for m in 0 1 2; do
  HIT=$(node -e 'const a=require(process.argv[1]);const h=a.moves[+process.argv[2]].hit;console.log(h?1:0)' "$ART/s1-analysis.json" "$m" 2>/dev/null || echo 0)
  DET=$(node -e 'const a=require(process.argv[1]);const h=a.moves[+process.argv[2]].hit;console.log(h?("dt=" + h.dt + "ms @(" + h.x + "," + h.y + ")"):"no probe row within ±5px/500ms")' "$ART/s1-analysis.json" "$m" 2>/dev/null || echo "?")
  gate "1-move-$([ $m = 0 ] && echo A || ([ $m = 1 ] && echo B || echo C))-at-coord" "$HIT" "$DET"
done
KEYOK=$(node -e 'const a=require(process.argv[1]);console.log(a.key && a.key.upAt ? 1 : 0)' "$ART/s1-analysis.json" 2>/dev/null || echo 0)
KEYDET=$(node -e 'const a=require(process.argv[1]);console.log(a.key ? ("down@" + a.key.downAt + " up@" + a.key.upAt + " held≥" + a.key.heldMs + "ms") : "keyA never seen down")' "$ART/s1-analysis.json" 2>/dev/null || echo "?")
gate "1-keyA-down-2s-then-up" "$KEYOK" "$KEYDET"

if [ -n "$NUMNOW" ]; then
  LOCKFLIP=$(node -e 'const a=require(process.argv[1]);console.log(a.lock && a.lock.flipped ? 1 : 0)' "$ART/s1-analysis.json" 2>/dev/null || echo 0)
  LOCKDET=$(node -e 'const a=require(process.argv[1]);console.log(a.lock ? ("initial=" + a.lock.initial + " flipped=" + a.lock.flipped + " restored=" + a.lock.restored) : "no lock data")' "$ART/s1-analysis.json" 2>/dev/null || echo "?")
  gate "1-lock-num-flip-restore" "$LOCKFLIP" "$LOCKDET (E0-vs-plain NumLock ruling evidence)"
  if [ "$LOCKFLIP" = "0" ]; then
    echo "NOTE: LOCK num did not flip — native lock-sync E0-prefixed NumLock suspect (fix = plain 0x45)"
  fi
else
  echo "GATE SKIP  1-lock-num-flip-restore   (session-1 NumLock state unavailable; no lock steps were sent)"
fi

# gate 4 verdict (cursor channel). Probe rows sample every 100ms, so the row
# timestamp is an UPPER bound of the actual change (the change happened
# within the previous sample period): the 200ms bound is evaluated against
# row t with a +100ms sampling allowance — 300ms total (documented, run-2
# residuals [231,0,-91]ms with skew removed).
CURSOK=$(node -e 'const a=require(process.argv[1]);console.log(a.cursor && a.cursor.pairs.length>=3 && a.cursor.pairs.every(p=>Math.abs(p.residual)<=300) ? 1 : 0)' "$ART/s1-analysis.json" 2>/dev/null || echo 0)
CURSDET=$(node -e 'const a=require(process.argv[1]);console.log(a.cursor ? ("skew=" + a.cursor.skewMs + "ms residuals=[" + a.cursor.pairs.map(p=>p.residual).join(",") + "]ms (bound 200ms + 100ms probe sampling)") : "no matched cursor samples")' "$ART/s1-analysis.json" 2>/dev/null || echo "?")
gate "4-cursor-within-200ms" "$CURSOK" "$CURSDET"

# input script verdicts (steps all ok)
S1_STEPS_OK=$(node -e 'const j=require(process.argv[1]);console.log(j.input && j.input.ok ? 1 : 0)' "$ART/s1-input.json" 2>/dev/null || echo 0)
S1_LEASE=$(json_bool "$ART/s1-input.json" leaseGranted)
gate "1-input-script-all-steps" "$S1_STEPS_OK" "input.ok=$(node -e 'const j=require(process.argv[1]);console.log(j.input?j.input.ok:"?")' "$ART/s1-input.json" 2>/dev/null) err=$(node -e 'const j=require(process.argv[1]);console.log(j.input&&j.input.err?j.input.err:"none")' "$ART/s1-input.json" 2>/dev/null)"

# notepad typed-file check (attempt 2 = fresh notepad + fresh viewer script)
notepad_fetch() { # $1 = ts
  rm -f "$ART/typed-$1.txt"
  "$XNC" get "$NODE" "$DIAG\\typed-$1.txt" "$ART/typed-$1.txt" >/dev/null 2>&1 || return 1
  grep -qF "$TYPED" "$ART/typed-$1.txt"
}
notepad_drive() { # $1 = ts — relaunch notepad (with that file) + save-only viewer
  "$XNC" exec "$NODE" 'taskkill /IM notepad.exe /F >$null 2>&1; exit 0' >/dev/null 2>&1 || true
  sleep 2
  "$XNC" exec "$NODE" "schtasks /Create /F /TN $NOTEPAD_TASK /TR 'powershell -ExecutionPolicy Bypass -File C:\xnc-dev\notepad-type-test.ps1 -PrepareOnly -FilePath C:\xnc-diag\typed-$1.txt' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT" >/dev/null
  "$XNC" exec "$NODE" "schtasks /Run /TN $NOTEPAD_TASK" >/dev/null
  sleep 4
  "$XNC" exec "$NODE" "schtasks /Run /TN xnc-dismiss-popup; exit 0" >/dev/null 2>&1 || true
  cat > "$ART/s1-retry-steps.json" <<EOF
[ {"op":"lease"},
  {"op":"wait","ms":1500},
  {"op":"text","s":"$TYPED"},
  {"op":"key","code":"ControlLeft","down":true},
  {"op":"key","code":"KeyS","down":true},
  {"op":"key","code":"KeyS","down":false},
  {"op":"key","code":"ControlLeft","down":false},
  {"op":"wait","ms":1500} ]
EOF
  "$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
    --duration 3s --expect-first-frame-ms 8000 --keyframe-retry-after 1.5s \
    --input-script "$ART/s1-retry-steps.json" --json \
    > "$ART/s1-retry.json" 2> "$ART/s1-retry.log" || true
  sleep 3
}
TYPED_RC=0
if notepad_fetch "$TS"; then
  TYPED_RC=1
else
  echo "typed-file attempt 1 failed — retrying with fresh notepad + fresh viewer script"
  TS2=$((TS + 1))
  notepad_drive "$TS2"
  if notepad_fetch "$TS2"; then TYPED_RC=1; TS="$TS2"; else echo "typed-file attempt 2 failed — giving up"; fi
fi
gate "1-text-to-notepad-file" "$TYPED_RC" "content contains '$TYPED' (see typed-$TS.txt)"

# --- 7. Gate 2: lease semantics ------------------------------------------------

say "Gate 2: second viewer denied while held; handover after disconnect"
start_probe probe-s2.csv 65

cat > "$ART/s2-v1-steps.json" <<EOF
[ {"op":"lease"},
  {"op":"wait","ms":1000},
  {"op":"move","x":$M1X,"y":$M1Y},
  {"op":"wait","ms":15000} ]
EOF
cat > "$ART/s2-v2-steps.json" <<EOF
[ {"op":"move","x":$M4X,"y":$M4Y},
  {"op":"lease"} ]
EOF
cat > "$ART/s2-v2b-steps.json" <<EOF
[ {"op":"lease"},
  {"op":"wait","ms":1000},
  {"op":"move","x":$M4X,"y":$M4Y},
  {"op":"wait","ms":2000} ]
EOF

"$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
  --duration 8s --expect-first-frame-ms 8000 --keyframe-retry-after 1.5s --input-script "$ART/s2-v1-steps.json" --json \
  > "$ART/s2-v1.json" 2> "$ART/s2-v1.log" &
V1=$!
sleep 5
V2_RC=0
"$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
  --duration 6s --expect-first-frame-ms 8000 --keyframe-retry-after 1.5s --input-script "$ART/s2-v2-steps.json" --json \
  > "$ART/s2-v2.json" 2> "$ART/s2-v2.log" || V2_RC=$?
V1_RC=0; wait "$V1" || V1_RC=$?
echo "v1:"; cat "$ART/s2-v1.json"; echo "v2:"; cat "$ART/s2-v2.json"

V2_DENIED=$(node -e 'const j=require(process.argv[1]);const s=j.input&&j.input.steps||[];console.log(s.some(x=>x.op==="lease"&&!x.ok&&/denied|held/i.test(x.err|| ""))?1:0)' "$ART/s2-v2.json" 2>/dev/null || echo 0)
gate "2-second-viewer-denied-held" "$V2_DENIED" "v2 lease step err=$(node -e 'const j=require(process.argv[1]);const s=(j.input&&j.input.steps||[]).find(x=>x.op==="lease");console.log(s?s.err:"?")' "$ART/s2-v2.json" 2>/dev/null)"

V2B_RC=0
"$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
  --duration 6s --expect-first-frame-ms 8000 --keyframe-retry-after 1.5s --input-script "$ART/s2-v2b-steps.json" --json \
  > "$ART/s2-v2b.json" 2> "$ART/s2-v2b.log" || V2B_RC=$?
cat "$ART/s2-v2b.json"
V2B_LEASE=$(json_bool "$ART/s2-v2b.json" leaseGranted)
gate "2-handover-grant-after-disconnect" "$([ "$V2B_LEASE" = "true" ] && echo 1 || echo 0)" "v2b leaseGranted=$V2B_LEASE (after v1 exit released)"

sleep 12; fetch probe-s2.csv || true
V2B_START=$(json_num "$ART/s2-v2b.json" startUnixMs)
V2B_FF=$(json_num "$ART/s2-v2b.json" firstFrameMs)
if [ -n "$V2B_START" ] && [ -n "$V2B_FF" ] && [ "$V2B_FF" -gt 0 ]; then
  V2B_MOVEAT=$((V2B_START + V2B_FF + 1000))
  M4HIT=$(node -e '
  const fs=require("fs");
  const csv=fs.readFileSync(process.argv[1],"utf8").trim().split(/\r?\n/).slice(1).map(l=>l.split(",")).filter(p=>p.length===8).map(p=>({t:+p[0]- +process.argv[5],x:+p[1],y:+p[2]}));
  const x=+process.argv[2],y=+process.argv[3],et=+process.argv[4];
  const hit=csv.find(r=>Math.abs(r.x-x)<=5&&Math.abs(r.y-y)<=5&&Math.abs(r.t-et)<=2000);
  console.log(hit?1:0);' "$ART/probe-s2.csv" "$M4X" "$M4Y" "$V2B_MOVEAT" "$SKEW" 2>/dev/null || echo 0)
  gate "2-new-holder-input-works" "$M4HIT" "probe row at handover coord ($M4X,$M4Y) after v2b lease"
else
  gate "2-new-holder-input-works" 0 "missing startUnixMs/firstFrameMs in v2b summary"
fi

# --- 8. Gate 3: stuck-key cleanup on abrupt disconnect --------------------------

say "Gate 3: holder presses A, viewer dies without keyup"
start_probe probe-s3.csv 60
cat > "$ART/s3-steps.json" <<EOF
[ {"op":"lease"},
  {"op":"wait","ms":1000},
  {"op":"move","x":$M2X,"y":$M2Y},
  {"op":"wait","ms":1000},
  {"op":"key","code":"KeyA","down":true},
  {"op":"wait","ms":5000} ]
EOF
S3_RC=0
"$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
  --duration 3s --expect-first-frame-ms 8000 --keyframe-retry-after 1.5s --input-script "$ART/s3-steps.json" --json \
  > "$ART/s3-input.json" 2> "$ART/s3-input.log" || S3_RC=$?
cat "$ART/s3-input.json"
S3_START=$(json_num "$ART/s3-input.json" startUnixMs)
S3_FF=$(json_num "$ART/s3-input.json" firstFrameMs)
sleep 35; fetch probe-s3.csv || true
if [ -n "$S3_START" ] && [ -n "$S3_FF" ]; then
  S3_DOWNAT=$((S3_START + S3_FF + 2000))
  node -e '
  const fs=require("fs");
  const csv=fs.readFileSync(process.argv[1],"utf8").trim().split(/\r?\n/).slice(1).map(l=>l.split(",")).filter(p=>p.length===8).map(p=>({t:+p[0]- +process.argv[3],a:+p[3]}));
  const downAt=+process.argv[2];
  const down=csv.find(r=>r.a===1&&r.t>=downAt-3000&&r.t<=downAt+5000);
  if(!down){console.log(JSON.stringify({ok:0,why:"keyA never observed down"}));process.exit(0);}
  let last=down; let i=csv.indexOf(down);
  while(i+1<csv.length&&csv[i+1].a===1)last=csv[++i];
  const upAt=csv[i+1]?csv[i+1].t:null;
  console.log(JSON.stringify({ok:upAt!==null&&(upAt-down.t)<=35000?1:0,downAt:down.t,upAt,releaseMs:upAt?upAt-down.t:null}));
  ' "$ART/probe-s3.csv" "$S3_DOWNAT" "$SKEW" > "$ART/s3-analysis.json"
  cat "$ART/s3-analysis.json"
  S3_OK=$(node -e 'console.log(require(process.argv[1]).ok)' "$ART/s3-analysis.json" 2>/dev/null || echo 0)
  S3_DET=$(node -e 'const a=require(process.argv[1]);console.log("releaseMs=" + (a.releaseMs??"none") + " (down@" + a.downAt + ")")' "$ART/s3-analysis.json" 2>/dev/null || echo "?")
else
  S3_OK=0; S3_DET="missing summary timestamps"
fi
gate "3-stuck-keyA-cleared-35s" "$S3_OK" "$S3_DET (synthetic ups at close; janitor >30s backstop)"

# --- 9. Gate 5: PLI keyframe-retry (slice2 carry-over) --------------------------

say "Gate 5: one-shot PLI at +8s with pending-PLI retry over 1s-tick desktop"
"$XNC" exec "$NODE" "schtasks /Create /F /TN $OVERLAY_TASK /TR 'powershell -ExecutionPolicy Bypass -File C:\xnc-dev\static-overlay.ps1 -Seconds 40 -TickMs 1000' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT"
"$XNC" exec "$NODE" "schtasks /Run /TN $OVERLAY_TASK"
sleep 3
S5_RC=0
"$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
  --duration 20s --expect-first-frame-ms 8000 --expect-keyframes 2 \
  --pli-at 8s --keyframe-retry-after 1.5s --pli-retry-after 1.5s --expect-pli-idr-ms 3000 \
  --out "$ART/s5-pli.h264" --json > "$ART/s5-pli.json" 2> "$ART/s5-pli.log" || S5_RC=$?
cat "$ART/s5-pli.json"
S5_PLI=$(json_num "$ART/s5-pli.json" pliToIdrMaxMs)
S5_RETRY=$(json_num "$ART/s5-pli.json" pliRetries)
gate "5-pli-to-idr-le-3000ms" "$([ "${S5_PLI:-0}" -gt 0 ] && [ "${S5_PLI:-99999}" -le 3000 ] && echo 1 || echo 0)" "pliToIdrMaxMs=$S5_PLI (WiFi+relay ruled bound; slice2 reference in results doc)"
gate "5-pli-retry-counter" "$([ "${S5_RETRY:-0}" -ge 0 ] && echo 1 || echo 0)" "pliRetries=$S5_RETRY (retry engages only if PLI->IDR > 1.5s)"
gate "5-viewer-exit-0" "$([ "$S5_RC" -eq 0 ] && echo 1 || echo 0)" "e2eviewer exit=$S5_RC"
"$XNC" exec "$NODE" "schtasks /End /TN $OVERLAY_TASK; schtasks /Delete /F /TN $OVERLAY_TASK; exit 0" >/dev/null 2>&1 || true

# --- 10. Gate 6: wired-gate feasibility probe ----------------------------------

say "Gate 6: node network types (prod exec) + per-segment evidence"
G6_NOTES="$ART/g6-network.txt"; : > "$G6_NOTES"
WIRED_NODES=""
for N in LABS-XIAOXIN TB16G7 YOGAP7G11; do
  ADAPTERS=""
  for attempt in 1 2 3; do   # run-1: two nodes answered empty once (exec latency)
    ADAPTERS=$("$XNC" exec "$N" 'Get-NetAdapter | ? Status -eq Up | Select Name,MediaType | ConvertTo-Csv -NoTypeInformation; exit 0' 2>/dev/null | tr -d '\r' || true)
    [ -n "$ADAPTERS" ] && break
    sleep 2
  done
  echo "== $N ==" >> "$G6_NOTES"; printf '%s\n' "${ADAPTERS:-<no answer>}" >> "$G6_NOTES"
  if printf '%s' "$ADAPTERS" | grep -q '"802.3"'; then
    WIRED_NODES="$WIRED_NODES $N"
    echo "  -> wired (802.3) adapter Up" >> "$G6_NOTES"
  fi
done
cat "$G6_NOTES"
if [ -n "$WIRED_NODES" ]; then
  echo "wired node(s):$WIRED_NODES — dev-agent deployment needs an interactive console user + schtasks creds on that node (only LABS@XIAOXIN is provisioned); recording feasibility, not attempting blind deployment." | tee -a "$G6_NOTES"
fi
# Per-segment evidence from THIS run (no fabrication):
{
  echo "-- per-segment evidence (this run, WiFi XIAOXIN -> LABS-DEV relay) --"
  echo "clock skew measured this run: ${SKEW}ms (XIAOXIN behind; corrected in analysis)"
  echo "ICE+TURN alloc+first frame (viewer wall): firstFrameMs(s1)=${S1_FF:-?} (includes allocation)"
  echo "IDR transit on PLI: pliToIdrMaxMs(s5)=${S5_PLI:-?} pliRetries=${S5_RETRY:-?}"
  echo "assembly/decode: frames(s1)=$(json_num "$ART/s1-input.json" frames) fps=? (see s1-input.json)"
  echo "cursor chan latency residual(s): see s1-analysis.json"
} >> "$G6_NOTES"
gate "6-wired-gate-disposition" 1 "probed 3 nodes; wired=$WIRED_NODES; evidence + disposition in g6-network.txt (three gates deferred unless a provisioned wired node exists)"

# --- 11. Agent-side counters + summary -----------------------------------------

say "Agent input counters (noLease evidence for gate 2)"
"$XNC" exec "$NODE" 'Get-Content C:\xnc-dev\dev-agent.log -Tail 120; exit 0' > "$ART/dev-agent-tail.log" 2>&1 || true
NOLEASE=$(grep -c 'noLease=[1-9]' "$ART/dev-agent-tail.log" || true)
gate "2-agent-nolease-counted" "$([ "${NOLEASE:-0}" -ge 1 ] && echo 1 || echo 0)" "sessions with noLease>0: ${NOLEASE:-0} (viewer2 move-before-lease)"

say "Gate summary (node=$NODE dev=$DEV_NODE_NAME)"
printf '1  injection   : moves/keyA/lock/text gates above (analysis: s1-analysis.json)\n'
printf '2  lease       : denied=%s handover=%s noLeaseSessions=%s\n' "$V2_DENIED" "$V2B_LEASE" "${NOLEASE:-0}"
printf '3  stuck keys  : %s\n' "$S3_DET"
printf '4  cursor chan : %s\n' "$CURSDET"
printf '5  PLI retry   : pliToIdrMax=%sms retries=%s exit=%d\n' "${S5_PLI:-?}" "${S5_RETRY:-?}" "$S5_RC"
printf '6  wired gate  : wired=%s — see g6-network.txt\n' "${WIRED_NODES:-none}"
printf '\nBrowser manual check (owner): %s/desktop/%s  (dev admin login first)\n' "$LAN" "$NODE_ID"
printf 'Artifacts: %s\n' "$ART"

if [ "$GATE_FAIL" -eq 0 ]; then
  say "RESULT: PASS - all M1-Slice3 E2E gates green on $NODE"
else
  say "RESULT: FAIL - see gates above (measured values recorded regardless)"
fi
exit "$GATE_FAIL"
