#!/usr/bin/env bash
# Phase 5 E2E：Scenario F RBAC 矩阵——首个全栈多用户运行。
# admin 经 REST 创建 operator+viewer（用户管理无 CLI，Phase 7 Web UI 消费），
# `xnc cluster member add` 按 role 入组，mockagent 上线后跑完整矩阵：
#   viewer  对 exec/shell/upload 全 403（CLI 241；shell 走 REST——CLI shell
#           需真 TTY，非 TTY exit 2 在 Phase 3 已验，与 RBAC 无关）
#   operator 对 exec/upload 正常（202 会话 → mockagent 真执行到完成）
#   owner disable → operator exec 403 NODE_DISABLED（CLI 250：exit 表无专用
#           码，NODE_DISABLED 落 default 桶）→ enable 恢复
# 最后 audit list（--since/--action 过滤 + admin-only）与 member list 收口。
# tunnel/RDP 的角色矩阵由 server 单测（rbac_test.go）覆盖——真 mstsc 隧道
# 抽验留生产手工（同 Phase 4 Gate C 之外的处理）。
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

echo "== login admin + create users (REST) =="
TOKEN_JSON=$(printf 'change-me' | "$XNC" login --server "$SERVER" --email admin@example.com --json)
export XNC_TOKEN=$(echo "$TOKEN_JSON" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$XNC_TOKEN" ] || { echo "login failed: $TOKEN_JSON"; exit 1; }
fail() { echo "FAIL: $*"; exit 1; }

# 用户创建仅 REST（admin-only；无 CLI user 命令），输出新用户 id。
mkuser() { # email display_name password → stdout: user id
  curl -sf -X POST -H "Authorization: Bearer $XNC_TOKEN" -H "Content-Type: application/json" \
    -d "{\"email\":\"$1\",\"display_name\":\"$2\",\"password\":\"$3\"}" "$SERVER/api/users" |
    sed -n 's/.*"id":"\([^"]*\)".*/\1/p'
}
OPID=$(mkuser op@test Op op-pass)
[ -n "$OPID" ] || fail "create operator user"
VWID=$(mkuser viewer@test Viewer viewer-pass)
[ -n "$VWID" ] || fail "create viewer user"
echo "users: operator=$OPID viewer=$VWID"

# admin 创建的账号必须能登录（DoD）：CLI 非 TTY 登录，token 从 envelope 取。
# login 会写 ~/.xnc/config.json，但后续命令全部走 XNC_TOKEN env（env > file），
# admin 身份不受 op/viewer 登录落盘影响。
OPTOK=$(printf 'op-pass' | "$XNC" login --server "$SERVER" --email op@test --json |
  sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
VWTOK=$(printf 'viewer-pass' | "$XNC" login --server "$SERVER" --email viewer@test --json |
  sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$OPTOK" ] && [ -n "$VWTOK" ] || fail "operator/viewer login (created users must authenticate)"

echo "== members: add operator+viewer to default cluster =="
"$XNC" cluster member add default "$OPID" --role operator --json | grep -q '"ok":true' ||
  fail "member add operator"
"$XNC" cluster member add default "$VWID" --role viewer --json | grep -q '"ok":true' ||
  fail "member add viewer"

echo "== mockagent online =="
ETOK=$("$XNC" token create default --json | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$ETOK" ] || fail "token create"
IDDIR=$(mktemp -d)
# mockagent logs JSON to $IDDIR/mock.log for debugging.
"$MOCK" --server "$SERVER" --token "$ETOK" --identity-dir "$IDDIR" --beat 2s >"$IDDIR/mock.log" 2>&1 &
MPID=$!

# 等 enroll + 控制连接就绪；同时取节点名与 UUID（shell 断言走 REST 需要后者）。
NODE=""; NID=""
for i in $(seq 1 15); do
  if "$XNC" node list --json | grep -q '"status":"online"'; then
    NODE=$("$XNC" node list --json | grep -o '"name":"[^"]*"' | head -1 | sed 's/"name":"//;s/"//') || NODE=""
    NID=$("$XNC" node list --json | grep -o '"id":"[^"]*"' | head -1 | sed 's/"id":"//;s/"//') || NID=""
    [ -n "$NODE" ] && [ -n "$NID" ] && break
  fi
  sleep 1
done
[ -n "$NODE" ] || fail "mock node never came online (mock log: $IDDIR/mock.log)"
echo "node: $NODE ($NID)"

echo "== F deny: viewer → 403 everywhere =="
# a) exec：CLI 全链路（node 名解析需 membership——viewer 是成员，解析过、exec 被拒）。
CODE=0; OUT=$(XNC_TOKEN=$VWTOK "$XNC" exec "$NODE" --json -- hostname) || CODE=$?
[ $CODE -eq 241 ] || fail "viewer exec exit=$CODE (want 241): $OUT"
echo "$OUT" | grep -q '"code":"FORBIDDEN"' || fail "viewer exec not FORBIDDEN: $OUT"
# b) shell：REST 直断 403（CLI shell 在无 TTY 下 exit 2，到不了 POST）。
HTTP_CODE=$(curl -s -o "$IDDIR/shell403.json" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $VWTOK" -H "Content-Type: application/json" -d '{}' \
  "$SERVER/api/nodes/$NID/shell")
[ "$HTTP_CODE" = "403" ] || fail "viewer shell POST http=$HTTP_CODE: $(cat "$IDDIR/shell403.json")"
grep -q '"code":"FORBIDDEN"' "$IDDIR/shell403.json" || fail "viewer shell not FORBIDDEN"
# c) upload：CLI 全链路（body 校验先于 RBAC，remote 须 Windows 绝对路径）。
DD=$(mktemp -d)
printf 'phase5-rbac' > "$DD/src.txt"
RDST=$(cygpath -w "$DD/dst.txt")
CODE=0; OUT=$(XNC_TOKEN=$VWTOK "$XNC" upload "$NODE" "$DD/src.txt" "$RDST" --json) || CODE=$?
[ $CODE -eq 241 ] || fail "viewer upload exit=$CODE (want 241): $OUT"
echo "$OUT" | grep -q '"code":"FORBIDDEN"' || fail "viewer upload not FORBIDDEN: $OUT"
echo "F deny: OK (exec/upload CLI exit 241, shell POST 403)"

echo "== F allow: operator sessions =="
# d) exec 202 → mockagent 真执行到 EXEC_RESULT（比仅断言 202 更强）。
CODE=0; OUT=$(XNC_TOKEN=$OPTOK "$XNC" exec "$NODE" --json -- hostname) || CODE=$?
[ $CODE -eq 0 ] || fail "operator exec exit=$CODE (want 0): $OUT"
echo "$OUT" | grep -q '"exitCode":0' || fail "operator exec no exitCode 0: $OUT"
echo "$OUT" | grep -q '"ok":true' || fail "operator exec envelope not ok: $OUT"
# upload 正向对照（viewer 同一文件同一路径只被 role 拒）。
CODE=0; OUT=$(XNC_TOKEN=$OPTOK "$XNC" upload "$NODE" "$DD/src.txt" "$RDST" --json) || CODE=$?
[ $CODE -eq 0 ] || fail "operator upload exit=$CODE (mock log: $IDDIR/mock.log): $OUT"
echo "F allow: OK (exec 202→完成, upload ok)"

echo "== F: node disable → NODE_DISABLED → enable 恢复 =="
# e) owner（admin）disable → operator exec 403 NODE_DISABLED（CLI 250，default 桶）。
"$XNC" node disable "$NODE" --json | grep -q '"ok":true' || fail "node disable"
"$XNC" node list --json | grep -q '"status":"disabled"' || fail "node list does not show disabled"
CODE=0; OUT=$(XNC_TOKEN=$OPTOK "$XNC" exec "$NODE" --json -- hostname) || CODE=$?
[ $CODE -eq 250 ] || fail "disabled exec exit=$CODE (want 250): $OUT"
echo "$OUT" | grep -q '"code":"NODE_DISABLED"' || fail "disabled exec not NODE_DISABLED: $OUT"
# f) enable → 会话恢复（enable 置 offline，控制连接仍在，exec 不经 DB status 判 online）。
"$XNC" node enable "$NODE" --json | grep -q '"ok":true' || fail "node enable"
CODE=0; OUT=$(XNC_TOKEN=$OPTOK "$XNC" exec "$NODE" --json -- hostname) || CODE=$?
[ $CODE -eq 0 ] || fail "post-enable operator exec exit=$CODE: $OUT"
echo "$OUT" | grep -q '"exitCode":0' || fail "post-enable exec no exitCode 0: $OUT"
echo "F disable/enable: OK"

echo "== audit list =="
OUT=$("$XNC" audit list --since 1h --json)
echo "$OUT" | grep -q '"action":"node.disable"' || fail "audit missing node.disable"
echo "$OUT" | grep -q '"action":"node.enable"' || fail "audit missing node.enable"
# action 过滤：命中自身、绝不泄漏其他 action。
FILT=$("$XNC" audit list --action node.disable --json)
echo "$FILT" | grep -q '"action":"node.disable"' || fail "action filter returned no rows"
if echo "$FILT" | grep -q '"action":"node.enable"'; then fail "action filter leaked node.enable"; fi
# admin-only：viewer 查审计 → 403 → 241。
CODE=0; OUT=$(XNC_TOKEN=$VWTOK "$XNC" audit list --json) || CODE=$?
[ $CODE -eq 241 ] || fail "viewer audit exit=$CODE (want 241): $OUT"
echo "audit: OK (since 过滤 + action 过滤 + admin-only)"

echo "== cluster member list =="
OUT=$("$XNC" cluster member list default --json)
echo "$OUT" | grep -q '"email":"op@test"' || fail "member list missing op@test"
echo "$OUT" | grep -q '"role":"operator"' || fail "member list missing operator role"
echo "$OUT" | grep -q '"email":"viewer@test"' || fail "member list missing viewer@test"
echo "$OUT" | grep -q '"role":"viewer"' || fail "member list missing viewer role"
# 成员即可列（viewer 视角同样可见矩阵成员）。
XNC_TOKEN=$VWTOK "$XNC" cluster member list default --json | grep -q '"email":"op@test"' ||
  fail "viewer cannot list members"
echo "member list: OK (admin/operator/viewer, viewer 可列)"

kill "$MPID" 2>/dev/null || true
wait "$MPID" 2>/dev/null || true
echo "== ALL PHASE5 E2E PASSED =="
