#!/usr/bin/env bash
# 注入主故障场景：Fault Lab 的 /health 返回 503（规格 §12.1；不用 api scale 0）。
#
# 原理：PUT /control/v1/state 需要 control bearer（只存在于操作者的本地 Secret
# 文件，不进任何 Pod）。脚本先 GET 当前 state 保存到备份文件，再把
# .health.mode 改成指定模式，最后用真实 HTTP 请求验证 /health 的行为。
#
# 幂等：重复执行不会覆盖已保存的原始 state（保证 restore 能回到最早的状态）。
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

require_tools kubectl
require_http_tools
require_context

STATE_BACKUP="${FAULT_STATE_BACKUP:-/tmp/alertflow-fault-lab-state.json}"
FAULT_MODE="${FAULT_MODE:-http_503}"
LOCAL_PORT="${FAULT_LAB_PORT:-18090}"

case "$FAULT_MODE" in
  http_503|slow|flap|connection_reset) ;;
  *) die "FAULT_MODE 仅支持 http_503|slow|flap|connection_reset（当前 $FAULT_MODE）" ;;
esac

TOKEN="$(secret_env_get FAULT_LAB_CONTROL_TOKEN)"
[ -n "$TOKEN" ] || die "本地 Secret 文件缺少 FAULT_LAB_CONTROL_TOKEN：先运行 create-secrets.sh"

kube -n fault-lab get deployment fault-lab >/dev/null 2>&1 || die "Fault Lab 未部署：先运行 deploy-fault-lab.sh"

cleanup() { [ -n "${PF_PID:-}" ] && kill "$PF_PID" 2>/dev/null || true; }
trap cleanup EXIT

section "1/4 port-forward fault-lab"
kube -n fault-lab port-forward svc/fault-lab "${LOCAL_PORT}:8090" >/tmp/alertflow-fault-pf.log 2>&1 &
PF_PID=$!
for _ in $(seq 1 50); do
  curl -s -o /dev/null --max-time 1 "http://127.0.0.1:${LOCAL_PORT}/healthz" 2>/dev/null && break
  sleep 0.2
done

auth=(-H "Authorization: Bearer ${TOKEN}")

section "2/4 保存原始 state（不覆盖已有备份）"
if [ -f "$STATE_BACKUP" ]; then
  info "已存在备份 $STATE_BACKUP，保留原始状态用于 restore"
else
  code="$(curl -s -o /tmp/alertflow-fault-state.json -w '%{http_code}' --max-time 5 "${auth[@]}" "http://127.0.0.1:${LOCAL_PORT}/control/v1/state" || true)"
  [ "$code" = "200" ] || die "GET /control/v1/state -> ${code:-无响应}（token 不对或 Fault Lab 未就绪）"
  jq -e '.state' /tmp/alertflow-fault-state.json >/dev/null || die "state 响应不是预期 JSON"
  jq '.state' /tmp/alertflow-fault-state.json > "${STATE_BACKUP}.tmp"
  mv "${STATE_BACKUP}.tmp" "$STATE_BACKUP"
  chmod 600 "$STATE_BACKUP"
  info "原始 state 已保存：$STATE_BACKUP"
fi

section "3/4 PUT /control/v1/state mode=${FAULT_MODE}"
jq --arg mode "$FAULT_MODE" '.health.mode = $mode' "$STATE_BACKUP" > /tmp/alertflow-fault-body.json
code="$(curl -s -o /tmp/alertflow-fault-put.json -w '%{http_code}' --max-time 5 \
  -X PUT "${auth[@]}" -H 'Content-Type: application/json' \
  --data-binary @/tmp/alertflow-fault-body.json \
  "http://127.0.0.1:${LOCAL_PORT}/control/v1/state" || true)"
[ "$code" = "200" ] || die "PUT /control/v1/state -> ${code:-无响应} body=$(head -c 400 /tmp/alertflow-fault-put.json 2>/dev/null || true)"
info "已应用 mode=$(jq -r '.state.health.mode' /tmp/alertflow-fault-put.json) updated_at=$(jq -r '.updated_at' /tmp/alertflow-fault-put.json)"

section "4/4 验证真实响应"
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:${LOCAL_PORT}/health" || true)"
if [ "$FAULT_MODE" = "http_503" ]; then
  [ "$code" = "503" ] || die "/health 期望 503，实际 ${code:-无响应}"
  check_pass "GET /health -> 503（故障已注入）"
else
  info "/health -> ${code}（模式 ${FAULT_MODE}，不断言具体状态码）"
fi
# /healthz 是 Fault Lab 自身存活探针，必须仍然 200，否则 kubelet 会把 Pod 重启掉。
hz="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:${LOCAL_PORT}/healthz" || true)"
if [ "$hz" = "200" ]; then
  check_pass "GET /healthz -> 200（simulator 自身存活，不会被重启）"
else
  check_fail "GET /healthz -> ${hz}"
fi

info "故障已注入：mode=${FAULT_MODE}。恢复：scripts/restore-fault.sh"
info "AlertFlow 会在下一个 check interval（seed 为 30s）后产生 incident/delivery；查看：kubectl -n alertflow logs deploy/alertflow-monitor -f"
