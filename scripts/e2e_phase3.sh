#!/usr/bin/env bash
# Phase 3 E2E：shell 会话级验收（SHELL_BEGIN/echo/resize/ctrl-c/exit）+ CLI 非 TTY + 限额。
# mockagent 跑在本机 Windows 并承载真 ConPTY 引擎：shellsmoke 的每一步都
# 是真实交互终端行为，非桩。若未来 CI 在 Linux 跑本脚本，agent 侧 shell
# 会拒绝非 windows（SHELL_START_FAILED）——届时需改用 Windows runner。
set -euo pipefail
cd "$(dirname "$0")/.."

# Fresh-checkout guard: compose needs deploy/.env (gitignored).
[ -f deploy/.env ] || cp deploy/.env.example deploy/.env

SERVER=http://127.0.0.1:8080
EXE=""
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) EXE=".exe";; esac
XNC="bin/xnc$EXE"; MOCK="bin/mockagent$EXE"; SMOKE="bin/shellsmoke$EXE"
export XNC_SERVER=$SERVER

echo "== build =="
(cd cli && go build -o "../$XNC" .)
(cd mockagent && go build -o "../$MOCK" .)
(cd shellsmoke && go build -o "../$SMOKE" .)

echo "== dev stack =="
COMPOSE="docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.dev.yml"
$COMPOSE up -d --build
# EXIT 清理三件事：杀 mock（若已启动）、拆 compose 栈、删身份临时目录。
# ${VAR:-} 防 set -u：trap 早于 IDDIR/MPID 赋值触发时两者可能未定义。
trap 'kill ${MPID:-} 2>/dev/null || true; $COMPOSE down -v; [ -n "${IDDIR:-}" ] && rm -rf "$IDDIR" || true' EXIT
for i in $(seq 1 30); do curl -sf "$SERVER/api/health" >/dev/null && break; sleep 1; done
curl -sf "$SERVER/api/health" >/dev/null || { echo "server not healthy"; exit 1; }

echo "== login + node up =="
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

# 等 enroll + 控制连接就绪，再从单行 envelope 取节点名（首个 name 即唯一 mock 节点）。
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

echo "== shellsmoke =="
"$SMOKE" --node "$NODE" || fail "shellsmoke (mock log: $IDDIR/mock.log)"
echo "SMOKE: OK"

echo "== CLI 非 TTY 拒绝 =="
set +e
# 不带 --json：shell 命令未注册该 flag（不同于 exec/run），带上会被 cobra 以
# unknown flag 拒绝（恰好也是 exit 2），TTY 检查根本没走到——步骤只会"碰巧绿"。
OUT=$("$XNC" shell "$NODE" </dev/null 2>&1); CODE=$?
set -e
[ $CODE -eq 2 ] || { echo "shell non-tty exit=$CODE: $OUT"; exit 1; }
# exit 2 必须来自 TTY 检查路径本身，而非任何其他 usage 错误。
echo "$OUT" | grep -q "interactive terminal" || { echo "shell non-tty missing tty hint: $OUT"; exit 1; }
echo "NON-TTY: OK (exit 2, tty check hit)"

echo "== 限额说明 =="
# 环境级限额验证（409 SESSION_LIMIT_EXCEEDED / CLI 246）由 server 单测覆盖
# （TestShellPerNodeLimit）——默认栈限额 10，会话级冒烟不会触达，触发需定制
# 栈属负面路径测试；生产观察随 Phase 5 配额 pass 汇总。脚本不留死代码。

kill "$MPID" 2>/dev/null || true
wait "$MPID" 2>/dev/null || true
echo "== ALL PHASE3 E2E PASSED =="
