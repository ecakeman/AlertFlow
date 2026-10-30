#!/usr/bin/env bash
# 端到端冒烟测试。PASS 判定必须是真实响应（HTTP 状态码 + 可解析 JSON、gRPC
# channel ready、psql/kafka/redis 的真实查询），不能只看 Pod Ready。
#
# 只读：除了一次性 config-inspect Pod（--rm，读 config PVC）外不创建常驻资源，
# 不删除任何 PVC。
#
# 用法：scripts/smoke-test.sh [--skip-fault-lab]
# 退出码：0=全部 PASS；1=存在 FAIL（明细见输出）。
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

SKIP_FAULT_LAB=no
for arg in "$@"; do
  case "$arg" in
    --skip-fault-lab) SKIP_FAULT_LAB=yes ;;
    -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
    *) die "未知参数：$arg" ;;
  esac
done

require_tools kubectl docker
require_http_tools
require_context
require_ns alertflow

PF_PIDS=()
cleanup() {
  local pid
  for pid in "${PF_PIDS[@]:-}"; do kill "$pid" 2>/dev/null || true; done
}
trap cleanup EXIT

# port_forward <ns> <svc> <local> <remote>
port_forward() {
  local ns="$1" svc="$2" local_port="$3" remote_port="$4"
  kube -n "$ns" port-forward "svc/${svc}" "${local_port}:${remote_port}" >/tmp/alertflow-pf-"${svc}"-"${local_port}".log 2>&1 &
  PF_PIDS+=("$!")
  local i
  for i in $(seq 1 50); do
    if curl -s -o /dev/null --max-time 1 "http://127.0.0.1:${local_port}/" 2>/dev/null \
      || grep -q 'Forwarding from' /tmp/alertflow-pf-"${svc}"-"${local_port}".log 2>/dev/null; then
      return 0
    fi
    sleep 0.2
  done
  return 1
}

section "0/9 前置"
kube get nodes -o wide || true

section "1/9 工作负载 rollout"
for sts in alertflow-postgres alertflow-kafka; do
  if kube -n alertflow rollout status "statefulset/${sts}" --timeout=10s >/dev/null 2>&1; then
    check_pass "statefulset/${sts} rollout 完成"
  else
    check_fail "statefulset/${sts} 未完成 rollout"
  fi
done
for dep in alertflow-redis alertflow-api alertflow-monitor alertflow-publisher alertflow-consumer alertflow-delivery alertflow-agent; do
  if kube -n alertflow rollout status "deployment/${dep}" --timeout=10s >/dev/null 2>&1; then
    check_pass "deployment/${dep} rollout 完成"
  else
    check_fail "deployment/${dep} 未完成 rollout"
  fi
done
if [ "$SKIP_FAULT_LAB" = "no" ]; then
  if kube -n fault-lab rollout status deployment/fault-lab --timeout=10s >/dev/null 2>&1; then
    check_pass "deployment/fault-lab rollout 完成"
  else
    check_fail "deployment/fault-lab 未完成 rollout（或未部署：用 --skip-fault-lab 跳过）"
  fi
fi

section "2/9 PVC"
for pvc in $(kube -n alertflow get pvc -o jsonpath='{range .items[*]}{.metadata.name} {.status.phase}{"\n"}{end}' | awk '$2!="Bound"{print $1}'); do
  check_fail "PVC alertflow/${pvc} 未 Bound"
done
kube -n alertflow get pvc
if [ "$SKIP_FAULT_LAB" = "no" ]; then
  kube -n fault-lab get pvc || true
fi

section "3/9 API HTTP（真实响应）"
if port_forward alertflow alertflow-api 18080 8080; then
  code="$(curl -s -o /tmp/alertflow-setup.json -w '%{http_code}' --max-time 5 http://127.0.0.1:18080/api/setup/status || true)"
  if [ "$code" = "200" ] && jq -e 'has("needsSetup")' /tmp/alertflow-setup.json >/dev/null 2>&1; then
    check_pass "GET /api/setup/status -> 200 $(cat /tmp/alertflow-setup.json)"
  else
    check_fail "GET /api/setup/status -> ${code:-无响应} body=$(cat /tmp/alertflow-setup.json 2>/dev/null || true)"
  fi
  # 受保护路由必须返回 401（证明 auth 中间件生效，不是全开放）
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:18080/api/services || true)"
  if [ "$code" = "401" ]; then
    check_pass "GET /api/services 未认证 -> 401"
  else
    check_fail "GET /api/services 未认证 -> ${code:-无响应}（期望 401）"
  fi
else
  check_fail "alertflow-api port-forward 失败"
fi

section "4/9 Agent HTTP + gRPC 9090/9000"
if port_forward alertflow alertflow-agent 18000 8000; then
  code="$(curl -s -o /tmp/alertflow-agent-health.json -w '%{http_code}' --max-time 5 http://127.0.0.1:18000/healthz || true)"
  if [ "$code" = "200" ] && jq -e '.ok == true' /tmp/alertflow-agent-health.json >/dev/null 2>&1; then
    check_pass "GET agent /healthz -> 200 $(cat /tmp/alertflow-agent-health.json)"
  else
    check_fail "GET agent /healthz -> ${code:-无响应} body=$(cat /tmp/alertflow-agent-health.json 2>/dev/null || true)"
  fi
else
  check_fail "alertflow-agent port-forward 失败"
fi

# 用 Agent 容器里的 grpcio 做真实 gRPC 握手（channel ready 需要 HTTP/2 + 服务端接受）。
if kube -n alertflow exec deploy/alertflow-agent -- python -c '
import grpc, sys
for target in ("alertflow-api.alertflow.svc:9090", "alertflow-agent.alertflow.svc:9000"):
    ch = grpc.insecure_channel(target)
    try:
        grpc.channel_ready_future(ch).result(timeout=5)
        print("GRPC_OK", target)
    except Exception as exc:
        print("GRPC_FAIL", target, exc)
        sys.exit(1)
' >/tmp/alertflow-grpc.log 2>&1; then
  check_pass "gRPC channel ready: $(grep -c GRPC_OK /tmp/alertflow-grpc.log) 个端点（api:9090, agent:9000）"
else
  check_fail "gRPC 握手失败：$(tr '\n' ' ' </tmp/alertflow-grpc.log)"
fi

section "5/9 跨 namespace Service DNS（从 Agent Pod 发起）"
if kube -n alertflow exec deploy/alertflow-agent -- python -c '
import json, urllib.request
for url in ("http://fault-lab.fault-lab.svc.cluster.local:8090/healthz",
            "http://alertflow-api.alertflow.svc.cluster.local:8080/api/setup/status"):
    try:
        with urllib.request.urlopen(url, timeout=5) as resp:
            print("DNS_OK", url, resp.status, resp.read(80).decode("utf-8", "replace"))
    except Exception as exc:
        print("DNS_FAIL", url, exc)
        raise SystemExit(1)
' >/tmp/alertflow-dns.log 2>&1; then
  check_pass "跨 namespace DNS 可达（fault-lab / alertflow-api）"
else
  check_fail "跨 namespace DNS 失败：$(tr '\n' ' ' </tmp/alertflow-dns.log)"
fi

section "6/9 PostgreSQL（pg_isready + 真实查询 + pgvector）"
if kube -n alertflow exec alertflow-postgres-0 -- pg_isready -U alertflow -d alertflow >/dev/null 2>&1; then
  check_pass "pg_isready -U alertflow -d alertflow"
else
  check_fail "pg_isready 失败"
fi
for db in alertflow alertflow_agent; do
  out="$(kube -n alertflow exec alertflow-postgres-0 -- psql -U alertflow -d "$db" -tAc 'select 1' 2>/dev/null | tr -d '[:space:]' || true)"
  if [ "$out" = "1" ]; then
    check_pass "psql ${db} select 1"
  else
    check_fail "psql ${db} 查询失败"
  fi
done
# pgvector 装在 alertflow_agent（Agent 长期记忆用）；控制面库 alertflow 不需要。
out="$(kube -n alertflow exec alertflow-postgres-0 -- psql -U alertflow -d alertflow_agent -tAc "select extversion from pg_extension where extname='vector'" 2>/dev/null | tr -d '[:space:]' || true)"
if [ -n "$out" ]; then
  check_pass "pgvector 扩展已安装于 alertflow_agent（v${out}）"
else
  check_fail "alertflow_agent 库缺少 pgvector 扩展（Agent 记忆会失败）"
fi

section "7/9 Kafka broker API + Redis PING"
if kube -n alertflow exec alertflow-kafka-0 -- /opt/kafka/bin/kafka-topics.sh --bootstrap-server alertflow-kafka.alertflow.svc:9092 --list >/tmp/alertflow-kafka.log 2>&1; then
  check_pass "Kafka broker API via Service DNS（topics: $(tr '\n' ',' </tmp/alertflow-kafka.log)）"
else
  check_fail "Kafka broker API 失败：$(tr '\n' ' ' </tmp/alertflow-kafka.log)"
fi
out="$(kube -n alertflow exec deploy/alertflow-redis -- redis-cli ping 2>/dev/null | tr -d '[:space:]' || true)"
if [ "$out" = "PONG" ]; then
  check_pass "redis-cli ping -> PONG"
else
  check_fail "redis-cli ping -> ${out:-无响应}"
fi

section "8/9 config.yaml 持久化（PVC 内文件存在且是 seed 内容）"
if kube -n alertflow get pvc alertflow-config -o jsonpath='{.status.phase}' | grep -qx Bound; then
  check_pass "PVC alertflow-config Bound"
else
  check_fail "PVC alertflow-config 未 Bound"
fi
if kube -n alertflow run "config-inspect-$RANDOM" --rm -i --restart=Never --image="$(lock_value init_image)" \
  --overrides='{"apiVersion":"v1","spec":{"containers":[{"name":"inspect","image":"alpine:3.19","command":["sh","-c","test -s /config/config.yaml && grep -q cluster_id /config/config.yaml && echo CONFIG_OK"],"volumeMounts":[{"name":"cfg","mountPath":"/config","readOnly":true}]}],"volumes":[{"name":"cfg","persistentVolumeClaim":{"claimName":"alertflow-config","readOnly":true}}]}}' \
  >/tmp/alertflow-config-inspect.log 2>&1; then
  check_pass "config PVC 上 config.yaml 存在且含 seed 的 cluster_id（seed 未被覆盖）"
else
  check_fail "config.yaml 检查失败：$(tr '\n' ' ' </tmp/alertflow-config-inspect.log)"
fi

section "9/9 RBAC 正反向 + Service endpoints"
rbac_check() {
  local verb="$1" res="$2" ns="$3" want="$4" got
  # kubectl auth can-i 对 "no" 的退出码是 1（这是合法答案，不是命令失败），
  # 所以先吞掉退出码再比较输出；空输出才视为 error。
  if [ "$ns" = "-" ]; then
    got="$(kube auth can-i "$verb" "$res" --as=system:serviceaccount:alertflow:alertflow-observer 2>/dev/null)" || true
  else
    got="$(kube auth can-i "$verb" "$res" -n "$ns" --as=system:serviceaccount:alertflow:alertflow-observer 2>/dev/null)" || true
  fi
  got="${got%%$'\n'*}"
  got="${got:-error}"
  if [ "$got" = "$want" ]; then
    check_pass "auth can-i ${verb} ${res} ns=${ns} -> ${got}"
  else
    check_fail "auth can-i ${verb} ${res} ns=${ns} -> ${got}（期望 ${want}）"
  fi
}
rbac_check list pods alertflow yes
rbac_check list deployments.apps alertflow yes
rbac_check list statefulsets.apps alertflow yes
rbac_check list pods fault-lab yes
rbac_check get secrets alertflow no
rbac_check create pods alertflow no
rbac_check list pods kube-system no
rbac_check list namespaces - no
rbac_check create clusterroles.rbac.authorization.k8s.io - no

for svc in alertflow-api alertflow-agent alertflow-postgres alertflow-kafka alertflow-redis; do
  addrs="$(kube -n alertflow get endpoints "$svc" -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null || true)"
  if [ -n "$addrs" ]; then
    check_pass "endpoints/${svc} 有后端（${addrs// /,}）"
  else
    check_fail "endpoints/${svc} 为空（Service selector 与 Pod label 不匹配？）"
  fi
done

section "结果"
if [ "$FAIL_COUNT" -eq 0 ]; then
  info "PASS：全部检查通过"
else
  die "FAIL：${FAIL_COUNT} 项检查失败"
fi
