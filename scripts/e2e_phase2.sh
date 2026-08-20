#!/usr/bin/env bash
# Phase 2 E2E：Scenario C（exec）+ H 的 run 部分 + 超时/退出码矩阵。
# mockagent 跑在本机并驱动真实 exec 引擎（PowerShell）：Start-Sleep /
# Write-Output 仅 Windows PowerShell 存在；若未来 CI 在 Linux 跑本脚本，
# 需按 uname 把 "Start-Sleep 30" 换成 "sleep 30"、t.ps1 换成 t.sh。
set -euo pipefail
cd "$(dirname "$0")/.."

# Fresh-checkout guard: compose needs deploy/.env (gitignored).
[ -f deploy/.env ] || cp deploy/.env.example deploy/.env

SERVER=http://127.0.0.1:8080
EXE=""
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) EXE=".exe";; esac
XNC="bin/xnc$EXE"; MOCK="bin/mockagent$EXE"
export XNC_SERVER=$SERVER

echo "== build =="
(cd cli && go build -o "../$XNC" .)
(cd mockagent && go build -o "../$MOCK" .)

echo "== dev stack up =="
COMPOSE="docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.dev.yml"
$COMPOSE up -d --build
trap '$COMPOSE down -v' EXIT
for i in $(seq 1 30); do curl -sf "$SERVER/api/health" >/dev/null && break; sleep 1; done
curl -sf "$SERVER/api/health" >/dev/null || { echo "server not healthy"; exit 1; }

echo "== login =="
TOKEN_JSON=$(printf 'change-me' | "$XNC" login --server "$SERVER" --email admin@example.com --json)
export XNC_TOKEN=$(echo "$TOKEN_JSON" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$XNC_TOKEN" ] || { echo "login failed: $TOKEN_JSON"; exit 1; }

echo "== node up =="
ETOK=$("$XNC" token create default --json | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$ETOK" ] || { echo "token create failed"; exit 1; }
IDDIR=$(mktemp -d)
# mockagent logs JSON to $IDDIR/mock.log for debugging.
"$MOCK" --server "$SERVER" --token "$ETOK" --identity-dir "$IDDIR" --beat 2s > "$IDDIR/mock.log" 2>&1 &
MPID=$!
fail() { echo "FAIL: $*"; kill "$MPID" 2>/dev/null || true; exit 1; }

# Wait for enroll + control connection, then take the node name from the
# single-line envelope (first "name" = the only mock node).
NODE=""
for i in $(seq 1 15); do
  if "$XNC" node list --json | grep -q '"status":"online"'; then
    NODE=$("$XNC" node list --json | grep -o '"name":"[^"]*"' | head -1 | sed 's/^"name":"//;s/"$//') || NODE=""
    [ -n "$NODE" ] && break
  fi
  sleep 1
done
[ -n "$NODE" ] || fail "mock node never came online (mock log: $IDDIR/mock.log)"
echo "node: $NODE"

echo "== C: exec hostname =="
CODE=0; OUT=$("$XNC" exec "$NODE" --json -- hostname) || CODE=$?
[ $CODE -eq 0 ] || fail "exec exit=$CODE (want 0): $OUT"
echo "$OUT" | grep -q '"exitCode":0' || fail "no exitCode 0: $OUT"
echo "$OUT" | grep -q '"stdout":"' || fail "no stdout: $OUT"
echo "C: OK"

echo "== H: run script exit code 7 =="
cat >"$IDDIR/t.ps1" <<'PSEOF'
Write-Output script-ran
exit 7
PSEOF
CODE2=0; OUT2=$("$XNC" run "$NODE" --file "$IDDIR/t.ps1" --json) || CODE2=$?
[ $CODE2 -eq 7 ] || fail "run exit=$CODE2 (want 7): $OUT2"
echo "$OUT2" | grep -q 'script-ran' || fail "no script output: $OUT2"
echo "H: OK (exit passthrough + output)"

echo "== timeout matrix: sleep 30 with timeout 2 → 243 =="
CODE3=0; OUT3=$("$XNC" exec "$NODE" --timeout 2 --json -- "Start-Sleep 30") || CODE3=$?
[ $CODE3 -eq 243 ] || fail "timeout exit=$CODE3 (want 243): $OUT3"
echo "$OUT3" | grep -q '"timedOut":true' || fail "not timedOut: $OUT3"
echo "T: OK"

kill "$MPID" 2>/dev/null || true
wait "$MPID" 2>/dev/null || true
echo "== ALL PHASE2 E2E PASSED =="
