#!/usr/bin/env bash
# 从监控确认异常到 FD 可检索的闭环。在 deploy 目录执行。
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"
export COMPOSE_PROFILES="${COMPOSE_PROFILES:-knowledge}"
export ALERTFLOW_KNOWLEDGE_ENABLED="${ALERTFLOW_KNOWLEDGE_ENABLED:-true}"
export KNOWLEDGE_A2A_TOKEN="${KNOWLEDGE_A2A_TOKEN:-knowledge-a2a-token}"
if [[ "${ALERTFLOW_SKIP_IMAGE_BUILD:-}" == "1" ]]; then
  docker compose up -d --no-build
else
  docker compose up -d --build
fi
docker compose exec -T fault python -c 'import os; os.path.exists("/tmp/alertflow-healthy") and os.remove("/tmp/alertflow-healthy")'
for _ in $(seq 1 60); do
  if curl --noproxy '*' -sf http://127.0.0.1:18080/healthz >/dev/null; then
    break
  fi
  sleep 2
done
curl --noproxy '*' -sf http://127.0.0.1:18080/healthz >/dev/null
for _ in $(seq 1 30); do
  if docker compose exec -T agent python -c 'import sys, urllib.request
try:
    urllib.request.urlopen("http://127.0.0.1:8000/health/live", timeout=2).read()
    urllib.request.urlopen("http://127.0.0.1:8000/health/ready", timeout=2).read()
except Exception:
    sys.exit(1)'; then
    break
  fi
  sleep 2
done
docker compose exec -T agent python -c 'import urllib.request; urllib.request.urlopen("http://127.0.0.1:8000/health/live", timeout=2).read(); urllib.request.urlopen("http://127.0.0.1:8000/health/ready", timeout=2).read()'
for _ in $(seq 1 60); do
  if docker compose exec -T integration bash -c 'echo >/dev/tcp/127.0.0.1/8081' \
    && docker compose exec -T knowledge-agent python -c 'import urllib.request; urllib.request.urlopen("http://127.0.0.1:8000/health", timeout=2).read()'; then
    break
  fi
  sleep 2
done
docker compose exec -T integration bash -c 'echo >/dev/tcp/127.0.0.1/8081'
docker compose exec -T knowledge-agent python -c 'import urllib.request; urllib.request.urlopen("http://127.0.0.1:8000/health", timeout=2).read()'
# 只暂停脚本开始前已经启用的目标，避免旧目标占满 4 个探测 worker。
# 本次新建的目标不在这份名单里。进程正常退出时按原 target_id 恢复。SIGKILL 不会执行恢复。
PRIOR_TARGETS="$(docker compose exec -T postgres psql -U alertflow -d alertflow -At -c "SELECT target_id FROM system.service_monitor_targets WHERE enabled")"
prior_target_list() {
  local id list=""
  while IFS= read -r id; do
    id="${id//$'\r'/}"
    [[ -z "$id" ]] && continue
    [[ "$id" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]] || {
      echo "unexpected monitor target id: $id" >&2
      exit 1
    }
    list+="'${id}',"
  done <<< "$PRIOR_TARGETS"
  printf '%s' "${list%,}"
}
restore_prior_targets() {
  local list
  list="$(prior_target_list)"
  [[ -z "$list" ]] && return 0
  docker compose exec -T postgres psql -U alertflow -d alertflow -c "UPDATE system.service_monitor_targets SET enabled=true WHERE target_id IN (${list})" >/dev/null
}
trap restore_prior_targets EXIT
PRIOR_LIST="$(prior_target_list)"
if [[ -n "$PRIOR_LIST" ]]; then
  docker compose exec -T postgres psql -U alertflow -d alertflow -c "UPDATE system.service_monitor_targets SET enabled=false WHERE target_id IN (${PRIOR_LIST})"
fi

KEY="orders-$(date +%s)"
BODY=$(curl --noproxy '*' -sf -X POST http://127.0.0.1:18080/api/v1/services \
  -H 'Content-Type: application/json' \
  -d "{\"service_key\":\"${KEY}\",\"name\":\"${KEY}\",\"authority_producer_id\":\"order-api\",\"targets\":[{\"target_key\":\"api\",\"target_name\":\"API\",\"target_type\":\"HTTP\",\"address\":\"http://fault:8081/health\",\"interval_ms\":400,\"timeout_ms\":300,\"failure_threshold\":2,\"recovery_threshold\":1}]}")
SID=$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["service_id"])' "$BODY")

for _ in $(seq 1 90); do
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
import asyncio
import os
from alertflow_agent.fd import connect, search_faults

async def main():
    conn = await connect(os.environ["ALERTFLOW_DATABASE_URL"])
    try:
        hits = await search_faults(conn, "${SID}", "pool exhausted")
    finally:
        await conn.close()
    assert hits, "fd was not retrieved"
    matched = [hit for hit in hits if hit["service_id"] == "${SID}"]
    assert matched, hits
    print(matched[0]["fd_id"])

asyncio.run(main())
PY
RECEIPTS=$(docker compose exec -T postgres psql -U alertflow -d alertflow -tAc "SELECT count(*) FROM system.episode_fault_document_refs WHERE episode_id='${EP}'::uuid")
if [[ "${RECEIPTS}" -lt 1 ]]; then
  echo "fault document receipt missing" >&2
  exit 1
fi
INVOKED=$(docker compose exec -T postgres psql -U alertflow -d alertflow -tAc "SELECT count(*) FROM gateway.agent_invocations WHERE status='COMPLETED'")
if [[ "${INVOKED}" -lt 1 ]]; then
  echo "knowledge invocation did not complete" >&2
  docker compose exec -T postgres psql -U alertflow -d alertflow -c "SELECT status, error_code, left(safe_message,120) FROM gateway.agent_invocations" >&2 || true
  exit 1
fi
echo "e2e archived ${EP}"
docker compose exec -T knowledge-agent python - <<'PY'
import json, time, urllib.request
token = "knowledge-a2a-token"
boundary = "----alertflow"
payload = (
    f"--{boundary}\r\n"
    'Content-Disposition: form-data; name="file"; filename="sample.txt"\r\n'
    "Content-Type: text/plain\r\n\r\n"
    "AlertFlow knowledge sample: pool exhausted on orders database\r\n"
    f"--{boundary}\r\n"
    'Content-Disposition: form-data; name="title"\r\n\r\n'
    "orders pool\r\n"
    f"--{boundary}--\r\n"
).encode()
req = urllib.request.Request(
    "http://127.0.0.1:8000/spaces/operations-manual/documents",
    data=payload,
    method="POST",
    headers={"Authorization": f"Bearer {token}", "Content-Type": f"multipart/form-data; boundary={boundary}"},
)
with urllib.request.urlopen(req, timeout=10) as resp:
    created = json.load(resp)
job_id = created["job_id"]
status = ""
for _ in range(20):
    with urllib.request.urlopen(urllib.request.Request(
        f"http://127.0.0.1:8000/jobs/{job_id}",
        headers={"Authorization": f"Bearer {token}"},
    ), timeout=5) as resp:
        job = json.load(resp)
    status = job["status"]
    if status == "READY":
        break
    time.sleep(1)
if status != "READY":
    raise SystemExit(f"ingest status={status} error={job.get('last_error')}")
message = {
    "jsonrpc": "2.0",
    "id": "sample",
    "method": "message/send",
    "params": {"message": {"parts": [{"data": {"query": "pool exhausted", "invocationId": "sample-ingest"}}]}},
}
req = urllib.request.Request(
    "http://127.0.0.1:8000/",
    data=json.dumps(message).encode(),
    method="POST",
    headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
)
with urllib.request.urlopen(req, timeout=30) as resp:
    result = json.load(resp)
text = json.dumps(result, ensure_ascii=False)
if "pool exhausted" not in text:
    raise SystemExit(text[:500])
print("knowledge sample retrieved")
PY
