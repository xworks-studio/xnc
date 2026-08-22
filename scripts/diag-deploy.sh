#!/usr/bin/env bash
# diag-deploy.sh - M1-Slice1 capture-spine acceptance loop on a console node.
#
# Builds both native targets (selftest included), deploys core+desktop to the
# node via xnc put, runs the two diag scenarios through xnc-core --diag-spawn
# (TokenManager session bridge), pulls h264+stats.json back, and gates both
# streams with tools/nalcheck built from the worktree.
#
# Usage: scripts/diag-deploy.sh [node] [artifacts-dir]
#   node            default LABS-XIAOXIN
#   artifacts-dir   default .superpowers/sdd/2026-08-22-m1-slice1-capture-spine
#   env CONSOLE_USER  console-session user for the change driver (default LABS)
#
# Gates (Task 7, calibrated 2026-08-22 from Task 5/6 XIAOXIN measurements;
# requested-vs-spontaneous keyframes are distinguished: stats.json counts
# what the pipeline requested, nalcheck counts IDRs in the bitstream - the
# software MFT emits spontaneous scene-cut IDRs):
#   static 60s : nalcheck frames <= 20        (near-zero output; AU come from
#                warmup refills + flush tail)
#                stats.json keyframes <= 2    (pipeline-requested)
#   change 30s : nalcheck frames >= 30        (change keeps encoding)
#                IDR ratio < 0.30             (E2-class storm threshold)
#                bitrate 0.1-10 Mbps
#   both       : nalcheck sps_pps_complete    (shaping contract spec 7.10)
#
# Change driver: xnc exec is SYSTEM@session0 and invisible on the console
# desktop (Task 6 measured captured=2 for a session-0 ping), so screen change
# is produced by an interactive scheduled task (/RU <user> /IT) running a
# visible scrolling console in session 1. Task deletion is trap-guaranteed.
#
# Exit 0 = all gates pass; 1 = any step or gate failed.

set -euo pipefail

NODE="${1:-LABS-XIAOXIN}"
CONSOLE_USER="${CONSOLE_USER:-LABS}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
ROOT_W="$(cd "$ROOT" && pwd -W)"   # C:/... form: xnc put/get need absolute local paths
ART="${2:-$ROOT_W/.superpowers/sdd/2026-08-22-m1-slice1-capture-spine}"
XNC="$ROOT/bin/xnc.exe"
NALCHECK="$ART/nalcheck-t7.exe"
TASK="xnc-diag-change-driver"

STATIC_H264="$ART/xiaoxin-static-t7.h264"
STATIC_STATS="$ART/xiaoxin-static-t7.stats.json"
STATIC_LOG="$ART/xiaoxin-static-t7.run.log"
STATIC_NAL="$ART/xiaoxin-static-t7.nalcheck.json"
CHANGE_H264="$ART/xiaoxin-change-t7.h264"
CHANGE_STATS="$ART/xiaoxin-change-t7.stats.json"
CHANGE_LOG="$ART/xiaoxin-change-t7.run.log"
CHANGE_NAL="$ART/xiaoxin-change-t7.nalcheck.json"
CHANGE_SESSION_LOG="$ART/xiaoxin-change-t7.session.log"

# Gate thresholds.
STATIC_MAX_FRAMES=20
STATIC_MAX_REQ_KEYFRAMES=2
CHANGE_MIN_FRAMES=30
CHANGE_MAX_IDR_RATIO=0.30
CHANGE_MIN_MBPS=0.1
CHANGE_MAX_MBPS=10

say() { printf '\n=== %s ===\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# json_num <file> <key>: first numeric value of a top-level JSON field.
json_num() {
  grep -oE "\"$2\": *[0-9]+([.][0-9]+)?" "$1" | head -1 | sed 's/.*: *//'
}

GATE_FAIL=0
gate() { # gate <name> <0|1 pass> <detail>
  if [ "$2" -eq 1 ]; then
    printf 'GATE PASS  %-30s %s\n' "$1" "$3"
  else
    printf 'GATE FAIL  %-30s %s\n' "$1" "$3"
    GATE_FAIL=1
  fi
}

cleanup() {
  "$XNC" exec "$NODE" "schtasks /Delete /F /TN $TASK" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

[ -x "$XNC" ] || fail "xnc CLI not found at $XNC (build the Go CLI first)"
mkdir -p "$ART"

# --- 1. Build both native targets (selftests included) ------------------

say "Build native targets (MSVC, selftests included)"
(cd "$ROOT/native/core" && cmd //c build.bat selftest)
(cd "$ROOT/native/desktop" && cmd //c build.bat selftest)
[ -f "$ROOT/bin/xnc-core.exe" ] || fail "bin/xnc-core.exe missing after build"
[ -f "$ROOT/bin/xnc-desktop.exe" ] || fail "bin/xnc-desktop.exe missing after build"

say "Build nalcheck"
(cd "$ROOT" && go build -o "$NALCHECK" ./tools/nalcheck)

# --- 2. Deploy to node ----------------------------------------------------

say "Deploy core+desktop to $NODE (remote dir C:\\xnc-diag)"
# qwinsta lists sessions fine but returns exit code 1 under SYSTEM exec -
# do not let that poison the passthrough (exit 0 is deliberate).
"$XNC" exec "$NODE" \
  'New-Item -ItemType Directory -Force -Path C:\xnc-diag | Out-Null; Remove-Item C:\xnc-diag\static.h264,C:\xnc-diag\change.h264,C:\xnc-diag\stats.json -ErrorAction SilentlyContinue; query session; exit 0'
"$XNC" put "$NODE" "$ROOT_W/bin/xnc-core.exe" 'C:\xnc-diag\xnc-core.exe'
"$XNC" put "$NODE" "$ROOT_W/bin/xnc-desktop.exe" 'C:\xnc-diag\xnc-desktop.exe'

# --- 3. Scenario 1: static desktop, 60 s ----------------------------------

say "Scenario 1/2 on $NODE: static desktop, 60s"
# xnc exec passes the remote exit code through (set -e aborts on failure);
# success is additionally cross-checked against the core's child-exit log.
"$XNC" exec "$NODE" \
  'C:\xnc-diag\xnc-core.exe --console --diag-spawn --console-diag --duration 60 --out C:\xnc-diag\static.h264' \
  2>&1 | tee "$STATIC_LOG"
grep -q 'diag_spawn child pid=[0-9]* exit=0' "$STATIC_LOG" || fail "static scenario: desktop child exit != 0"
"$XNC" get "$NODE" 'C:\xnc-diag\static.h264' "$STATIC_H264"
"$XNC" get "$NODE" 'C:\xnc-diag\stats.json' "$STATIC_STATS"
[ -s "$STATIC_H264" ] || fail "static h264 empty/missing"
[ -s "$STATIC_STATS" ] || fail "static stats.json empty/missing"

# --- 4. Scenario 2: change, 30 s + session-1 ping driver ------------------

say "Scenario 2/2 on $NODE: change 30s, interactive ping driver (session 1)"
"$XNC" exec "$NODE" \
  "schtasks /Create /F /TN $TASK /TR \"cmd.exe /c ping -n 30 127.0.0.11\" /SC ONCE /ST 23:59 /RU $CONSOLE_USER /IT"

# Capture in background; at +3s open the scrolling console on the interactive
# desktop; at +8s capture residency evidence (desktop must be in session 1).
"$XNC" exec "$NODE" \
  'C:\xnc-diag\xnc-core.exe --console --diag-spawn --console-diag --duration 30 --out C:\xnc-diag\change.h264' \
  > "$CHANGE_LOG" 2>&1 &
CAP_PID=$!
sleep 3
"$XNC" exec "$NODE" "schtasks /Run /TN $TASK"
sleep 5
"$XNC" exec "$NODE" \
  'tasklist /V /FI "IMAGENAME eq xnc-desktop.exe" /FO CSV; query session' \
  > "$CHANGE_SESSION_LOG" 2>&1 || true
wait "$CAP_PID"
grep -q 'diag_spawn child pid=[0-9]* exit=0' "$CHANGE_LOG" || fail "change scenario: desktop child exit != 0"
cat "$CHANGE_LOG"
"$XNC" exec "$NODE" "schtasks /Delete /F /TN $TASK" || true

"$XNC" get "$NODE" 'C:\xnc-diag\change.h264' "$CHANGE_H264"
"$XNC" get "$NODE" 'C:\xnc-diag\stats.json' "$CHANGE_STATS"
[ -s "$CHANGE_H264" ] || fail "change h264 empty/missing"
[ -s "$CHANGE_STATS" ] || fail "change stats.json empty/missing"

# --- 5. Gates --------------------------------------------------------------

say "nalcheck analysis"
set +e
"$NALCHECK" --duration 60 "$STATIC_H264"
STATIC_RC=$?
"$NALCHECK" --json --duration 60 "$STATIC_H264" > "$STATIC_NAL"
"$NALCHECK" --duration 30 --max-idr-ratio "$CHANGE_MAX_IDR_RATIO" "$CHANGE_H264"
CHANGE_RC=$?
"$NALCHECK" --json --duration 30 --max-idr-ratio "$CHANGE_MAX_IDR_RATIO" "$CHANGE_H264" > "$CHANGE_NAL"
set -e

S_FRAMES=$(json_num "$STATIC_NAL" frames)
S_BYTES=$(json_num "$STATIC_NAL" bytes)
S_IDR=$(json_num "$STATIC_NAL" idr_frames)
S_REQ_KF=$(json_num "$STATIC_STATS" keyframes)
S_CAP=$(json_num "$STATIC_STATS" captured)
S_AUS=$(json_num "$STATIC_STATS" aus_written)
S_TIMEOUTS=$(json_num "$STATIC_STATS" timeouts)
C_FRAMES=$(json_num "$CHANGE_NAL" frames)
C_BYTES=$(json_num "$CHANGE_NAL" bytes)
C_IDR=$(json_num "$CHANGE_NAL" idr_frames)
C_RATIO=$(json_num "$CHANGE_NAL" idr_ratio)
C_MBPS=$(json_num "$CHANGE_NAL" bitrate_mbps)
C_REQ_KF=$(json_num "$CHANGE_STATS" keyframes)
C_CAP=$(json_num "$CHANGE_STATS" captured)
C_AUS=$(json_num "$CHANGE_STATS" aus_written)

say "Gate summary (node=$NODE)"
printf 'static : stats captured=%s aus=%s keyframes(requested)=%s timeouts=%s | nalcheck frames=%s idr=%s bytes=%s\n' \
  "$S_CAP" "$S_AUS" "$S_REQ_KF" "$S_TIMEOUTS" "$S_FRAMES" "$S_IDR" "$S_BYTES"
printf 'change : stats captured=%s aus=%s keyframes(requested)=%s | nalcheck frames=%s idr=%s ratio=%s bytes=%s bitrate=%s Mbps\n' \
  "$C_CAP" "$C_AUS" "$C_REQ_KF" "$C_FRAMES" "$C_IDR" "$C_RATIO" "$C_BYTES" "$C_MBPS"

[ "${S_FRAMES:-0}" -le "$STATIC_MAX_FRAMES" ] \
  && gate static-frames-le-$STATIC_MAX_FRAMES 1 "frames=$S_FRAMES" \
  || gate static-frames-le-$STATIC_MAX_FRAMES 0 "frames=$S_FRAMES"
[ "${S_REQ_KF:-99}" -le "$STATIC_MAX_REQ_KEYFRAMES" ] \
  && gate static-requested-keyframes-le-$STATIC_MAX_REQ_KEYFRAMES 1 "keyframes=$S_REQ_KF" \
  || gate static-requested-keyframes-le-$STATIC_MAX_REQ_KEYFRAMES 0 "keyframes=$S_REQ_KF"
[ "${C_FRAMES:-0}" -ge "$CHANGE_MIN_FRAMES" ] \
  && gate change-frames-ge-$CHANGE_MIN_FRAMES 1 "frames=$C_FRAMES" \
  || gate change-frames-ge-$CHANGE_MIN_FRAMES 0 "frames=$C_FRAMES"
[ "$CHANGE_RC" -eq 0 ] \
  && gate change-idr-ratio-lt-$CHANGE_MAX_IDR_RATIO 1 "ratio=$C_RATIO (nalcheck rc=0)" \
  || gate change-idr-ratio-lt-$CHANGE_MAX_IDR_RATIO 0 "ratio=$C_RATIO (nalcheck rc=$CHANGE_RC)"
if awk -v a="${C_MBPS:-0}" -v lo="$CHANGE_MIN_MBPS" -v hi="$CHANGE_MAX_MBPS" \
  'BEGIN{exit !(a>=lo && a<=hi)}'; then
  gate change-bitrate-in-$CHANGE_MIN_MBPS-$CHANGE_MAX_MBPS-Mbps 1 "bitrate=$C_MBPS Mbps"
else
  gate change-bitrate-in-$CHANGE_MIN_MBPS-$CHANGE_MAX_MBPS-Mbps 0 "bitrate=$C_MBPS Mbps"
fi
if [ "$STATIC_RC" -eq 0 ] && [ "$CHANGE_RC" -eq 0 ]; then
  gate shaping-sps-pps-complete-both 1 "nalcheck rc=0/0"
else
  gate shaping-sps-pps-complete-both 0 "nalcheck rc=$STATIC_RC/$CHANGE_RC"
fi

say "Artifacts in $ART"
ls -la "$STATIC_H264" "$STATIC_STATS" "$CHANGE_H264" "$CHANGE_STATS" "$STATIC_NAL" "$CHANGE_NAL"

if [ "$GATE_FAIL" -eq 0 ]; then
  say "RESULT: PASS - all M1-Slice1 gates green on $NODE"
else
  say "RESULT: FAIL - see gates above (measured values recorded regardless)"
fi
exit "$GATE_FAIL"
