package kubernetes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func podFingerprint(pod *corev1.Pod) (string, []byte) {
	ready := "unknown"
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			ready = string(cond.Status)
		}
	}
	owners := make([]string, 0, len(pod.OwnerReferences))
	for _, ref := range pod.OwnerReferences {
		owners = append(owners, ref.Kind+"/"+string(ref.UID))
	}
	sort.Strings(owners)
	var containers []string
	for _, cs := range pod.Status.ContainerStatuses {
		state, reason, code := containerState(cs.State)
		started, finished := containerTimes(cs.State)
		containers = append(containers, fmt.Sprintf("%s %d %s %s %d %s %s", cs.Name, cs.RestartCount, state, reason, code, started, finished))
	}
	sort.Strings(containers)
	deleted := ""
	if pod.DeletionTimestamp != nil {
		deleted = pod.DeletionTimestamp.Time.UTC().Format("2006-01-02T15:04:05Z")
	}
	canonical := strings.Join([]string{
		string(pod.Status.Phase),
		ready,
		pod.Spec.NodeName,
		deleted,
		strings.Join(owners, ","),
		strings.Join(containers, ","),
	}, "\n")
	raw, _ := json.Marshal(map[string]any{
		"phase": string(pod.Status.Phase), "ready": ready, "node": pod.Spec.NodeName,
		"deleted": deleted, "containers": containers,
	})
	return hash(canonical), raw
}

func deploymentFingerprint(deploy *appsv1.Deployment) (string, []byte) {
	var desired int32
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}
	var conditions []string
	for _, cond := range deploy.Status.Conditions {
		if cond.Type == appsv1.DeploymentProgressing || cond.Type == appsv1.DeploymentAvailable {
			conditions = append(conditions, fmt.Sprintf("%s %s %s", cond.Type, cond.Status, cond.Reason))
		}
	}
	sort.Strings(conditions)
	canonical := fmt.Sprintf("%d %d %d %d %d %d %d %s",
		deploy.Generation, deploy.Status.ObservedGeneration, desired,
		deploy.Status.Replicas, deploy.Status.ReadyReplicas, deploy.Status.AvailableReplicas,
		deploy.Status.UpdatedReplicas, strings.Join(conditions, ","),
	)
	raw, _ := json.Marshal(map[string]any{
		"generation": deploy.Generation, "observedGeneration": deploy.Status.ObservedGeneration,
		"replicas": desired, "readyReplicas": deploy.Status.ReadyReplicas,
		"availableReplicas": deploy.Status.AvailableReplicas, "updatedReplicas": deploy.Status.UpdatedReplicas,
		"conditions": conditions,
	})
	return hash(canonical), raw
}

func statefulSetFingerprint(set *appsv1.StatefulSet) (string, []byte) {
	var desired int32
	if set.Spec.Replicas != nil {
		desired = *set.Spec.Replicas
	}
	canonical := fmt.Sprintf("%d %d %d %d %d %d %d %s %s",
		set.Generation, set.Status.ObservedGeneration, desired,
		set.Status.Replicas, set.Status.ReadyReplicas, set.Status.CurrentReplicas,
		set.Status.UpdatedReplicas, set.Status.CurrentRevision, set.Status.UpdateRevision,
	)
	raw, _ := json.Marshal(map[string]any{
		"generation": set.Generation, "observedGeneration": set.Status.ObservedGeneration,
		"replicas": desired, "readyReplicas": set.Status.ReadyReplicas,
		"currentReplicas": set.Status.CurrentReplicas, "updatedReplicas": set.Status.UpdatedReplicas,
		"currentRevision": set.Status.CurrentRevision, "updateRevision": set.Status.UpdateRevision,
	})
	return hash(canonical), raw
}

func hash(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

func containerTimes(state corev1.ContainerState) (started, finished string) {
	const layout = "2006-01-02T15:04:05Z"
	switch {
	case state.Running != nil && !state.Running.StartedAt.IsZero():
		started = state.Running.StartedAt.Time.UTC().Format(layout)
	case state.Terminated != nil:
		if !state.Terminated.StartedAt.IsZero() {
			started = state.Terminated.StartedAt.Time.UTC().Format(layout)
		}
		if !state.Terminated.FinishedAt.IsZero() {
			finished = state.Terminated.FinishedAt.Time.UTC().Format(layout)
		}
	}
	return started, finished
}

func containerState(state corev1.ContainerState) (name, reason string, code int32) {
	switch {
	case state.Waiting != nil:
		return "waiting", state.Waiting.Reason, 0
	case state.Terminated != nil:
		return "terminated", state.Terminated.Reason, state.Terminated.ExitCode
	case state.Running != nil:
		return "running", "", 0
	default:
		return "unknown", "", 0
	}
}

func controllerIdentity(refs []metav1.OwnerReference, kind string) (string, types.UID, bool) {
	for _, ref := range refs {
		if ref.Kind == kind && ref.Name != "" && ref.UID != "" {
			return ref.Name, ref.UID, true
		}
	}
	return "", "", false
}

func podSeverity(pod *corev1.Pod) string {
	if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodPending {
		return "warning"
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status != corev1.ConditionTrue {
			return "warning"
		}
	}
	return "info"
}

func deploymentSeverity(deploy *appsv1.Deployment) string {
	var desired int32 = 1
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}
	if deploy.Status.AvailableReplicas < desired {
		return "warning"
	}
	return "info"
}

func statefulSetSeverity(set *appsv1.StatefulSet) string {
	var desired int32 = 1
	if set.Spec.Replicas != nil {
		desired = *set.Spec.Replicas
	}
	if set.Status.ReadyReplicas < desired {
		return "warning"
	}
	return "info"
}

func notableEvent(ev *corev1.Event) bool {
	if ev.Type == corev1.EventTypeWarning {
		return true
	}
	switch ev.Reason {
	case "BackOff", "FailedScheduling", "OOMKilled", "FailedMount":
		return true
	default:
		return false
	}
}
