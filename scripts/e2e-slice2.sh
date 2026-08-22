#!/usr/bin/env bash
# e2e-slice2.sh - M1-Slice2 live-video full-chain acceptance gate on a real
# node (LABS-XIAOXIN), exercising everything the plan's Task 6 gate requires.
#
# Topology (scripts/dev-topology.md is authoritative):
#   LABS-DEV (docker): dev xnc-server + coturn, LAN http://$LAN_IP:18080
#   LABS-XIAOXIN: dev core (xnc-core --console --smoke-secret, SYSTEM),
#                dev agent (run-dev-console, LABS /RL HIGHEST session 1 —
#                the core pipe DACL is SYSTEM+Admins, so an elevated token
#                is required to connect), xnc-desktop spawned by the core
#                via the 0x0100 StartCapture RPC (stdin secret channel).
#   this machine: e2eviewer (server mode) over the LAN URL.
#
# Scenarios (all via e2eviewer → POST /api/nodes/{uuid}/desktop → session
# WS signaling → RTCPeerConnection relay-only through coturn):
#   ① change 30s: session-1 schtasks ping driver; first frame ≤5s (relay
#      first frame includes TURN allocation), ≥2 keyframes
#   ② static 45s: no driver; ≤10 AUs (near-silent desktop)
#   ③ second viewer joins mid-static (gate = on-demand IDR pickup): first
#      AU must be IDR and arrive ≤5s; runs concurrently with ②
#   ④ PLI recovery: one-shot PLI at +8s over a static desktop → new IDR
#      within 2s (warm-up replay semantics, the Slice1 carry-over)
#   ⑤ cleanup: after the last viewer exits no xnc-desktop.exe survives
#      (agent Stop refcount → core StopCapture → child exit)
#
# The run also closes the T3 carry-over debt by evidence: scenario ①
# drives the full core 0x0100 spawn path (stdin-secret channel) — video
# flowing through it IS the end-to-end proof of the spawn channel (a real
# elevated Go dev env does not exist on XIAOXIN; see the results doc).
#
# Usage: scripts/e2e-slice2.sh [node] [artifacts-dir]
#   node            default LABS-XIAOXIN (prod name for xnc exec/put)
#   artifacts-dir   default .superpowers/sdd/2026-08-23-m1-slice2-live-video/e2e
#   env CONSOLE_USER  console-session user for the change driver (default LABS)
#
# The dev core + dev agent + dev stack are intentionally LEFT RUNNING at
# exit so the owner can immediately open the browser page:
#   http://<LAN_IP>:18080/desktop/<node-uuid>   (login as dev admin first)
# Teardown commands are in scripts/dev-topology.md §6.
#
# Exit 0 = all gates pass; 1 = any step or gate failed.

set -euo pipefail

NODE="${1:-LABS-XIAOXIN}"
CONSOLE_USER="${CONSOLE_USER:-LABS}"
DEV_NODE_NAME="XIAOXIN-DEV"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
ROOT_W="$(cd "$ROOT" && pwd -W)"   # C:/... form: xnc put needs absolute local paths
ART="${2:-$ROOT_W/.superpowers/sdd/2026-08-23-m1-slice2-live-video/e2e}"
XNC="$ROOT/bin/xnc.exe"
E2E="$ROOT/bin/e2eviewer.exe"
DRIVER_TASK="xnc-slice2-change-driver"
OVERLAY_TASK="xnc-slice2-static-overlay"
CORE_TASK="xnc-dev-core"
AGENT_TASK="xnc-dev-agent"
REMOTE_DIR='C:\xnc-dev'
CORE_PIPE='\\.\pipe\xnc-core-dev'

say() { printf '\n=== %s ===\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
gate() { # gate <name> <0|1 pass> <detail>
  if [ "$2" -eq 1 ]; then
    printf 'GATE PASS  %-32s %s\n' "$1" "$3"
  else
    printf 'GATE FAIL  %-32s %s\n' "$1" "$3"
    GATE_FAIL=1
  fi
}
# json field helpers (stdout JSON from e2eviewer --json / REST). The { }||true
# arm matters: grep exits 1 on no match and set -o pipefail would otherwise
# kill the caller's assignment on the FIRST poll (run-3/4 silent-exit cause).
json_num()  { { grep -oE "\"$2\": *[0-9]+"  "$1" | head -1 | sed 's/.*: *//'; } || true; }
json_bool() { { grep -oE "\"$2\": *(true|false)" "$1" | head -1 | sed 's/.*: *//'; } || true; }

# Node-list parsing without python (this box has no real python — run-2
# finding: the WindowsApps stub exits 49). The list is a flat JSON array,
# so split per-object and filter on the object's own text. The dev server
# renames colliding node names to <name>-2, -3, ... — a previous run's
# stale (offline) row makes the fresh enroll land on a suffix, so match
# the base name plus an optional -N suffix.
dev_node_id() { # id of the ONLINE node named <name>(-N)?, else nothing
  { curl -sf -H "Authorization: Bearer $JWT" "$LAN/api/nodes/" 2>/dev/null \
    | grep -oE '\{[^}]*\}' | grep -E "\"name\":\"$1(-[0-9]+)?\"" | grep -F '"status":"online"' \
    | grep -oE '"id": *"[^"]*"' | head -1 | sed 's/.*: *"//;s/"$//'; } || true
}
dev_node_online_count() { # count of online nodes named <name>(-N)?
  { curl -sf -H "Authorization: Bearer $JWT" "$LAN/api/nodes/" 2>/dev/null \
    | grep -oE '\{[^}]*\}' | grep -E "\"name\":\"$1(-[0-9]+)?\"" | grep -cF '"status":"online"'; } || true
}

cleanup() {
  "$XNC" exec "$NODE" "schtasks /Delete /F /TN $DRIVER_TASK" >/dev/null 2>&1 || true
  "$XNC" exec "$NODE" "schtasks /Delete /F /TN $OVERLAY_TASK" >/dev/null 2>&1 || true
  # fix-round-1 (P2): the one-shot popup-dismiss helper and the per-run
  # firewall allow rules used to linger as XIAOXIN residue after every run.
  # The dev core/agent themselves stay running on purpose (owner browser
  # check); teardown for those lives in dev-topology.md §6.
  "$XNC" exec "$NODE" "schtasks /Delete /F /TN xnc-dismiss-popup" >/dev/null 2>&1 || true
  "$XNC" exec "$NODE" 'netsh advfirewall firewall delete rule name="xnc-dev-agent-dev" >$null; netsh advfirewall firewall delete rule name="xnc-dev-agent-dev-out" >$null; exit 0' >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

GATE_FAIL=0
mkdir -p "$ART"

# --- 0. Preflight -----------------------------------------------------------

say "Preflight"
[ -f "$ROOT/deploy/.env" ] || fail "deploy/.env missing (gitignored; see dev-topology.md §1)"
# shellcheck disable=SC1091
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
[ -f "$ROOT/bin/xnc-core.exe" ] || fail "bin/xnc-core.exe missing after build"
[ -f "$ROOT/bin/xnc-desktop.exe" ] || fail "bin/xnc-desktop.exe missing after build"

# --- 2. Dev stack rebuild (slice2 code + TURN envs) --------------------------

say "Dev stack up (docker, slice2 overlay)"
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
# The dev agent runs from C:\xnc-dev\xnc-agent.exe: a put over a running
# exe fails at rename (run-1 finding). Kill order is path-scoped — the
# PROD agent (C:\xnc\xnc-agent.exe, session 0) is never touched.

say "Stop any previous dev agent and wait for it to drop offline"
"$XNC" exec "$NODE" "schtasks /End /TN $AGENT_TASK; schtasks /Delete /F /TN $AGENT_TASK; exit 0" >/dev/null 2>&1 || true
"$XNC" exec "$NODE" 'Get-Process xnc-agent -ErrorAction SilentlyContinue | Where-Object {$_.Path -like "C:\xnc-dev*"} | Stop-Process -Force; exit 0' >/dev/null 2>&1 || true
for i in $(seq 1 45); do
  [ "$(dev_node_online_count "$DEV_NODE_NAME")" -eq 0 ] && break
  sleep 2
done
# Not fatal if it lingers offline-late: the id picker matches online only.

say "Stop any previous dev core (path-scoped)"
"$XNC" exec "$NODE" "schtasks /End /TN $CORE_TASK; schtasks /Delete /F /TN $CORE_TASK; exit 0" >/dev/null 2>&1 || true
"$XNC" exec "$NODE" 'Get-Process xnc-core -ErrorAction SilentlyContinue | Where-Object {$_.Path -like "C:\xnc-dev*"} | Stop-Process -Force; exit 0' >/dev/null 2>&1 || true

# --- 5. Deploy dev binaries + start dev core (SYSTEM) + dev agent (session 1) --

say "Deploy agent + core + desktop to $NODE ($REMOTE_DIR)"
"$XNC" exec "$NODE" \
  'New-Item -ItemType Directory -Force -Path C:\xnc-dev | Out-Null; exit 0'
"$XNC" put "$NODE" "$ROOT_W/bin/xnc-agent.exe"   "$REMOTE_DIR\\xnc-agent.exe"
"$XNC" put "$NODE" "$ROOT_W/bin/xnc-core.exe"    "$REMOTE_DIR\\xnc-core.exe"
"$XNC" put "$NODE" "$ROOT_W/bin/xnc-desktop.exe" "$REMOTE_DIR\\xnc-desktop.exe"

say "Start dev core as SYSTEM (console pipe server) on $NODE"
# 64-hex secret: generated here, lands in the .cmd on the node and in the
# agent task args; never in any log (dev-topology.md records the same
# caveat as the enroll token: schtasks /TR content is admin-visible).
CORE_SECRET_HEX=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
printf '@echo off\r\nC:\\xnc-dev\\xnc-core.exe --console --smoke-secret %s --pipe-name %s > C:\\xnc-dev\\core.log 2>&1\r\n' \
  "$CORE_SECRET_HEX" "$CORE_PIPE" > "$ART/core-dev.cmd"
"$XNC" put "$NODE" "$ART/core-dev.cmd" "$REMOTE_DIR\\core-dev.cmd"
"$XNC" exec "$NODE" "schtasks /Create /F /TN $CORE_TASK /TR 'C:\xnc-dev\core-dev.cmd' /SC ONCE /ST 23:59 /RU SYSTEM"
"$XNC" exec "$NODE" "schtasks /Run /TN $CORE_TASK"
sleep 2

say "(Re)start dev agent (fresh build, desktop kind enabled) on $NODE"
# /RL HIGHEST: the core pipe DACL is SYSTEM+Admins; the LABS dev agent
# needs the unfiltered admin token to connect (T6 finding). Console output
# is redirected: a live-scrolling agent log window on the console desktop
# is a change source that pollutes the quiet scenarios (run-7 screenshot
# finding).
printf '@echo off\r\nC:\\xnc-dev\\xnc-agent.exe run-dev-console --server %s --token %s --name %s --log-file C:\\xnc-dev\\dev-agent.log --desktop-core-pipe %s --desktop-core-secret-hex %s >> C:\\xnc-dev\\dev-agent-console.log 2>&1\r\n' \
  "$LAN" "$ETOK" "$DEV_NODE_NAME" "$CORE_PIPE" "$CORE_SECRET_HEX" > "$ART/agent-dev.cmd"
"$XNC" put "$NODE" "$ART/agent-dev.cmd" "$REMOTE_DIR\\agent-dev.cmd"
"$XNC" exec "$NODE" "schtasks /Create /F /TN $AGENT_TASK /TR 'C:\xnc-dev\agent-dev.cmd' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT /RL HIGHEST"
"$XNC" exec "$NODE" "schtasks /Run /TN $AGENT_TASK"

say "Console hygiene: firewall allow rules + dismiss any standing firewall popup"
# The fresh agent exe per run triggers a Windows Security prompt on the
# console; it floats above TopMost overlays and animates (screenshot
# evidence in the results doc). Allow rules make the choice moot; the
# ESC-dismiss (session 1 task) clears whatever is already standing.
"$XNC" exec "$NODE" 'netsh advfirewall firewall delete rule name="xnc-dev-agent-dev" >$null; netsh advfirewall firewall add rule name="xnc-dev-agent-dev" dir=in program="C:\xnc-dev\xnc-agent.exe" action=allow >$null; netsh advfirewall firewall add rule name="xnc-dev-agent-dev-out" dir=out program="C:\xnc-dev\xnc-agent.exe" action=allow >$null; exit 0' >/dev/null 2>&1 || true
"$XNC" put "$NODE" "$ROOT_W/scripts/dismiss-popup.ps1" "$REMOTE_DIR\\dismiss-popup.ps1"
"$XNC" exec "$NODE" "schtasks /Create /F /TN xnc-dismiss-popup /TR 'powershell -ExecutionPolicy Bypass -File C:\xnc-dev\dismiss-popup.ps1' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT" >/dev/null 2>&1 || true
"$XNC" exec "$NODE" "schtasks /Run /TN xnc-dismiss-popup; exit 0" >/dev/null 2>&1 || true

say "Wait for $DEV_NODE_NAME online on the dev server"
# schtasks /Run start latency is real (observed ~25s) and enrollment adds a
# beat; 3 min covers it with margin (run-3 finding: 90s was marginal).
NODE_ID=""
for i in $(seq 1 90); do
  NODE_ID=$(dev_node_id "$DEV_NODE_NAME")
  [ -n "$NODE_ID" ] && break
  sleep 2
done
if [ -z "$NODE_ID" ]; then
  "$XNC" exec "$NODE" 'Get-Content C:\xnc-dev\dev-agent.log -Tail 25; exit 0' > "$ART/agent-start-failure.log" 2>&1 || true
  fail "$DEV_NODE_NAME never came online (agent log tail captured to $ART/agent-start-failure.log)"
fi
echo "dev node: $DEV_NODE_NAME ($NODE_ID)"

# --- 6. Scenario ①: change-driven single viewer -------------------------------

say "Scenario 1: single viewer, 30s change-driven (session-1 ping console)"
"$XNC" exec "$NODE" "schtasks /Create /F /TN $DRIVER_TASK /TR 'cmd.exe /c ping -n 40 127.0.0.11' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT"
"$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
  --duration 30s --expect-first-frame-ms 5000 --expect-keyframes 2 \
  --keyframe-retry-after 2s \
  --out "$ART/s1-change.h264" --json > "$ART/s1-change.json" 2> "$ART/s1-change.log" &
S1=$!
sleep 3 # let the viewer attach on the (initially static) desktop
"$XNC" exec "$NODE" "schtasks /Run /TN $DRIVER_TASK"
S1_RC=0; wait "$S1" || S1_RC=$?
cat "$ART/s1-change.json"
S1_FF=$(json_num "$ART/s1-change.json" firstFrameMs)
S1_KF=$(json_num "$ART/s1-change.json" keyframes)
S1_FR=$(json_num "$ART/s1-change.json" frames)
gate "1-first-frame-le-5000ms" "$([ "${S1_FF:-99999}" -le 5000 ] && [ "${S1_FF:-0}" -gt 0 ] && echo 1 || echo 0)" "firstFrameMs=$S1_FF (relay, incl. TURN allocation)"
gate "1-keyframes-ge-2"        "$([ "${S1_KF:-0}" -ge 2 ] && echo 1 || echo 0)" "keyframes=$S1_KF frames=$S1_FR"
gate "1-viewer-exit-0"         "$([ "$S1_RC" -eq 0 ] && echo 1 || echo 0)" "e2eviewer exit=$S1_RC"
# Driver out of the way before the static scenarios (its scrolling console
# is a change source). Its window closing costs 1-2 AUs — within gate ②.
"$XNC" exec "$NODE" "schtasks /End /TN $DRIVER_TASK; schtasks /Delete /F /TN $DRIVER_TASK; exit 0" >/dev/null 2>&1 || true
sleep 3

# --- 7. Scenario ②: perfectly static desktop → near-zero stream ---------------
# Solid black fullscreen overlay (TickMs 0): the encoder emits NOTHING by
# design on identical input (§7.4 + the real Mf encoder skips even with
# ForceIDR armed — run-6 finding; aus=0 in core logs). The viewer still
# completes the full chain (session → ready → offer/answer → relay-only ICE
# connect) — exit 0 is the liveness; decoded AUs ≤10 is the quietness gate.
# (A tick-driven screen cannot serve this gate: each repaint costs ~2 AU
# and the attach warm-up re-feed bursts ~34 more — run-9/10 measurements.)

say "Scenario 2: perfectly static desktop (solid overlay), 45s"
"$XNC" put "$NODE" "$ROOT_W/scripts/static-overlay.ps1" "$REMOTE_DIR\\static-overlay.ps1"
"$XNC" exec "$NODE" "schtasks /Create /F /TN $OVERLAY_TASK /TR 'powershell -ExecutionPolicy Bypass -File C:\xnc-dev\static-overlay.ps1 -Seconds 55 -TickMs 0' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT"
"$XNC" exec "$NODE" "schtasks /Run /TN $OVERLAY_TASK"
sleep 4 # opening repaint settles before the viewer attaches
S2_RC=0
"$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
  --duration 45s --expect-first-frame-ms 0 --expect-keyframes 0 --expect-frames-max 10 \
  --out "$ART/s2-static.h264" --json > "$ART/s2-static.json" 2> "$ART/s2-static.log" || S2_RC=$?
cat "$ART/s2-static.json"
S2_FR=$(json_num "$ART/s2-static.json" frames)
S2_FF=$(json_num "$ART/s2-static.json" firstFrameMs)
S2_RTP=$(json_num "$ART/s2-static.json" rtpPackets)
gate "2-static-frames-le-10"  "$([ "${S2_FR:-999}" -le 10 ] && echo 1 || echo 0)" "decoded AUs=$S2_FR in 45s over a perfectly static screen"
gate "2-static-viewer-exit-0" "$([ "$S2_RC" -eq 0 ] && echo 1 || echo 0)" "e2eviewer exit=$S2_RC (session+signaling+relay ICE all completed)"
"$XNC" exec "$NODE" "schtasks /End /TN $OVERLAY_TASK; schtasks /Delete /F /TN $OVERLAY_TASK; exit 0" >/dev/null 2>&1 || true

# --- 8. Scenario ③: second viewer joins mid-quiet (on-demand IDR) --------------
# 2s-tick overlay: sparse real frames so an armed sub_join/connect IDR can
# surface (the run-6 mechanism finding bounds this at the tick interval),
# while the stream stays visibly quiet. Viewer A holds the session; B joins
# at +8s mid-quiet. Link reality: XIAOXIN WiFi (~10% UDP burst loss) — B
# carries --keyframe-retry-after (signaling-level IDR re-request).

say "Scenario 3: quiet 2s-tick desktop; second viewer joins at +8s"
"$XNC" exec "$NODE" "schtasks /Create /F /TN $OVERLAY_TASK /TR 'powershell -ExecutionPolicy Bypass -File C:\xnc-dev\static-overlay.ps1 -Seconds 40 -TickMs 2000' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT"
"$XNC" exec "$NODE" "schtasks /Run /TN $OVERLAY_TASK"
sleep 4
"$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
  --duration 24s --expect-first-frame-ms 8000 --expect-keyframes 1 --expect-frames-max 40 \
  --out "$ART/s3a-quiet.h264" --json > "$ART/s3a-quiet.json" 2> "$ART/s3a-quiet.log" &
S3A=$!
sleep 8
S3_RC=0
"$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
  --duration 15s --expect-first-frame-ms 5000 --expect-first-key --expect-keyframes 1 \
  --keyframe-retry-after 1.5s \
  --out "$ART/s3-join.h264" --json > "$ART/s3-join.json" 2> "$ART/s3-join.log" || S3_RC=$?
S3A_RC=0; wait "$S3A" || S3A_RC=$?
echo "s3a:"; cat "$ART/s3a-quiet.json"; echo "s3:"; cat "$ART/s3-join.json"
S3_FF=$(json_num "$ART/s3-join.json" firstFrameMs)
S3_FK=$(json_bool "$ART/s3-join.json" firstKey)
gate "3-join-first-IDR"        "$([ "$S3_FK" = "true" ] && echo 1 || echo 0)" "firstKey=$S3_FK"
gate "3-join-first-le-5000ms"  "$([ "${S3_FF:-99999}" -le 5000 ] && [ "${S3_FF:-0}" -gt 0 ] && echo 1 || echo 0)" "firstFrameMs=$S3_FF (joined mid-quiet; tick bound 2s + retry)"
gate "3-join-viewer-exit-0"    "$([ "$S3_RC" -eq 0 ] && echo 1 || echo 0)" "e2eviewer exit=$S3_RC (companion viewer exit=$S3A_RC)"
"$XNC" exec "$NODE" "schtasks /End /TN $OVERLAY_TASK; schtasks /Delete /F /TN $OVERLAY_TASK; exit 0" >/dev/null 2>&1 || true

# --- 9. Scenario ④: PLI over a quiet desktop → on-demand IDR ------------------

say "Scenario 4: one-shot PLI (--pli-at 8s) over a 1s-tick quiet desktop → IDR ≤2s"
# 1s tick: the armed explicit IDR must ride a real frame within the 2s PLI
# budget (same mechanism finding as ③). One-shot PLI (fix-round-1 ruling):
# the old --pli-interval 2s made every send overwrite the viewer's pending
# pliMu timestamp (e2eviewer main.go), silently forgiving a WiFi-shredded
# IDR; the gate must assert a SINGLE PLI → next decoded IDR ≤ 2000ms.
# --keyframe-retry-after still covers the join IDR while no frame has landed;
# the PLI IDR itself gets no retry — one PLI, one IDR, honest number
# (desktop-side idr_delivered stays ≤2s in core logs either way). One-shot
# mode cannot hit the overwrite (a single pliMu store), so main.go is
# untouched — the script simply abandons interval mode (ruling alternative).
"$XNC" exec "$NODE" "schtasks /Create /F /TN $OVERLAY_TASK /TR 'powershell -ExecutionPolicy Bypass -File C:\xnc-dev\static-overlay.ps1 -Seconds 40 -TickMs 1000' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT"
"$XNC" exec "$NODE" "schtasks /Run /TN $OVERLAY_TASK"
sleep 3
S4_RC=0
"$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
  --duration 20s --expect-first-frame-ms 8000 --expect-keyframes 2 \
  --pli-at 8s --keyframe-retry-after 1.5s --expect-pli-idr-ms 3000 \
  --out "$ART/s4-pli.h264" --json > "$ART/s4-pli.json" 2> "$ART/s4-pli.log" || S4_RC=$?
cat "$ART/s4-pli.json"
S4_PLI=$(json_num "$ART/s4-pli.json" pliToIdrMaxMs)
gate "4-pli-to-idr-le-2000ms"  "$([ "${S4_PLI:-0}" -gt 0 ] && [ "${S4_PLI:-99999}" -le 2000 ] && echo 1 || echo 0)" "pliToIdrMaxMs=$S4_PLI (one-shot PLI at +8s; armed force rides the 1s tick)"
gate "4-viewer-exit-0"         "$([ "$S4_RC" -eq 0 ] && echo 1 || echo 0)" "e2eviewer exit=$S4_RC"
"$XNC" exec "$NODE" "schtasks /End /TN $OVERLAY_TASK; schtasks /Delete /F /TN $OVERLAY_TASK; exit 0" >/dev/null 2>&1 || true

# --- 9. Scenario ⑤: cleanup + spawn-channel evidence --------------------------

say "Scenario 5: unsubscribe cleanup (no residual xnc-desktop.exe) + core log evidence"
sleep 6 # give the agent refcount StopCapture + child exit a beat
TASKLIST=$("$XNC" exec "$NODE" 'tasklist /FI "IMAGENAME eq xnc-desktop.exe" /FO CSV; exit 0' 2>&1 || true)
echo "$TASKLIST" > "$ART/s5-tasklist.txt"
if echo "$TASKLIST" | grep -q 'xnc-desktop.exe'; then
  gate "5-no-residual-xnc-desktop" 0 "process still alive (see s5-tasklist.txt)"
else
  gate "5-no-residual-xnc-desktop" 1 "no xnc-desktop.exe after all viewers exited"
fi
# 0x0100 spawn evidence from the core log (secret never logged by design):
"$XNC" exec "$NODE" 'Get-Content C:\xnc-dev\core.log; exit 0' > "$ART/core.log" 2>&1 || true
if grep -q 'start_capture: spawn' "$ART/core.log" && grep -q 'stop_capture: terminating' "$ART/core.log"; then
  gate "5-core-0100-spawn-evidence" 1 "$(grep -c 'start_capture' "$ART/core.log") start_capture line(s) + stop observed (stdin-secret channel closed by evidence)"
else
  gate "5-core-0100-spawn-evidence" 0 "core.log lacks spawn/stop lines"
fi
"$XNC" exec "$NODE" 'Get-Content C:\xnc-dev\dev-agent.log -Tail 80; exit 0' > "$ART/dev-agent-tail.log" 2>&1 || true

# --- 10. Summary ----------------------------------------------------------------

say "Gate summary (node=$NODE dev=$DEV_NODE_NAME)"
printf '1  change 30s   : firstFrame=%sms keyframes=%s frames=%s exit=%d\n' "${S1_FF:-?}" "${S1_KF:-?}" "${S1_FR:-?}" "$S1_RC"
printf '2  static 45s   : rtpPackets=%s decodedAUs=%s firstFrame=%sms exit=%d\n' "${S2_RTP:-?}" "${S2_FR:-?}" "${S2_FF:-?}" "$S2_RC"
printf '3  late join    : firstFrame=%sms firstKey=%s exit=%d\n' "${S3_FF:-?}" "${S3_FK:-?}" "$S3_RC"
printf '4  PLI recovery : pliToIdrMax=%sms exit=%d\n' "${S4_PLI:-?}" "$S4_RC"
printf '5  cleanup      : gates above + %s\n' "$ART/s5-tasklist.txt"
printf '\nBrowser manual check (owner one-click): %s/desktop/%s\n' "$LAN" "$NODE_ID"
printf 'Artifacts: %s\n' "$ART"

if [ "$GATE_FAIL" -eq 0 ]; then
  say "RESULT: PASS - all M1-Slice2 E2E gates green on $NODE"
else
  say "RESULT: FAIL - see gates above (measured values recorded regardless)"
fi
exit "$GATE_FAIL"
