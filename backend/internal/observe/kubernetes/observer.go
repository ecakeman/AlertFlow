// Package kubernetes watches mapped Deployments and their Pods. It records
// semantic changes on the timeline and does not open incidents or call the
// Kubernetes API with a write verb.
package kubernetes

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
)

// Enabled reports whether ALERTFLOW_K8S_ENABLED is true or 1.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ALERTFLOW_K8S_ENABLED"))) {
	case "1", "true":
		return true
	default:
		return false
	}
}

type target struct {
	ServiceID string
	ClusterID string
	Namespace string
	Name      string
}

type group struct {
	factory informers.SharedInformerFactory
	cancel  context.CancelFunc
	done    chan struct{}
	again   chan struct{}
	synced  bool
}

// cacheSyncTimeout is how long the initial list/watch may take before the
// source is marked unavailable. The wait runs off the config loop.
const cacheSyncTimeout = 15 * time.Second

// Observer collects one cluster. A nil client means collection is off or the
// connection failed; Sync then only records unavailable.
type Observer struct {
	db      *db.DB
	present func(string) bool
	client  clientset.Interface
	connErr error

	lifecycle sync.Mutex
	mu        sync.Mutex
	targets   []target
	groups    map[string]*group
	// syncTimeout bounds one cache-sync attempt. Zero uses cacheSyncTimeout.
	syncTimeout time.Duration
}

func New(database *db.DB, present func(string) bool) *Observer {
	return &Observer{db: database, present: present, groups: map[string]*group{}}
}

// Connect builds a client from ALERTFLOW_KUBECONFIG or the in-cluster config.
// A failure is stored and does not stop the caller.
func (o *Observer) Connect() error {
	if o == nil || !Enabled() {
		return nil
	}
	cfg, err := restConfig()
	if err != nil {
		o.connErr = err
		return err
	}
	client, err := clientset.NewForConfig(cfg)
	if err != nil {
		o.connErr = err
		return err
	}
	o.client = client
	return nil
}

func restConfig() (*rest.Config, error) {
	if path := strings.TrimSpace(os.Getenv("ALERTFLOW_KUBECONFIG")); path != "" {
		return clientcmd.BuildConfigFromFlags("", path)
	}
	return rest.InClusterConfig()
}

// Sync keeps one informer set per namespace that has a mapped Deployment.
func (o *Observer) Sync(services []config.Service) {
	if o == nil {
		return
	}
	o.lifecycle.Lock()
	defer o.lifecycle.Unlock()
	next := targetsOf(services)
	if o.client == nil && o.connErr == nil {
		return
	}
	o.mu.Lock()
	prev := append([]target(nil), o.targets...)
	changed := !sameTargets(o.targets, next)
	o.targets = next
	o.mu.Unlock()
	if o.client == nil {
		msg := "kubernetes is not connected"
		if o.connErr != nil {
			msg = o.connErr.Error()
		}
		for _, item := range next {
			_ = o.db.SetKubernetesSource(item.ServiceID, "unavailable", clip(msg), o.allow(item.ServiceID))
		}
		o.releaseTargets(removedTargets(prev, next))
		return
	}

	wanted := map[string]bool{}
	var wait []*group
	for _, item := range next {
		wanted[item.Namespace] = true
		if o.group(item.Namespace) == nil {
			o.startNamespace(item.Namespace)
			continue
		}
		if changed {
			o.reconcileNamespace(item.Namespace)
		}
	}
	o.mu.Lock()
	for ns, g := range o.groups {
		if !wanted[ns] {
			delete(o.groups, ns)
			wait = append(wait, g)
		}
	}
	o.mu.Unlock()
	for _, g := range wait {
		g.cancel()
		<-g.done
	}
	o.releaseTargets(removedTargets(prev, next))
}

func (o *Observer) releaseTargets(items []target) {
	for _, item := range items {
		_ = o.db.ReleaseKubernetesMapping(item.ServiceID, item.ClusterID, item.Namespace, o.allow(item.ServiceID))
	}
}

// Close stops every informer and waits for it to leave its current callback.
func (o *Observer) Close() {
	if o == nil {
		return
	}
	o.lifecycle.Lock()
	defer o.lifecycle.Unlock()
	o.mu.Lock()
	groups := o.groups
	o.groups = map[string]*group{}
	o.mu.Unlock()
	for _, g := range groups {
		g.cancel()
	}
	for _, g := range groups {
		<-g.done
	}
}

func (o *Observer) startNamespace(ns string) {
	ctx, cancel := context.WithCancel(context.Background())
	factory := informers.NewSharedInformerFactoryWithOptions(o.client, 0, informers.WithNamespace(ns))
	g := &group{factory: factory, cancel: cancel, done: make(chan struct{}), again: make(chan struct{}, 1)}
	o.mu.Lock()
	o.groups[ns] = g
	o.mu.Unlock()
	pods := factory.Core().V1().Pods().Informer()
	deploys := factory.Apps().V1().Deployments().Informer()
	live := func() bool { return o.namespaceSynced(ns) }
	pods.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { o.onPod(obj, live()) },
		UpdateFunc: func(_, obj interface{}) { o.onPod(obj, live()) },
		DeleteFunc: func(obj interface{}) { o.onPodGone(obj) },
	})
	deploys.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { o.onDeployment(obj, live()) },
		UpdateFunc: func(_, obj interface{}) { o.onDeployment(obj, live()) },
		DeleteFunc: func(obj interface{}) { o.onDeploymentGone(obj) },
	})
	events := factory.Core().V1().Events().Informer()
	sets := factory.Apps().V1().ReplicaSets().Informer()
	events.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { o.onEvent(obj) },
		UpdateFunc: func(_, obj interface{}) { o.onEvent(obj) },
	})
	watchErr := func(_ *cache.Reflector, err error) { o.onWatchError(ns, err) }
	for _, inf := range []cache.SharedInformer{pods, deploys, events, sets} {
		inf.SetWatchErrorHandler(watchErr)
	}
	go func() {
		defer close(g.done)
		factory.Start(ctx.Done())
		o.syncThenReconcile(ctx, ns, factory)
		o.reconcileAfterErrors(ctx, ns, factory)
		<-ctx.Done()
		factory.Shutdown()
	}()
}

func (o *Observer) reconcileAfterErrors(ctx context.Context, ns string, factory informers.SharedInformerFactory) {
	g := o.group(ns)
	if g == nil {
		return
	}
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-g.again:
			if o.awaitCache(ctx, factory) && ctx.Err() == nil {
				o.listNamespace(ns)
			}
		}
	}
}

func (o *Observer) requestReconcile(ns string) {
	g := o.group(ns)
	if g == nil || g.again == nil {
		return
	}
	select {
	case g.again <- struct{}{}:
	default:
	}
}

func (o *Observer) cacheSyncTimeout() time.Duration {
	if o.syncTimeout > 0 {
		return o.syncTimeout
	}
	return cacheSyncTimeout
}

// syncThenReconcile waits for the informer cache off the config loop. A timed
// out or forbidden sync marks the source unavailable and tries again until the
// namespace context is cancelled.
func (o *Observer) syncThenReconcile(ctx context.Context, ns string, factory informers.SharedInformerFactory) {
	for ctx.Err() == nil {
		if o.awaitCache(ctx, factory) {
			o.listNamespace(ns)
			o.markSynced(ns)
			return
		}
		if ctx.Err() != nil {
			return
		}
		o.markNamespace(ns, "unavailable", "kubernetes cache sync timed out")
	}
}

func (o *Observer) awaitCache(ctx context.Context, factory informers.SharedInformerFactory) bool {
	waitCtx, cancel := context.WithTimeout(ctx, o.cacheSyncTimeout())
	defer cancel()
	result := factory.WaitForCacheSync(waitCtx.Done())
	if ctx.Err() != nil || len(result) == 0 {
		return false
	}
	for _, ok := range result {
		if !ok {
			return false
		}
	}
	return true
}

func (o *Observer) markSynced(ns string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if g := o.groups[ns]; g != nil {
		g.synced = true
	}
}

func (o *Observer) namespaceSynced(ns string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	g := o.groups[ns]
	return g != nil && g.synced
}

func (o *Observer) markNamespace(ns, status, msg string) {
	for _, item := range o.current(ns) {
		_ = o.db.SetKubernetesSource(item.ServiceID, status, clip(msg), o.allow(item.ServiceID))
	}
}

func (o *Observer) reconcileNamespace(ns string) {
	if !o.namespaceSynced(ns) {
		return
	}
	o.listNamespace(ns)
}

func (o *Observer) listNamespace(ns string) {
	g := o.group(ns)
	if g == nil {
		return
	}
	pods, err := g.factory.Core().V1().Pods().Lister().Pods(ns).List(labels.Everything())
	if err != nil {
		slog.Error("kubernetes: list pods", "namespace", ns, "error", err)
		return
	}
	deploys, err := g.factory.Apps().V1().Deployments().Lister().Deployments(ns).List(labels.Everything())
	if err != nil {
		slog.Error("kubernetes: list deployments", "namespace", ns, "error", err)
		return
	}
	events, err := g.factory.Core().V1().Events().Lister().Events(ns).List(labels.Everything())
	if err != nil {
		slog.Error("kubernetes: list events", "namespace", ns, "error", err)
		return
	}
	seenPod := map[string][]string{}
	seenDeploy := map[string][]string{}
	for _, item := range o.current(ns) {
		seenPod[item.ServiceID] = nil
		seenDeploy[item.ServiceID] = nil
	}
	for _, pod := range pods {
		for _, id := range o.observePod(pod, false) {
			seenPod[id] = append(seenPod[id], string(pod.UID))
		}
	}
	for _, deploy := range deploys {
		for _, id := range o.observeDeployment(deploy, false) {
			seenDeploy[id] = append(seenDeploy[id], string(deploy.UID))
		}
	}
	for _, ev := range events {
		o.observeEvent(ev)
	}
	for _, item := range o.current(ns) {
		_ = o.db.DeactivateUnseenKubernetes(item.ServiceID, item.ClusterID, ns, "Pod", seenPod[item.ServiceID], 0, o.allow(item.ServiceID))
		_ = o.db.DeactivateUnseenKubernetes(item.ServiceID, item.ClusterID, ns, "Deployment", seenDeploy[item.ServiceID], 0, o.allow(item.ServiceID))
		_ = o.db.SetKubernetesSource(item.ServiceID, "ok", "", o.allow(item.ServiceID))
	}
}

func (o *Observer) onPod(obj interface{}, live bool) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	o.observePod(pod, live)
}

func (o *Observer) onPodGone(obj interface{}) {
	pod, ok := tombstone(obj).(*corev1.Pod)
	if !ok {
		return
	}
	name, ok := deploymentOfPod(pod, o.replicaSets(pod.Namespace))
	if !ok {
		return
	}
	fp, snap := podFingerprint(pod)
	for _, item := range o.matching(pod.Namespace, name) {
		_ = o.db.ApplyKubernetesObject(db.KubernetesObject{
			ServiceID: item.ServiceID, ClusterID: item.ClusterID, Namespace: pod.Namespace,
			Kind: "Pod", Name: pod.Name, UID: string(pod.UID), Fingerprint: fp, Snapshot: snap,
			ResourceVersion: pod.ResourceVersion, Active: false, Live: true,
			Severity: "warning", Summary: pod.Name + " deleted",
		}, o.allow(item.ServiceID))
	}
}

func (o *Observer) observePod(pod *corev1.Pod, live bool) []string {
	name, ok := deploymentOfPod(pod, o.replicaSets(pod.Namespace))
	if !ok {
		slog.Debug("kubernetes: pod is not owned by a mapped deployment", "namespace", pod.Namespace, "pod", pod.Name)
		return nil
	}
	matches := o.matching(pod.Namespace, name)
	if len(matches) == 0 {
		slog.Debug("kubernetes: pod deployment is not mapped", "namespace", pod.Namespace, "deployment", name)
		return nil
	}
	fp, snap := podFingerprint(pod)
	ids := make([]string, 0, len(matches))
	for _, item := range matches {
		_ = o.db.ApplyKubernetesObject(db.KubernetesObject{
			ServiceID: item.ServiceID, ClusterID: item.ClusterID, Namespace: pod.Namespace,
			Kind: "Pod", Name: pod.Name, UID: string(pod.UID), Fingerprint: fp, Snapshot: snap,
			ResourceVersion: pod.ResourceVersion, Active: pod.DeletionTimestamp == nil, Live: live,
			Severity: podSeverity(pod), Summary: string(pod.Status.Phase),
		}, o.allow(item.ServiceID))
		ids = append(ids, item.ServiceID)
	}
	return ids
}

func (o *Observer) onDeployment(obj interface{}, live bool) {
	deploy, ok := obj.(*appsv1.Deployment)
	if !ok {
		return
	}
	o.observeDeployment(deploy, live)
}

func (o *Observer) onDeploymentGone(obj interface{}) {
	deploy, ok := tombstone(obj).(*appsv1.Deployment)
	if !ok {
		return
	}
	fp, snap := deploymentFingerprint(deploy)
	for _, item := range o.matching(deploy.Namespace, deploy.Name) {
		_ = o.db.ApplyKubernetesObject(db.KubernetesObject{
			ServiceID: item.ServiceID, ClusterID: item.ClusterID, Namespace: deploy.Namespace,
			Kind: "Deployment", Name: deploy.Name, UID: string(deploy.UID), Fingerprint: fp, Snapshot: snap,
			ResourceVersion: deploy.ResourceVersion, Active: false, Live: true,
			Severity: "warning", Summary: deploy.Name + " deleted",
		}, o.allow(item.ServiceID))
	}
}

func (o *Observer) observeDeployment(deploy *appsv1.Deployment, live bool) []string {
	matches := o.matching(deploy.Namespace, deploy.Name)
	if len(matches) == 0 {
		return nil
	}
	fp, snap := deploymentFingerprint(deploy)
	ids := make([]string, 0, len(matches))
	for _, item := range matches {
		_ = o.db.ApplyKubernetesObject(db.KubernetesObject{
			ServiceID: item.ServiceID, ClusterID: item.ClusterID, Namespace: deploy.Namespace,
			Kind: "Deployment", Name: deploy.Name, UID: string(deploy.UID), Fingerprint: fp, Snapshot: snap,
			ResourceVersion: deploy.ResourceVersion, Active: deploy.DeletionTimestamp == nil, Live: live,
			Severity: deploymentSeverity(deploy), Summary: deploy.Name,
		}, o.allow(item.ServiceID))
		ids = append(ids, item.ServiceID)
	}
	return ids
}

func (o *Observer) onEvent(obj interface{}) {
	ev, ok := obj.(*corev1.Event)
	if !ok {
		return
	}
	o.observeEvent(ev)
}

func (o *Observer) observeEvent(ev *corev1.Event) {
	if !notableEvent(ev) {
		return
	}
	workload, kind, uid, ok := o.involved(ev)
	if !ok {
		return
	}
	for _, item := range o.matching(ev.Namespace, workload) {
		count := ev.Count
		if count < 1 {
			count = 1
		}
		_ = o.db.ApplyKubernetesNotice(db.KubernetesNotice{
			ServiceID: item.ServiceID, ClusterID: item.ClusterID, Namespace: ev.Namespace,
			Kind: kind, Name: ev.InvolvedObject.Name, UID: string(uid), EventUID: string(ev.UID),
			Count: count, EventType: "kubernetes.event", Severity: "warning",
			Title: ev.Reason, Summary: ev.Message,
		}, o.allow(item.ServiceID))
	}
}

func (o *Observer) involved(ev *corev1.Event) (workload, kind string, uid types.UID, ok bool) {
	ref := ev.InvolvedObject
	g := o.group(ev.Namespace)
	if g == nil {
		return "", "", "", false
	}
	switch ref.Kind {
	case "Pod":
		pod, err := g.factory.Core().V1().Pods().Lister().Pods(ev.Namespace).Get(ref.Name)
		if err != nil {
			return "", "", "", false
		}
		name, found := deploymentOfPod(pod, o.replicaSets(ev.Namespace))
		return name, "Pod", pod.UID, found
	case "ReplicaSet":
		rs, err := g.factory.Apps().V1().ReplicaSets().Lister().ReplicaSets(ev.Namespace).Get(ref.Name)
		if err != nil {
			return "", "", "", false
		}
		name, found := controllerName(rs.OwnerReferences, "Deployment")
		return name, "ReplicaSet", rs.UID, found
	case "Deployment":
		return ref.Name, "Deployment", ref.UID, ref.Name != ""
	default:
		return "", "", "", false
	}
}

func (o *Observer) onWatchError(ns string, err error) {
	if err == nil {
		return
	}
	switch {
	case apierrors.IsResourceExpired(err) || apierrors.IsGone(err):
		slog.Warn("kubernetes: watch expired", "namespace", ns)
		o.markNamespace(ns, "gap", "watch expired")
		o.requestReconcile(ns)
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
		slog.Warn("kubernetes: permission denied", "namespace", ns)
		o.markNamespace(ns, "unavailable", "kubernetes permission denied")
	}
}

func (o *Observer) allow(serviceID string) func() bool {
	if o.present == nil {
		return nil
	}
	return func() bool { return o.present(serviceID) }
}

func (o *Observer) replicaSets(ns string) map[types.UID]*appsv1.ReplicaSet {
	g := o.group(ns)
	if g == nil {
		return nil
	}
	list, err := g.factory.Apps().V1().ReplicaSets().Lister().ReplicaSets(ns).List(labels.Everything())
	if err != nil {
		return nil
	}
	out := make(map[types.UID]*appsv1.ReplicaSet, len(list))
	for _, rs := range list {
		out[rs.UID] = rs
	}
	return out
}

func (o *Observer) group(ns string) *group {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.groups[ns]
}

func (o *Observer) current(ns string) []target {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []target
	for _, item := range o.targets {
		if item.Namespace == ns {
			out = append(out, item)
		}
	}
	return out
}

func (o *Observer) matching(ns, deployment string) []target {
	var out []target
	for _, item := range o.current(ns) {
		if item.Name == deployment {
			out = append(out, item)
		}
	}
	return out
}

func targetsOf(services []config.Service) []target {
	var out []target
	for _, svc := range services {
		k := svc.Kubernetes
		if k.ClusterID == "" || k.Namespace == "" || k.WorkloadName == "" || k.WorkloadKind != "Deployment" {
			continue
		}
		out = append(out, target{ServiceID: svc.ID, ClusterID: k.ClusterID, Namespace: k.Namespace, Name: k.WorkloadName})
	}
	return out
}

func removedTargets(prev, next []target) []target {
	keep := make(map[target]bool, len(next))
	for _, item := range next {
		keep[item] = true
	}
	var out []target
	for _, item := range prev {
		if !keep[item] {
			out = append(out, item)
		}
	}
	return out
}

func sameTargets(a, b []target) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func tombstone(obj interface{}) interface{} {
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		return tomb.Obj
	}
	return obj
}

func clip(msg string) string {
	if len(msg) > 200 {
		return msg[:200]
	}
	return msg
}
