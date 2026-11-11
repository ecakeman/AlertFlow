package monitor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ecakeman/alertflow/backend/internal/system"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func int32Ptr(v int32) *int32 { return &v }

func testDeployment(replicas *int32, ready, available, updated int32, conditions ...appsv1.DeploymentCondition) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Replicas: replicas},
		Status: appsv1.DeploymentStatus{
			Replicas: ready + 1, ReadyReplicas: ready, AvailableReplicas: available,
			UpdatedReplicas: updated, Conditions: conditions,
		},
	}
}

func TestEvaluateDeploymentHealth(t *testing.T) {
	now := time.Now().UTC()
	healthy, class, status := classifyEvidence(EvaluateDeployment(testDeployment(int32Ptr(3), 3, 3, 3), now))
	if !healthy || class != "" || status != "healthy" {
		t.Fatalf("available==desired: %v %q %q", healthy, class, status)
	}
	ev := EvaluateDeployment(testDeployment(int32Ptr(3), 2, 2, 3), now)
	if healthy, class, _ := classifyEvidence(ev); healthy || class != "REPLICAS_UNAVAILABLE" {
		t.Fatalf("available<desired: %v %q", healthy, class)
	}
	// spec.replicas 缺省时按 1 处理。
	if ev := EvaluateDeployment(testDeployment(nil, 1, 1, 1), now); ev.Desired != 1 {
		t.Fatalf("default desired = %d", ev.Desired)
	}
	// 期望副本为 0 视为健康。
	if healthy, _, _ := classifyEvidence(EvaluateDeployment(testDeployment(int32Ptr(0), 0, 0, 0), now)); !healthy {
		t.Fatal("desired==0 must be healthy")
	}
	// Progressing=False 且 ProgressDeadlineExceeded 判为异常。
	cond := appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: "False", Reason: "ProgressDeadlineExceeded"}
	if healthy, class, _ := classifyEvidence(EvaluateDeployment(testDeployment(int32Ptr(3), 3, 3, 3, cond), now)); healthy || class != "PROGRESS_DEADLINE_EXCEEDED" {
		t.Fatalf("progress deadline: %v %q", healthy, class)
	}
}

func TestEvaluateStatefulSetHealth(t *testing.T) {
	now := time.Now().UTC()
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32Ptr(3)},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 3, UpdatedReplicas: 0},
	}
	if healthy, _, _ := classifyEvidence(EvaluateStatefulSet(sts, now)); !healthy {
		t.Fatal("ready==desired must be healthy, updatedReplicas is ignored")
	}
	sts.Status.ReadyReplicas = 2
	if healthy, class, _ := classifyEvidence(EvaluateStatefulSet(sts, now)); healthy || class != "REPLICAS_UNAVAILABLE" {
		t.Fatalf("ready<desired: %v %q", healthy, class)
	}
}

func TestStateFingerprintStability(t *testing.T) {
	base := time.Now().UTC()
	ev := k8sEvidence{Kind: "Deployment", Name: "web", Desired: 3, Ready: 2, Available: 2, Updated: 3,
		Conditions: []evidenceCondition{{Type: "Progressing", Status: "True"}}, ObservedAt: base}
	fp1 := fingerprint(ev, false, "REPLICAS_UNAVAILABLE")
	// 采集时间、resourceVersion 等不参与指纹。
	ev.ObservedAt = base.Add(time.Minute)
	if fp2 := fingerprint(ev, false, "REPLICAS_UNAVAILABLE"); fp1 != fp2 {
		t.Fatal("fingerprint must ignore collection time")
	}
	// 2/3 -> 3/3 必须改变指纹，仅这样能形成新的输入版本。
	ev.Ready, ev.Available = 3, 3
	if fp3 := fingerprint(ev, true, ""); fp3 == fp1 {
		t.Fatal("replica change must change fingerprint")
	}
}

func TestProbeDispatchesByType(t *testing.T) {
	ctx := context.Background()
	if _, err := Probe(ctx, system.DueTarget{TargetType: "ICMP"}, nil); err == nil {
		t.Fatal("unknown target type must not fall through to HTTP")
	}
	cfg, _ := json.Marshal(map[string]string{"resource_kind": "Deployment", "resource_name": "web"})
	if _, err := Probe(ctx, system.DueTarget{TargetType: "KUBERNETES", Config: cfg}, nil); err == nil {
		t.Fatal("kubernetes target without observer must error")
	}
}

func TestK8sProbeNotFound(t *testing.T) {
	obs := &K8sObserver{namespace: "default", client: fake.NewSimpleClientset()}
	cfg, _ := json.Marshal(map[string]string{"resource_kind": "Deployment", "resource_name": "missing"})
	sample, err := obs.Probe(context.Background(), system.DueTarget{
		TargetType: "KUBERNETES", Config: cfg, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sample.Success || sample.ErrorClass != "RESOURCE_NOT_FOUND" || sample.StateFingerprint == "" {
		t.Fatalf("not found sample = %+v", sample)
	}
}

func TestK8sProbeDeployment(t *testing.T) {
	dep := testDeployment(int32Ptr(3), 2, 2, 3)
	obs := &K8sObserver{namespace: "default", client: fake.NewSimpleClientset(dep)}
	cfg, _ := json.Marshal(map[string]string{"resource_kind": "Deployment", "resource_name": "web"})
	sample, err := obs.Probe(context.Background(), system.DueTarget{
		TargetType: "KUBERNETES", Config: cfg, Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sample.Success || sample.ErrorClass != "REPLICAS_UNAVAILABLE" || sample.StateFingerprint == "" {
		t.Fatalf("deployment sample = %+v", sample)
	}
	var raw map[string]any
	if err := json.Unmarshal(sample.Raw, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["kind"] != "Deployment" || raw["name"] != "web" || raw["desired"].(float64) != 3 {
		t.Fatalf("raw evidence = %s", sample.Raw)
	}
	if _, ok := raw["resourceVersion"]; ok {
		t.Fatal("raw evidence must not contain the full object")
	}
}
