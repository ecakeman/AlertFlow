#!/usr/bin/env bash
# 恢复 Fault Lab 到注入前的状态。
#
# 优先 PUT 回 inject-fault.sh 保存的原始 state；备份不存在时退化为
# POST /control/v1/reset（回到服务端默认状态）。最后用真实请求验证 /health 200。
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

require_tools kubectl
require_http_tools
require_context

STATE_BACKUP="${FAULT_STATE_BACKUP:-/tmp/alertflow-fault-lab-state.json}"
LOCAL_PORT="${FAULT_LAB_PORT:-18090}"

TOKEN="$(secret_env_get FAULT_LAB_CONTROL_TOKEN)"
[ -n "$TOKEN" ] || die "本地 Secret 文件缺少 FAULT_LAB_CONTROL_TOKEN：先运行 create-secrets.sh"

kube -n fault-lab get deployment fault-lab >/dev/null 2>&1 || die "Fault Lab 未部署"

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

section "2/4 恢复 state"
if [ -f "$STATE_BACKUP" ]; then
  info "PUT 原始 state（$STATE_BACKUP）"
  code="$(curl -s -o /tmp/alertflow-fault-restore.json -w '%{http_code}' --max-time 5 \
    -X PUT "${auth[@]}" -H 'Content-Type: application/json' \
    --data-binary @"$STATE_BACKUP" \
    "http://127.0.0.1:${LOCAL_PORT}/control/v1/state" || true)"
else
  warn "没有 $STATE_BACKUP，退化为 POST /control/v1/reset"
  code="$(curl -s -o /tmp/alertflow-fault-restore.json -w '%{http_code}' --max-time 5 \
    -X POST "${auth[@]}" "http://127.0.0.1:${LOCAL_PORT}/control/v1/reset" || true)"
fi
[ "$code" = "200" ] || die "恢复失败 -> ${code:-无响应} body=$(head -c 400 /tmp/alertflow-fault-restore.json 2>/dev/null || true)"
info "恢复后 mode=$(jq -r '.state.health.mode' /tmp/alertflow-fault-restore.json)"

section "3/4 验证真实响应"
code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:${LOCAL_PORT}/health" || true)"
if [ "$code" = "200" ]; then
  check_pass "GET /health -> 200（已恢复正常）"
else
  check_fail "GET /health -> ${code}（期望 200）"
fi

section "4/4 清理备份"
rm -f "$STATE_BACKUP"
info "done：已恢复并删除备份文件"

if [ "$FAIL_COUNT" -ne 0 ]; then
  die "恢复流程存在 ${FAIL_COUNT} 项失败"
fi
