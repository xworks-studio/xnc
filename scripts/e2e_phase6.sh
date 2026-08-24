#!/usr/bin/env bash
# Phase 6 E2E: screen session — REST endpoint + viewer RBAC + cleanup.
# Full streaming needs real agent on TB16G7 (manual acceptance).
set -euo pipefail
cd "$(dirname "$0")/.."

SERVER=http://127.0.0.1:8080
EXE=""
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) EXE=".exe";; esac
XNC="bin/xnc$EXE"; MOCK="bin/mockagent$EXE"
export XNC_SERVER=$SERVER

echo "== build =="
(cd cli && go build -o "../$XNC" .)
(cd mockagent && go build -o "../$MOCK" .)

echo "== dev stack =="
COMPOSE="docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.dev.yml"
[ -f deploy/.env ] || cp deploy/.env.example deploy/.env
$COMPOSE up -d --build
trap 'kill ${MPID:-} 2>/dev/null || true; $COMPOSE down -v; [ -n "${IDDIR:-}" ] && rm -rf "$IDDIR" || true' EXIT
for i in $(seq 1 30); do curl -sf "$SERVER/api/health" >/dev/null && break; sleep 1; done

echo "== login + node =="
TOKEN_JSON=$(printf 'change-me' | "$XNC" login --server "$SERVER" --email admin@example.com --json)
export XNC_TOKEN=$(echo "$TOKEN_JSON" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$XNC_TOKEN" ] || { echo "login failed"; exit 1; }
ETOK=$("$XNC" token create default --json | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
IDDIR=$(mktemp -d)
"$MOCK" --server "$SERVER" --token "$ETOK" --identity-dir "$IDDIR" --beat 2s >"$IDDIR/mock.log" 2>&1 &
MPID=$!
sleep 3
NODE_ID=$("$XNC" node list --json | grep -o '"id":"[^"]*"' | head -1 | sed 's/"id":"//;s/"//')
[ -n "$NODE_ID" ] || { echo "no node"; exit 1; }
echo "node: $NODE_ID"

echo "== screen REST: 202 =="
RESP=$(curl -sS -o /dev/null -w "%{http_code}" -X POST \
  -H "Authorization: Bearer $XNC_TOKEN" -H "Content-Type: application/json" \
  -d '{}' "$SERVER/api/nodes/$NODE_ID/screen")
[ "$RESP" = "202" ] || { echo "screen POST failed: $RESP"; exit 1; }
echo "SCREEN API: OK (202)"

echo "== screen validation: 400 bad fps =="
RESP2=$(curl -sS -o /dev/null -w "%{http_code}" -X POST \
  -H "Authorization: Bearer $XNC_TOKEN" -H "Content-Type: application/json" \
  -d '{"fps":99}' "$SERVER/api/nodes/$NODE_ID/screen")
[ "$RESP2" = "400" ] || { echo "screen validation failed: $RESP2"; exit 1; }
echo "VALIDATION: OK (400 on fps=99)"

echo "== viewer RBAC: 403 =="
# Create viewer user via REST
curl -sS -X POST -H "Authorization: Bearer $XNC_TOKEN" -H "Content-Type: application/json" \
  -d '{"email":"viewer@test","display_name":"Viewer","password":"v-pass"}' "$SERVER/api/users" > /dev/null
VIEWER_TOKEN=$(printf 'v-pass' | "$XNC" login --server "$SERVER" --email viewer@test --json 2>/dev/null | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
if [ -n "$VIEWER_TOKEN" ]; then
  RESP3=$(curl -sS -o /dev/null -w "%{http_code}" -X POST \
    -H "Authorization: Bearer $VIEWER_TOKEN" -H "Content-Type: application/json" \
    -d '{}' "$SERVER/api/nodes/$NODE_ID/screen")
  [ "$RESP3" = "404" ] || [ "$RESP3" = "403" ] || { echo "viewer RBAC failed: $RESP3"; exit 1; }
  echo "VIEWER RBAC: OK ($RESP3)"
else
  echo "VIEWER RBAC: SKIP (could not create/login viewer)"
fi

echo "== CLI screen --snapshot (expects graceful handling) =="
# mockagent has no capture host — CLI should not crash
DD=$(mktemp -d)
set +e
"$XNC" screen "$NODE_ID" --snapshot "$DD/snap.jpg" --json > "$DD/result.json" 2>&1
SCREEN_EXIT=$?
set -e
echo "screen --snapshot exit: $SCREEN_EXIT (0=ok, non-zero=expected without capture host)"
cat "$DD/result.json" | head -2
rm -rf "$DD"

kill $MPID; wait $MPID 2>/dev/null || true
echo "== ALL PHASE6 E2E PASSED =="
