#!/usr/bin/env bash
# 创建（或复用）kind 集群 alertflow-lab，并准备好 namespace。
#
# 幂等：集群已存在时跳过创建，只重新导出 kubeconfig / 应用 namespace。
# 不做删除操作（删集群只能用 scripts/delete-cluster.sh 且需显式确认参数）。
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

require_tools docker kind kubectl
setup_context

section "1/4 节点镜像"
NODE_IMAGE="$(lock_value kind_node_image)"
NODE_SOURCE_IMAGE="$(lock_value kind_node_source_image)"
if "$DOCKER" image inspect "$NODE_IMAGE" >/dev/null 2>&1; then
  info "节点镜像已存在：$NODE_IMAGE"
else
  info "拉取 $NODE_SOURCE_IMAGE 并 retag 为 $NODE_IMAGE（Docker Hub 直连不通）"
  "$DOCKER" pull "$NODE_SOURCE_IMAGE"
  "$DOCKER" tag "$NODE_SOURCE_IMAGE" "$NODE_IMAGE"
fi

section "2/4 集群 $CLUSTER_NAME"
if "$KIND" get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  info "集群已存在，跳过创建（不删除、不重建）"
else
  info "创建单节点集群（config: overlays/local-kind/kind-config.yaml）"
  "$KIND" create cluster \
    --name "$CLUSTER_NAME" \
    --image "$NODE_IMAGE" \
    --config "$K8S_DIR/overlays/local-kind/kind-config.yaml" \
    --wait 180s
fi

section "3/4 kubeconfig"
"$KIND" export kubeconfig --name "$CLUSTER_NAME"
"$KUBECTL" config use-context "$KUBE_CONTEXT" >/dev/null
info "context=$KUBE_CONTEXT"
kube get nodes -o wide

section "4/4 namespace + 存储类"
apply_namespaces
kube get storageclass
kube get ns alertflow observability fault-lab runbook

# 记录 node 可分配资源，后续资源预算以它为准。
kube describe node "${CLUSTER_NAME}-control-plane" | sed -n '/Allocatable:/,/System Info:/p' | head -n 12

info "done: 集群就绪。下一步 scripts/build-and-load-images.sh → create-secrets.sh → deploy-core.sh"
