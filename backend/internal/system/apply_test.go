package system

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ecakeman/alertflow/backend/internal/db"
	"github.com/google/uuid"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	conn, err := db.Open("postgres://alertflow:alertflow@127.0.0.1:55433/alertflow?sslmode=disable")
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Exec(`TRUNCATE
		system.episode_input_versions,
		system.episode_artifact_refs,
		system.outbox_events,
		system.agent_tasks,
		system.episode_app_events,
		system.application_event_receipts,
		system.episodes,
		system.service_monitor_points,
		system.service_source_states,
		system.service_monitor_target_states,
		system.service_monitor_targets,
		system.services,
		cognitive.fault_documents,
		cognitive.episode_artifacts,
		cognitive.agent_task_receipts
		CASCADE`); err != nil {
		t.Fatal(err)
	}
	return New(conn)
}

func boolPtr(v bool) *bool { return &v }

func newSvc(t *testing.T, s *Store, failTh, recTh int) (uuid.UUID, uuid.UUID) {
	t.Helper()
	view, err := s.CreateService(context.Background(), CreateServiceInput{
		ServiceKey: "svc-" + uuid.NewString()[:8], Name: "orders", AuthorityProducerID: "order-api",
		Targets: []TargetInput{{
			TargetKey: "api", TargetName: "API", TargetType: "HTTP", Address: "http://orders/health",
			IntervalMS: 1000, TimeoutMS: 200, FailureThreshold: failTh, RecoveryThreshold: recTh,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if view.MonitorStatus != StatusUnknown || view.AppStatus != StatusUnknown {
		t.Fatalf("initial sources = %s %s", view.MonitorStatus, view.AppStatus)
	}
	return view.ServiceID, view.Targets[0].TargetID
}

func sample(svc, target uuid.UUID, ok bool, class string, at time.Time) MonitorSample {
	return MonitorSample{ServiceID: svc, TargetID: target, ObservedAt: at, Success: ok, StatusClass: class}
}

func appBody(id, kind, health string, rev *int64) ([]byte, AppEvent) {
	ev := AppEvent{
		EventID: id, ProducerID: "order-api", EventTime: time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC),
		SourceRevision: rev, EventKind: kind, HealthStatus: health, SchemaVersion: 1,
		Payload: json.RawMessage(`{"message":"pool exhausted"}`),
	}
	raw, _ := json.Marshal(ev)
	return raw, ev
}

func TestHashIgnoresSampleIdentity(t *testing.T) {
	proj := []MonitorField{{TargetKey: "api", TargetType: "HTTP", Status: StatusAbnormal, StatusClass: "5xx"}}
	a, err := HashInput(proj, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashInput([]MonitorField{{TargetKey: "api", TargetType: "HTTP", Status: StatusAbnormal, StatusClass: "5xx"}}, nil)
	if err != nil || a != b {
		t.Fatalf("hash drift %s %s %v", a, b, err)
	}
	changed := proj
	changed[0].StatusClass = "timeout"
	c, _ := HashInput(changed, nil)
	if c == a {
		t.Fatal("status class change must change hash")
	}
}

func TestMonitorCreatesOneEpisodeAndReusesIt(t *testing.T) {
	s := testStore(t)
	svc, target := newSvc(t, s, 2, 2)
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	first, err := s.ApplyMonitorSample(ctx, sample(svc, target, false, "5xx", base))
	if err != nil {
		t.Fatal(err)
	}
	if first.EpisodeID != uuid.Nil {
		t.Fatal("single failure must not open an episode")
	}
	second, err := s.ApplyMonitorSample(ctx, sample(svc, target, false, "5xx", base.Add(time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if !second.CreatedEpisode || second.MonitorStatus != StatusAbnormal || second.InputVersion != 1 {
		t.Fatalf("create %+v", second)
	}
	third, err := s.ApplyMonitorSample(ctx, sample(svc, target, false, "5xx", base.Add(2*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if third.CreatedEpisode || third.EpisodeID != second.EpisodeID || third.InputVersion != 1 {
		t.Fatalf("same fields must reuse episode without a new version: %+v", third)
	}
	points, err := s.ServiceTimeline(ctx, svc, 10)
	if err != nil || len(points) != 3 {
		t.Fatalf("timeline points %d %v", len(points), err)
	}
}

func TestConcurrentSourcesCreateOneEpisode(t *testing.T) {
	s := testStore(t)
	svc, target := newSvc(t, s, 1, 1)
	ctx := context.Background()
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := s.ApplyMonitorSample(ctx, sample(svc, target, false, "5xx", time.Now()))
		errCh <- err
	}()
	go func() {
		defer wg.Done()
		raw, ev := appBody("e-concurrent", "HEALTH_STATUS", StatusAbnormal, nil)
		_, err := s.ApplyAppEvent(ctx, svc, ev, raw)
		errCh <- err
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM system.episodes WHERE service_id=$1`, svc).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("episodes = %d", n)
	}
	var active int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM system.episodes WHERE service_id=$1 AND status='ACTIVE'`, svc).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active = %d", active)
	}
}

func TestAppEventRules(t *testing.T) {
	s := testStore(t)
	svc, _ := newSvc(t, s, 2, 2)
	ctx := context.Background()
	raw, ev := appBody("e1", "DIAGNOSTIC_INFO", "", nil)
	res, err := s.ApplyAppEvent(ctx, svc, ev, raw)
	if err != nil || res.Body["error_code"] != "NO_ACTIVE_EPISODE" {
		t.Fatalf("%+v %v", res, err)
	}
	again, err := s.ApplyAppEvent(ctx, svc, ev, raw)
	if err != nil || again.Body["duplicate"] != true || again.Body["error_code"] != "NO_ACTIVE_EPISODE" {
		t.Fatalf("replay %+v %v", again, err)
	}
	raw2, ev2 := appBody("e1", "HEALTH_STATUS", StatusAbnormal, nil)
	conflict, err := s.ApplyAppEvent(ctx, svc, ev2, raw2)
	if err != nil || conflict.Body["error_code"] != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("conflict %+v %v", conflict, err)
	}
	raw3, ev3 := appBody("e2", "HEALTH_STATUS", StatusNormal, nil)
	normal, err := s.ApplyAppEvent(ctx, svc, ev3, raw3)
	if err != nil || normal.Body["episode_id"] != nil {
		t.Fatalf("normal before failure must not create episode: %+v %v", normal.Body, err)
	}
	rev := int64(2)
	raw4, ev4 := appBody("e3", "HEALTH_STATUS", StatusAbnormal, &rev)
	opened, err := s.ApplyAppEvent(ctx, svc, ev4, raw4)
	if err != nil || opened.Body["episode_status"] != EpisodeActive {
		t.Fatalf("open %+v %v", opened.Body, err)
	}
	old := int64(1)
	raw5, ev5 := appBody("e4", "HEALTH_STATUS", StatusNormal, &old)
	stale, err := s.ApplyAppEvent(ctx, svc, ev5, raw5)
	if err != nil || stale.Body["error_code"] != "STALE_SOURCE_REVISION" {
		t.Fatalf("stale %+v %v", stale.Body, err)
	}
	ev4.ProducerID = "other"
	rawOther, _ := json.Marshal(ev4)
	ev4.EventID = "e5"
	rawOther, _ = json.Marshal(ev4)
	denied, err := s.ApplyAppEvent(ctx, svc, ev4, rawOther)
	if err != nil || denied.Body["error_code"] != "PRODUCER_NOT_AUTHORITATIVE" {
		t.Fatalf("producer %+v %v", denied.Body, err)
	}
}

func TestRegionHashAndResolution(t *testing.T) {
	s := testStore(t)
	svc, target := newSvc(t, s, 1, 2)
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	opened, err := s.ApplyMonitorSample(ctx, sample(svc, target, false, "5xx", base))
	if err != nil {
		t.Fatal(err)
	}
	raw, ev := appBody("d1", "DIAGNOSTIC_INFO", "", nil)
	ev.Payload = json.RawMessage(`{"message":"timeouts"}`)
	raw, _ = json.Marshal(ev)
	appRes, err := s.ApplyAppEvent(ctx, svc, ev, raw)
	if err != nil || fmt.Sprint(appRes.Body["input_version"]) != "2" {
		t.Fatalf("diagnostic must append version: %+v %v", appRes.Body, err)
	}
	replay, err := s.ApplyAppEvent(ctx, svc, ev, raw)
	if err != nil || replay.Body["duplicate"] != true {
		t.Fatalf("replay %+v", replay.Body)
	}
	var events int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM system.episode_app_events WHERE episode_id=$1`, opened.EpisodeID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("events=%d %v", events, err)
	}
	// 恢复需要连续两次成功。第一次仍是 ABNORMAL，第二次 MONITOR 变为 NORMAL，投影更新一次。
	if _, err := s.ApplyMonitorSample(ctx, sample(svc, target, true, "2xx", base.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.ApplyMonitorSample(ctx, sample(svc, target, true, "2xx", base.Add(2*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if recovered.MonitorStatus != StatusNormal {
		t.Fatal(recovered.MonitorStatus)
	}
	frozenVersion := recovered.InputVersion
	later, err := s.ApplyMonitorSample(ctx, sample(svc, target, true, "2xx", base.Add(3*time.Minute)))
	if err != nil || later.InputVersion != frozenVersion {
		t.Fatalf("frozen %+v %v", later, err)
	}
	// APP 仍是 UNKNOWN，不能进入恢复。
	ep, err := s.GetEpisode(ctx, opened.EpisodeID)
	if err != nil || ep.ResolutionPending {
		t.Fatalf("unknown app must not pend resolution: %+v %v", ep, err)
	}
	rev := int64(1)
	rawN, evN := appBody("n1", "HEALTH_STATUS", StatusNormal, &rev)
	pendingRes, err := s.ApplyAppEvent(ctx, svc, evN, rawN)
	if err != nil || pendingRes.Body["resolution_pending"] != true {
		t.Fatalf("pending %+v %v", pendingRes.Body, err)
	}
	rawD, evD := appBody("d2", "DIAGNOSTIC_INFO", "", nil)
	closed, err := s.ApplyAppEvent(ctx, svc, evD, rawD)
	if err != nil || closed.Body["error_code"] != "EPISODE_CLOSING" {
		t.Fatalf("closing %+v %v", closed.Body, err)
	}
	for i := 0; i < 6; i++ {
		view, err := s.GetEpisode(ctx, opened.EpisodeID)
		if err != nil {
			t.Fatal(err)
		}
		if view.ActiveTaskType == TaskArchive {
			break
		}
		if view.ActiveTaskType != TaskDiagnosis || view.ActiveTaskID == nil {
			t.Fatalf("expected diagnosis task: %+v", view)
		}
		var to, gen int64
		if err := s.DB.QueryRow(`SELECT input_version_to, execution_generation FROM system.agent_tasks WHERE task_id=$1`, *view.ActiveTaskID).Scan(&to, &gen); err != nil {
			t.Fatal(err)
		}
		ack, err := s.CompleteTask(ctx, CompleteInput{
			TaskID: *view.ActiveTaskID, EpisodeID: view.EpisodeID, TaskType: TaskDiagnosis,
			Generation: gen, Outcome: "SUCCEEDED", ProcessedInputVersion: to,
		})
		if err != nil || !ack.Accepted {
			t.Fatalf("complete %+v %v", ack, err)
		}
	}
	done, err := s.GetEpisode(ctx, opened.EpisodeID)
	if err != nil || done.Status != EpisodeResolved || done.ActiveTaskType != TaskArchive {
		t.Fatalf("resolved %+v %v", done, err)
	}
	fd := uuid.New()
	rejected, err := s.CompleteTask(ctx, CompleteInput{
		TaskID: *done.ActiveTaskID, EpisodeID: done.EpisodeID, TaskType: TaskArchive,
		Generation: 1, Outcome: "SUCCEEDED", FDID: fd.String(),
	})
	if err != nil || rejected.Accepted {
		t.Fatalf("archive without receipt %+v %v", rejected, err)
	}
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	ok, message, err := s.RegisterFaultDocument(ctx, RegisterFaultDocumentInput{
		TaskID: *done.ActiveTaskID, EpisodeID: done.EpisodeID, Generation: 1, FDID: fd.String(), ContentHash: hash,
	})
	if err != nil || !ok || message != "registered" {
		t.Fatalf("register %+v %s %v", ok, message, err)
	}
	again, message, err := s.RegisterFaultDocument(ctx, RegisterFaultDocumentInput{
		TaskID: *done.ActiveTaskID, EpisodeID: done.EpisodeID, Generation: 1, FDID: fd.String(), ContentHash: hash,
	})
	if err != nil || !again || message != "duplicate" {
		t.Fatalf("duplicate register %+v %s %v", again, message, err)
	}
	stale, message, err := s.RegisterFaultDocument(ctx, RegisterFaultDocumentInput{
		TaskID: *done.ActiveTaskID, EpisodeID: done.EpisodeID, Generation: 0, FDID: fd.String(), ContentHash: hash,
	})
	if err != nil || stale || message != "stale execution generation" {
		t.Fatalf("stale generation %+v %s %v", stale, message, err)
	}
	archived, err := s.CompleteTask(ctx, CompleteInput{
		TaskID: *done.ActiveTaskID, EpisodeID: done.EpisodeID, TaskType: TaskArchive,
		Generation: 1, Outcome: "SUCCEEDED", FDID: fd.String(),
	})
	if err != nil || !archived.Accepted {
		t.Fatalf("archive %+v %v", archived, err)
	}
	final, err := s.GetEpisode(ctx, opened.EpisodeID)
	if err != nil || final.Status != EpisodeArchived {
		t.Fatalf("archived %+v %v", final, err)
	}
}

func TestBatchBoundaryAndFailure(t *testing.T) {
	s := testStore(t)
	svc, target := newSvc(t, s, 1, 1)
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	opened, err := s.ApplyMonitorSample(ctx, sample(svc, target, false, "5xx", base))
	if err != nil {
		t.Fatal(err)
	}
	// 任务已在执行，后续不同的有效字段进入下一批。
	if _, err := s.ApplyMonitorSample(ctx, sample(svc, target, false, "timeout", base.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	var tasks int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM system.agent_tasks WHERE episode_id=$1`, opened.EpisodeID).Scan(&tasks); err != nil || tasks != 1 {
		t.Fatalf("tasks=%d %v", tasks, err)
	}
	var pending int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM system.episode_input_versions WHERE episode_id=$1 AND queue_status='PENDING'`, opened.EpisodeID).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("pending=%d %v", pending, err)
	}
	ep, _ := s.GetEpisode(ctx, opened.EpisodeID)
	fail, err := s.CompleteTask(ctx, CompleteInput{
		TaskID: *ep.ActiveTaskID, EpisodeID: ep.EpisodeID, TaskType: TaskDiagnosis, Generation: 1,
		Outcome: "RETRYABLE_FAILURE", ErrorCode: "MODEL", ProcessedInputVersion: 1,
	})
	if err != nil || !fail.Accepted {
		t.Fatal(fail, err)
	}
	var processed int64
	if err := s.DB.QueryRow(`SELECT processed_input_version FROM system.episodes WHERE episode_id=$1`, ep.EpisodeID).Scan(&processed); err != nil || processed != 0 {
		t.Fatalf("processed=%d", processed)
	}
	stale, err := s.ReportTaskStarted(ctx, *ep.ActiveTaskID, ep.EpisodeID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.promoteRetries(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var gen int64
	if err := s.DB.QueryRow(`SELECT execution_generation FROM system.agent_tasks WHERE task_id=$1`, *ep.ActiveTaskID).Scan(&gen); err != nil || gen != 2 {
		t.Fatalf("generation=%d", gen)
	}
	late, err := s.CompleteTask(ctx, CompleteInput{
		TaskID: *ep.ActiveTaskID, EpisodeID: ep.EpisodeID, TaskType: TaskDiagnosis, Generation: 1,
		Outcome: "SUCCEEDED", ProcessedInputVersion: 1,
	})
	if err != nil || late.Accepted {
		t.Fatalf("old generation must be rejected: %+v %v", late, err)
	}
	_ = stale
}

func TestPublishPendingMarksSent(t *testing.T) {
	s := testStore(t)
	svc, target := newSvc(t, s, 1, 1)
	if _, err := s.ApplyMonitorSample(context.Background(), sample(svc, target, false, "5xx", time.Now())); err != nil {
		t.Fatal(err)
	}
	n, err := s.PublishPending(context.Background(), 10, func(payload []byte) error { return nil })
	if err != nil || n != 1 {
		t.Fatalf("published %d %v", n, err)
	}
	left, err := s.UnpublishedCount(context.Background())
	if err != nil || left != 0 {
		t.Fatalf("unpublished %d %v", left, err)
	}
}

func TestActiveEpisodeUniqueIndex(t *testing.T) {
	s := testStore(t)
	svc, target := newSvc(t, s, 1, 1)
	opened, err := s.ApplyMonitorSample(context.Background(), sample(svc, target, false, "5xx", time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec(`INSERT INTO system.episodes
		(episode_id, service_id, status, started_by, started_at, created_at, updated_at)
		VALUES ($1,$2,'ACTIVE','MONITOR',now(),now(),now())`, uuid.New(), svc)
	if err == nil {
		t.Fatal("second active episode must violate the partial unique index")
	}
	_ = opened
}
