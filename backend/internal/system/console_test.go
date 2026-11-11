package system

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func strPtr(v string) *string { return &v }
func intPtr(v int) *int       { return &v }

func TestKubernetesTargetConfigValidationAndDefaults(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	view, err := s.CreateService(ctx, CreateServiceInput{
		ServiceKey: "k8s-" + uuid.NewString()[:8], Name: "k8s", AuthorityProducerID: "ops",
		Targets: []TargetInput{{
			TargetKey: "web", TargetName: "web", TargetType: "KUBERNETES",
			Config:           json.RawMessage(`{"resource_kind":"Deployment","resource_name":"web"}`),
			FailureThreshold: 3, RecoveryThreshold: 2,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tg := view.Targets[0]
	if tg.Address != "" || tg.IntervalMS != 10000 || tg.TimeoutMS != 3000 {
		t.Fatalf("k8s defaults = %+v", tg)
	}
	cfg, err := ParseKubernetesTargetConfig(tg.Config)
	if err != nil || cfg.ResourceKind != "Deployment" || cfg.ResourceName != "web" {
		t.Fatalf("config = %+v %v", cfg, err)
	}

	cases := []struct {
		name   string
		target TargetInput
	}{
		{"address must be empty", TargetInput{TargetType: "KUBERNETES", Address: "http://web/health",
			Config: json.RawMessage(`{"resource_kind":"Deployment","resource_name":"web"}`)}},
		{"unsupported kind", TargetInput{TargetType: "KUBERNETES",
			Config: json.RawMessage(`{"resource_kind":"Pod","resource_name":"web"}`)}},
		{"missing name", TargetInput{TargetType: "KUBERNETES",
			Config: json.RawMessage(`{"resource_kind":"StatefulSet","resource_name":" "}`)}},
		{"unknown type", TargetInput{TargetType: "ICMP", Address: "127.0.0.1"}},
	}
	for _, tc := range cases {
		_, err := s.CreateService(ctx, CreateServiceInput{
			ServiceKey: "bad-" + uuid.NewString()[:8], Name: "bad", AuthorityProducerID: "ops",
			Targets: []TargetInput{{
				TargetKey: "t", TargetName: "t", TargetType: tc.target.TargetType,
				Address: tc.target.Address, Config: tc.target.Config,
				IntervalMS: 1000, TimeoutMS: 200, FailureThreshold: 1, RecoveryThreshold: 1,
			}},
		})
		if !IsCode(err, "VALIDATION") {
			t.Fatalf("%s: err = %v", tc.name, err)
		}
	}
}

func TestTargetCRUDAndSoftDelete(t *testing.T) {
	s := testStore(t)
	svc, _ := newSvc(t, s, 1, 1)
	ctx := context.Background()
	in := TargetInput{
		TargetKey: "extra", TargetName: "extra", TargetType: "HTTP", Address: "http://extra/health",
		IntervalMS: 1000, TimeoutMS: 200, FailureThreshold: 1, RecoveryThreshold: 1,
	}
	created, err := s.CreateTarget(ctx, svc, in)
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != StatusUnknown || created.LastCheckAt != nil {
		t.Fatalf("created = %+v", created)
	}
	updated, serviceID, err := s.UpdateTarget(ctx, created.TargetID, PatchTargetInput{
		TargetName: strPtr("renamed"), IntervalMS: intPtr(2000),
	})
	if err != nil || serviceID != svc || updated.TargetName != "renamed" || updated.IntervalMS != 2000 {
		t.Fatalf("updated = %+v svc=%s err=%v", updated, serviceID, err)
	}
	// 合并后按类型校验：HTTP 地址不能为空。
	if _, _, err := s.UpdateTarget(ctx, created.TargetID, PatchTargetInput{Address: strPtr("")}); !IsCode(err, "VALIDATION") {
		t.Fatalf("merge validation err = %v", err)
	}
	targets, err := s.GetTargets(ctx, svc)
	if err != nil || len(targets) != 2 {
		t.Fatalf("targets = %d %v", len(targets), err)
	}
	// 软删除后列表排除，且允许同一 target_key 重建。
	if sid, err := s.SoftDeleteTarget(ctx, created.TargetID); err != nil || sid != svc {
		t.Fatalf("delete = %s %v", sid, err)
	}
	targets, err = s.GetTargets(ctx, svc)
	if err != nil || len(targets) != 1 {
		t.Fatalf("after delete targets = %d %v", len(targets), err)
	}
	again, err := s.CreateTarget(ctx, svc, in)
	if err != nil || again.TargetID == created.TargetID {
		t.Fatalf("recreate = %+v %v", again, err)
	}
	if _, err := s.SoftDeleteTarget(ctx, created.TargetID); !IsCode(err, "TARGET_NOT_FOUND") {
		t.Fatalf("double delete err = %v", err)
	}
	if _, _, err := s.UpdateTarget(ctx, created.TargetID, PatchTargetInput{}); !IsCode(err, "TARGET_NOT_FOUND") {
		t.Fatalf("update deleted err = %v", err)
	}
	if _, err := s.GetTargets(ctx, uuid.New()); !IsCode(err, "SERVICE_NOT_FOUND") {
		t.Fatalf("unknown service err = %v", err)
	}
}

func TestListDueTargetsConfigAndSoftDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	view, err := s.CreateService(ctx, CreateServiceInput{
		ServiceKey: "k8s-" + uuid.NewString()[:8], Name: "k8s", AuthorityProducerID: "ops",
		Targets: []TargetInput{{
			TargetKey: "web", TargetName: "web", TargetType: "KUBERNETES",
			Config:           json.RawMessage(`{"resource_kind":"Deployment","resource_name":"web"}`),
			FailureThreshold: 1, RecoveryThreshold: 1,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	due, err := s.ListDueTargets(ctx)
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %d %v", len(due), err)
	}
	if due[0].Interval != 10*time.Second || due[0].Timeout != 3*time.Second {
		t.Fatalf("due interval/timeout = %s/%s", due[0].Interval, due[0].Timeout)
	}
	if !strings.Contains(string(due[0].Config), "resource_name") {
		t.Fatalf("due config = %s", due[0].Config)
	}
	if _, err := s.SoftDeleteTarget(ctx, view.Targets[0].TargetID); err != nil {
		t.Fatal(err)
	}
	if due, err = s.ListDueTargets(ctx); err != nil || len(due) != 0 {
		t.Fatalf("after target delete due = %d %v", len(due), err)
	}
	svc2, _ := newSvc(t, s, 1, 1)
	if err := s.SoftDeleteService(ctx, svc2); err != nil {
		t.Fatal(err)
	}
	if due, err = s.ListDueTargets(ctx); err != nil || len(due) != 0 {
		t.Fatalf("after service delete due = %d %v", len(due), err)
	}
}

func TestListServicesEpisodesAndDashboard(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	svc1, target1 := newSvc(t, s, 1, 1)
	view2, err := s.CreateService(ctx, CreateServiceInput{
		ServiceKey: "svc2-" + uuid.NewString()[:8], Name: "payments", AuthorityProducerID: "payment-api",
		Targets: []TargetInput{{
			TargetKey: "api", TargetName: "API", TargetType: "HTTP", Address: "http://payments/health",
			IntervalMS: 1000, TimeoutMS: 200, FailureThreshold: 1, RecoveryThreshold: 1,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	all, total, err := s.ListServices(ctx, ServiceFilter{})
	if err != nil || total != 2 || len(all) != 2 {
		t.Fatalf("services = %d/%d %v", len(all), total, err)
	}
	if page, total, err := s.ListServices(ctx, ServiceFilter{Status: StatusUnknown}); err != nil || total != 2 || len(page) != 2 {
		t.Fatalf("unknown services = %d/%d %v", len(page), total, err)
	}
	if page, total, err := s.ListServices(ctx, ServiceFilter{Status: StatusAbnormal}); err != nil || total != 0 || len(page) != 0 {
		t.Fatalf("abnormal services = %d/%d %v", len(page), total, err)
	}
	if page, total, err := s.ListServices(ctx, ServiceFilter{Limit: 1, Offset: 1}); err != nil || total != 2 || len(page) != 1 {
		t.Fatalf("paged services = %d/%d %v", len(page), total, err)
	}
	if _, _, err := s.ListServices(ctx, ServiceFilter{Status: "BAD"}); !IsCode(err, "VALIDATION") {
		t.Fatalf("bad status err = %v", err)
	}

	base := time.Now().UTC()
	res, err := s.ApplyMonitorSample(ctx, sample(svc1, target1, false, "5xx", base))
	if err != nil || !res.CreatedEpisode {
		t.Fatalf("episode = %+v %v", res, err)
	}
	sums, total, err := s.ListServices(ctx, ServiceFilter{Status: StatusAbnormal})
	if err != nil || total != 1 || sums[0].ServiceID != svc1 {
		t.Fatalf("abnormal summary = %+v %v", sums, err)
	}
	if sums[0].LastCheckAt == nil || sums[0].TargetCount != 1 {
		t.Fatalf("summary detail = %+v", sums[0])
	}
	if sums[0].ActiveEpisodeID == nil || *sums[0].ActiveEpisodeID != res.EpisodeID {
		t.Fatalf("active episode = %v", sums[0].ActiveEpisodeID)
	}

	eps, totalE, err := s.ListEpisodes(ctx, EpisodeFilter{Status: EpisodeActive})
	if err != nil || totalE != 1 || eps[0].ServiceName != "orders" || eps[0].UpdatedAt.IsZero() {
		t.Fatalf("episodes = %+v %v", eps, err)
	}
	if _, n, err := s.ListEpisodes(ctx, EpisodeFilter{ServiceID: &view2.ServiceID}); err != nil || n != 0 {
		t.Fatalf("episode filter by service = %d %v", n, err)
	}
	from, to := base.Add(-time.Minute), base.Add(time.Minute)
	if _, n, err := s.ListEpisodes(ctx, EpisodeFilter{From: &from, To: &to}); err != nil || n != 1 {
		t.Fatalf("episode time filter = %d %v", n, err)
	}
	future := base.Add(time.Hour)
	if _, n, err := s.ListEpisodes(ctx, EpisodeFilter{From: &future}); err != nil || n != 0 {
		t.Fatalf("episode future filter = %d %v", n, err)
	}
	if _, _, err := s.ListEpisodes(ctx, EpisodeFilter{Status: "BAD"}); !IsCode(err, "VALIDATION") {
		t.Fatalf("bad episode status err = %v", err)
	}

	dash, err := s.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dash.Services.Total != 2 || dash.Services.Abnormal != 1 || dash.Services.Unknown != 1 {
		t.Fatalf("dashboard services = %+v", dash.Services)
	}
	if dash.Targets.Total != 2 || dash.Targets.Abnormal != 1 || dash.Targets.Unknown != 1 {
		t.Fatalf("dashboard targets = %+v", dash.Targets)
	}
	if dash.Episodes.Active != 1 || len(dash.RecentEpisodes) != 1 {
		t.Fatalf("dashboard episodes = %+v recent=%d", dash.Episodes, len(dash.RecentEpisodes))
	}
	if len(dash.MonitorTrend) != 6 {
		t.Fatalf("trend = %+v", dash.MonitorTrend)
	}
	var failures int64
	for _, b := range dash.MonitorTrend {
		failures += b.Failure
	}
	if failures != 1 {
		t.Fatalf("trend failures = %d", failures)
	}

	// 软删除保留历史 Episode，但列表与 dashboard 排除服务。
	beforeDelete, err := s.GetService(ctx, svc1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SoftDeleteService(ctx, svc1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetService(ctx, svc1); !IsCode(err, "SERVICE_NOT_FOUND") {
		t.Fatalf("deleted get err = %v", err)
	}
	if _, total, err := s.ListServices(ctx, ServiceFilter{}); err != nil || total != 1 {
		t.Fatalf("services after delete = %d %v", total, err)
	}
	if eps, n, err := s.ListEpisodes(ctx, EpisodeFilter{ServiceID: &svc1}); err != nil || n != 1 || len(eps) != 1 {
		t.Fatalf("history episodes = %d/%d %v", len(eps), n, err)
	}
	if _, err := s.GetEpisode(ctx, res.EpisodeID); err != nil {
		t.Fatalf("history episode = %v", err)
	}
	if err := s.SoftDeleteService(ctx, svc1); !IsCode(err, "SERVICE_NOT_FOUND") {
		t.Fatalf("double service delete err = %v", err)
	}
	if dash, err = s.Dashboard(ctx); err != nil || dash.Services.Total != 1 {
		t.Fatalf("dashboard after delete = %+v %v", dash.Services, err)
	}
	// 部分唯一索引允许软删除后用同一 service_key 重建。
	if _, err := s.CreateService(ctx, CreateServiceInput{
		ServiceKey: beforeDelete.ServiceKey, Name: "reborn", AuthorityProducerID: "ops",
		Targets: []TargetInput{{
			TargetKey: "api", TargetName: "API", TargetType: "HTTP", Address: "http://reborn/health",
			IntervalMS: 1000, TimeoutMS: 200, FailureThreshold: 1, RecoveryThreshold: 1,
		}},
	}); err != nil {
		t.Fatalf("recreate service key: %v", err)
	}
}

func TestStateFingerprintDrivesInputVersion(t *testing.T) {
	s := testStore(t)
	svc, target := newSvc(t, s, 1, 1)
	ctx := context.Background()
	base := time.Now().UTC()
	mk := func(fp string, at time.Time) MonitorSample {
		return MonitorSample{
			ServiceID: svc, TargetID: target, ObservedAt: at, Success: false,
			StatusClass: "unhealthy", ErrorClass: "REPLICAS_UNAVAILABLE", StateFingerprint: fp,
		}
	}
	first, err := s.ApplyMonitorSample(ctx, mk("fp-a", base))
	if err != nil || !first.CreatedEpisode || first.InputVersion != 1 {
		t.Fatalf("first = %+v %v", first, err)
	}
	// 2/3 -> 3/3 这类状态变化产生新的输入版本。
	second, err := s.ApplyMonitorSample(ctx, mk("fp-b", base.Add(time.Second)))
	if err != nil || second.CreatedEpisode || second.InputVersion != 2 {
		t.Fatalf("second = %+v %v", second, err)
	}
	// 采集时间变化不产生新版本。
	third, err := s.ApplyMonitorSample(ctx, mk("fp-b", base.Add(2*time.Second)))
	if err != nil || third.InputVersion != 2 {
		t.Fatalf("third = %+v %v", third, err)
	}
	var diag string
	if err := s.DB.QueryRowContext(ctx, `
		SELECT diagnostic_fields->>'state_fingerprint'
		FROM system.service_monitor_points ORDER BY point_seq DESC LIMIT 1`).Scan(&diag); err != nil || diag != "fp-b" {
		t.Fatalf("diagnostic_fields = %q %v", diag, err)
	}
	var projection []byte
	if err := s.DB.QueryRowContext(ctx,
		`SELECT current_monitor_projection FROM system.episodes WHERE episode_id=$1`, first.EpisodeID).Scan(&projection); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(projection), "fp-b") {
		t.Fatalf("projection = %s", projection)
	}
}

// TestKubernetesTargetValidationMessages 覆盖 K8s 目标非正 interval/timeout 与非法 DNS 名的报错契约。
func TestKubernetesTargetValidationMessages(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	create := func(interval, timeout int, resourceName string) error {
		_, err := s.CreateService(ctx, CreateServiceInput{
			ServiceKey: "k8s-bad-" + uuid.NewString()[:8], Name: "k8s-bad", AuthorityProducerID: "ops",
			Targets: []TargetInput{{
				TargetKey: "web", TargetName: "web", TargetType: "KUBERNETES",
				Config:     json.RawMessage(`{"resource_kind":"Deployment","resource_name":"` + resourceName + `"}`),
				IntervalMS: interval, TimeoutMS: timeout,
				FailureThreshold: 1, RecoveryThreshold: 1,
			}},
		})
		return err
	}
	for _, tc := range []struct {
		name              string
		interval, timeout int
	}{
		{"negative interval", -1, 3000},
		{"negative timeout", 10000, -1},
		{"both negative", -10000, -3000},
	} {
		err := create(tc.interval, tc.timeout, "web")
		e, ok := err.(*Error)
		if !ok || e.Code != "VALIDATION" || e.Message != "interval_ms and timeout_ms must be positive" || e.HTTP != 400 {
			t.Fatalf("%s: err = %+v", tc.name, err)
		}
	}
	// 0 仍视为未设置并补默认值。
	if err := create(0, 0, "web"); err != nil {
		t.Fatalf("zero interval/timeout should default: %v", err)
	}
	for _, name := range []string{"Web_App", "web..app", "-web", "web app", "web/app"} {
		err := create(10000, 3000, name)
		e, ok := err.(*Error)
		if !ok || e.Code != "VALIDATION" || e.Message != "config.resource_name is not a valid Kubernetes resource name" || e.HTTP != 400 {
			t.Fatalf("resource_name %q: err = %+v", name, err)
		}
	}
	if _, err := ParseKubernetesTargetConfig(json.RawMessage(`{"resource_kind":"Deployment","resource_name":"Web.App"}`)); err == nil {
		t.Fatal("invalid resource_name accepted by parser")
	}
}

// TestListServicesQueryFilter 覆盖服务列表 q 过滤：名称/服务键模糊匹配、大小写不敏感、% 与 _ 转义。
func TestListServicesQueryFilter(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	mk := func(key, name string) uuid.UUID {
		t.Helper()
		view, err := s.CreateService(ctx, CreateServiceInput{
			ServiceKey: key, Name: name, AuthorityProducerID: "ops",
			Targets: []TargetInput{{
				TargetKey: "api", TargetName: "API", TargetType: "HTTP", Address: "http://x/health",
				IntervalMS: 1000, TimeoutMS: 200, FailureThreshold: 1, RecoveryThreshold: 1,
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return view.ServiceID
	}
	ordersID := mk("orders-api-"+suffix, "Orders API")
	mk("payments-api-"+suffix, "Payments")
	mk("svc_under-"+suffix, "50%off")

	for _, tc := range []struct {
		name string
		q    string
		want int
	}{
		{"match by name", "orders", 1},
		{"match by service_key", "payments-api", 1},
		{"case insensitive", "ORDERS", 1},
		{"trimmed", "  orders  ", 1},
		{"percent is literal", "%", 1},
		{"underscore is literal", "_", 1},
		{"no match", "no-such-service", 0},
	} {
		list, total, err := s.ListServices(ctx, ServiceFilter{Q: tc.q})
		if err != nil || total != tc.want || len(list) != tc.want {
			t.Fatalf("%s: list=%d total=%d err=%v", tc.name, len(list), total, err)
		}
	}
	// q 与 limit/status 组合时 total 仍为过滤后的总数。
	list, total, err := s.ListServices(ctx, ServiceFilter{Q: "api", Limit: 1})
	if err != nil || total != 2 || len(list) != 1 {
		t.Fatalf("q+limit: list=%d total=%d err=%v", len(list), total, err)
	}
	if _, total, err := s.ListServices(ctx, ServiceFilter{Q: "orders", Status: StatusUnknown}); err != nil || total != 1 {
		t.Fatalf("q+status total=%d err=%v", total, err)
	}
	// 软删除的服务不应出现在 q 结果中。
	if err := s.SoftDeleteService(ctx, ordersID); err != nil {
		t.Fatal(err)
	}
	if list, total, err := s.ListServices(ctx, ServiceFilter{Q: "orders"}); err != nil || total != 0 || len(list) != 0 {
		t.Fatalf("q after delete: list=%d total=%d err=%v", len(list), total, err)
	}
}
