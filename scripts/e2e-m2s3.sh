#!/usr/bin/env bash
# e2e-m2s3.sh - M2-Slice3 XIAOXIN E2E acceptance gate (Task 6): service
# topology logoff/logon self-heal chain + dual-viewer lease transfer +
# rt-active snapshot probe.
#
# Topology (scripts/dev-services.md, AUTHORITATIVE):
#   LABS-DEV (docker): dev xnc-server + coturn, LAN http://$LAN_IP:18080
#   LABS-XIAOXIN: XNCAgentDev + XNCCoreDev SERVICES (LocalSystem, Automatic,
#                 C:\xnc-dev\ binaries) - the SUT. Prod XNCAgent untouched
#                 and used ONLY as the exec/put driver (session 0 SYSTEM).
#   this machine: xnc CLI (dev profile) + e2eviewer (server mode).
#
# Gates (numbers honest; failures recorded with conclusions):
#   3 rt-snapshot  : viewer streaming -> concurrent `xnc screen --snap` ->
#                    JPEG ok OR clean SNAPSHOT_FAILED (DXGI single-dup
#                    limit) - whichever occurs is recorded honestly.
#   2 lease xfer   : viewer1 holder (lease granted) + viewer2 lease ->
#                    denied{held}; viewer1 dies -> viewer3 NEW session ->
#                    lease granted + input works (move probe).
#   1 logoff chain : viewer (long run, input script) attached -> `logoff
#                    <console-session>` via exec SYSTEM -> node stays
#                    online >=120s, services stay Running, viewer sees
#                    recovering/reattached (self-heal into the logon-UI
#                    session), input script clicks the user tile + Enter
#                    (empty password) -> desktop restored; input
#                    regression sample afterwards.
#   4+ evidence of 5 (stale display fallback) = native selftest sel-* cases
#     (ran at build; recorded here from the run log).
#
# V1 DoD (spec §53) checklist is NOT in this script - it is hand-curated
# into the results doc from all slices' gate evidence.
#
# Usage: scripts/e2e-m2s3.sh [prod-node] [artifacts-dir]
#   prod-node       default LABS-XIAOXIN (exec/put driver)
#   artifacts-dir   default
#     .superpowers/sdd/2026-08-23-m2-slice3-services/e2e
#   env CONSOLE_USER  console-session user (default LABS; blank password
#                     logon injection is the tested lever, M2-Slice1)
#
# Safety: scoped kills only (dev-path processes); NO service reinstalls;
# prod XNCAgent / C:\xnc / C:\ProgramData\XNCAgent untouched (verify block).
# After the logoff gate the dev services are verified Running + node online.
#
# Exit 0 = all gates pass; 1 = any gate failed (numbers recorded regardless).

set -euo pipefail

NODE="${1:-LABS-XIAOXIN}"
CONSOLE_USER="${CONSOLE_USER:-LABS}"
# dev agent service enrolled with hostname default: node name = LABS-XIAOXIN
DEV_NODE_NAME="${DEV_NODE_NAME:-LABS-XIAOXIN}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
ROOT_W="$(cd "$ROOT" && pwd -W)"
ART="${2:-$ROOT_W/.superpowers/sdd/2026-08-23-m2-slice3-services/e2e}"
XNC="$ROOT/bin/xnc.exe"
E2E="$ROOT/bin/e2eviewer.exe"
REMOTE_DIR='C:\xnc-dev'

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
nodejs() { node -e "$1" "${@:2}"; }

GATE_FAIL=0
mkdir -p "$ART"

cleanup() {
  # scoped: kill any stray LOCAL viewer processes; remote dev services are
  # deliberately left Running (they are the SUT and must survive logoff).
  taskkill //F //IM e2eviewer.exe >/dev/null 2>&1 || true
  stop_motion || true
}
trap cleanup EXIT INT TERM

# Session-1 motion driver (m1-slice2 ping-driver precedent): a static
# desktop never emits a post-join IDR (one-shot base-frame rewind needs a
# content change), so every viewer-attach gate needs on-screen change.
MOTION_TASK="xnc-m2s3-motion"
start_motion() {
  printf '%s' '@echo off
:loop
echo m2s3-motion %RANDOM%
ping -n 2 127.0.0.1 >nul
goto loop
' | sed 's/$/\r/' > "$ART/motion.cmd"
  "$XNC" put "$NODE" "$ART/motion.cmd" "$REMOTE_DIR\\motion.cmd" >/dev/null
  "$XNC" exec "$NODE" "schtasks /Create /F /TN $MOTION_TASK /TR 'cmd /c C:\\xnc-dev\\motion.cmd' /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT; schtasks /Run /TN $MOTION_TASK; exit 0" >/dev/null 2>&1 || true
}
stop_motion() {
  "$XNC" exec "$NODE" "schtasks /End /TN $MOTION_TASK; schtasks /Delete /F /TN $MOTION_TASK; exit 0" >/dev/null 2>&1 || true
}

# --- 0. Preflight -----------------------------------------------------------

say "Preflight"
[ -f "$ROOT/deploy/.env" ] || fail "deploy/.env missing (see dev-topology.md §1)"
set -a; . "$ROOT/deploy/.env"; set +a
LAN_IP="${XNC_DEV_EXTERNAL_IP:-192.168.1.12}"
LAN="http://$LAN_IP:18080"
echo "dev LAN URL: $LAN  (driver=$NODE dev-node=$DEV_NODE_NAME)"
curl -sf "$LAN/api/health" >/dev/null || fail "dev server not healthy at $LAN"

# --- 1. Build CLI + viewer (natives built + selftested in task flow) --------

say "Build: cli + e2eviewer"
(cd "$ROOT/cli" && go build -o "../bin/xnc.exe" .)
go build -C "$ROOT" -o "$E2E" ./tools/e2eviewer
[ -x "$E2E" ] || fail "e2eviewer build"

say "Dev admin login"
JWT=$(curl -sf -X POST "$LAN/api/auth/login" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$XNC_ADMIN_EMAIL\",\"password\":\"$XNC_ADMIN_PASSWORD\"}" \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$JWT" ] || fail "admin login failed"

dev_node_id() {
  curl -sf -H "Authorization: Bearer $JWT" "$LAN/api/nodes/" 2>/dev/null \
    | tr '{' '\n' | grep -F "\"name\":\"$1\"" | grep -F '"status":"online"' \
    | grep -oE '"id":"[^"]*"' | head -1 | sed 's/.*:"//;s/"$//' || true
}
dev_node_online() { # 1 if the NEWEST online node of that name exists
  [ -n "$(dev_node_id "$1")" ] && echo 1 || echo 0
}

# --- 2. Deploy fresh xnc-desktop.exe (Task 6 fix 5) into dev services -------

say "Deploy new xnc-desktop.exe to $REMOTE_DIR (services stay installed)"
# Stop XNCCoreDev for the swap: core crash-supervision otherwise respawns
# xnc-desktop within ~1s of the kill and the exe stays locked (run-3
# lesson: put upload ends FILE_NOT_FOUND while the image is in use).
"$XNC" exec "$NODE" 'Stop-Service XNCCoreDev; Start-Sleep 2; Get-Process xnc-desktop -ErrorAction SilentlyContinue | Where-Object {$_.Path -like "C:\xnc-dev*"} | Stop-Process -Force; exit 0' >/dev/null 2>&1 || true
sleep 3
"$XNC" put "$NODE" "$ROOT_W/bin/xnc-desktop.exe" "$REMOTE_DIR\\xnc-desktop.exe"
"$XNC" exec "$NODE" 'Start-Service XNCCoreDev; Start-Sleep 3; exit 0' >/dev/null 2>&1 || true
svc_check() { # 3 attempts (run-4 lesson: one exec stream hung for 17min)
  for a in 1 2 3; do
    if timeout 60 "$XNC" exec --timeout 45 "$NODE" 'Get-Service XNCAgentDev,XNCCoreDev | Select Name,Status | Format-Table -HideTableHeaders; exit 0' 2>/dev/null | tr -d '\r' | grep -E "XNC" | tr -s ' ' | tee "$1"; then
      [ -s "$1" ] && return 0
    fi
    sleep 3
  done
  return 1
}
svc_check "$ART/services-pre.txt"
grep -q "XNCAgentDev Running" "$ART/services-pre.txt" || fail "XNCAgentDev not Running"
grep -q "XNCCoreDev Running" "$ART/services-pre.txt" || fail "XNCCoreDev not Running"

say "Wait for $DEV_NODE_NAME online"
NODE_ID=""
for i in $(seq 1 60); do
  NODE_ID=$(dev_node_id "$DEV_NODE_NAME")
  [ -n "$NODE_ID" ] && break
  sleep 2
done
[ -n "$NODE_ID" ] || fail "$DEV_NODE_NAME not online"
echo "dev node: $DEV_NODE_NAME ($NODE_ID)"

viewer() { # viewer <out.json> <out.log> <duration> <extra args...>
  local outj="$1" outl="$2" dur="$3"; shift 3
  "$E2E" --server "$LAN" --node "$NODE_ID" --token "$JWT" \
    --duration "$dur" --expect-first-frame-ms 8000 --keyframe-retry-after 1.5s \
    --json "$@" > "$outj" 2> "$outl" || true
}
jf() { # jf <js-expr over j> <viewer-json> [default=0]
  node -e "try{const j=require(process.argv[1]);console.log($1)}catch(e){console.log(process.argv[2]||0)}" "$2" "${3:-0}" 2>/dev/null || echo "${3:-0}"
}

agent_log() { timeout 90 "$XNC" exec --timeout 60 "$NODE" 'Get-Content C:\ProgramData\XNCAgentDev\agent-service.log -Tail 200; exit 0' 2>/dev/null || true; }

# --- GATE 3: rt-active snapshot probe ---------------------------------------

say "Start session-1 motion driver (static desktop emits no IDR)"
start_motion
sleep 3

say "Gate 3: viewer streaming -> concurrent screen --snapshot"
rm -f "$ART/g3-viewer.json" "$ART/g3-snap.jpg"
viewer "$ART/g3-viewer.json" "$ART/g3-viewer.log" 40s &
G3_VPID=$!
sleep 12   # stream established (rt duplication active)
G3_CLI_OUT=$("$ROOT/bin/xnc.exe" --server "$LAN" --token "$JWT" screen "$NODE_ID" --snapshot "$ART/g3-snap.jpg" 2>&1 || true)
echo "$G3_CLI_OUT" > "$ART/g3-cli.txt"
wait $G3_VPID || true
G3_JFIF=0
if [ -s "$ART/g3-snap.jpg" ] && head -c 2 "$ART/g3-snap.jpg" | od -An -tx1 | grep -q "ff d8"; then G3_JFIF=1; fi
G3_CLEANFAIL=0
printf '%s' "$G3_CLI_OUT" | grep -qiE "snapshot_failed|SNAPSHOT_FAILED" && G3_CLEANFAIL=1
G3_FRAMES=$(jf "j.frames" "$ART/g3-viewer.json")
if [ "$G3_JFIF" = "1" ]; then
  gate "3-rt-active-snapshot" 1 "concurrent JPEG snapshot OK while rt streaming (frames=$G3_FRAMES); cli out in g3-cli.txt"
elif [ "$G3_CLEANFAIL" = "1" ]; then
  gate "3-rt-active-snapshot" 1 "clean SNAPSHOT_FAILED while rt duplication held (DXGI single-duplication limit; frames=$G3_FRAMES) - honest record"
else
  gate "3-rt-active-snapshot" 0 "neither JPEG nor clean SNAPSHOT_FAILED (out: $(printf '%s' "$G3_CLI_OUT" | head -c 200); frames=$G3_FRAMES)"
fi

# --- GATE 2: dual-viewer lease transfer --------------------------------------

say "Gate 2: viewer1 holder -> viewer2 denied{held} -> viewer1 dies -> viewer3 granted + input"
cat > "$ART/g2-holder.json.steps" <<'EOF'
[ {"op":"lease"},
  {"op":"wait","ms":55000} ]
EOF
cat > "$ART/g2-challenger.json.steps" <<'EOF'
[ {"op":"lease"} ]
EOF
cat > "$ART/g2-takeover.json.steps" <<'EOF'
[ {"op":"lease"},
  {"op":"move","x":400,"y":300},
  {"op":"move","x":600,"y":400},
  {"op":"wait","ms":1500} ]
EOF

rm -f "$ART/g2-holder.json" "$ART/g2-challenger.json" "$ART/g2-takeover.json"
viewer "$ART/g2-holder.json" "$ART/g2-holder.log" 70s --input-script "$ART/g2-holder.json.steps" &
G2_HPID=$!
sleep 15   # holder connected + lease granted
viewer "$ART/g2-challenger.json" "$ART/g2-challenger.log" 35s --input-before-keyframe --input-script "$ART/g2-challenger.json.steps" &
G2_CPID=$!
wait $G2_CPID || true
G2C_GRANTED=$(jf "(j.input&&j.input.leaseGranted)?1:0" "$ART/g2-challenger.json")
G2C_DENIED=$(jf "((j.input&&j.input.steps||[])[0]||{}).err||''" "$ART/g2-challenger.json" "x" | grep -qi "denied" && echo 1 || echo 0)
G2C_HELD=$(jf "((j.input&&j.input.steps||[])[0]||{}).err||''" "$ART/g2-challenger.json" "x" | grep -qi "held" && echo 1 || echo 0)
gate "2-challenger-denied-held" "$([ "$G2C_GRANTED" = "0" ] && [ "$G2C_HELD" = "1" ] && echo 1 || echo 0)" "viewer2 lease: granted=$G2C_GRANTED err='$(jf "((j.input&&j.input.steps||[])[0]||{}).err||''" "$ART/g2-challenger.json" "")'"

echo "[$(date +%T)] wait for holder viewer to end (disconnect at duration end)"
wait $G2_HPID || true
G2H_GRANT=$(jf "(j.input&&j.input.leaseGranted)?1:0" "$ART/g2-holder.json")
gate "2-holder-granted" "$G2H_GRANT" "viewer1 lease granted=$G2H_GRANT (leaseId $(jf "(j.input&&j.input.leaseId||'').slice(0,8)" "$ART/g2-holder.json"))"
sleep 10   # holder session close frees the node lease server-side
viewer "$ART/g2-takeover.json" "$ART/g2-takeover.log" 40s --input-script "$ART/g2-takeover.json.steps"
G2T_GRANTED=$(jf "(j.input&&j.input.leaseGranted)?1:0" "$ART/g2-takeover.json")
G2T_STEPSOK=$(jf "j.input&&j.input.ok?1:0" "$ART/g2-takeover.json")
G2T_MOVES=$(jf "(j.input&&j.input.steps||[]).filter(s=>s.op==='move'&&s.ok).length" "$ART/g2-takeover.json")
gate "2-transfer-granted" "$G2T_GRANTED" "viewer3 NEW session lease granted=$G2T_GRANTED (leaseId $(jf "(j.input&&j.input.leaseId||'').slice(0,8)" "$ART/g2-takeover.json"))"
gate "2-transfer-input-works" "$([ "$G2T_STEPSOK" = "1" ] && [ "${G2T_MOVES:-0}" -ge 2 ] && echo 1 || echo 0)" "post-transfer input: steps ok=$G2T_STEPSOK moves=$G2T_MOVES"
# server-side evidence: capture docker logs slice of the window
docker logs --since 10m xnc-server-dev > "$ART/g2-server.log" 2>&1 || docker logs --since 10m deploy-xnc-server-dev-1 > "$ART/g2-server.log" 2>&1 || true

# --- GATE 1: logoff/logon full chain -----------------------------------------

say "Gate 1: logoff/logon self-heal chain (LONG gate, ~7 min)"
# Resolve the ACTIVE console session id (qwinsta 'console ... <id> Active').
# A prior logoff/logon cycle renumbers the console session (run-8 lesson:
# stale parse fell back to 1 which no longer existed -> logoff no-op).
CSID="${CONSOLE_SESSION:-}"
if [ -z "$CSID" ]; then
  CSID=$("$XNC" exec "$NODE" '$s = (qwinsta | Out-String); if ($s -match "console\s+(\S+\s+)?(\d+)\s+Active") { $Matches[2] }; exit 0' 2>/dev/null | tr -d '\r\n ' | grep -E '^[0-9]+$' | head -1 || true)
fi
CSID="${CSID:-1}"
echo "console session to log off: $CSID (sessions follow:)"
"$XNC" exec "$NODE" 'qwinsta; exit 0' 2>/dev/null | tr -d '\r' | tee "$ART/g1-qwinsta.txt" | head -6
grep -qE "console.*$CSID.*Active" "$ART/g1-qwinsta.txt" || fail "console session $CSID not Active - refusing to logoff blind"

# Input script rides the SELF-HEALED session (the tested capability):
# lease -> wait out the logoff+recovery window -> click the logon-UI user
# tile (screen center) -> Enter (empty password) -> wait -> probe moves.
cat > "$ART/g1-script.json" <<'EOF'
[ {"op":"lease"},
  {"op":"wait","ms":40000},
  {"op":"wait","ms":35000},
  {"op":"move","x":1440,"y":860},
  {"op":"button","btn":1,"down":true},
  {"op":"button","btn":1,"down":false},
  {"op":"wait","ms":2500},
  {"op":"key","code":"Enter","down":true},
  {"op":"key","code":"Enter","down":false},
  {"op":"wait","ms":12000},
  {"op":"move","x":400,"y":300},
  {"op":"move","x":700,"y":500},
  {"op":"wait","ms":1500} ]
EOF

rm -f "$ART/g1-viewer.json" "$ART/g1-online.txt"
viewer "$ART/g1-viewer.json" "$ART/g1-viewer.log" 300s --input-script "$ART/g1-script.json" &
G1_VPID=$!
sleep 25   # first keyframe + steady stream + lease granted
: > "$ART/g1-online.txt"
LO_TIME=$(date +%s)
echo "[$(date +%T)] LOGOFF session $CSID (exec SYSTEM)"
timeout 60 "$XNC" exec --timeout 45 "$NODE" "logoff $CSID; exit 0" >/dev/null 2>&1 || true
# node online probes at +20s/+60s/+120s/+180s (post-logoff)
for d in 20 40 60 60 60; do
  sleep $d
  echo "$(date +%T) +$(( $(date +%s) - LO_TIME ))s online=$(dev_node_online "$DEV_NODE_NAME")" >> "$ART/g1-online.txt"
done
wait $G1_VPID || true

G1_ONLINE120=$(tail -3 "$ART/g1-online.txt" | grep -c "online=1" || true)
G1_SVC=$(svc_check "$ART/g1-services.txt" >/dev/null 2>&1; grep -c "Running" "$ART/g1-services.txt" 2>/dev/null || echo 0)
G1_STATES=$(jf "JSON.stringify((j.stateSamples||[]).map(s=>s.code))" "$ART/g1-viewer.json" "[]")
G1_RECOVER=$(printf '%s' "$G1_STATES" | grep -qiE "recovering|reattached" && echo 1 || echo 0)
G1_REATTACH=$(printf '%s' "$G1_STATES" | grep -qi "reattached" && echo 1 || echo 0)
G1_FRAMES=$(jf "j.frames" "$ART/g1-viewer.json")
G1_KEYFRAMES=$(jf "j.keyframes" "$ART/g1-viewer.json")
G1_INPUT_OK=$(jf "j.input&&j.input.ok?1:0" "$ART/g1-viewer.json")
G1_ENTER_OK=$(jf "(j.input&&j.input.steps||[]).filter(s=>s.op==='key'&&s.ok).length" "$ART/g1-viewer.json")
agent_log > "$ART/g1-agent.log"

gate "1-node-online-120s" "$([ "${G1_ONLINE120:-0}" -ge 2 ] && echo 1 || echo 0)" "online probes post-logoff: $(tr '\n' ' ' < "$ART/g1-online.txt")"
gate "1-services-running" "$([ "${G1_SVC:-0}" -ge 2 ] && echo 1 || echo 0)" "XNCAgentDev+XNCCoreDev Running after logoff (count=$G1_SVC)"
gate "1-viewer-selfheal-states" "$G1_RECOVER" "viewer states=$G1_STATES frames=$G1_FRAMES keys=$G1_KEYFRAMES"
gate "1-logon-ui-reaminated" "$([ "$G1_REATTACH" = "1" ] && [ "${G1_FRAMES:-0}" -gt 10 ] && echo 1 || echo 0)" "reattached=$G1_REATTACH frames=$G1_FRAMES (frames resumed on the logon-UI session => UI visible via self-heal)"
gate "1-login-injected" "$([ "${G1_ENTER_OK:-0}" -ge 2 ] && echo 1 || echo 0)" "tile click + Enter steps ok=$G1_ENTER_OK (input script full ok=$G1_INPUT_OK)"

# 1b: input regression sample on the RESTORED desktop (fresh viewer).
# The logoff killed the session-1 motion driver; re-arm it (logon restored
# the interactive session, /IT tasks can run again) so the fresh viewer's
# keyframe wait has content change.
say "Gate 1b: input regression sample on restored desktop"
start_motion
sleep 3
cat > "$ART/g1b-script.json" <<'EOF'
[ {"op":"lease"},
  {"op":"move","x":500,"y":400},
  {"op":"move","x":900,"y":600},
  {"op":"wait","ms":1000} ]
EOF
rm -f "$ART/g1b-viewer.json"
viewer "$ART/g1b-viewer.json" "$ART/g1b-viewer.log" 40s --input-script "$ART/g1b-script.json"
G1B_GRANT=$(jf "(j.input&&j.input.leaseGranted)?1:0" "$ART/g1b-viewer.json")
G1B_OK=$(jf "j.input&&j.input.ok?1:0" "$ART/g1b-viewer.json")
G1B_FRAMES=$(jf "j.frames" "$ART/g1b-viewer.json")
gate "1-desktop-restored-input" "$([ "$G1B_GRANT" = "1" ] && [ "$G1B_OK" = "1" ] && [ "${G1B_FRAMES:-0}" -gt 3 ] && echo 1 || echo 0)" "restored desktop: lease=$G1B_GRANT steps-ok=$G1B_OK frames=$G1B_FRAMES"

# --- Verify / isolate ---------------------------------------------------------

stop_motion
say "Verify: prod untouched + dev services healthy"
timeout 60 "$XNC" exec --timeout 45 "$NODE" 'Get-Service XNCAgent | Select Name,Status | Format-Table -HideTableHeaders; Get-Item C:\ProgramData\XNCAgent\identity.json | Select LastWriteTime; exit 0' > "$ART/isolate.txt" 2>&1 || true
grep -q "XNCAgent Running" "$ART/isolate.txt" && echo "prod XNCAgent Running (untouched)"
[ "$(dev_node_online "$DEV_NODE_NAME")" = "1" ] && echo "dev node online at gate end" || echo "WARN: dev node offline at gate end"

say "Summary"
if [ "$GATE_FAIL" = "1" ]; then
  echo "RESULT: FAIL (see gates above; artifacts in $ART)"
  exit 1
fi
echo "RESULT: PASS (artifacts in $ART)"
