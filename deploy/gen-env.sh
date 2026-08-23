#!/usr/bin/env bash
# gen-env.sh - 以 machines.env 为部署标准生成 deploy/.env(dev 栈)。
# 映射: SRV_ADMIN_EMAIL/PASSWORD -> XNC_ADMIN_*, SRV_DOMAIN -> XNC_DOMAIN。
# 本地独有键(POSTGRES_PASSWORD/XNC_JWT_SECRET/XNC_DEV_EXTERNAL_IP)保留现有值。
set -euo pipefail
cd "$(dirname "$0")"
get() { sed -n "s/^$1=//p" machines.env | head -1; }
cur() { sed -n "s/^$1=//p" .env | head -1; }
{
  echo "# 由 gen-env.sh 从 machines.env 生成(勿手改;改 machines.env 后重跑)"
  echo "XNC_DOMAIN=$(get SRV_DOMAIN)"
  echo "POSTGRES_PASSWORD=$(cur POSTGRES_PASSWORD)"
  echo "XNC_JWT_SECRET=$(cur XNC_JWT_SECRET)"
  echo "XNC_ADMIN_EMAIL=$(get SRV_ADMIN_EMAIL)"
  echo "XNC_ADMIN_PASSWORD=$(get SRV_ADMIN_PASSWORD)"
  echo "XNC_DEV_EXTERNAL_IP=$(cur XNC_DEV_EXTERNAL_IP)"
} > .env.new
mv .env.new .env
echo ".env regenerated from machines.env (admin/domain replaced, local keys preserved)"
