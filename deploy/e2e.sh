#!/usr/bin/env bash
# AlertFlow 端到端演练（BACKEND_AND_DRILL_REVIEW.md 场景 B）：
# 真实 HTTP 目标连续失败 -> Go 监控判定 ABNORMAL -> Episode -> 诊断任务 ->
# 产物可读 -> 恢复/归档。在 deploy 目录执行，可重复运行（每次创建唯一 service_key）。
#
# 重要：脚本会创建服务、Episode、产物与 FD 数据。请使用独立的 Compose project /
# 数据卷运行，勿对既有用户数据或生产环境执行。示例：
#   COMPOSE_PROJECT_NAME=alertflow-drill-$(date +%s) ./e2e.sh
# 结束后销毁演练数据：
#   COMPOSE_PROJECT_NAME=<同一个名字> docker compose down -v
#
# 依赖：docker compose、curl、python3（不使用 jq）。
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

RUN_ID="$(date +%s)-$$"
API_URL="http://127.0.0.1:18080"

fail() {
  echo "e2e 失败: $*" >&2
  exit 1
}

# 读取变量：优先当前 shell 环境，其次 deploy/.env，最后默认值。
# 不 source .env（模板注释和带空格的值不适合 bash 解析）。
env_value() {
  local key="$1" default="${2:-}" value=""
  if [[ -n "${!key:-}" ]]; then
    value="${!key}"
  elif [[ -f .env ]]; then
    value="$(sed -n "s/^${key}=//p" .env | tail -n 1 | tr -d '\r')"
  fi
  if [[ -n "$value" ]]; then printf '%s' "$value"; else printf '%s' "$default"; fi
}

FAULT_PORT="$(env_value FAULT_PORT 18081)"
FAULT_URL="http://127.0.0.1:${FAULT_PORT}"

# Knowledge/FD 校验仅在显式启用 Knowledge 时执行：
# COMPOSE_PROFILES 含 knowledge，或 ALERTFLOW_KNOWLEDGE_ENABLED=true。
PROFILES_IN="${COMPOSE_PROFILES:-}"
KNOWLEDGE_FLAG="$(env_value ALERTFLOW_KNOWLEDGE_ENABLED false)"
KNOWLEDGE_ON=false
case ",${PROFILES_IN// /}," in
  *",knowledge,"*) KNOWLEDGE_ON=true ;;
esac
if [[ "$KNOWLEDGE_FLAG" == "true" ]]; then
  KNOWLEDGE_ON=true
fi
if [[ "$KNOWLEDGE_ON" == "true" ]]; then
  case ",${PROFILES_IN// /}," in
    *",knowledge,"*) ;;
    *) PROFILES_IN="${PROFILES_IN:+${PROFILES_IN},}knowledge" ;;
  esac
  export ALERTFLOW_KNOWLEDGE_ENABLED=true
  export KNOWLEDGE_A2A_TOKEN="$(env_value KNOWLEDGE_A2A_TOKEN knowledge-a2a-token)"
else
  export ALERTFLOW_KNOWLEDGE_ENABLED=false
  export KNOWLEDGE_A2A_TOKEN="$(env_value KNOWLEDGE_A2A_TOKEN "")"
fi
export COMPOSE_PROFILES="$PROFILES_IN"

if [[ -z "${COMPOSE_PROJECT_NAME:-}" ]]; then
  echo "警告：未设置 COMPOSE_PROJECT_NAME，本次演练将写入默认 Compose project 的数据卷。"
  echo "      建议：COMPOSE_PROJECT_NAME=alertflow-drill-${RUN_ID} ./e2e.sh，结束后 docker compose down -v 销毁。"
fi

# ---------- HTTP 辅助函数（curl + python3，不依赖 jq） ----------
api_get() { # $1: path；输出 JSON body，非 200 时报错
  local path="$1" out status
  out="$(mktemp)"
  status="$(curl --noproxy '*' -s -o "$out" -w '%{http_code}' "${API_URL}${path}")"
  if [[ "$status" != "200" ]]; then
    echo "GET ${path} -> HTTP ${status}: $(cat "$out")" >&2
    rm -f "$out"
    return 1
  fi
  cat "$out"
  rm -f "$out"
}

api_post() { # $1: path，$2: JSON body；输出 JSON body，非 2xx 时报错
  local path="$1" body="$2" out status
  out="$(mktemp)"
  status="$(curl --noproxy '*' -s -o "$out" -w '%{http_code}' -X POST "${API_URL}${path}" \
    -H 'Content-Type: application/json' -d "$body")"
  if [[ "$status" != "200" && "$status" != "201" ]]; then
    echo "POST ${path} -> HTTP ${status}: $(cat "$out")" >&2
    rm -f "$out"
    return 1
  fi
  cat "$out"
  rm -f "$out"
}

artifact_ready() { # $1: INVESTIGATION | DIAGNOSIS_REPORT
  local body
  body="$(api_get "/api/v1/episodes/${EP}/artifacts?type=$1" 2>/dev/null)" || return 1
  printf '%s' "$body" | python3 -c '
import json, sys
d = json.load(sys.stdin)
text = (d.get("content_text") or "").strip()
content_json = d.get("content_json")
sys.exit(0 if (text or content_json) else 1)
'
}

# ---------- 启动服务并等待就绪 ----------
if [[ "${ALERTFLOW_SKIP_IMAGE_BUILD:-}" == "1" ]]; then
  docker compose up -d --no-build
else
  docker compose up -d --build
fi

for _ in $(seq 1 60); do
  if curl --noproxy '*' -sf "${API_URL}/healthz" >/dev/null; then
    break
  fi
  sleep 2
done
curl --noproxy '*' -sf "${API_URL}/healthz" >/dev/null

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
  if docker compose exec -T integration bash -c 'echo >/dev/tcp/127.0.0.1/8081'; then
    break
  fi
  sleep 2
done
docker compose exec -T integration bash -c 'echo >/dev/tcp/127.0.0.1/8081'

if [[ "$KNOWLEDGE_ON" == "true" ]]; then
  for _ in $(seq 1 60); do
    if docker compose exec -T knowledge-agent python -c 'import urllib.request; urllib.request.urlopen("http://127.0.0.1:8000/health", timeout=2).read()'; then
      break
    fi
    sleep 2
  done
  docker compose exec -T knowledge-agent python -c 'import urllib.request; urllib.request.urlopen("http://127.0.0.1:8000/health", timeout=2).read()'
fi

# 故障辅助服务只绑定宿主机回环地址，确认端口可用（初始 /health 可能是 200 或 500）。
FAULT_CODE=""
for _ in $(seq 1 30); do
  FAULT_CODE="$(curl --noproxy '*' -s -o /dev/null -w '%{http_code}' "${FAULT_URL}/health" || true)"
  if [[ "$FAULT_CODE" == "200" || "$FAULT_CODE" == "500" ]]; then
    break
  fi
  sleep 1
done
[[ "$FAULT_CODE" == "200" || "$FAULT_CODE" == "500" ]] \
  || fail "故障辅助服务 ${FAULT_URL}/health 不可达（当前 HTTP ${FAULT_CODE:-none}），检查 FAULT_PORT/端口映射"

# ---------- 暂停演练前已启用的监控目标，避免旧目标占满探测 worker ----------
# 只暂停脚本开始前已经启用的目标；本次新建的目标不在这份名单里。
# 进程正常退出或失败退出时按原 target_id 恢复；SIGKILL 不会执行恢复。
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
cleanup() {
  restore_prior_targets || true
}
trap cleanup EXIT
PRIOR_LIST="$(prior_target_list)"
if [[ -n "$PRIOR_LIST" ]]; then
  docker compose exec -T postgres psql -U alertflow -d alertflow -c "UPDATE system.service_monitor_targets SET enabled=false WHERE target_id IN (${PRIOR_LIST})"
fi

# ---------- 1. 创建服务与真实 HTTP 目标（间隔 1000ms / 超时 500ms / 失败阈值 3 / 恢复阈值 2） ----------
KEY="orders-api-${RUN_ID}"
CREATE_BODY=$(cat <<JSON
{"service_key":"${KEY}","name":"orders-api 演练 ${RUN_ID}","description":"HTTP 故障演练：下游订单库连接池耗尽","authority_producer_id":"order-api","targets":[{"target_key":"api","target_name":"Orders API Health","target_type":"HTTP","address":"http://fault:8081/health","interval_ms":1000,"timeout_ms":500,"failure_threshold":3,"recovery_threshold":2}]}
JSON
)
SERVICE="$(api_post "/api/v1/services" "$CREATE_BODY")"
SID="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["service_id"])' <<<"$SERVICE")"
[[ -n "$SID" ]] || fail "创建服务未返回 service_id: $SERVICE"
echo "服务已创建: service_key=${KEY} service_id=${SID}"

# ---------- 2. 显式触发故障，确认真实健康检查返回 500 ----------
BREAK_CODE="$(curl --noproxy '*' -s -o /dev/null -w '%{http_code}' -X POST "${FAULT_URL}/break" || true)"
[[ "$BREAK_CODE" == "204" ]] || fail "POST ${FAULT_URL}/break 返回 HTTP ${BREAK_CODE:-连接失败}"
HEALTH_CODE="$(curl --noproxy '*' -s -o /dev/null -w '%{http_code}' "${FAULT_URL}/health" || true)"
HEALTH_BODY="$(curl --noproxy '*' -s "${FAULT_URL}/health" || true)"
[[ "$HEALTH_CODE" == "500" && "$HEALTH_BODY" == "down" ]] \
  || fail "触发故障后 /health 应为 500/down，实际 HTTP ${HEALTH_CODE} body=${HEALTH_BODY}"

# ---------- 3. 等监控真实采样判定 ABNORMAL，且该服务恰好 1 个 ACTIVE Episode ----------
MON=""
for _ in $(seq 1 90); do
  SVC="$(api_get "/api/v1/services/${SID}" 2>/dev/null || true)"
  if [[ -n "$SVC" ]]; then
    MON="$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("monitor_status",""))' <<<"$SVC")"
  fi
  [[ "$MON" == "ABNORMAL" ]] && break
  sleep 1
done
[[ "$MON" == "ABNORMAL" ]] || fail "监控未在预期时间内判定 ABNORMAL（当前 ${MON:-unknown}），检查探测 worker 与目标地址"

EP=""
ACTIVE_TOTAL=0
for _ in $(seq 1 30); do
  EPS="$(api_get "/api/v1/episodes?service_id=${SID}&status=ACTIVE&limit=20" 2>/dev/null || true)"
  if [[ -n "$EPS" ]]; then
    read -r ACTIVE_TOTAL EP_ID <<<"$(printf '%s' "$EPS" | python3 -c '
import json, sys
d = json.load(sys.stdin)
eps = d.get("episodes") or []
print(d.get("total", 0), eps[0].get("episode_id", "") if len(eps) == 1 else "")
')"
  fi
  [[ "${ACTIVE_TOTAL:-0}" -gt 1 ]] && fail "该服务存在 ${ACTIVE_TOTAL} 个 ACTIVE Episode，期望恰好 1 个"
  if [[ "${ACTIVE_TOTAL:-0}" -eq 1 && -n "${EP_ID:-}" ]]; then
    EP="$EP_ID"
    break
  fi
  sleep 1
done
[[ -n "$EP" ]] || fail "未观测到该服务唯一的 ACTIVE Episode（total=${ACTIVE_TOTAL:-0}）"
echo "Episode 已创建: ${EP}（${ACTIVE_TOTAL} 个 ACTIVE）"

# ---------- 4. 应用侧补充诊断信息（连接池耗尽 / 等待超时 / 影响范围） ----------
NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
DIAG_EVENT_ID="diag-pool-${RUN_ID}"
DIAG_BODY=$(cat <<JSON
{"event_id":"${DIAG_EVENT_ID}","producer_id":"order-api","event_time":"${NOW}","event_kind":"DIAGNOSTIC_INFO","schema_version":1,"payload":{"message":"orders database connection pool exhausted; checkout requests waiting for a connection timed out","pool":"orders-db","pool_state":"exhausted","waiting_requests":128,"wait_timeout_ms":5000,"impact_scope":"orders-api checkout and order status queries"}}
JSON
)
DIAG_RES="$(api_post "/api/v1/services/${SID}/events" "$DIAG_BODY")"
read -r EP_FROM_EVENT INPUT_VERSION <<<"$(printf '%s' "$DIAG_RES" | python3 -c '
import json, sys
d = json.load(sys.stdin)
print(d.get("episode_id", ""), d.get("input_version", 0))
')"
[[ "$EP_FROM_EVENT" == "$EP" ]] || fail "DIAGNOSTIC_INFO 返回 episode_id=${EP_FROM_EVENT}，与观测的 ${EP} 不一致"
ACCEPTED="$(printf '%s' "$DIAG_RES" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("accepted"))')"
[[ "$ACCEPTED" == "True" ]] || fail "DIAGNOSTIC_INFO 未被接受: $DIAG_RES"
echo "DIAGNOSTIC_INFO 已上报（event_id=${DIAG_EVENT_ID}，input_version=${INPUT_VERSION}）"

# ---------- 5. 等诊断任务 SUCCEEDED，两个产物均可读且非空 ----------
TASK_OK=false
PROCESSED=0
for _ in $(seq 1 150); do
  EPVIEW="$(api_get "/api/v1/episodes/${EP}" 2>/dev/null || true)"
  [[ -z "$EPVIEW" ]] && { sleep 2; continue; }
  read -r PROCESSED TASK_STATUS <<<"$(printf '%s' "$EPVIEW" | python3 -c '
import json, sys
d = json.load(sys.stdin)
print(d.get("processed_input_version", 0), d.get("active_task_status") or "")
')"
  if [[ "${PROCESSED:-0}" -ge "${INPUT_VERSION:-0}" && -z "${TASK_STATUS:-}" ]] \
    && artifact_ready INVESTIGATION && artifact_ready DIAGNOSIS_REPORT; then
    TASK_OK=true
    break
  fi
  sleep 2
done
if [[ "$TASK_OK" != "true" ]]; then
  echo "诊断未完成: processed_input_version=${PROCESSED:-0} active_task_status=${TASK_STATUS:-} episode=${EP}" >&2
  curl --noproxy '*' -s "${API_URL}/api/v1/episodes/${EP}" >&2 || true
  exit 1
fi

for ARTIFACT_TYPE in INVESTIGATION DIAGNOSIS_REPORT; do
  ARTIFACT_BODY="$(api_get "/api/v1/episodes/${EP}/artifacts?type=${ARTIFACT_TYPE}")" \
    || fail "GET artifacts?type=${ARTIFACT_TYPE} 未返回 200"
  printf '%s' "$ARTIFACT_BODY" | python3 -c '
import json, sys
d = json.load(sys.stdin)
text = (d.get("content_text") or "").strip()
content_json = d.get("content_json")
if not text and not content_json:
    raise SystemExit("artifact content is empty")
print("artifact ok:", d.get("artifact_type", ""), "version", d.get("version", ""))
' || fail "artifacts?type=${ARTIFACT_TYPE} 内容为空"
done

LLM_KEY="$(env_value ALERTFLOW_LLM_API_KEY "")"
LLM_MODEL="$(env_value ALERTFLOW_LLM_MODEL "")"
LLM_BASE="$(env_value ALERTFLOW_LLM_BASE_URL "")"
if [[ -n "$LLM_KEY" ]]; then
  echo "诊断模型: 真实模型（model=${LLM_MODEL}，base=${LLM_BASE}）"
else
  echo "诊断模型: ScriptedModel（链路验证，非真实模型质量）"
fi

# ---------- 6. 恢复故障，上报告警恢复事件，等待 Episode 归档 ----------
RECOVER_CODE="$(curl --noproxy '*' -s -o /dev/null -w '%{http_code}' -X POST "${FAULT_URL}/recover" || true)"
[[ "$RECOVER_CODE" == "204" ]] || fail "POST ${FAULT_URL}/recover 返回 HTTP ${RECOVER_CODE:-连接失败}"
HEALTH_CODE="$(curl --noproxy '*' -s -o /dev/null -w '%{http_code}' "${FAULT_URL}/health" || true)"
HEALTH_BODY="$(curl --noproxy '*' -s "${FAULT_URL}/health" || true)"
[[ "$HEALTH_CODE" == "200" && "$HEALTH_BODY" == "ok" ]] \
  || fail "恢复后 /health 应为 200/ok，实际 HTTP ${HEALTH_CODE} body=${HEALTH_BODY}"

LATER="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
NORMAL_EVENT_ID="health-normal-${RUN_ID}"
NORMAL_BODY=$(cat <<JSON
{"event_id":"${NORMAL_EVENT_ID}","producer_id":"order-api","event_time":"${LATER}","source_revision":1,"event_kind":"HEALTH_STATUS","health_status":"NORMAL","schema_version":1,"payload":{"message":"orders database connection pool recovered; connections available again"}}
JSON
)
NORMAL_RES="$(api_post "/api/v1/services/${SID}/events" "$NORMAL_BODY")"
NORMAL_ACCEPTED="$(printf '%s' "$NORMAL_RES" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("accepted"))')"
[[ "$NORMAL_ACCEPTED" == "True" ]] || fail "HEALTH_STATUS=NORMAL 未被接受: $NORMAL_RES"
echo "HEALTH_STATUS=NORMAL 已上报（event_id=${NORMAL_EVENT_ID}）"

STATUS=""
for _ in $(seq 1 120); do
  EPVIEW="$(api_get "/api/v1/episodes/${EP}" 2>/dev/null || true)"
  if [[ -n "$EPVIEW" ]]; then
    STATUS="$(printf '%s' "$EPVIEW" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))')"
  fi
  [[ "$STATUS" == "ARCHIVED" ]] && break
  sleep 2
done
if [[ "$STATUS" != "ARCHIVED" ]]; then
  echo "Episode 未归档: status=${STATUS:-unknown}" >&2
  curl --noproxy '*' -s "${API_URL}/api/v1/episodes/${EP}" >&2 || true
  exit 1
fi
echo "Episode 已归档: ${EP}"

# ---------- 7. Knowledge / FD 校验（仅显式启用 Knowledge 时执行） ----------
if [[ "$KNOWLEDGE_ON" == "true" ]]; then
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
  docker compose exec -T -e KNOWLEDGE_A2A_TOKEN="${KNOWLEDGE_A2A_TOKEN}" knowledge-agent python - <<'PY'
import json, os, time, urllib.request
token = os.environ["KNOWLEDGE_A2A_TOKEN"]
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
fi

# ---------- 8. 恢复演练前暂停的目标，并断言恢复成功 ----------
restore_prior_targets
if [[ -n "$PRIOR_LIST" ]]; then
  EXPECTED_COUNT=$(( $(printf '%s' "$PRIOR_LIST" | tr -cd ',' | wc -c) + 1 ))
  RESTORED_COUNT="$(docker compose exec -T postgres psql -U alertflow -d alertflow -tAc "SELECT count(*) FROM system.service_monitor_targets WHERE enabled AND target_id IN (${PRIOR_LIST})")"
  RESTORED_COUNT="${RESTORED_COUNT//[[:space:]]/}"
  [[ "$RESTORED_COUNT" == "$EXPECTED_COUNT" ]] \
    || fail "演练前已启用目标未全部恢复：期望 ${EXPECTED_COUNT}，实际 ${RESTORED_COUNT}"
  echo "演练前已启用的 ${EXPECTED_COUNT} 个监控目标已恢复 enabled=true"
fi
trap - EXIT

echo "e2e archived ${EP}（service_id=${SID}，service_key=${KEY}）"
echo "提示：本脚本创建了服务/Episode/产物/FD 等演练数据。"
echo "      如使用独立 COMPOSE_PROJECT_NAME 启动，结束后执行以下命令整体销毁演练数据："
echo "      COMPOSE_PROJECT_NAME=<演练项目名> docker compose down -v   # -v 会删除该项目全部命名卷数据"
