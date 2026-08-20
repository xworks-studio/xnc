#!/usr/bin/env bash
# Phase 4 E2E：Scenario H 文件（upload/download roundtrip + sha256 完整性 + diff）
# + Gate C 吞吐（10MB 文件经 dev 栈传输计时）。mockagent 跑在本机 Windows 并承载
# 真 file handler：写盘发生在本机，remote 路径必须 cygpath -w 成 Windows 绝对路径
# （server 的 absPathRe 只认盘符/UNC 前缀，Git Bash 的 /tmp POSIX 串两头都过不去）。
# HASH_MISMATCH 半成品删除（.xnc-part 不留）由 agent/cli 单测覆盖
# （TestFileUploadHashMismatchDeletes）；tunnel 白名单/TCP 泵/不可达 ERROR 由
# server+agent 单测覆盖——真 mstsc 隧道抽验留生产手工（Phase 4 DoD）。
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

echo "== dev stack =="
COMPOSE="docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.dev.yml"
$COMPOSE up -d --build
# EXIT 清理：杀 mock、拆 compose 栈（含卷）、删身份/数据临时目录。
# ${VAR:-} 防 set -u：trap 早于 IDDIR/MPID/DD 赋值触发时可能未定义。
trap 'kill ${MPID:-} 2>/dev/null || true; $COMPOSE down -v; [ -n "${IDDIR:-}" ] && rm -rf "$IDDIR" || true; [ -n "${DD:-}" ] && rm -rf "$DD" || true' EXIT
for i in $(seq 1 30); do curl -sf "$SERVER/api/health" >/dev/null && break; sleep 1; done
curl -sf "$SERVER/api/health" >/dev/null || { echo "server not healthy"; exit 1; }

echo "== login + node =="
TOKEN_JSON=$(printf 'change-me' | "$XNC" login --server "$SERVER" --email admin@example.com --json)
export XNC_TOKEN=$(echo "$TOKEN_JSON" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$XNC_TOKEN" ] || { echo "login failed: $TOKEN_JSON"; exit 1; }
ETOK=$("$XNC" token create default --json | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$ETOK" ] || { echo "token create failed"; exit 1; }
IDDIR=$(mktemp -d)
# mockagent logs JSON to $IDDIR/mock.log for debugging.
"$MOCK" --server "$SERVER" --token "$ETOK" --identity-dir "$IDDIR" --beat 2s >"$IDDIR/mock.log" 2>&1 &
MPID=$!
fail() { echo "FAIL: $*"; exit 1; }

# 等 enroll + 控制连接就绪（file 会话要求节点 online 才能建立），再取节点名。
NODE=""
for i in $(seq 1 15); do
  if "$XNC" node list --json | grep -q '"status":"online"'; then
    NODE=$("$XNC" node list --json | grep -o '"name":"[^"]*"' | head -1 | sed 's/"name":"//;s/"//') || NODE=""
    [ -n "$NODE" ] && break
  fi
  sleep 1
done
[ -n "$NODE" ] || fail "mock node never came online (mock log: $IDDIR/mock.log)"
echo "node: $NODE"

echo "== H: upload + download + sha256 =="
DD=$(mktemp -d)
printf 'phase4-test-content' > "$DD/src.txt"
# remote 路径 cygpath -w：见文件头注释（本机写盘 + server 绝对路径白名单）。
RDST=$(cygpath -w "$DD/dst.txt")
"$XNC" upload "$NODE" "$DD/src.txt" "$RDST" --json | grep -q '"ok":true' || fail "upload (mock log: $IDDIR/mock.log)"
"$XNC" download "$NODE" "$RDST" "$DD/back.txt" --json | grep -q '"ok":true' || fail "download (mock log: $IDDIR/mock.log)"
diff -q "$DD/src.txt" "$DD/back.txt" >/dev/null || fail "content mismatch after roundtrip"
echo "H: OK (roundtrip + diff)"

echo "== H2: hash mismatch path =="
# 注入坏数据需篡改传输流，属传输层故障注入——由单测精确覆盖：
# agent 侧 TestFileUploadHashMismatchDeletes（.xnc-part 半成品删除）+
# cli 侧本地复验（ok=false / hash 不符 → exit 246）。E2E 只验 happy path 完整性。
echo "H2: covered by unit tests (TestFileUploadHashMismatchDeletes)"

echo "== Gate C: 10MB throughput =="
head -c 10485760 /dev/zero | tr '\0' 'X' > "$DD/big.bin"
RBIG=$(cygpath -w "$DD/big-remote.bin")
T0=$(date +%s%N)
"$XNC" upload "$NODE" "$DD/big.bin" "$RBIG" --json | grep -q '"ok":true' || fail "big upload (mock log: $IDDIR/mock.log)"
T1=$(date +%s%N)
UP_MS=$(( (T1 - T0) / 1000000 ))
"$XNC" download "$NODE" "$RBIG" "$DD/big-back.bin" --json | grep -q '"ok":true' || fail "big download (mock log: $IDDIR/mock.log)"
T2=$(date +%s%N)
DOWN_MS=$(( (T2 - T1) / 1000000 ))
SIZE=$(stat -c %s "$DD/big-back.bin")
[ "$SIZE" -eq 10485760 ] || fail "big roundtrip size mismatch: $SIZE (expected 10485760)"
echo "Gate C: upload 10MB in ${UP_MS}ms, download in ${DOWN_MS}ms (docker loopback)"

kill "$MPID" 2>/dev/null || true
wait "$MPID" 2>/dev/null || true
echo "== ALL PHASE4 E2E PASSED =="
