package kubernetes

import (
	"context"
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
)

func TestFakeClientRecordsMappedChangesOnly(t *testing.T) {
	replicas := int32(2)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "lab", UID: types.UID("deploy-1"), ResourceVersion: "1"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 2, ReadyReplicas: 2, AvailableReplicas: 2, UpdatedReplicas: 2},
	}
	deploy.Generation = 1
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "api-rs", Namespace: "lab", UID: types.UID("rs-1"),
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api", UID: types.UID("deploy-1")}},
	}}
	pod := readyPod("api-0", "pod-1", "rs-1")
	otherRS := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "other-rs", Namespace: "lab", UID: types.UID("rs-other"),
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "other", UID: types.UID("deploy-other")}},
	}}
	other := readyPod("other-0", "pod-other", "rs-other")
	warning := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "warn", Namespace: "lab", UID: types.UID("ev-1")},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "api-0", Namespace: "lab", UID: types.UID("pod-1")},
		Type:           corev1.EventTypeWarning, Reason: "Unhealthy", Message: "probe failed", Count: 2,
	}
	client := fake.NewClientset(deploy, rs, otherRS, pod, other, warning)
	database := db.OpenTest(t)
	obs := New(database, func(string) bool { return true })
	obs.client = client
	obs.Sync([]config.Service{mapped()})
	t.Cleanup(obs.Close)
	waitSynced(t, obs, "lab")
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.pod.first_seen'`); n != 1 {
		t.Fatalf("pod first_seen = %d", n)
	}
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.deployment.first_seen'`); n != 1 {
		t.Fatalf("deployment first_seen = %d", n)
	}
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE resource_name = 'other-0'`); n != 0 {
		t.Fatalf("unmapped pod events = %d", n)
	}
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.event'`); n != 1 {
		t.Fatalf("warning events = %d", n)
	}

	pod.ResourceVersion = "2"
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	obs.observePod(pod, true)
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.pod.changed' AND severity = 'warning'`); n != 1 {
		t.Fatalf("not ready = %d", n)
	}
	obs.observePod(pod, true)
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.pod.changed'`); n != 1 {
		t.Fatalf("repeated not ready = %d", n)
	}

	pod.ResourceVersion = "3"
	pod.Status.ContainerStatuses[0].RestartCount = 3
	obs.observePod(pod, true)
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.pod.changed'`); n != 2 {
		t.Fatalf("restart changes = %d", n)
	}

	deploy.ResourceVersion = "2"
	deploy.Status.AvailableReplicas = 1
	obs.observeDeployment(deploy, true)
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.deployment.changed'`); n != 1 {
		t.Fatalf("replica change = %d", n)
	}

	obs.observeEvent(warning)
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.event'`); n != 1 {
		t.Fatalf("repeated warning = %d", n)
	}

	again := New(database, func(string) bool { return true })
	again.client = client
	again.Sync([]config.Service{mapped()})
	t.Cleanup(again.Close)
	waitSynced(t, again, "lab")
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.pod.first_seen'`); n != 1 {
		t.Fatalf("restart rewrote first_seen = %d", n)
	}

	pod.ResourceVersion = "4"
	pod.Status.ContainerStatuses[0].RestartCount = 4
	restarted := New(database, func(string) bool { return true })
	restarted.client = client
	restarted.Sync([]config.Service{mapped()})
	t.Cleanup(restarted.Close)
	waitSynced(t, restarted, "lab")
	var accuracy string
	if err := database.QueryRow(
		`SELECT details->>'time_accuracy' FROM runtime_events
		 WHERE event_type = 'kubernetes.pod.changed' ORDER BY id DESC LIMIT 1`,
	).Scan(&accuracy); err != nil {
		t.Fatal(err)
	}
	if accuracy != "reconciled_after_gap" {
		t.Fatalf("accuracy = %s", accuracy)
	}
	if n := rows(t, database, `SELECT COUNT(*) FROM incidents`); n != 0 {
		t.Fatalf("incidents = %d", n)
	}
}

func TestWatchExpiryMarksAGap(t *testing.T) {
	database := db.OpenTest(t)
	obs := New(database, nil)
	obs.mu.Lock()
	obs.targets = []target{{ServiceID: "svc", ClusterID: "local", Namespace: "lab", Name: "api"}}
	obs.mu.Unlock()
	obs.onWatchError("lab", apierrors.NewResourceExpired("expired"))
	var status string
	if err := database.QueryRow(`SELECT status FROM runtime_source_state WHERE service_id = 'svc' AND source = 'kubernetes'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "gap" {
		t.Fatalf("status = %s", status)
	}
}

func TestConnectionFailureStaysUnavailable(t *testing.T) {
	database := db.OpenTest(t)
	obs := New(database, nil)
	obs.connErr = errConnected
	obs.Sync([]config.Service{mapped()})
	var status string
	if err := database.QueryRow(`SELECT status FROM runtime_source_state WHERE service_id = 'svc'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "unavailable" {
		t.Fatalf("status = %s", status)
	}
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events`); n != 0 {
		t.Fatalf("events = %d", n)
	}
}

var errConnected = errString("no configuration")

type errString string

func (e errString) Error() string { return string(e) }

func mapped() config.Service {
	return config.Service{
		ID: "svc", Name: "API", URL: "https://example.com",
		Kubernetes: config.ServiceKubernetes{
			ClusterID: "local", Namespace: "lab", WorkloadKind: "Deployment", WorkloadName: "api",
		},
	}
}

func readyPod(name, uid, rsUID string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "lab", UID: types.UID(uid),
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "rs", UID: types.UID(rsUID)}},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", RestartCount: 1, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}

func TestObjectsAddedAfterSyncAreFirstSeen(t *testing.T) {
	client := fake.NewClientset()
	database := db.OpenTest(t)
	obs := New(database, func(string) bool { return true })
	obs.client = client
	obs.Sync([]config.Service{mapped()})
	t.Cleanup(obs.Close)
	waitSynced(t, obs, "lab")
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type LIKE 'kubernetes.%.first_seen'`); n != 0 {
		t.Fatalf("initial events = %d", n)
	}

	replicas := int32(1)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "lab", UID: types.UID("deploy-new"), ResourceVersion: "1"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1},
	}
	if _, err := client.AppsV1().Deployments("lab").Create(context.Background(), deploy, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "api-rs", Namespace: "lab", UID: types.UID("rs-new"), ResourceVersion: "1",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api", UID: types.UID("deploy-new")}},
	}}
	if _, err := client.AppsV1().ReplicaSets("lab").Create(context.Background(), rs, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.deployment.first_seen'`, 1)
	pod := readyPod("api-1", "pod-new", "rs-new")
	pod.ResourceVersion = "1"
	if _, err := client.CoreV1().Pods("lab").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.pod.first_seen'`, 1)
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.pod.changed'`); n != 0 {
		t.Fatalf("add was stored as a live change: %d", n)
	}
}

func TestWatchExpiryReconcilesWithoutALiveChange(t *testing.T) {
	replicas := int32(1)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "lab", UID: types.UID("deploy-1"), ResourceVersion: "1"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1},
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "api-rs", Namespace: "lab", UID: types.UID("rs-1"),
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api", UID: types.UID("deploy-1")}},
	}}
	pod := readyPod("api-0", "pod-1", "rs-1")
	pod.ResourceVersion = "1"
	client := fake.NewClientset(deploy, rs, pod)
	database := db.OpenTest(t)
	obs := New(database, func(string) bool { return true })
	obs.client = client
	obs.Sync([]config.Service{mapped()})
	t.Cleanup(obs.Close)
	waitSynced(t, obs, "lab")

	stored, err := obs.group("lab").factory.Core().V1().Pods().Lister().Pods("lab").Get("api-0")
	if err != nil {
		t.Fatal(err)
	}
	stored.ResourceVersion = "2"
	stored.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	obs.onWatchError("lab", apierrors.NewResourceExpired("expired"))
	waitRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.pod.changed' AND details->>'time_accuracy' = 'reconciled_after_gap'`, 1)
	waitSource(t, database, "ok")
}

func TestRemovingTheMappingKeepsTimeline(t *testing.T) {
	replicas := int32(1)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "lab", UID: types.UID("deploy-1"), ResourceVersion: "1"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
	client := fake.NewClientset(deploy)
	database := db.OpenTest(t)
	obs := New(database, func(string) bool { return true })
	obs.client = client
	obs.Sync([]config.Service{mapped()})
	t.Cleanup(obs.Close)
	waitSynced(t, obs, "lab")
	waitRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.deployment.first_seen'`, 1)

	bare := mapped()
	bare.Kubernetes = config.ServiceKubernetes{}
	obs.Sync([]config.Service{bare})
	waitSource(t, database, "disabled")
	var active bool
	if err := database.QueryRow(`SELECT active FROM runtime_object_states WHERE resource_uid = 'deploy-1'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("object stayed active after the mapping was removed")
	}
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.deployment.inactive'`); n != 0 {
		t.Fatalf("unmapping wrote an inactive event: %d", n)
	}
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.deployment.first_seen'`); n != 1 {
		t.Fatalf("timeline = %d", n)
	}
}

func TestDisabledCollectionClosesTheSource(t *testing.T) {
	replicas := int32(1)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "lab", UID: types.UID("deploy-1"), ResourceVersion: "1"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	}
	client := fake.NewClientset(deploy)
	database := db.OpenTest(t)
	obs := New(database, func(string) bool { return true })
	obs.client = client
	obs.Sync([]config.Service{mapped()})
	t.Cleanup(obs.Close)
	waitSynced(t, obs, "lab")
	waitRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.deployment.first_seen'`, 1)

	obs.client = nil
	obs.connErr = nil
	obs.Sync([]config.Service{mapped()})
	waitSource(t, database, "disabled")
	var active bool
	var reason string
	if err := database.QueryRow(`SELECT active FROM runtime_object_states WHERE resource_uid = 'deploy-1'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("object stayed active after collection was disabled")
	}
	if err := database.QueryRow(`SELECT last_error_safe FROM runtime_source_state WHERE source = 'kubernetes'`).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "collection disabled" {
		t.Fatalf("reason = %q", reason)
	}
	if _, err := database.Exec(`UPDATE runtime_source_state SET updated_at = 1 WHERE source = 'kubernetes'`); err != nil {
		t.Fatal(err)
	}
	obs.Sync([]config.Service{mapped()})
	var updated int64
	if err := database.QueryRow(`SELECT updated_at FROM runtime_source_state WHERE source = 'kubernetes'`).Scan(&updated); err != nil {
		t.Fatal(err)
	}
	if updated != 1 {
		t.Fatalf("unchanged config rewrote the source at %d", updated)
	}
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.deployment.inactive'`); n != 0 {
		t.Fatalf("disabling wrote an inactive event: %d", n)
	}
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.deployment.first_seen'`); n != 1 {
		t.Fatalf("timeline = %d", n)
	}
}

func TestCacheSyncTimeoutDoesNotBlockReloadOrClose(t *testing.T) {
	database := db.OpenTest(t)
	client := fake.NewClientset()
	client.PrependReactor("*", "*", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("unreachable")
	})
	client.PrependWatchReactor("*", func(clienttesting.Action) (bool, watch.Interface, error) {
		return true, nil, apierrors.NewServiceUnavailable("unreachable")
	})
	obs := New(database, func(string) bool { return true })
	obs.client = client
	obs.syncTimeout = 200 * time.Millisecond
	started := time.Now()
	obs.Sync([]config.Service{mapped()})
	if time.Since(started) > time.Second {
		t.Fatal("sync waited for the cache")
	}
	waitSource(t, database, "unavailable")
	started = time.Now()
	obs.Close()
	if time.Since(started) > 2*time.Second {
		t.Fatal("close waited on the cache sync")
	}
}

func TestPermissionErrorMarksUnavailable(t *testing.T) {
	database := db.OpenTest(t)
	client := fake.NewClientset()
	denied := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "lab", errors.New("forbidden"))
	client.PrependReactor("*", "*", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, denied
	})
	client.PrependWatchReactor("*", func(clienttesting.Action) (bool, watch.Interface, error) {
		return true, nil, denied
	})
	obs := New(database, func(string) bool { return true })
	obs.client = client
	obs.syncTimeout = 200 * time.Millisecond
	obs.Sync([]config.Service{mapped()})
	t.Cleanup(obs.Close)
	waitSource(t, database, "unavailable")
}

func TestCloseWhileCacheSyncIsPending(t *testing.T) {
	database := db.OpenTest(t)
	client := fake.NewClientset()
	client.PrependReactor("*", "*", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("unreachable")
	})
	obs := New(database, func(string) bool { return true })
	obs.client = client
	obs.syncTimeout = 30 * time.Second
	obs.Sync([]config.Service{mapped()})
	started := time.Now()
	obs.Close()
	if time.Since(started) > 2*time.Second {
		t.Fatal("close blocked on cache sync")
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM runtime_source_state`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("shutdown wrote source rows = %d", n)
	}
}

func TestStatefulSetStaysOnItsOwnService(t *testing.T) {
	replicas := int32(1)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "lab", UID: "deploy-1", ResourceVersion: "1"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1, Replicas: 1, UpdatedReplicas: 1},
	}
	set := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "lab", UID: "set-1", ResourceVersion: "1"},
		Spec:       appsv1.StatefulSetSpec{Replicas: &replicas},
		Status:     appsv1.StatefulSetStatus{Replicas: 1, ReadyReplicas: 1, CurrentReplicas: 1, UpdatedReplicas: 1, CurrentRevision: "a", UpdateRevision: "a"},
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "api-rs", Namespace: "lab", UID: "rs-1",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api", UID: "deploy-1"}},
	}}
	deployPod := readyPod("api-pod", "pod-d", "rs-1")
	setPod := readyPod("api-0", "pod-s", "rs-1")
	setPod.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: "api", UID: "set-1"}}
	loose := readyPod("api-loose", "pod-loose", "rs-1")
	loose.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: "api"}}
	warning := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "set-warn", Namespace: "lab", UID: "ev-set"},
		InvolvedObject: corev1.ObjectReference{Kind: "StatefulSet", Name: "api", Namespace: "lab", UID: "set-1"},
		Type:           corev1.EventTypeWarning, Reason: "FailedCreate", Message: "create failed", Count: 1,
	}
	client := fake.NewClientset(deploy, set, rs, deployPod, setPod, loose, warning)
	database := db.OpenTest(t)
	obs := New(database, func(string) bool { return true })
	obs.client = client
	obs.SyncTargets([]config.Service{
		{ID: "alertflow", Name: "AlertFlow", URL: "https://example.com"},
		{ID: "fault-lab", Name: "Fault Lab", URL: "https://example.com"},
	}, config.KubernetesSettings{
		ClusterID: "local", Namespace: "lab",
		Targets: []config.KubernetesTarget{
			{ServiceID: "alertflow", Kind: "Deployment", Name: "api"},
			{ServiceID: "fault-lab", Kind: "StatefulSet", Name: "api"},
		},
	})
	t.Cleanup(obs.Close)
	waitSynced(t, obs, "lab")
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE service_id = 'alertflow' AND event_type = 'kubernetes.deployment.first_seen'`); n != 1 {
		t.Fatalf("deployment events = %d", n)
	}
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE service_id = 'fault-lab' AND event_type = 'kubernetes.statefulset.first_seen'`); n != 1 {
		t.Fatalf("statefulset events = %d", n)
	}
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE service_id = 'alertflow' AND resource_uid = 'pod-d'`); n != 1 {
		t.Fatalf("deployment pod = %d", n)
	}
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE service_id = 'fault-lab' AND resource_uid = 'pod-s'`); n != 1 {
		t.Fatalf("statefulset pod = %d", n)
	}
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE resource_uid = 'pod-loose'`); n != 0 {
		t.Fatalf("uid-less pod = %d", n)
	}
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE service_id = 'alertflow' AND resource_uid = 'pod-s'`); n != 0 {
		t.Fatalf("statefulset pod crossed services = %d", n)
	}
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE service_id = 'fault-lab' AND event_type = 'kubernetes.event'`); n != 1 {
		t.Fatalf("statefulset warning = %d", n)
	}
	set.ResourceVersion = "2"
	if written, _ := obs.observeStatefulSet(set, true); len(written) != 1 {
		t.Fatalf("same fingerprint written = %v", written)
	}
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.statefulset.first_seen' OR event_type = 'kubernetes.statefulset.changed'`); n != 1 {
		t.Fatalf("resourceVersion wrote a timeline row, count = %d", n)
	}
}

func TestObjectWriteFailureMarksAGap(t *testing.T) {
	database := db.OpenTest(t)
	obs := New(database, func(string) bool { return true })
	obs.mu.Lock()
	obs.targets = []target{{ServiceID: "svc", ClusterID: "local", Namespace: "lab", Kind: "Deployment", Name: "api"}}
	obs.mu.Unlock()
	written, failed := obs.observeDeployment(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "lab"},
	}, true)
	if len(written) != 0 || len(failed) != 1 {
		t.Fatalf("written %v failed %v", written, failed)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM runtime_source_state WHERE service_id = 'svc'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "gap" {
		t.Fatalf("status = %s", status)
	}
}

func TestSyncReconnectsAfterTheInitialFailure(t *testing.T) {
	t.Setenv("ALERTFLOW_K8S_ENABLED", "true")
	replicas := int32(1)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "lab", UID: "deploy-1", ResourceVersion: "1"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1},
	}
	client := fake.NewClientset(deploy)
	database := db.OpenTest(t)
	obs := New(database, func(string) bool { return true })
	calls := 0
	obs.dial = func() (clientset.Interface, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("down")
		}
		return client, nil
	}
	if err := obs.Connect(); err == nil {
		t.Fatal("first connect succeeded")
	}
	obs.Sync([]config.Service{mapped()})
	t.Cleanup(obs.Close)
	waitSynced(t, obs, "lab")
	if calls != 2 {
		t.Fatalf("dials = %d", calls)
	}
	if n := rows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.deployment.first_seen'`); n != 1 {
		t.Fatalf("first_seen = %d", n)
	}
}

func waitRows(t *testing.T, database *db.DB, query string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var n int
	for time.Now().Before(deadline) {
		n = rows(t, database, query)
		if n == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s = %d, want %d", query, n, want)
}

func waitSynced(t *testing.T, obs *Observer, ns string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if obs.namespaceSynced(ns) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cache did not sync")
}

func waitSource(t *testing.T, database *db.DB, status string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		err := database.QueryRow(`SELECT status FROM runtime_source_state WHERE service_id = 'svc' AND source = 'kubernetes'`).Scan(&got)
		if err == nil && got == status {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("status = %q, want %s", got, status)
}

func rows(t *testing.T, database *db.DB, query string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
