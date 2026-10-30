#!/usr/bin/env bash
# 生成/更新 K8s Secret（从仓库外的本地文件读取，值不 echo、不落命令行）。
#
# 本地文件：$HOME/.config/alertflow/k8s-secrets.env（0600），可用
# ALERTFLOW_SECRETS_FILE 覆盖。模板见 base/secrets/secrets.env.example。
#
# 生成的 Secret：
#   alertflow-secrets（ns alertflow）：
#     postgres-password, database-url, agent-database-url,
#     agent-grpc-token, agent-internal-token, agent-task-grpc-token,
#     agent-api-token, agent-webhook-token,
#     llm-api-key, prometheus-mcp-token, loki-mcp-token, ragflow-api-key（可选）
#   fault-lab-secrets（ns fault-lab）：
#     control-token, diagnostics-token（可选）
#
# 幂等性：文件里已有值优先；文件为空但集群里已有 Secret 时读回集群值；
# 都没有才生成随机 token。重跑不会轮换 token，不会让已运行 Pod 失联。
#
# 注意：POSTGRES_PASSWORD 只在数据库首次初始化时生效。Postgres 已初始化后
# 改它需要进库 ALTER ROLE，否则连接失败（README「常见问题」有说明）。
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

require_tools kubectl
require_context
apply_namespaces

section "1/3 准备本地 Secret 文件"
mkdir -p "$(dirname "$SECRETS_FILE")"
chmod 700 "$(dirname "$SECRETS_FILE")"
if [ ! -f "$SECRETS_FILE" ]; then
  info "创建 $SECRETS_FILE（模板：base/secrets/secrets.env.example）"
  install -m 600 "$K8S_DIR/base/secrets/secrets.env.example" "$SECRETS_FILE"
fi
chmod 600 "$SECRETS_FILE"

# env 名 -> Secret key 映射（同名字段共享一个值）
REQUIRED_PAIRS=(
  "POSTGRES_PASSWORD:postgres-password"
  "AGENT_GRPC_TOKEN:agent-grpc-token"
  "AGENT_INTERNAL_TOKEN:agent-internal-token"
  "AGENT_TASK_GRPC_TOKEN:agent-task-grpc-token"
  "AGENT_API_TOKEN:agent-api-token"
  "AGENT_WEBHOOK_TOKEN:agent-webhook-token"
)
OPTIONAL_PAIRS=(
  "LLM_API_KEY:llm-api-key"
  "PROMETHEUS_MCP_TOKEN:prometheus-mcp-token"
  "LOKI_MCP_TOKEN:loki-mcp-token"
  "RAGFLOW_API_KEY:ragflow-api-key"
)

section "2/3 缺失值检查（只打印 key 名）"
declare -a SECRET_LINES=()
for pair in "${REQUIRED_PAIRS[@]}" "${OPTIONAL_PAIRS[@]}"; do
  env_key="${pair%%:*}"
  secret_key="${pair##*:}"
  value="$(secret_env_get "$env_key")"
  if [ -z "$value" ] && kubectl_secret_has_key alertflow-secrets "$secret_key" alertflow; then
    value="$(kubectl_secret_read alertflow-secrets "$secret_key" alertflow)"
    [ -n "$value" ] && secret_env_set "$env_key" "$value"
  fi
  if [ -z "$value" ]; then
    case " ${REQUIRED_PAIRS[*]} " in
      *"$pair"*) value="$(gen_token)"; secret_env_set "$env_key" "$value"; info "生成 $env_key" ;;
      *) info "跳过可选 $env_key（未配置）"; continue ;;
    esac
  fi
  SECRET_LINES+=("${secret_key}	${value}")
done

# 派生 DSN。密码是 hex，无需 URL 编码。
POSTGRES_PASSWORD="$(secret_env_get POSTGRES_PASSWORD)"
[ -n "$POSTGRES_PASSWORD" ] || die "POSTGRES_PASSWORD 为空"
SECRET_LINES+=("database-url	postgres://alertflow:${POSTGRES_PASSWORD}@alertflow-postgres.alertflow.svc:5432/alertflow?sslmode=disable")
SECRET_LINES+=("agent-database-url	postgres://alertflow:${POSTGRES_PASSWORD}@alertflow-postgres.alertflow.svc:5432/alertflow_agent?sslmode=disable")

write_secret() {
  local ns="$1" name="$2"
  shift 2
  local tmp rc=0
  tmp="$(mktemp)"
  chmod 600 "$tmp"
  {
    printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: %s\n  namespace: %s\ntype: Opaque\ndata:\n' "$name" "$ns"
    local line k v
    for line in "$@"; do
      k="${line%%	*}"
      v="${line#*	}"
      printf '  %s: %s\n' "$k" "$(printf '%s' "$v" | base64 | tr -d '\n')"
    done
  } > "$tmp"
  info "apply Secret ${ns}/${name}（keys: $(printf '%s\n' "$@" | cut -d'	' -f1 | paste -sd, -)）"
  kube apply -f "$tmp" || rc=$?
  rm -f "$tmp"
  [ "$rc" -eq 0 ] || die "apply Secret ${ns}/${name} 失败"
}

section "3/3 写入集群"
write_secret alertflow alertflow-secrets "${SECRET_LINES[@]}"

# Fault Lab 的控制 token：只给操作者（inject-fault/restore-fault 脚本），不进 Agent Pod。
FAULT_LAB_CONTROL_TOKEN="$(secret_env_get FAULT_LAB_CONTROL_TOKEN)"
if [ -z "$FAULT_LAB_CONTROL_TOKEN" ]; then
  if kubectl_secret_has_key fault-lab-secrets control-token fault-lab; then
    FAULT_LAB_CONTROL_TOKEN="$(kubectl_secret_read fault-lab-secrets control-token fault-lab)"
  else
    FAULT_LAB_CONTROL_TOKEN="$(gen_token)"
  fi
  secret_env_set FAULT_LAB_CONTROL_TOKEN "$FAULT_LAB_CONTROL_TOKEN"
fi
FAULT_LAB_SECRET_LINES=("control-token	${FAULT_LAB_CONTROL_TOKEN}")
FAULT_LAB_DIAGNOSTICS_TOKEN="$(secret_env_get FAULT_LAB_DIAGNOSTICS_TOKEN)"
if [ -z "$FAULT_LAB_DIAGNOSTICS_TOKEN" ] && kubectl_secret_has_key fault-lab-secrets diagnostics-token fault-lab; then
  FAULT_LAB_DIAGNOSTICS_TOKEN="$(kubectl_secret_read fault-lab-secrets diagnostics-token fault-lab)"
fi
[ -n "$FAULT_LAB_DIAGNOSTICS_TOKEN" ] && secret_env_set FAULT_LAB_DIAGNOSTICS_TOKEN "$FAULT_LAB_DIAGNOSTICS_TOKEN"
[ -n "$FAULT_LAB_DIAGNOSTICS_TOKEN" ] && FAULT_LAB_SECRET_LINES+=("diagnostics-token	${FAULT_LAB_DIAGNOSTICS_TOKEN}")
write_secret fault-lab fault-lab-secrets "${FAULT_LAB_SECRET_LINES[@]}"

# 非秘密的逐环境参数（LLM_BASE_URL/LLM_MODEL 等）写成**独立** ConfigMap
# alertflow-env-local：base 里的 alertflow-env 由 kustomize 管理，deploy-core 每次
# apply 都会把它的空占位重置回来；env-local 不被 kustomize 管理，因此能长期保留。
# Agent Deployment 用 envFrom 引用它（optional），优先级高于 envFrom alertflow-env。
NON_SECRET_KEYS="LLM_BASE_URL LLM_MODEL PROMETHEUS_MCP_URL LOKI_MCP_URL A2A_ADAPTER_URL RAGFLOW_BASE_URL RAGFLOW_AGENT_ID"
ENV_LOCAL_TMP="$(mktemp)"
trap 'rm -f "$ENV_LOCAL_TMP"' EXIT
chmod 600 "$ENV_LOCAL_TMP"
for env_key in $NON_SECRET_KEYS; do
  value="$(secret_env_get "$env_key")"
  [ -n "$value" ] && printf '%s=%s\n' "$env_key" "$value" >> "$ENV_LOCAL_TMP"
done
if [ -s "$ENV_LOCAL_TMP" ]; then
  info "写入 ConfigMap alertflow/alertflow-env-local（非秘密覆盖值，keys: $(cut -d= -f1 "$ENV_LOCAL_TMP" | paste -sd, -)）"
  kube -n alertflow create configmap alertflow-env-local \
    --from-env-file="$ENV_LOCAL_TMP" --dry-run=client -o yaml | kube apply -f -
else
  warn "本地文件里没有非秘密覆盖值（LLM_BASE_URL 等），Agent 将使用 ConfigMap 默认值"
fi

section "done"
info "Secret 已就绪。下一步 scripts/deploy-core.sh"
info "本地 Secret 文件：$SECRETS_FILE（0600，不在 repo 内，勿提交）"
