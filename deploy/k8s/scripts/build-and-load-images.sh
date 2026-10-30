#!/usr/bin/env bash
# 构建 AlertFlow 的本地镜像并导入 kind 节点。
#
# 背景（本机实测）：
#   - kind 节点的 containerd 不能联网拉取镜像（代理指向自身），所以所有镜像必须
#     从宿主 Docker 导入，清单统一 `imagePullPolicy: IfNotPresent`。
#   - `kind load docker-image` 对多平台镜像会报 `content digest ... not found`，
#     因此统一用 `docker save | docker exec -i <node> ctr -n k8s.io images import`。
#   - 导入必须**串行**（一次一张，导入后立即校验），并行导入在 6.6 GiB 节点上会
#     触发 containerd 崩溃。
#
# 用法：
#   scripts/build-and-load-images.sh                    # 构建 + 导入（默认，auto 探测 fault-lab）
#   scripts/build-and-load-images.sh --without-fault-lab
#   scripts/build-and-load-images.sh --build-only       # 只构建，不碰集群（集群不可用时用）
#   scripts/build-and-load-images.sh --load-only        # 只导入（集群重建后；不重复 build）
#
# 导入清单（缺一个就会 ImagePullBackOff / PVC ProvisioningFailed）：
#   alertflow:local、alertflow-agent:local、fault-lab:local（可选）、
#   alpine:3.19（initContainer）、pgvector/pgvector:pg16、apache/kafka:3.9.1、
#   redis:7-alpine、kindest/local-path-helper:v20241212-8ac705d0（PVC 供应必需）。
#
# 幂等：可重复执行；NOT push：本脚本不推任何 registry。
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

WITH_FAULT_LAB=auto
MODE=all                      # all | build | load
for arg in "$@"; do
  case "$arg" in
    --with-fault-lab) WITH_FAULT_LAB=yes ;;
    --without-fault-lab) WITH_FAULT_LAB=no ;;
    --build-only) MODE=build ;;
    --load-only) MODE=load ;;
    -h|--help) sed -n '2,26p' "$0"; exit 0 ;;
    *) die "未知参数：$arg（支持 --with-fault-lab / --without-fault-lab / --build-only / --load-only）" ;;
  esac
done

require_tools docker
setup_context

APP_IMAGE="$(lock_value app_image)"
AGENT_IMAGE="$(lock_value agent_image)"
FAULT_LAB_IMAGE="$(lock_value fault_lab_image)"
INIT_IMAGE="$(lock_value init_image)"
RUNTIME_IMAGE="$(lock_value app_runtime_image)"
NODE_IMAGE="$(lock_value build_node_image)"
GO_IMAGE="$(lock_value build_go_image)"
PYTHON_IMAGE="$(lock_value build_python_image)"
LOCAL_PATH_HELPER_IMAGE="$(lock_value runtime_local_path_helper_image)"
LOCAL_PATH_HELPER_SOURCE="$(lock_value runtime_local_path_helper_source_image)"
FAULT_LAB_REPO="${FAULT_LAB_REPO:-$HOME/projects/fault-lab}"

# ---------- 构建 -------------------------------------------------------------
BUILD_FAULT_LAB=no
if [ "$MODE" != "load" ]; then
  section "0/4 基础镜像（构建期）"
  # distroless 用 digest 固定；本地没有则拉取（registry 不可达时快速报错）。
  "$DOCKER" image inspect "$RUNTIME_IMAGE" >/dev/null 2>&1 \
    || "$DOCKER" pull "$RUNTIME_IMAGE"
  "$DOCKER" image inspect "$INIT_IMAGE" >/dev/null 2>&1 \
    || "$DOCKER" pull "$INIT_IMAGE"

  section "1/4 构建 $APP_IMAGE（Go 控制面，单镜像 + ALERTFLOW_ROLE）"
  # GOPROXY/NPM_REGISTRY：本机直连 proxy.golang.org / npmjs 会超时（实测），
  # 默认走国内镜像；可用环境变量覆盖为空以回退官方源。
  "$DOCKER" build \
    --build-arg NODE_IMAGE="$NODE_IMAGE" \
    --build-arg GO_IMAGE="$GO_IMAGE" \
    --build-arg RUNTIME_IMAGE="$RUNTIME_IMAGE" \
    --build-arg NPM_REGISTRY="${NPM_REGISTRY:-https://registry.npmmirror.com}" \
    --build-arg GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" \
    -t "$APP_IMAGE" \
    "$REPO_ROOT"

  section "2/4 构建 $AGENT_IMAGE（Python Agent）"
  "$DOCKER" build \
    --build-arg PYTHON_IMAGE="$PYTHON_IMAGE" \
    -t "$AGENT_IMAGE" \
    "$REPO_ROOT/agent"

  section "3/4 Fault Lab"
  if [ "$WITH_FAULT_LAB" = "yes" ]; then
    BUILD_FAULT_LAB=yes
  elif [ "$WITH_FAULT_LAB" = "auto" ] && [ -f "$FAULT_LAB_REPO/Dockerfile" ]; then
    BUILD_FAULT_LAB=yes
  fi
  if [ "$BUILD_FAULT_LAB" = "yes" ]; then
    [ -f "$FAULT_LAB_REPO/Dockerfile" ] \
      || die "Fault Lab 仓库不存在：$FAULT_LAB_REPO（用 FAULT_LAB_REPO=... 指定，或 --without-fault-lab）"
    "$DOCKER" build -t "$FAULT_LAB_IMAGE" "$FAULT_LAB_REPO"
  else
    warn "跳过 Fault Lab 构建（overlays/fault-lab 需要 $FAULT_LAB_IMAGE，部署前必须补上）"
  fi
fi

if [ "$MODE" = "build" ]; then
  section "done（--build-only：未导入节点）"
  "$DOCKER" images --format 'table {{.Repository}}:{{.Tag}}\t{{.CreatedSince}}\t{{.Size}}' \
    | grep -E 'REPOSITORY|alertflow|fault-lab|alpine|pgvector|kafka|redis|local-path-helper' || true
  info "集群就绪后执行：scripts/build-and-load-images.sh --load-only"
  exit 0
fi

# ---------- 导入 -------------------------------------------------------------
# 节点不在线时明确失败，而不是部分导入后退出（避免“看起来成功”）。
require_tools kubectl
"$KUBECTL" --context "$KUBE_CONTEXT" wait --for=condition=Ready node --all --timeout=120s 2>/dev/null \
  || die "kind 节点未 Ready（Lead 可能正在重建集群）：稍后用 --load-only 重跑"

# 运行期镜像：本地缺失时先从 daocloud 镜像源拉取再 retag（Docker Hub 直连不通）。
ensure_local_image() {
  local image="$1" source="${2:-}"
  if "$DOCKER" image inspect "$image" >/dev/null 2>&1; then return 0; fi
  [ -n "$source" ] || die "宿主缺少镜像 $image 且没有配置镜像源（节点不能联网拉取）"
  info "本地缺少 $image，从镜像源拉取：$source"
  "$DOCKER" pull "$source"
  [ "$source" = "$image" ] || "$DOCKER" tag "$source" "$image"
}

section "4/4 串行导入 kind 节点（逐镜像 + 导入后校验）"
IMAGES_TO_LOAD=("$APP_IMAGE" "$AGENT_IMAGE" "$INIT_IMAGE")
if [ -f "$FAULT_LAB_REPO/Dockerfile" ] || "$DOCKER" image inspect "$FAULT_LAB_IMAGE" >/dev/null 2>&1; then
  IMAGES_TO_LOAD+=("$FAULT_LAB_IMAGE")
fi
for key in runtime_postgres_image runtime_kafka_image runtime_redis_image; do
  IMAGES_TO_LOAD+=("$(lock_value "$key")")
done
IMAGES_TO_LOAD+=("$LOCAL_PATH_HELPER_IMAGE")

for image in "${IMAGES_TO_LOAD[@]}"; do
  case "$image" in
    "$LOCAL_PATH_HELPER_IMAGE") ensure_local_image "$image" "$LOCAL_PATH_HELPER_SOURCE" ;;
    *) ensure_local_image "$image" ;;
  esac
  load_image_to_node "$image"
done

section "校验：节点内镜像（ctr images ls | grep）"
"$DOCKER" exec "${CLUSTER_NAME}-control-plane" ctr -n k8s.io images ls -q 2>/dev/null \
  | grep -E 'alertflow|fault-lab|alpine|pgvector|kafka|redis|local-path-helper' | sort

info "done。下一步 scripts/create-secrets.sh（Secret 已存在时幂等）与 scripts/deploy-core.sh"
