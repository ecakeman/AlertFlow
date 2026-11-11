package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/ecakeman/alertflow/backend/internal/system"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// K8sObserver 用 client-go 直接 Get Deployment/StatefulSet 做健康探测。
// 只读 Get，不使用 Informer / Watch，也不做本地缓存。
type K8sObserver struct {
	namespace string
	client    kubernetes.Interface
	initErr   error
}

// NewK8sObserver 初始化集群内客户端。启用但无法访问集群时保留错误，
// 采样会返回观察器错误而不是伪造业务故障。
func NewK8sObserver(enabled bool, namespace string, log *slog.Logger) *K8sObserver {
	if !enabled {
		return nil
	}
	if namespace == "" {
		namespace = "default"
	}
	o := &K8sObserver{namespace: namespace}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		o.initErr = fmt.Errorf("in-cluster config: %w", err)
		if log != nil {
			log.Error("kubernetes observer unavailable", "namespace", namespace, "err", err)
		}
		return o
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		o.initErr = fmt.Errorf("kubernetes client: %w", err)
		if log != nil {
			log.Error("kubernetes observer unavailable", "namespace", namespace, "err", err)
		}
		return o
	}
	o.client = client
	return o
}

// evidenceCondition 是 raw_result 中保留的重要 condition。
type evidenceCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// k8sEvidence 是写入 raw_result 的精简证据，不包含完整对象。
type k8sEvidence struct {
	Kind       string              `json:"kind"`
	Name       string              `json:"name"`
	Desired    int32               `json:"desired"`
	Ready      int32               `json:"ready"`
	Available  int32               `json:"available"`
	Updated    int32               `json:"updated"`
	Conditions []evidenceCondition `json:"conditions"`
	ObservedAt time.Time           `json:"observed_at"`
}

// Probe 执行一次 Kubernetes 采样。只有业务状态（含 404）返回采样结果，
// 观察器自身异常通过 error 返回，由调度器跳过写库。
func (o *K8sObserver) Probe(ctx context.Context, target system.DueTarget) (system.MonitorSample, error) {
	sample := system.MonitorSample{
		ServiceID: target.ServiceID, TargetID: target.TargetID, ObservedAt: time.Now().UTC(),
	}
	if o == nil || o.client == nil {
		if o != nil && o.initErr != nil {
			return sample, fmt.Errorf("kubernetes observer is not initialized: %w", o.initErr)
		}
		return sample, errors.New("kubernetes observer is not initialized")
	}
	cfg, err := system.ParseKubernetesTargetConfig(target.Config)
	if err != nil {
		return sample, fmt.Errorf("invalid kubernetes target config: %w", err)
	}
	callCtx := ctx
	if target.Timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, target.Timeout)
		defer cancel()
	}
	switch cfg.ResourceKind {
	case "Deployment":
		obj, err := o.client.AppsV1().Deployments(o.namespace).Get(callCtx, cfg.ResourceName, metav1.GetOptions{})
		if err != nil {
			return o.apiErrorSample(sample, cfg, err)
		}
		ev := EvaluateDeployment(obj, sample.ObservedAt)
		return sampleFromEvidence(sample, ev), nil
	case "StatefulSet":
		obj, err := o.client.AppsV1().StatefulSets(o.namespace).Get(callCtx, cfg.ResourceName, metav1.GetOptions{})
		if err != nil {
			return o.apiErrorSample(sample, cfg, err)
		}
		ev := EvaluateStatefulSet(obj, sample.ObservedAt)
		return sampleFromEvidence(sample, ev), nil
	default:
		return sample, fmt.Errorf("unsupported kubernetes resource_kind %q", cfg.ResourceKind)
	}
}

// apiErrorSample 把 404 映射为业务失败采样，其余归为观察器错误。
func (o *K8sObserver) apiErrorSample(sample system.MonitorSample, cfg system.KubernetesTargetConfig, err error) (system.MonitorSample, error) {
	if apierrors.IsNotFound(err) {
		ev := k8sEvidence{
			Kind: cfg.ResourceKind, Name: cfg.ResourceName,
			Conditions: []evidenceCondition{}, ObservedAt: sample.ObservedAt,
		}
		sample.Success = false
		sample.StatusClass = "unhealthy"
		sample.ErrorClass = "RESOURCE_NOT_FOUND"
		sample.Raw = mustEvidenceJSON(ev)
		sample.StateFingerprint = fingerprint(ev, false, "RESOURCE_NOT_FOUND")
		return sample, nil
	}
	return sample, fmt.Errorf("kubernetes get %s/%s: %w", cfg.ResourceKind, cfg.ResourceName, err)
}

func sampleFromEvidence(sample system.MonitorSample, ev k8sEvidence) system.MonitorSample {
	healthy, errorClass, statusClass := classifyEvidence(ev)
	sample.Success = healthy
	sample.StatusClass = statusClass
	sample.ErrorClass = errorClass
	sample.Raw = mustEvidenceJSON(ev)
	sample.StateFingerprint = fingerprint(ev, healthy, errorClass)
	return sample
}

func mustEvidenceJSON(ev k8sEvidence) []byte {
	if ev.Conditions == nil {
		ev.Conditions = []evidenceCondition{}
	}
	raw, _ := json.Marshal(ev)
	return raw
}

// EvaluateDeployment 判定 Deployment 健康状态，并返回诊断证据。
func EvaluateDeployment(dep *appsv1.Deployment, observedAt time.Time) k8sEvidence {
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	ev := k8sEvidence{
		Kind: "Deployment", Name: dep.Name, Desired: desired,
		Ready: dep.Status.ReadyReplicas, Available: dep.Status.AvailableReplicas,
		Updated: dep.Status.UpdatedReplicas, ObservedAt: observedAt,
	}
	for _, cond := range dep.Status.Conditions {
		ev.Conditions = append(ev.Conditions, evidenceCondition{
			Type: string(cond.Type), Status: string(cond.Status), Reason: cond.Reason,
		})
	}
	return ev
}

// EvaluateStatefulSet 判定 StatefulSet 健康状态，不比较 updatedReplicas。
func EvaluateStatefulSet(sts *appsv1.StatefulSet, observedAt time.Time) k8sEvidence {
	desired := int32(1)
	if sts.Spec.Replicas != nil {
		desired = *sts.Spec.Replicas
	}
	ev := k8sEvidence{
		Kind: "StatefulSet", Name: sts.Name, Desired: desired,
		Ready: sts.Status.ReadyReplicas, Available: sts.Status.AvailableReplicas,
		Updated: sts.Status.UpdatedReplicas, ObservedAt: observedAt,
	}
	for _, cond := range sts.Status.Conditions {
		ev.Conditions = append(ev.Conditions, evidenceCondition{
			Type: string(cond.Type), Status: string(cond.Status), Reason: cond.Reason,
		})
	}
	return ev
}

// classifyEvidence 把证据归一化为健康判定与诊断字段。
func classifyEvidence(ev k8sEvidence) (healthy bool, errorClass, statusClass string) {
	// desired==0（主动缩容到 0）不算异常，优先于 ProgressDeadlineExceeded 判定。
	if ev.Desired == 0 {
		return true, "", "healthy"
	}
	if ev.Kind == "Deployment" && progressDeadlineExceeded(ev.Conditions) {
		return false, "PROGRESS_DEADLINE_EXCEEDED", "unhealthy"
	}
	if ev.Kind == "StatefulSet" {
		if ev.Ready >= ev.Desired {
			return true, "", "healthy"
		}
		return false, "REPLICAS_UNAVAILABLE", "unhealthy"
	}
	if ev.Available >= ev.Desired {
		return true, "", "healthy"
	}
	return false, "REPLICAS_UNAVAILABLE", "unhealthy"
}

func progressDeadlineExceeded(conditions []evidenceCondition) bool {
	for _, cond := range conditions {
		if cond.Type == "Progressing" && cond.Status == "False" && cond.Reason == "ProgressDeadlineExceeded" {
			return true
		}
	}
	return false
}

// fingerprint 计算稳定状态指纹：排除 resourceVersion/UID/时间戳/labels 等高频字段。
func fingerprint(ev k8sEvidence, healthy bool, errorClass string) string {
	conds := append([]evidenceCondition(nil), ev.Conditions...)
	sort.Slice(conds, func(i, j int) bool { return conds[i].Type < conds[j].Type })
	body, _ := json.Marshal(struct {
		Kind       string              `json:"kind"`
		Name       string              `json:"name"`
		Desired    int32               `json:"desired"`
		Ready      int32               `json:"ready"`
		Available  int32               `json:"available"`
		Updated    int32               `json:"updated"`
		Healthy    bool                `json:"healthy"`
		ErrorClass string              `json:"error_class"`
		Conditions []evidenceCondition `json:"conditions"`
	}{ev.Kind, ev.Name, ev.Desired, ev.Ready, ev.Available, ev.Updated, healthy, errorClass, conds})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
