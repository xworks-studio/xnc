#!/bin/sh
# pull-server.sh — SRV 侧 server 镜像拉取部署（设计文档 §4）：
# CI 只发布 channel=server 的镜像 tar（GET /server-image，头 X-Xnc-Version/
# X-Xnc-Sha256）；本脚本对比运行版本，不同才下载 → sha 校验 → docker load
# → tag :local → up -d → 健康验证，失败自动回滚到旧版本 tag。
# cron：*/5 * * * * /opt/xnc/deploy/pull-server.sh >> /var/log/xnc-pull.log 2>&1
set -e
cd /opt/xnc
BASE="https://xnc.app"

exec 9>/tmp/xnc-pull.lock
flock -n 9 || { echo "$(date -Is) previous pull still running; skip"; exit 0; }

hdr() { curl -sI "$BASE/server-image?channel=server" | tr -d '\r' | awk -F': ' -v k="$1" '$1==k{print $2}'; }

want=$(hdr X-Xnc-Version)
[ -n "$want" ] || { echo "$(date -Is) no server-image published; skip"; exit 0; }

# 当前运行版本 = 运行中服务自报（经 caddy 容器内网探测；xnc-server 无 shell）
have=$(docker compose -f deploy/docker-compose.yml exec -T caddy \
  wget -qO- http://xnc-server:8080/api/health 2>/dev/null | jq -r .version 2>/dev/null || true)
[ "$want" = "$have" ] && { echo "$(date -Is) up-to-date ($want)"; exit 0; }

echo "$(date -Is) updating ${have:-<none>} -> $want"
curl -sf "$BASE/server-image?channel=server" -o "images/server-image-$want.tar.gz"
sha_got=$(sha256sum "images/server-image-$want.tar.gz" | cut -d' ' -f1)
[ "$sha_got" = "$(hdr X-Xnc-Sha256)" ] || { echo "sha mismatch; abort"; rm -f "images/server-image-$want.tar.gz"; exit 1; }

docker load -i "images/server-image-$want.tar.gz" >/dev/null
docker tag "xnc-server:$want" xnc-server:local
docker compose -f deploy/docker-compose.yml up -d xnc-server >/dev/null

ls -1t images/server-image-*.tar.gz 2>/dev/null | tail -n +6 | xargs -r rm --

ok=""
for i in $(seq 1 12); do
  v=$(docker compose -f deploy/docker-compose.yml exec -T caddy \
    wget -qO- http://xnc-server:8080/api/health 2>/dev/null | jq -r .version 2>/dev/null || true)
  [ "$v" = "$want" ] && { ok=1; break; }
  sleep 5
done
if [ -z "$ok" ]; then
  echo "$(date -Is) ERROR: health did not report $want; rolling back to ${have:-none}"
  if [ -n "$have" ] && docker image inspect "xnc-server:$have" >/dev/null 2>&1; then
    docker tag "xnc-server:$have" xnc-server:local
    docker compose -f deploy/docker-compose.yml up -d xnc-server >/dev/null
  fi
  exit 1
fi
echo "$(date -Is) live: $want"
