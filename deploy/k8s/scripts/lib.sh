#!/usr/bin/env bash
# AlertFlow K8s 部署脚本的公共函数库。
# 用法：`source "$(dirname "$0")/lib.sh"`。本文件不单独执行。
#
# 约定（规格 §9.9）：
#   - 所有脚本 set -euo pipefail，前置检查快速失败，输出当前 context/namespace；
#   - 可重复执行（幂等），失败即退，不吞 kubectl 错误；
#   - Secret 只从仓库外的本地文件读取，绝不 echo 值。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
K8S_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
REPO_ROOT="$(cd "$K8S_DIR/../.." && pwd)"
VERSIONS_FILE="$K8S_DIR/versions.lock.yaml"
KUBECONFIG_PATH="${KUBECONFIG:-$HOME/.kube/config}"
SECRETS_FILE="${ALERTFLOW_SECRETS_FILE:-$HOME/.config/alertflow/k8s-secrets.env}"

# --- 输出 -------------------------------------------------------------------
info() { printf '[info] %s\n' "$*"; }
warn() { printf '[warn] %s\n' "$*" >&2; }
die()  { printf '[fail] %s\n' "$*" >&2; exit 1; }
section() { printf '\n== %s ==\n' "$*"; }

# --- 工具定位（PATH 里可能没有 ~/bin）----------------------------------------
find_bin() {
  local name="$1"
  if [ -x "$HOME/bin/$name" ]; then printf '%s\n' "$HOME/bin/$name"; return 0; fi
  if command -v "$name" >/dev/null 2>&1; then command -v "$name"; return 0; fi
  return 1
}

# require_tools docker kind kubectl kustomize
# 只要求传入的工具存在；kind/kubectl/kustomize 会赋值给 KIND/KUBECTL/KUSTOMIZE。
require_tools() {
  local name path
  for name in "$@"; do
    path="$(find_bin "$name" || true)"
    [ -n "$path" ] || die "找不到 $name：请安装到 ~/bin 或加入 PATH（见 README 前置条件）"
    case "$name" in
      kind) KIND="$path" ;;
      kubectl) KUBECTL="$path" ;;
      kustomize) KUSTOMIZE="$path" ;;
      docker) DOCKER="$path" ;;
    esac
    info "$name -> $path"
  done
}

# --- versions.lock.yaml（单行 key: value）------------------------------------
lock_value() {
  local key="$1" value
  value="$(sed -n "s/^${key}:[[:space:]]*//p" "$VERSIONS_FILE" | head -n1)"
  value="${value%\"}"; value="${value#\"}"
  [ -n "$value" ] || die "versions.lock.yaml 缺少键 $key"
  printf '%s\n' "$value"
}

# --- 集群上下文 --------------------------------------------------------------
setup_context() {
  CLUSTER_NAME="$(lock_value cluster_name)"
  KUBE_CONTEXT="kind-${CLUSTER_NAME}"
  NAMESPACE="${NAMESPACE:-alertflow}"
}

require_context() {
  setup_context
  "$KUBECTL" config get-contexts -o name | grep -qx "$KUBE_CONTEXT" \
    || die "找不到 context ${KUBE_CONTEXT}：先运行 scripts/create-cluster.sh"
  local current
  current="$("$KUBECTL" config current-context 2>/dev/null || true)"
  info "context=${KUBE_CONTEXT}（当前 ${current:-?}），namespace=${NAMESPACE}"
}

# 统一加 --context 的 kubectl，避免误打到别的集群。
kube() {
  "$KUBECTL" --context "$KUBE_CONTEXT" "$@"
}

require_ns() {
  local ns="${1:-$NAMESPACE}"
  kube get namespace "$ns" >/dev/null 2>&1 || die "namespace ${ns} 不存在：先创建集群/应用 namespaces"
}

apply_namespaces() {
  kube apply -f "$K8S_DIR/base/namespaces/namespaces.yaml"
}

# --- 镜像导入 ---------------------------------------------------------------
# 节点内的 containerd 不能联网拉取（代理指向 127.0.0.1:7897），所有镜像必须
# 从宿主导入；`kind load docker-image` 在本机对多平台镜像会报
# `content digest ... not found`，所以统一用 docker save | ctr import。
# 导入后镜像名是 docker.io/library/<name>:<tag>，清单里写短名即可解析。
# 串行导入（一次一张）+ 每张导入后打印 ctr 校验行；不要并行。
load_image_to_node() {
  local image="$1" ref matched
  # 已存在就跳过：重复导入同一 digest 虽幂等，但 6.6 GiB 节点上没必要冒内存风险。
  if verify_image_in_node "$image"; then
    info "节点已有 $image（ctr 校验命中，跳过导入）"
    return 0
  fi
  info "导入镜像 $image → ${CLUSTER_NAME}-control-plane"
  "$DOCKER" image inspect "$image" >/dev/null 2>&1 || die "宿主缺少镜像 $image"
  "$DOCKER" save "$image" | "$DOCKER" exec -i "${CLUSTER_NAME}-control-plane" \
    ctr -n k8s.io images import --platform linux/amd64 --snapshotter=overlayfs -
  ref="$(image_ref_in_node "$image")"
  matched="$("$DOCKER" exec "${CLUSTER_NAME}-control-plane" ctr -n k8s.io images ls -q 2>/dev/null \
    | grep -F "$ref" || true)"
  [ -n "$matched" ] || die "导入后节点 containerd 里仍找不到 $ref（$image）"
  info "节点内校验：ctr -n k8s.io images ls -q | grep ${ref} → ${matched}"
}

# 镜像在节点 containerd 里的规范引用名：短名补 docker.io/library/。
image_ref_in_node() {
  case "$1" in
    */*) printf 'docker.io/%s\n' "$1" ;;
    *)   printf 'docker.io/library/%s\n' "$1" ;;
  esac
}

# assert_images_in_node <rendered.yaml>：apply 前逐个校验渲染结果里的镜像是否已在
# kind 节点 containerd 中。节点不能联网拉取，缺镜像时立刻失败并提示导入命令，
# 而不是等 180s 的 ImagePullBackOff。
assert_images_in_node() {
  local rendered="$1" image missing=()
  while IFS= read -r image; do
    [ -n "$image" ] || continue
    verify_image_in_node "$image" || missing+=("$image")
  done < <(grep -E '^[[:space:]]+image: ' "$rendered" | sed -E 's/^[[:space:]]+image:[[:space:]]*//; s/^"//; s/"$//' | sort -u)
  if [ "${#missing[@]}" -gt 0 ]; then
    warn "节点 containerd 里缺少以下镜像（节点不能联网拉取，必须从宿主导入）："
    printf '  - %s\n' "${missing[@]}" >&2
    die "先执行 scripts/build-and-load-images.sh --load-only 补齐后再 deploy"
  fi
  info "镜像预检通过：渲染结果引用的镜像均已在节点内"
}

# verify_image_in_node <image>：按 containerd 里的规范名（docker.io/...）查一次。
verify_image_in_node() {
  local ref
  ref="$(image_ref_in_node "$1")"
  "$DOCKER" exec "${CLUSTER_NAME}-control-plane" ctr -n k8s.io images ls -q 2>/dev/null \
    | grep -qF "$ref"
}

# kustomize_build <overlay-dir> 输出到 stdout（供 diff/校验）
kustomize_build() {
  "$KUSTOMIZE" build "$1"
}

# kustomize_apply <overlay-dir>
kustomize_apply() {
  local dir="$1"
  info "kustomize build ${dir#"$K8S_DIR"/} | kubectl apply -f -"
  kustomize_build "$dir" | kube apply -f -
}

wait_rollout() {
  local kind="$1" name="$2" ns="${3:-$NAMESPACE}" timeout="${4:-300s}"
  kube -n "$ns" rollout status "${kind}/${name}" --timeout="$timeout"
}

# wait_pods_ready <ns> [timeout]：等待该 namespace 所有 Pod Ready。
wait_pods_ready() {
  local ns="${1:-$NAMESPACE}" timeout="${2:-300s}"
  kube -n "$ns" wait --for=condition=Ready pod --all --timeout="$timeout"
}

wait_pvcs_bound() {
  local ns="${1:-$NAMESPACE}" timeout="${2:-300s}"
  kube -n "$ns" wait --for=jsonpath='{.status.phase}'=Bound pvc --all --timeout="$timeout"
}

# assert_pods_healthy <ns> [timeout-seconds]
# 不把 “apply 成功” 当成 “部署成功”：等 Pod 全部 Running/Ready；一旦出现
# ImagePullBackOff/ErrImagePull/CrashLoopBackOff/CreateContainerConfigError 立即
# 打印 describe 尾部并返回非 0（由调用方 die）。
assert_pods_healthy() {
  local ns="${1:-$NAMESPACE}" timeout="${2:-180}" deadline=$((SECONDS + ${2:-180})) status bad pod
  while [ "$SECONDS" -lt "$deadline" ]; do
    status="$(kube -n "$ns" get pods --no-headers 2>/dev/null || true)"
    if [ -z "$status" ]; then sleep 3; continue; fi
    bad="$(printf '%s\n' "$status" | grep -E 'ImagePullBackOff|ErrImagePull|CrashLoopBackOff|InvalidImageName|CreateContainerConfigError' || true)"
    if [ -n "$bad" ]; then
      warn "检测到异常 Pod 状态："
      printf '%s\n' "$bad" >&2
      pod="$(printf '%s\n' "$bad" | awk '{print $1}' | head -n1)"
      kube -n "$ns" describe pod "$pod" 2>/dev/null | tail -n 30 >&2 || true
      return 1
    fi
    # READY 形如 1/1，且 STATUS=Running 才算健康。
    if printf '%s\n' "$status" | awk 'NF>=4 {split($2,a,"/"); if ($3 != "Running" || a[1] != a[2]) bad=1} END {exit bad?1:0}'; then
      return 0
    fi
    sleep 5
  done
  warn "${timeout}s 内 Pod 未全部 Running/Ready："
  kube -n "$ns" get pods -o wide >&2 || true
  return 1
}

# --- Secret 本地文件 ---------------------------------------------------------
# 只解析 KEY=VALUE，不 source（避免执行任意内容）。
secret_env_get() {
  local key="$1"
  [ -f "$SECRETS_FILE" ] || return 0
  sed -n "s/^${key}=//p" "$SECRETS_FILE" | head -n1
}

secret_env_set() {
  local key="$1" value="$2"
  mkdir -p "$(dirname "$SECRETS_FILE")"
  chmod 700 "$(dirname "$SECRETS_FILE")"
  [ -f "$SECRETS_FILE" ] || : > "$SECRETS_FILE"
  if grep -q "^${key}=" "$SECRETS_FILE"; then
    # 用临时文件替换，不把值放到命令行（避免 ps 泄漏）。
    awk -v k="$key" -v v="$value" 'BEGIN{FS=OFS="="} $1==k {print k "=" v; next} {print}' \
      "$SECRETS_FILE" > "$SECRETS_FILE.tmp"
    mv "$SECRETS_FILE.tmp" "$SECRETS_FILE"
  else
    printf '%s=%s\n' "$key" "$value" >> "$SECRETS_FILE"
  fi
  chmod 600 "$SECRETS_FILE"
}

gen_token() {
  # 48 个 hex 字符（192 bit），URL/DSN 安全。
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 24
  else
    head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

# kubectl_secret_has_key <name> <key> [ns]
kubectl_secret_has_key() {
  local name="$1" key="$2" ns="${3:-$NAMESPACE}"
  kube -n "$ns" get secret "$name" -o "jsonpath={.data.${key}}" 2>/dev/null | grep -q .
}

# 从集群读回已有值（避免重跑脚本时轮换 token 导致已运行 Pod 失联）。
kubectl_secret_read() {
  local name="$1" key="$2" ns="${3:-$NAMESPACE}"
  kube -n "$ns" get secret "$name" -o "jsonpath={.data.${key}}" 2>/dev/null | base64 -d 2>/dev/null || true
}

# count_fail 供 smoke-test 汇总
FAIL_COUNT=0
check_pass() { printf '[PASS] %s\n' "$*"; }
check_fail() { printf '[FAIL] %s\n' "$*" >&2; FAIL_COUNT=$((FAIL_COUNT + 1)); }
check_skip() { printf '[SKIP] %s\n' "$*"; }

# 需要 curl/jq 的脚本用
require_http_tools() {
  command -v curl >/dev/null 2>&1 || die "缺少 curl"
  command -v jq   >/dev/null 2>&1 || die "缺少 jq"
}

# 记录每步资源使用（规格/lead 要求：部署各阶段留证据）。
# metrics-server 在 kind 默认未安装，退化为 docker stats 看节点容器真实 RSS。
report_usage() {
  local ns="${1:-$NAMESPACE}"
  section "资源使用（${ns}）"
  if kube -n "$ns" top pods 2>/dev/null; then
    :
  else
    warn "kubectl top 不可用（kind 默认无 metrics-server），改用 docker stats"
    "$DOCKER" stats --no-stream --format 'table {{.Name}}\t{{.MemUsage}}\t{{.CPUPerc}}' 2>/dev/null \
      | grep -E 'NAME|alertflow-lab' || true
  fi
  kube -n "$ns" get pods -o wide 2>/dev/null || true
}

