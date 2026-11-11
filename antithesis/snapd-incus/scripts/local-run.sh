#!/bin/bash
# Local loop: build, start, run the workload for N seconds, collect evidence.
# Usage: scripts/local-run.sh <seconds> <out-dir> [EVENTUALLY_AFTER_SECONDS]
set -eu
SECS=${1:-300}; OUT=${2:?out dir}; EV=${3:-}
cd "$(dirname "$0")/../config"
export COMPOSE_FILE=docker-compose.yaml:docker-compose.local.yaml
mkdir -p "$OUT"
docker compose build >/dev/null
docker compose down -v >/dev/null 2>&1 || true
if [ -n "$EV" ]; then
  NO_EVENTUALLY= EVENTUALLY_AFTER_SECONDS=$EV docker compose up -d >/dev/null 2>&1
else
  docker compose up -d >/dev/null 2>&1
fi
docker compose exec -T driver /opt/antithesis/test/v1/main/first_start_workload || true
timeout 900 sh -c 'until docker compose exec -T driver grep -qs antithesis_setup /tmp/sdk.jsonl; do sleep 5; done'
sleep "$SECS"
docker compose cp driver:/tmp/sdk.jsonl "$OUT/sdk.jsonl" >/dev/null 2>&1
docker compose logs > "$OUT/all.log" 2>&1
docker compose ps -a > "$OUT/ps.txt"
grep "stats:" "$OUT/all.log" | tail -1
echo "== failing Always (hit and false):"
jq -c 'select(.antithesis_assert.hit==true and .antithesis_assert.condition==false and .antithesis_assert.assert_type=="always") | .antithesis_assert | {message, details}' "$OUT/sdk.jsonl" | head -20
echo "== never hit:"
jq -rs '[.[].antithesis_assert // empty] | group_by(.message)[] | select(.[0].must_hit and (any(.hit) | not)) | .[0].message' "$OUT/sdk.jsonl"
echo "== sometimes never true:"
jq -rs '[.[].antithesis_assert // empty] | group_by(.message)[] | select(.[0].assert_type=="sometimes" and (any(.hit and .condition)|not) and any(.hit)) | .[0].message' "$OUT/sdk.jsonl"
