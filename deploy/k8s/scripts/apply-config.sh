#!/usr/bin/env bash
# 把仓库里的 seed（base/config/config-seed.yaml 的 data.config.yaml）同步到
# PVC alertflow-config 上的 config.yaml。
#
# 背景：config.yaml 是权威配置（UI 可写），所以 API Pod 的 initContainer 只在文件
# **不存在**时写 seed。改了仓库 seed 后，已有集群不会自动生效——用本脚本同步。
#
# 用法：
#   scripts/apply-config.sh                  # 只读比较：PVC 已有 config.yaml 时不覆盖，打印差异摘要
#   scripts/apply-config.sh --force          # 覆盖（先备份 config.yaml.bak.<时间戳>），并重启 API
#   scripts/apply-config.sh --force --no-restart   # 覆盖但不重启 API（不推荐：API 启动时读一次配置）
#
# 退出码：0 成功（含"未覆盖"分支）；非 0 失败即退（不吞 kubectl 错误）。
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

require_tools kubectl kustomize
setup_context
require_ns alertflow

FORCE=no
RESTART=yes
for arg in "$@"; do
  case "$arg" in
    --force) FORCE=yes ;;
    --no-restart) RESTART=no ;;
    -h|--help) sed -n '2,17p' "$0"; exit 0 ;;
    *) die "未知参数：$arg（支持 --force / --no-restart）" ;;
  esac
done

PVC_NAME="alertflow-config"
POD="apply-config-$RANDOM-$$"
TS="$(date +%Y%m%d-%H%M%S)"
SEED_FILE="$(mktemp)"
CUR_FILE="$(mktemp)"
trap 'rm -f "$SEED_FILE" "$CUR_FILE"; "$KUBECTL" --context "$KUBE_CONTEXT" -n alertflow delete pod "$POD" --ignore-not-found --wait=false >/dev/null 2>&1 || true' EXIT

# seed 来源 = 仓库文件（不是集群里的旧 ConfigMap），这样改了 seed 立刻可用。
"$KUBECTL" create -f "$K8S_DIR/base/config/config-seed.yaml" \
  --dry-run=client -o jsonpath='{.data.config\.yaml}' > "$SEED_FILE"
[ -s "$SEED_FILE" ] || die "无法从 base/config/config-seed.yaml 提取 config.yaml 内容"

section "1/4 启动一次性 Pod 读取 PVC $PVC_NAME"
kube -n alertflow apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${POD}
  namespace: alertflow
  labels:
    app.kubernetes.io/name: alertflow
    app.kubernetes.io/component: config-maintenance
spec:
  restartPolicy: Never
  containers:
    - name: maint
      image: $(lock_value init_image)
      imagePullPolicy: IfNotPresent
      # PVC 内文件属主是 10001；用 root 才能备份/改名，不改文件属主。
      securityContext: {runAsUser: 0, runAsGroup: 0}
      command: ["sh", "-c", "sleep 300"]
      volumeMounts:
        - {name: config, mountPath: /config}
  volumes:
    - name: config
      persistentVolumeClaim: {claimName: ${PVC_NAME}}
EOF
kube -n alertflow wait --for=condition=Ready "pod/$POD" --timeout=60s >/dev/null \
  || die "一次性 Pod $POD 未 Ready（PVC 是否被其他 Pod 以只读方式占用？）"

section "2/4 对比 seed 与 PVC 上的现有文件"
HAS_CUR=no
if kube -n alertflow exec "$POD" -- test -s /config/config.yaml >/dev/null 2>&1; then
  HAS_CUR=yes
  kube -n alertflow exec "$POD" -- cat /config/config.yaml > "$CUR_FILE"
  CUR_IDS="$(grep -c '^  - id:' "$CUR_FILE" || true)"
  CUR_BYTES="$(wc -c < "$CUR_FILE" | tr -d ' ')"
  SEED_IDS="$(grep -c '^  - id:' "$SEED_FILE" || true)"
  info "PVC config.yaml：${CUR_BYTES} bytes，services=${CUR_IDS}；seed services=${SEED_IDS}"
  if diff -q "$CUR_FILE" "$SEED_FILE" >/dev/null 2>&1; then
    info "内容与 seed 完全一致，无需同步"
    if [ "$FORCE" != "yes" ]; then
      section "3/4 跳过（默认不覆盖）"
      exit 0
    fi
    info "--force 指定，但仍会备份并重写（保持操作可追溯）"
  else
    ADDED=$(diff -u "$CUR_FILE" "$SEED_FILE" | grep -c '^+[^+]' || true)
    REMOVED=$(diff -u "$CUR_FILE" "$SEED_FILE" | grep -c '^-[^-]' || true)
    warn "seed 与现有文件不同：将新增 ${ADDED} 行 / 删除 ${REMOVED} 行"
    diff -u "$CUR_FILE" "$SEED_FILE" | head -40 || true
  fi
else
  info "PVC 上不存在 config.yaml（将被首次写入）"
fi

if [ "$FORCE" != "yes" ] && [ "$HAS_CUR" = "yes" ]; then
  section "3/4 未覆盖（如需以 seed 覆盖：scripts/apply-config.sh --force）"
  info "提示：UI/手工改过的 config.yaml 不会被本脚本默认重置。"
  exit 0
fi

section "3/4 写入 config.yaml（备份为 config.yaml.bak.${TS}）"
if [ "$HAS_CUR" = "yes" ]; then
  # 同目录 cp + mv，避免写入中断留下半个文件（与 Go 侧 Save 的原子 rename 语义一致）。
  kube -n alertflow exec "$POD" -- cp -p /config/config.yaml "/config/config.yaml.bak.${TS}"
  info "已备份 /config/config.yaml.bak.${TS}"
fi
kube -n alertflow exec -i "$POD" -- sh -c 'cat > /config/.config.yaml.new' < "$SEED_FILE"
kube -n alertflow exec "$POD" -- mv /config/.config.yaml.new /config/config.yaml
kube -n alertflow exec "$POD" -- chown 10001:10001 /config/config.yaml
NEW_BYTES="$(kube -n alertflow exec "$POD" -- sh -c 'wc -c < /config/config.yaml' | tr -d '[:space:]')"
NEW_IDS="$(kube -n alertflow exec "$POD" -- sh -c "grep -c '^  - id:' /config/config.yaml" || true)"
info "写入完成：${NEW_BYTES} bytes，services=${NEW_IDS}"
[ "$NEW_IDS" = "$(grep -c '^  - id:' "$SEED_FILE" || true)" ] \
  || die "写入后 services 数量与 seed 不一致（${NEW_IDS}）"
for sid in alertflow fault-lab; do
  kube -n alertflow exec "$POD" -- grep -q "id: ${sid}" /config/config.yaml \
    || die "写入后缺少服务条目 id=${sid}"
done
info "校验通过：alertflow、fault-lab 两个 service 条目都在"

section "4/4 让新配置生效"
if [ "$RESTART" = "yes" ]; then
  # monitor/delivery 每个调度周期重新读 config.yaml；API 只在启动时读一次，需重启。
  kube -n alertflow rollout restart deploy/alertflow-api
  kube -n alertflow rollout status deploy/alertflow-api --timeout=180s
  info "API 已重启并读取新配置（monitor/delivery 会自动重读）"
else
  warn "--no-restart：config.yaml 已更新，但 API 仍用旧配置，需自行 rollout restart deploy/alertflow-api"
fi

section "done"
info "查看服务列表：kubectl -n alertflow logs deploy/alertflow-monitor --tail=50 | grep -E 'fault-lab|service'"
