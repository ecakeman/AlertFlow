#!/usr/bin/env bash
# 从监控确认异常到 FD 可检索的闭环。在 deploy 目录执行。
set -euo pipefail
cd "$(dirname "$0")"
docker compose up -d --build
docker compose exec -T fault python -c 'import os; os.path.exists("/tmp/alertflow-healthy") and os.remove("/tmp/alertflow-healthy")'
for _ in $(seq 1 60); do
  if curl --noproxy '*' -sf http://127.0.0.1:18080/healthz >/dev/null; then
    break
  fi
  sleep 2
done
curl --noproxy '*' -sf http://127.0.0.1:18080/healthz >/dev/null

KEY="orders-$(date +%s)"
BODY=$(curl --noproxy '*' -sf -X POST http://127.0.0.1:18080/api/v1/services \
  -H 'Content-Type: application/json' \
  -d "{\"service_key\":\"${KEY}\",\"name\":\"${KEY}\",\"authority_producer_id\":\"order-api\",\"targets\":[{\"target_key\":\"api\",\"target_name\":\"API\",\"target_type\":\"HTTP\",\"address\":\"http://fault:8081/health\",\"interval_ms\":400,\"timeout_ms\":300,\"failure_threshold\":2,\"recovery_threshold\":1}]}")
SID=$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["service_id"])' "$BODY")

for _ in $(seq 1 40); do
  MON=$(curl --noproxy '*' -sf "http://127.0.0.1:18080/api/v1/services/${SID}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["monitor_status"])')
  if [[ "$MON" == "ABNORMAL" ]]; then
    break
  fi
  sleep 1
done
if [[ "$MON" != "ABNORMAL" ]]; then
  echo "monitor did not confirm abnormal: $MON" >&2
  exit 1
fi
NOW=$(date -u +%Y-%m-%dT%H:%M:%SZ)
RES=$(curl --noproxy '*' -sf -X POST "http://127.0.0.1:18080/api/v1/services/${SID}/events" \
  -H 'Content-Type: application/json' \
  -d "{\"event_id\":\"boot\",\"producer_id\":\"order-api\",\"event_time\":\"${NOW}\",\"event_kind\":\"DIAGNOSTIC_INFO\",\"schema_version\":1,\"payload\":{\"message\":\"pool exhausted\"}}")
EP=$(python3 -c 'import json,sys; print(json.loads(sys.argv[1]).get("episode_id",""))' "$RES")
if [[ -z "$EP" ]]; then
  echo "episode was not created: $RES" >&2
  exit 1
fi

LATER=$(date -u +%Y-%m-%dT%H:%M:%SZ)
curl --noproxy '*' -sf -X POST "http://127.0.0.1:18080/api/v1/services/${SID}/events" \
  -H 'Content-Type: application/json' \
  -d "{\"event_id\":\"up\",\"producer_id\":\"order-api\",\"event_time\":\"${LATER}\",\"source_revision\":2,\"event_kind\":\"HEALTH_STATUS\",\"health_status\":\"NORMAL\",\"schema_version\":1,\"payload\":{\"message\":\"recovered\"}}" >/dev/null
curl --noproxy '*' -sf -X POST http://fault:8081/recover >/dev/null || docker compose exec -T fault python -c 'open("/tmp/alertflow-healthy","w").close()'

STATUS=""
for _ in $(seq 1 60); do
  STATUS=$(curl --noproxy '*' -sf "http://127.0.0.1:18080/api/v1/episodes/${EP}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')
  if [[ "$STATUS" == "ARCHIVED" ]]; then
    break
  fi
  sleep 2
done
if [[ "$STATUS" != "ARCHIVED" ]]; then
  echo "episode status=$STATUS" >&2
  curl --noproxy '*' -s "http://127.0.0.1:18080/api/v1/episodes/${EP}" >&2 || true
  exit 1
fi

docker compose exec -T agent python - <<PY
import os
from alertflow_agent.fd import connect, search_faults
conn = connect(os.environ["ALERTFLOW_DATABASE_URL"])
hits = search_faults(conn, "${SID}", "pool exhausted")
assert hits, "fd was not retrieved"
assert hits[0]["service_id"] == "${SID}"
print(hits[0]["fd_id"])
PY
echo "e2e archived ${EP}"
