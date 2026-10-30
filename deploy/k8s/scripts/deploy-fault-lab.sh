#!/usr/bin/env bash
# 部署 Fault Lab（namespace fault-lab）：被监控的演示服务 + http_503 注入点。
#
# 镜像来自 sibling repo（默认 $HOME/projects/fault-lab），由
# build-and-load-images.sh 构建后 kind load；本脚本不构建 Python 项目。
#
# 幂等：可重复执行；不删除 PVC（SIMULATOR_CONFIG_PATH 的状态会保留）。
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

require_tools kubectl kustomize docker kind
require_context
require_ns fault-lab

FAULT_LAB_IMAGE="$(lock_value fault_lab_image)"
"$DOCKER" image inspect "$FAULT_LAB_IMAGE" >/dev/null 2>&1 \
  || die "本地缺少 $FAULT_LAB_IMAGE：先运行 scripts/build-and-load-images.sh --with-fault-lab"

kube -n fault-lab get secret fault-lab-secrets >/dev/null 2>&1 \
  || die "缺少 Secret fault-lab/fault-lab-secrets：先运行 scripts/create-secrets.sh"

section "1/4 确保镜像在 kind 节点"
load_image_to_node "$FAULT_LAB_IMAGE"

section "2/4 应用清单"
OVERLAY="$K8S_DIR/overlays/fault-lab"
kustomize_apply "$OVERLAY"

section "3/4 确保 monitor 在 fault-lab 有只读权限"
# 幂等；若 core 已应用过则无变化。
kube apply -f "$K8S_DIR/observer-rbac.yaml"

section "4/4 等待就绪"
wait_rollout deployment fault-lab fault-lab 180s
kube -n fault-lab get deploy,pods -o wide
kube -n fault-lab get svc,pvc

info "done。Fault Lab DNS: fault-lab.fault-lab.svc.cluster.local:8090 ; /health 是注入目标，/healthz 是自身存活"
info "注入 503：scripts/inject-fault.sh ; 恢复：scripts/restore-fault.sh"
