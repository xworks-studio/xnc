#!/usr/bin/env bash
# Phase 1 E2E：A(注册上线) B(出站连接经 server) G(断线重连)。dev 栈 HeartbeatTimeout=10s。
set -euo pipefail
cd "$(dirname "$0")/.."

# Fresh-checkout guard: compose needs deploy/.env (gitignored).
[ -f deploy/.env ] || cp deploy/.env.example deploy/.env

SERVER=http://127.0.0.1:8080
EXE=""
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) EXE=".exe";; esac
XNC="bin/xnc$EXE"
MOCK="bin/mockagent$EXE"
export XNC_SERVER=$SERVER

echo "== build =="
(cd cli && go build -o "../$XNC" .)
(cd mockagent && go build -o "../$MOCK" .)

echo "== dev stack up =="
COMPOSE="docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.dev.yml"
$COMPOSE up -d --build
trap '$COMPOSE down -v' EXIT
for i in $(seq 1 30); do
  curl -sf "$SERVER/api/health" >/dev/null && break; sleep 1
done

echo "== login =="
ADMIN_EMAIL=admin@example.com ADMIN_PW=change-me
TOKEN_JSON=$(printf 'change-me' | "$XNC" login --server "$SERVER" --email "$ADMIN_EMAIL" --json)
TOKEN=$(echo "$TOKEN_JSON" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$TOKEN" ] || { echo "login failed: $TOKEN_JSON"; exit 1; }
export XNC_TOKEN=$TOKEN

echo "== A: enroll + online =="
ETOK=$("$XNC" token create default --json | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$ETOK" ] || { echo "token create failed"; exit 1; }

IDDIR=$(mktemp -d)
# mockagent logs JSON to stdout; capture to $IDDIR/mock.log for debugging (both runs append).
"$MOCK" --server "$SERVER" --token "$ETOK" --identity-dir "$IDDIR" --beat 2s > "$IDDIR/mock.log" 2>&1 &
MPID=$!
sleep 3
"$XNC" node list --json | grep -q '"status":"online"' || { echo "node not online"; kill $MPID; exit 1; }
echo "A: OK (node online via outbound connection)"

echo "== G: disconnect -> offline -> reconnect -> online =="
kill $MPID; wait $MPID 2>/dev/null || true
sleep 15   # HeartbeatTimeout=10s + 余量
"$XNC" node list --json | grep -q '"status":"offline"' || { echo "node not offline"; exit 1; }
echo "G1: OK (offline after disconnect)"

"$MOCK" --server "$SERVER" --identity-dir "$IDDIR" --beat 2s >> "$IDDIR/mock.log" 2>&1 &
MPID=$!
sleep 3
"$XNC" node list --json | grep -q '"status":"online"' || { echo "node not re-online"; kill $MPID; exit 1; }
kill $MPID; wait $MPID 2>/dev/null || true
echo "G2: OK (reconnect -> online, identity reused)"

echo "== ALL PHASE1 E2E PASSED =="
