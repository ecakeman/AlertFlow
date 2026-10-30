#!/usr/bin/env bash
# 删除本地 kind 集群。数据损失：PVC 由 kind local-path 提供，全部存放在 node
# 容器内，`kind delete cluster` 会一并删除，无法恢复。
#
# 安全设计（规格 §9.9）：必须显式传 --yes 且 --name 与真实集群名一致才执行；
# 不带参数只打印将要删除的内容。默认不做任何删除（本仓库不会自动执行本脚本）。
#
# 用法：scripts/delete-cluster.sh --yes --name alertflow-lab
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

require_tools kind kubectl
setup_context

CONFIRM=no
CONFIRM_NAME=""
for arg in "$@"; do
  case "$arg" in
    --yes) CONFIRM=yes ;;
    --name=*) CONFIRM_NAME="${arg#--name=}" ;;
    -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
    *) die "未知参数：$arg（仅支持 --yes 与 --name=<cluster>）" ;;
  esac
done

if [ "$CONFIRM" != "yes" ]; then
  section "预览：不会删除任何东西"
  printf '集群名        : %s\n' "$CLUSTER_NAME"
  [ -n "${CONFIRM_NAME:-}" ] && printf '--name 参数   : %s\n' "$CONFIRM_NAME"
  printf 'kind 将删除  : 名为 %s 的集群（含全部 namespace、Deployment、StatefulSet、PVC）\n' "$CLUSTER_NAME"
  printf 'PVC 数据损失 :\n'
  printf '  alertflow/alertflow-config        （config.yaml + images，UI 修改会丢）\n'
  printf '  alertflow/data-alertflow-postgres-0（两个 database: alertflow / alertflow_agent）\n'
  printf '  alertflow/data-alertflow-kafka-0  （Kafka 日志/元数据）\n'
  printf '  fault-lab/fault-lab-data          （simulator 状态 JSON）\n'
  printf '\n如确实要删除，请显式执行：\n  %s --yes --name=%s\n' "$0" "$CLUSTER_NAME"
  exit 0
fi

[ "${CONFIRM_NAME:-}" = "$CLUSTER_NAME" ] \
  || die "--yes 必须同时传 --name ${CLUSTER_NAME}（当前：${CONFIRM_NAME:-空}）。这是防误删的二次确认。"

section "删除前状态"
kube get nodes 2>/dev/null || true
kube get ns 2>/dev/null || true

section "删除 kind 集群 ${CLUSTER_NAME}"
"$KIND" delete cluster --name "$CLUSTER_NAME"

section "删除 kubeconfig context"
"$KUBECTL" config delete-context "kind-${CLUSTER_NAME}" 2>/dev/null || true
"$KUBECTL" config delete-cluster "kind-${CLUSTER_NAME}" 2>/dev/null || true
"$KUBECTL" config delete-user "kind-${CLUSTER_NAME}" 2>/dev/null || true

info "done：集群已删除，PVC 数据已随节点容器一起丢失。"
