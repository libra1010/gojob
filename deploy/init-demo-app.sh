#!/bin/sh
# 等待 gojob-server 就绪后，创建 demo worker 所需的 app（幂等：已存在则忽略）。
set -eu

BASE="${GOJOB_BASE:-http://gojob-server:7800}"
USER_NAME="${GOJOB_ADMIN_USER:-admin}"
PASS="${GOJOB_ADMIN_PASSWORD:-admin123}"
APP_ID="${GOJOB_DEMO_APP_ID:-demo_app}"
APP_PASS="${GOJOB_DEMO_APP_PASSWORD:-demo123}"

echo "waiting for $BASE ..."
i=0
until curl -sf "$BASE/healthz" -o /dev/null; do
    i=$((i+1)); [ "$i" -gt 60 ] && echo "server not ready" >&2 && exit 1
    sleep 2
done

TOKEN=$(curl -sf -X POST "$BASE/openapi/user/login" \
    -H 'Content-Type: application/json' \
    -d "{\"username\":\"$USER_NAME\",\"password\":\"$PASS\"}" \
    | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$TOKEN" ] || { echo "login failed" >&2; exit 1; }

curl -sf -X POST "$BASE/openapi/app/create" \
    -H "X-GoJob-Token: $TOKEN" -H 'Content-Type: application/json' \
    -d "{\"app_id\":\"$APP_ID\",\"app_name\":\"Demo App\",\"password\":\"$APP_PASS\"}" \
    || echo "app create skipped (already exists?)"
echo "init-demo done"
