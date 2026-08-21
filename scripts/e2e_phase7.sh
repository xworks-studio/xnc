#!/usr/bin/env bash
# Phase 7 E2E：Web UI 构建链 + 内嵌 SPA 服务全链路。
# 链路：npm build → web/dist →（本地验证：同步到 server/web/dist → go build
# 嵌入）→（镜像验证：Dockerfile Node 阶段重建 → 拷入 server/web/dist →
# go:embed）→ distroless 单镜像。断言：
#   1. 本地 web 构建产物存在且非占位（index.html + assets/*，大小检查）
#   2. 本地 server go build 通过（embed 指令命中真实产物）
#   3. dev compose 栈：GET / → 200 index.html（<div id="root"> 且引用
#      /assets/ hash 资源——后者排除"二进制仍是占位页"的假绿）
#   4. GET /api/health → {"status":"ok"}
#   5. GET /nodes（客户端路由）→ 200 且与 / 字节一致（SPA fallback）
#   6. GET /api/nope → 404（API 前缀不被 SPA 吞掉）
# 交互（登录→节点→Terminal→用户管理）属浏览器手工验收，不在此脚本范围。
set -euo pipefail
cd "$(dirname "$0")/.."

# Fresh-checkout guard: compose needs deploy/.env (gitignored).
[ -f deploy/.env ] || cp deploy/.env.example deploy/.env

SERVER=http://127.0.0.1:8080
fail() { echo "FAIL: $*"; exit 1; }

echo "== web build =="
# node_modules 被忽略不入库；新检出先 npm ci（Docker 内始终 npm ci，此处仅本地）
[ -x web/node_modules/.bin/vite ] || (cd web && npm ci --no-fund --no-audit)
(cd web && npm run build)
[ -f web/dist/index.html ] || fail "web/dist/index.html missing after npm run build"
ls web/dist/assets/*.js >/dev/null 2>&1 || fail "web/dist/assets/*.js missing"
# 构建产物大小检查：真实构建（含 xterm）远大于占位；50KB 下限防"空壳"假绿。
SIZE_KB=$(du -sk web/dist | cut -f1)
[ "$SIZE_KB" -gt 50 ] || fail "web/dist too small: ${SIZE_KB}KB (placeholder leak?)"
echo "web/dist: ${SIZE_KB}KB ($(ls web/dist/assets | tr '\n' ' '))"

echo "== local embed check: sync dist + go build =="
# 同步真实产物到嵌入位置（合并拷贝，保留占位 .gitkeep；未跟踪文件被
# .gitignore 豁免规则忽略）。EXIT 时还原被覆盖的占位 index.html，保持树干净。
mkdir -p server/web/dist
cp -r web/dist/. server/web/dist/
touch server/web/dist/.gitkeep
(cd server && go build ./cmd/xnc-server)
echo "local go build with embedded dist: OK"

echo "== dev stack (multi-stage image with embedded ui) =="
COMPOSE="docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.dev.yml"
$COMPOSE up -d --build
trap 'cd "$(dirname "$0")/.." && $COMPOSE down -v; git checkout -- server/web/dist/index.html 2>/dev/null || true' EXIT
for i in $(seq 1 60); do curl -sf "$SERVER/api/health" >/dev/null && break; sleep 1; done
curl -sf "$SERVER/api/health" >/dev/null || { echo "server not healthy"; exit 1; }

echo "== / serves real index.html =="
BODY=$(curl -sf "$SERVER/")
echo "$BODY" | grep -q '<div id="root">' || fail "/ missing root div: $BODY"
echo "$BODY" | grep -q '/assets/' || fail "/ has no hashed asset refs (placeholder served, image build stale?): $BODY"

echo "== /api/health =="
curl -sf "$SERVER/api/health" | grep -q '"status":"ok"' || fail "health payload wrong"

echo "== SPA fallback: /nodes == "
SPA=$(curl -sf "$SERVER/nodes")
echo "$SPA" | grep -q '<div id="root">' || fail "/nodes missing root div"
[ "$SPA" = "$BODY" ] || fail "/nodes body differs from / (fallback broken)"

echo "== hashed assets served =="
JS=$(echo "$BODY" | grep -o '/assets/[^"]*\.js' | head -1)
CSS=$(echo "$BODY" | grep -o '/assets/[^"]*\.css' | head -1)
[ -n "$JS" ] || fail "no js asset in index.html"
curl -sf "$SERVER$JS" >/dev/null || fail "asset $JS not served"
[ -n "$CSS" ] || fail "no css asset in index.html"
curl -sf "$SERVER$CSS" >/dev/null || fail "asset $CSS not served"
echo "assets ok: $JS $CSS"

echo "== /api/unknown stays 404 (SPA must not swallow api) =="
CODE=$(curl -s -o /dev/null -w '%{http_code}' "$SERVER/api/nope")
[ "$CODE" = "404" ] || fail "/api/nope http=$CODE (want 404)"

echo "== ALL PHASE7 E2E PASSED =="
