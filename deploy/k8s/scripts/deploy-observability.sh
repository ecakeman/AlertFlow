#!/usr/bin/env bash
# 部署观测栈（namespace observability）。
#
# 边界：overlays/observability/**、deploy/observability*/** 的清单由
# observability-agent 维护；本脚本只负责“加载镜像 + kustomize apply + 等就绪”，
# 不修改其清单内容。观测栈镜像必须在 kind 节点里（kind 节点不能直连 Docker Hub）。
#
# 幂等：可重复执行；不删除 PVC。
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

require_tools kubectl kustomize docker kind
require_context
require_ns observability

OVERLAY="$K8S_DIR/overlays/observability"
[ -d "$OVERLAY" ] || die "缺少 $OVERLAY（应由 observability-agent 提供；本脚本不创建它）"

section "1/3 加载观测镜像到 kind 节点"
IMAGES=(
  "$(lock_value prometheus_image)"
  "$(lock_value blackbox_image)"
  "$(lock_value loki_image)"
  "$(lock_value alloy_image)"
  "$(lock_value prometheus_mcp_image)"
  "$(lock_value loki_mcp_image)"
)
for image in "${IMAGES[@]}"; do
  if "$DOCKER" image inspect "$image" >/dev/null 2>&1; then
    load_image_to_node "$image"
  else
    warn "本地缺少镜像 $image：若清单需要它会 ImagePullBackOff，请先 docker pull 后重跑"
  fi
done

section "2/3 应用观测清单"
kustomize_apply "$OVERLAY"

section "3/3 等待观测 Pod 就绪"
# 观测组件的 Deployment/StatefulSet/DaemonSet 名字由 observability-agent 定义，
# 这里不 hardcode 资源名，只等 Pod。
if kube -n observability wait --for=condition=Ready pod --all --timeout=420s; then
  info "观测栈就绪"
else
  warn "有 Pod 未 Ready，查看：kubectl -n observability get pods; kubectl -n observability describe pod <name>"
fi

kube -n observability get pods -o wide
kube -n observability get svc
report_usage observability
info "done"
