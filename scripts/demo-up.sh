#!/usr/bin/env bash
# =============================================================================
# 一键起全套演示栈，并把「被监控服务 + Webhook 集成」预置好。
#
#   ./scripts/demo-up.sh              # 起 agent + demo 两个 profile
#   ADMIN_USER=admin ADMIN_PASS=*** ./scripts/demo-up.sh
#
# 前置：两个仓库并排（AlertFlow/ 与 fault-lab/），Docker + compose v2。
# 幂等：重复执行只会把配置改成期望值，不会重复创建。
# =============================================================================
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT="$(pwd)"
DOCKER_CONFIG="${DOCKER_CONFIG:-/tmp/dc_empty}"   # 绕过 Docker Desktop 凭据助手（WSL 常见）
AF="http://localhost:8080"
FL="http://localhost:8090"
SERVICE_URL="http://fault-lab:8090/health"
WEBHOOK_URL="http://agent:8000/webhooks/alertflow"
ADMIN_USER="${ADMIN_USER:-admin}"
ADMIN_PASS="${ADMIN_PASS:-}"

dc() { DOCKER_CONFIG="$DOCKER_CONFIG" docker compose --profile agent --profile demo "$@"; }

echo "==> 1/4 构建并启动（agent + demo）"
dc up -d --build

echo "==> 2/4 等待就绪"
for _ in $(seq 1 60); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' "$AF/api/setup/status" || true)" = "200" ] && \
  [ "$(curl -s -o /dev/null -w '%{http_code}' "$FL/healthz" || true)" = "200" ] && break
  sleep 2
done
echo "    AlertFlow $(curl -s -o /dev/null -w '%{http_code}' "$AF/api/setup/status") · Fault Lab $(curl -s -o /dev/null -w '%{http_code}' "$FL/healthz")"

echo "==> 3/4 预置配置"
if [ -z "$ADMIN_PASS" ]; then
  read -r -s -p "    AlertFlow 管理员密码（首次会用它创建 admin）: " ADMIN_PASS; echo
fi
COOKIE_JAR="$(mktemp)"; trap 'rm -f "$COOKIE_JAR"' EXIT

status="$(curl -s "$AF/api/setup/status")"
if echo "$status" | grep -q '"needsSetup":true'; then
  echo "    首次启动：创建管理员 $ADMIN_USER"
  curl -s -o /dev/null -X POST "$AF/api/setup" -H 'Content-Type: application/json' \
    -d "{\"username\":\"$ADMIN_USER\",\"password\":\"$ADMIN_PASS\"}"
fi
curl -s -o /dev/null -c "$COOKIE_JAR" -X POST "$AF/api/auth/login" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$ADMIN_USER\",\"password\":\"$ADMIN_PASS\"}" || true

# 被监控服务：存在则改 URL，不存在则创建
services="$(curl -s -b "$COOKIE_JAR" "$AF/api/services")"
if echo "$services" | grep -q '"id":"target"'; then
  curl -s -o /dev/null -b "$COOKIE_JAR" -X PUT "$AF/api/services/target" -H 'Content-Type: application/json' \
    -d "{\"name\":\"target\",\"url\":\"$SERVICE_URL\",\"interval\":30}"
  echo "    服务 target → $SERVICE_URL（已更新）"
else
  curl -s -o /dev/null -b "$COOKIE_JAR" -X POST "$AF/api/services" -H 'Content-Type: application/json' \
    -d "{\"name\":\"target\",\"url\":\"$SERVICE_URL\",\"interval\":30,\"mode\":\"name\"}"
  echo "    服务 target → $SERVICE_URL（已创建）"
fi

# Agent 诊断集成：类型必须是 agent_webhook。
# 诊断上下文只会发给这个专用类型；通用 webhook/slack/email 的载荷保持原样。
# 已存在的（旧版是普通 webhook）会被原地改类型，URL 与认证头一律保留 —— 改类型
# 走 PUT，不重建，所以连接的 id 与投递历史都留着。
INTEGRATIONS="$(curl -s -b "$COOKIE_JAR" "$AF/api/integrations")"
agent_seg="$(printf '%s' "$INTEGRATIONS" | tr '{' '\n' | grep '"name":"agent"' | head -1 || true)"
agent_id="$(printf '%s' "$agent_seg" | grep -o '"id":[0-9]*' | head -1 | cut -d: -f2)"
agent_type="$(printf '%s' "$agent_seg" | grep -o '"type":"[a-z_]*"' | head -1 | cut -d'"' -f4)"

if [ -z "$agent_id" ]; then
  curl -s -o /dev/null -b "$COOKIE_JAR" -X POST "$AF/api/integrations" -H 'Content-Type: application/json' \
    -d "{\"type\":\"agent_webhook\",\"name\":\"agent\",\"enabled\":true,\"notifyWarnings\":false,\"config\":{\"url\":\"$WEBHOOK_URL\",\"method\":\"POST\",\"headers\":{\"Authorization\":\"Bearer ${AGENT_WEBHOOK_TOKEN:-}\"}}}"
  echo "    集成 agent → $WEBHOOK_URL（已创建，类型 agent_webhook）"
else
  # config 留空 = 一个字段都不动；接口会按 agent_webhook 的要求重新校验。
  curl -s -o /dev/null -b "$COOKIE_JAR" -X PUT "$AF/api/integrations/$agent_id" -H 'Content-Type: application/json' \
    -d '{"type":"agent_webhook","name":"agent","enabled":true,"notifyWarnings":false,"config":{}}'
  if [ "$agent_type" = "agent_webhook" ]; then
    echo "    集成 agent（#$agent_id）已是 agent_webhook（已置为启用）"
  else
    echo "    集成 agent（#$agent_id）已从 $agent_type 迁移为 agent_webhook（URL 与认证头保留）"
  fi
fi

echo "==> 4/4 完成"
cat <<EOF

  AlertFlow 控制台 : $AF/
  事件与诊断       : $AF/incidents
  演示目标控制台   : $FL/   （token: ${SIMULATOR_CONTROL_TOKEN:-dev-control-token}）
  Agent API        : http://localhost:8000/
  场景目录         : $FL/scenarios

  下一步：在 AlertFlow 设置页填「诊断 Agent / 检索」的 LLM 与嵌入端点；
  然后在 Fault Lab 控制台切到 http_503，等一个检查周期即可看到告警与诊断报告。
EOF
