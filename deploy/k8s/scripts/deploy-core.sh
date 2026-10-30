#!/usr/bin/env bash
# 部署核心业务栈（namespace alertflow）：postgres / kafka / redis / api /
# monitor / publisher / consumer / delivery / agent + config PVC + observer RBAC。
#
# 前置：create-cluster.sh、build-and-load-images.sh、create-secrets.sh 已执行。
# 顺序：kustomize build → 静态校验（无 latest / 无 hostPath / 无明文 Secret）→
#        apply → observer-rbac → rollout → PVC Bound → Pods Ready → 资源记录。
#
# 不使用 `kubectl delete pvc`：普通重部署只 update；PVC 数据保留。
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

require_tools kubectl kustomize docker
require_context
apply_namespaces
require_ns alertflow
kube -n alertflow get secret alertflow-secrets >/dev/null 2>&1 \
  || die "缺少 Secret alertflow-secrets：先运行 scripts/create-secrets.sh"

OVERLAY="$K8S_DIR/overlays/local-kind"

section "1/6 渲染并静态校验清单"
RENDERED="$(mktemp)"
trap 'rm -f "$RENDERED"' EXIT
kustomize_build "$OVERLAY" > "$RENDERED"
info "资源数：$(grep -c '^kind:' "$RENDERED")"

# 版本/安全自检：这些规则在渲染结果上验证，而不是靠人肉 review。
if grep -Eq '^[[:space:]]*image:.*:latest([[:space:]]|$)' "$RENDERED"; then
  grep -En '^[[:space:]]*image:.*:latest([[:space:]]|$)' "$RENDERED" >&2
  die "渲染结果包含 :latest 镜像（禁止）"
fi
if grep -Eq '^[[:space:]]*hostPath:' "$RENDERED"; then
  die "渲染结果包含 hostPath 挂载（禁止）"
fi
if grep -Eq 'kind: Secret' "$RENDERED"; then
  die "渲染结果包含 Secret 对象（Secret 只能由 create-secrets.sh 从未跟踪文件生成）"
fi
if grep -Eq '^(data|stringData):' "$RENDERED" && grep -A3 -Eq '^(data|stringData):' "$RENDERED" | grep -q 'password:'; then
  die "渲染结果疑似包含明文密码字段"
fi
info "静态校验通过：无 :latest、无 hostPath、无内联 Secret"

# 失败即退：节点缺镜像（ImagePullBackOff）比 rollout 慢 180s 更早暴露。
assert_images_in_node "$RENDERED"

section "2/6 应用清单"
kustomize_apply "$OVERLAY"

info "应用 observer RBAC（alertflow + fault-lab 两个 namespace，最小只读）"
kube apply -f "$K8S_DIR/observer-rbac.yaml"

section "3/6 等待 Pod 健康（最多 180s；镜像/崩溃类错误立即失败）"
# 不把 apply 成功当成部署成功：ImagePullBackOff/ErrImagePull/CrashLoopBackOff 时
# 打印 describe 尾部并以非 0 退出，避免“看起来成功、实际全红”。
assert_pods_healthy alertflow 180 \
  || die "核心 Pod 未在 180s 内全部 Running/Ready（见上面的 describe 输出）"
info "9 个 Pod 均已 Running/Ready"

section "4/6 等待 StatefulSet / Deployment rollout"
for sts in alertflow-postgres alertflow-kafka; do
  wait_rollout statefulset "$sts" alertflow 600s
done
for dep in alertflow-redis alertflow-api alertflow-monitor alertflow-publisher alertflow-consumer alertflow-delivery alertflow-agent; do
  wait_rollout deployment "$dep" alertflow 300s
done

section "5/6 等待 PVC Bound 并复核"
wait_pvcs_bound alertflow 300s

section "5/6b 等待 Pod Ready"
wait_pods_ready alertflow 300s

section "6/6 汇总"
kube -n alertflow get deploy,statefulset,pods -o wide
kube -n alertflow get svc
kube -n alertflow get pvc
kube -n alertflow get endpoints

report_usage alertflow

info "done: 核心栈就绪。下一步 scripts/deploy-fault-lab.sh → scripts/smoke-test.sh"
info "提示：config.yaml 的 seed 只在文件不存在时写入；想恢复默认 seed 需先备份再删 config PVC（会丢 UI 修改）。"
