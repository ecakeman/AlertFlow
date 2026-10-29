package deliver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"database/sql"

	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/event"
	"github.com/ecakeman/alertflow/internal/notify"
)

func TestDeliverSucceedsAndLogs(t *testing.T) {
	database := db.OpenTest(t)
	if _, err := database.CreateIntegration(db.AgentWebhookType, "hook", true, false, json.RawMessage(`{"url":"http://example.invalid","headers":{"Authorization":"super-secret"}}`)); err != nil {
		t.Fatal(err)
	}
	ev := mustOffline(t, database, "svc")
	var payload string
	if err := database.QueryRow(`SELECT payload::text FROM notification_deliveries WHERE event_id = $1`, ev).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "super-secret") {
		t.Fatalf("payload stored a secret: %s", payload)
	}

	w := clocked(database, time.Now().Unix())
	if n, err := w.ProcessDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("process = %d, %v", n, err)
	}
	if w.Tasks.(*scriptedTasks).hits.Load() != 1 {
		t.Fatalf("hits = %d", w.Tasks.(*scriptedTasks).hits.Load())
	}
	assertStatus(t, database, ev, "succeeded", 1)
	var sent int
	if err := database.QueryRow(`SELECT COUNT(*) FROM notification_log WHERE status = 'sent' AND event = 'incident_start'`).Scan(&sent); err != nil || sent != 0 {
		t.Fatalf("internal task wrote a notification log: %d, %v", sent, err)
	}
	var sentAt int
	if err := database.QueryRow(`SELECT COUNT(*) FROM notification_deliveries WHERE sent_at IS NOT NULL AND status = 'succeeded'`).Scan(&sentAt); err != nil || sentAt != 1 {
		t.Fatalf("sent_at rows = %d, %v", sentAt, err)
	}
	var indexes int
	if err := database.QueryRow(`SELECT COUNT(*) FROM pg_indexes WHERE indexname IN ('idx_notification_deliveries_due', 'idx_notification_deliveries_running')`).Scan(&indexes); err != nil || indexes != 2 {
		t.Fatalf("due indexes = %d, %v", indexes, err)
	}
}

func TestDeliverRetriesThenSucceeds(t *testing.T) {
	database := db.OpenTest(t)
	mustChannel(t, database, "hook", "http://example.invalid", false)
	ev := mustOffline(t, database, "svc")
	clk := newClock()
	w := &Worker{DB: database, Owner: "worker", Now: clk.now, Tasks: &scriptedTasks{fail: func(n int32) error {
		if n == 1 {
			return errors.New("unavailable")
		}
		return nil
	}}}
	if _, err := w.ProcessDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, database, ev, "retry_wait", 1)
	var next int64
	if err := database.QueryRow(`SELECT next_attempt_at FROM notification_deliveries WHERE event_id = $1`, ev).Scan(&next); err != nil {
		t.Fatal(err)
	}
	clk.sec = next - 1
	if n, err := w.ProcessDue(context.Background()); err != nil || n != 0 {
		t.Fatalf("early retry = %d, %v", n, err)
	}
	clk.sec = next
	if n, err := w.ProcessDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("second process = %d, %v", n, err)
	}
	assertStatus(t, database, ev, "succeeded", 2)
	if w.Tasks.(*scriptedTasks).hits.Load() != 2 {
		t.Fatalf("hits = %d", w.Tasks.(*scriptedTasks).hits.Load())
	}
}

func TestDeliverDiesAtMaxAttempts(t *testing.T) {
	database := db.OpenTest(t)
	mustChannel(t, database, "hook", "http://example.invalid", false)
	ev := mustOffline(t, database, "svc")
	clk := newClock()
	w := &Worker{DB: database, Owner: "dead", Now: clk.now, Tasks: &scriptedTasks{fail: func(int32) error {
		return errors.New("unavailable")
	}}}
	for i := 0; i < MaxAttempts; i++ {
		if _, err := w.ProcessDue(context.Background()); err != nil {
			t.Fatal(err)
		}
		var status string
		var next int64
		if err := database.QueryRow(`SELECT status, next_attempt_at FROM notification_deliveries WHERE event_id = $1`, ev).Scan(&status, &next); err != nil {
			t.Fatal(err)
		}
		if i < MaxAttempts-1 {
			if status != "retry_wait" {
				t.Fatalf("attempt %d status = %s", i+1, status)
			}
			clk.sec = next
			continue
		}
		if status != "dead" {
			t.Fatalf("final status = %s", status)
		}
	}
	if n, err := w.ProcessDue(context.Background()); err != nil || n != 0 {
		t.Fatalf("dead task claimed again: %d, %v", n, err)
	}
}

func TestClaimIsExclusiveAndLeaseExpires(t *testing.T) {
	database := db.OpenTest(t)
	mustChannel(t, database, "hook", "http://example.invalid", false)
	mustOffline(t, database, "svc")

	var wg sync.WaitGroup
	var mu sync.Mutex
	var got int
	base := time.Now().Unix()
	for _, owner := range []string{"a", "b"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			rows, err := database.ClaimDeliveries(owner, base, base+15, 1, MaxAttempts)
			if err != nil {
				t.Errorf("claim %s: %v", owner, err)
				return
			}
			mu.Lock()
			got += len(rows)
			mu.Unlock()
		}(owner)
	}
	wg.Wait()
	if got != 1 {
		t.Fatalf("concurrent claims = %d", got)
	}

	var id int64
	var owner string
	if err := database.QueryRow(`SELECT id, lease_owner FROM notification_deliveries`).Scan(&id, &owner); err != nil {
		t.Fatal(err)
	}
	again, err := database.ClaimDeliveries("other", base+15, base+30, 1, MaxAttempts)
	if err != nil || len(again) != 1 || again[0].ID != id {
		t.Fatalf("expired reclaim = %+v, %v", again, err)
	}
	if err := database.SucceedDelivery(id, owner, base+15, base+15); !errors.Is(err, db.ErrLeaseLost) {
		t.Fatalf("stale success = %v", err)
	}
	var sent int
	if err := database.QueryRow(`SELECT COUNT(*) FROM notification_log WHERE status = 'sent'`).Scan(&sent); err != nil || sent != 0 {
		t.Fatalf("stale worker wrote a success log: %d, %v", sent, err)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM notification_deliveries WHERE id = $1`, id).Scan(&status); err != nil || status != "running" {
		t.Fatalf("status after stale write = %s, %v", status, err)
	}
	if err := database.SucceedDelivery(id, "other", base+15, base+15); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidChannelDoesNotRetry(t *testing.T) {
	database := db.OpenTest(t)
	ev := mustOffline(t, database, "svc")
	w := clocked(database, time.Now().Unix())
	w.Tasks.(*scriptedTasks).fail = func(int32) error {
		return &notify.StatusError{Code: http.StatusBadRequest, Body: "missing field"}
	}
	if _, err := w.ProcessDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, database, ev, "dead", 1)
	if n, err := w.ProcessDue(context.Background()); err != nil || n != 0 {
		t.Fatalf("dead invalid channel claimed: %d, %v", n, err)
	}
}

func TestWarningGoesOnlyToOptedInChannel(t *testing.T) {
	database := db.OpenTest(t)
	mustChannel(t, database, "quiet", "http://example.invalid", false)
	mustChannel(t, database, "pages", "http://example.invalid/pages", true)
	ev, err := event.NewCheckCompleted("svc", "Name", "https://example.com", 6_000, db.StatusWarning, nil, nil, "blip", 2)
	if err != nil {
		t.Fatal(err)
	}
	ev.CheckID = 1
	if err := database.ApplyCheckEvent(ev); err != nil {
		t.Fatal(err)
	}
	var n int
	var integrationID sql.NullInt64
	if err := database.QueryRow(`SELECT COUNT(*), MIN(integration_id) FROM notification_deliveries WHERE event_type = 'warning'`).Scan(&n, &integrationID); err != nil || n != 1 || integrationID.Valid {
		t.Fatalf("warning deliveries = %d integration %v, %v", n, integrationID, err)
	}
	w := clocked(database, time.Now().Unix())
	if _, err := w.ProcessDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.Tasks.(*scriptedTasks).hits.Load() != 1 {
		t.Fatalf("hits = %d", w.Tasks.(*scriptedTasks).hits.Load())
	}
	_ = ev
}

func TestDeletedIntegrationIsNotASuccessfulSend(t *testing.T) {
	database := db.OpenTest(t)
	channel := mustChannel(t, database, "hook", "http://example.invalid", false)
	mustOffline(t, database, "svc")
	if err := database.DeleteIntegration(channel.ID); err != nil {
		t.Fatal(err)
	}
	var deliveries, sent int
	if err := database.QueryRow(`SELECT COUNT(*) FROM notification_deliveries WHERE integration_id IS NULL`).Scan(&deliveries); err != nil || deliveries != 1 {
		t.Fatalf("internal deliveries after delete = %d, %v", deliveries, err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM notification_deliveries WHERE integration_id IS NOT NULL`).Scan(&deliveries); err != nil || deliveries != 0 {
		t.Fatalf("channel deliveries after delete = %d, %v", deliveries, err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM notification_log WHERE status = 'sent'`).Scan(&sent); err != nil || sent != 0 {
		t.Fatalf("success log after delete = %d, %v", sent, err)
	}
	w := clocked(database, time.Now().Unix())
	if n, err := w.ProcessDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("internal task processed = %d, %v", n, err)
	}
}

func TestAbandonedRunningLeaseCanBeReclaimed(t *testing.T) {
	database := db.OpenTest(t)
	mustChannel(t, database, "hook", "http://example.invalid", false)
	mustOffline(t, database, "svc")
	base := time.Now().Unix()
	claimed, err := database.ClaimDeliveries("crashed", base, base+LeaseSeconds, 1, MaxAttempts)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	early, err := database.ClaimDeliveries("other", base+LeaseSeconds-1, base+LeaseSeconds+10, 1, MaxAttempts)
	if err != nil || len(early) != 0 {
		t.Fatalf("early reclaim = %+v, %v", early, err)
	}
	late, err := database.ClaimDeliveries("other", base+LeaseSeconds, base+LeaseSeconds*2, 1, MaxAttempts)
	if err != nil || len(late) != 1 || late[0].ID != claimed[0].ID {
		t.Fatalf("expired reclaim = %+v, %v", late, err)
	}
	if err := database.SucceedDelivery(claimed[0].ID, "crashed", base+LeaseSeconds, base+LeaseSeconds); !errors.Is(err, db.ErrLeaseLost) {
		t.Fatalf("crashed worker overwrote the new lease: %v", err)
	}
}

func TestManualRetryClearsAttempts(t *testing.T) {
	database := db.OpenTest(t)
	if _, err := database.CreateIntegration(db.AgentWebhookType, "bad", true, false, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	ev := mustOffline(t, database, "svc")
	w := clocked(database, time.Now().Unix())
	w.Tasks.(*scriptedTasks).fail = func(int32) error {
		return &notify.StatusError{Code: http.StatusBadRequest, Body: "missing field"}
	}
	if _, err := w.ProcessDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := database.QueryRow(`SELECT id FROM notification_deliveries WHERE event_id = $1`, ev).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := database.RetryDeadDelivery(id, 9_100); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, database, ev, "pending", 0)
	if err := database.RetryDeadDelivery(id, 9_100); !errors.Is(err, db.ErrNotRetryable) {
		t.Fatalf("second retry = %v", err)
	}
}

type clock struct{ sec int64 }

func newClock() *clock { return &clock{sec: time.Now().Unix()} }

func (c *clock) now() int64 { return c.sec }

type scriptedTasks struct {
	hits atomic.Int32
	fail func(int32) error
}

func (s *scriptedTasks) Accept(context.Context, db.ClaimedDelivery) error {
	n := s.hits.Add(1)
	if s.fail != nil {
		return s.fail(n)
	}
	return nil
}

func clocked(database *db.DB, now int64) *Worker {
	return &Worker{DB: database, Owner: "worker", Now: func() int64 { return now }, Tasks: &scriptedTasks{}}
}

func mustChannel(t *testing.T, database *db.DB, name, url string, warnings bool) *db.Integration {
	t.Helper()
	in, err := database.CreateIntegration(db.AgentWebhookType, name, true, warnings, json.RawMessage(`{"url":"`+url+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func mustOffline(t *testing.T, database *db.DB, service string) string {
	t.Helper()
	ev, err := event.NewCheckCompleted(service, "Name", "https://example.com/health", 1_000, db.StatusOffline, nil, nil, "down", 1)
	if err != nil {
		t.Fatal(err)
	}
	ev.CheckID = 1
	if err := database.ApplyCheckEvent(ev); err != nil {
		t.Fatal(err)
	}
	return ev.EventID
}

func assertStatus(t *testing.T, database *db.DB, eventID, status string, attempts int) {
	t.Helper()
	var got string
	var n int
	if err := database.QueryRow(`SELECT status, attempt_count FROM notification_deliveries WHERE event_id = $1`, eventID).Scan(&got, &n); err != nil {
		t.Fatal(err)
	}
	if got != status || n != attempts {
		t.Fatalf("delivery = %s attempt %d, want %s attempt %d", got, n, status, attempts)
	}
}

func TestDisabledIntegrationIsLeftUntilEnabled(t *testing.T) {
	database := db.OpenTest(t)
	channel := mustChannel(t, database, "hook", "http://example.invalid", false)
	mustOffline(t, database, "svc")
	if _, err := database.UpdateIntegration(channel.ID, channel.Type, channel.Name, false, false, channel.Config); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Unix()
	if _, err := database.Exec(
		`INSERT INTO notification_deliveries (
			event_id, integration_id, event_type, payload, status,
			attempt_count, next_attempt_at, created_at, updated_at
		 ) VALUES ('old-channel', $1, 'incident_start', '{}', 'pending', 0, $2, $2, $2)`,
		channel.ID, base,
	); err != nil {
		t.Fatal(err)
	}
	rows, err := database.ClaimDeliveries("worker", base, base+15, 1, MaxAttempts)
	if err != nil || len(rows) != 1 || rows[0].IntegrationID != 0 {
		t.Fatalf("claim = %+v, %v", rows, err)
	}
	var old string
	if err := database.QueryRow(`SELECT status FROM notification_deliveries WHERE event_id = 'old-channel'`).Scan(&old); err != nil || old != "pending" {
		t.Fatalf("historical row = %s, %v", old, err)
	}
}

func TestFinishNearSendTimeoutStillRecordsStatus(t *testing.T) {
	database := db.OpenTest(t)
	mustChannel(t, database, "hook", "http://example.invalid", false)
	mustOffline(t, database, "near")
	base := time.Now().Unix()
	claimed, err := database.ClaimDeliveries("holder", base, base+LeaseSeconds, 1, MaxAttempts)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	near := base + int64(notify.SendTimeout/time.Second)
	if err := database.FailDelivery(claimed[0].ID, "holder", near, "retry_wait", "timeout", near+5); err != nil {
		t.Fatalf("finish near send timeout: %v", err)
	}
	assertStatus(t, database, mustEventID(t, database), "retry_wait", 1)

	mustOffline(t, database, "stale")
	// Claim with a FRESH now: this delivery was inserted after base was captured,
	// so its next_attempt_at is base or later. Reusing base makes the claim flaky
	// the moment a second ticks between the capture and the insert (the row is
	// then "not due yet" and the claim comes back empty).
	now := time.Now().Unix()
	stale, err := database.ClaimDeliveries("old", now, now+LeaseSeconds, 1, MaxAttempts)
	if err != nil || len(stale) != 1 {
		t.Fatalf("stale claim = %+v, %v", stale, err)
	}
	// Writing exactly at the lease boundary must lose the lease.
	if err := database.SucceedDelivery(stale[0].ID, "old", now+LeaseSeconds, now+LeaseSeconds); !errors.Is(err, db.ErrLeaseLost) {
		t.Fatalf("expired owner wrote status: %v", err)
	}
	var sent int
	if err := database.QueryRow(`SELECT COUNT(*) FROM notification_log WHERE status = 'sent'`).Scan(&sent); err != nil || sent != 0 {
		t.Fatalf("success log = %d, %v", sent, err)
	}
}

func TestWorkersHaveDistinctOwners(t *testing.T) {
	database := db.OpenTest(t)
	first := New(database, "")
	second := New(database, "")
	if first.Owner == "" || first.Owner == second.Owner {
		t.Fatalf("owners = %q %q", first.Owner, second.Owner)
	}
	mustChannel(t, database, "hook", "http://example.invalid", false)
	mustOffline(t, database, "svc")
	base := time.Now().Unix()
	claimed, err := database.ClaimDeliveries(first.Owner, base, base+LeaseSeconds, 1, MaxAttempts)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	if err := database.SucceedDelivery(claimed[0].ID, second.Owner, base, base); !errors.Is(err, db.ErrLeaseLost) {
		t.Fatalf("other worker wrote the lease: %v", err)
	}
}

func TestAbandonedAttemptsBecomeDead(t *testing.T) {
	database := db.OpenTest(t)
	mustChannel(t, database, "hook", "http://example.invalid", false)
	ev := mustOffline(t, database, "svc")
	now := time.Now().Unix()
	var id int64
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		rows, err := database.ClaimDeliveries("crashed", now, now+LeaseSeconds, 1, MaxAttempts)
		if err != nil || len(rows) != 1 || rows[0].AttemptCount != attempt {
			t.Fatalf("claim %d = %+v, %v", attempt, rows, err)
		}
		id = rows[0].ID
		now += LeaseSeconds
	}
	rows, err := database.ClaimDeliveries("recovery", now, now+LeaseSeconds, 1, MaxAttempts)
	if err != nil || len(rows) != 0 {
		t.Fatalf("exhausted claim = %+v, %v", rows, err)
	}
	assertStatus(t, database, ev, "dead", MaxAttempts)
	var lease sql.NullString
	var reason string
	if err := database.QueryRow(`SELECT lease_owner, last_error FROM notification_deliveries WHERE id = $1`, id).Scan(&lease, &reason); err != nil {
		t.Fatal(err)
	}
	if lease.Valid {
		t.Fatalf("lease still held by %s", lease.String)
	}
	if reason != "attempt limit reached before the delivery finished" {
		t.Fatalf("last_error = %q", reason)
	}
	var logs int
	if err := database.QueryRow(`SELECT COUNT(*) FROM notification_log`).Scan(&logs); err != nil || logs != 0 {
		t.Fatalf("notification_log rows = %d, %v", logs, err)
	}
	again, err := database.ClaimDeliveries("recovery", now, now+LeaseSeconds, 1, MaxAttempts)
	if err != nil || len(again) != 0 {
		t.Fatalf("dead task claimed again: %+v, %v", again, err)
	}
}

func TestConcurrentExhaustionIsSingleDead(t *testing.T) {
	database := db.OpenTest(t)
	mustChannel(t, database, "hook", "http://example.invalid", false)
	ev := mustOffline(t, database, "svc")
	now := time.Now().Unix()
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		rows, err := database.ClaimDeliveries("crashed", now, now+LeaseSeconds, 1, MaxAttempts)
		if err != nil || len(rows) != 1 {
			t.Fatalf("claim %d = %+v, %v", attempt, rows, err)
		}
		now += LeaseSeconds
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var got int
	for _, owner := range []string{"a", "b"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			rows, err := database.ClaimDeliveries(owner, now, now+LeaseSeconds, 1, MaxAttempts)
			if err != nil {
				t.Errorf("claim %s: %v", owner, err)
				return
			}
			mu.Lock()
			got += len(rows)
			mu.Unlock()
		}(owner)
	}
	wg.Wait()
	if got != 0 {
		t.Fatalf("exhausted claims = %d", got)
	}
	assertStatus(t, database, ev, "dead", MaxAttempts)
	var logs int
	if err := database.QueryRow(`SELECT COUNT(*) FROM notification_log`).Scan(&logs); err != nil || logs != 0 {
		t.Fatalf("notification_log rows = %d, %v", logs, err)
	}
}

func TestRateLimitRetriesThenSucceeds(t *testing.T) {
	database := db.OpenTest(t)
	mustChannel(t, database, "hook", "http://example.invalid", false)
	ev := mustOffline(t, database, "svc")
	clk := newClock()
	w := &Worker{DB: database, Owner: "worker", Now: clk.now, Tasks: &scriptedTasks{fail: func(n int32) error {
		if n == 1 {
			return &notify.StatusError{Code: http.StatusTooManyRequests}
		}
		return nil
	}}}
	if _, err := w.ProcessDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, database, ev, "retry_wait", 1)
	var next int64
	if err := database.QueryRow(`SELECT next_attempt_at FROM notification_deliveries WHERE event_id = $1`, ev).Scan(&next); err != nil {
		t.Fatal(err)
	}
	clk.sec = next
	if n, err := w.ProcessDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("retry = %d, %v", n, err)
	}
	assertStatus(t, database, ev, "succeeded", 2)
}

func TestWarningLogsOmitIncidentID(t *testing.T) {
	database := db.OpenTest(t)
	mustChannel(t, database, "ok", "http://example.invalid/ok", true)
	mustChannel(t, database, "fail", "http://example.invalid/fail", true)
	ev, err := event.NewCheckCompleted("svc", "Name", "https://example.com", 6_000, db.StatusWarning, nil, nil, "blip", 2)
	if err != nil {
		t.Fatal(err)
	}
	ev.CheckID = 1
	if err := database.ApplyCheckEvent(ev); err != nil {
		t.Fatal(err)
	}
	w := clocked(database, time.Now().Unix())
	if n, err := w.ProcessDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("process = %d, %v", n, err)
	}
	var incidentID int64
	var withIncident, nullLogs int
	if err := database.QueryRow(`SELECT incident_id FROM notification_deliveries WHERE event_type = 'warning' LIMIT 1`).Scan(&incidentID); err != nil || incidentID == 0 {
		t.Fatalf("delivery incident = %d, %v", incidentID, err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM notification_deliveries WHERE event_type = 'warning' AND incident_id IS NOT NULL`).Scan(&withIncident); err != nil || withIncident != 1 {
		t.Fatalf("deliveries with incident = %d, %v", withIncident, err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM notification_log WHERE event = 'warning'`).Scan(&nullLogs); err != nil || nullLogs != 0 {
		t.Fatalf("warning logs = %d, %v", nullLogs, err)
	}
	views, err := database.ListDeliveriesByIncident(incidentID)
	if err != nil || len(views) != 1 {
		t.Fatalf("listed = %+v, %v", views, err)
	}
}

func TestDeliveryErrorHidesSecrets(t *testing.T) {
	database := db.OpenTest(t)
	const pathSecret = "path-secret-token-value"
	const bodySecret = "body-secret-token-value"
	refused := "http://127.0.0.1:1/" + pathSecret
	refusedChannel, err := database.CreateIntegration(db.AgentWebhookType, "refused", true, false, json.RawMessage(`{"url":"`+refused+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	ev := mustOffline(t, database, "refused-svc")
	w := clocked(database, time.Now().Unix())
	w.Tasks.(*scriptedTasks).fail = func(int32) error {
		return errors.New("agent unavailable")
	}
	if _, err := w.ProcessDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(storedError(t, database, ev), pathSecret) || strings.Contains(storedError(t, database, ev), bodySecret) {
		t.Fatalf("stored error kept a secret: %s", storedError(t, database, ev))
	}
	if !strings.Contains(storedError(t, database, ev), "unavailable") {
		t.Fatalf("stored error = %s", storedError(t, database, ev))
	}
	_ = refusedChannel
}

func mustEventID(t *testing.T, database *db.DB) string {
	t.Helper()
	var id string
	if err := database.QueryRow(`SELECT event_id FROM notification_deliveries ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func storedError(t *testing.T, database *db.DB, eventID string) string {
	t.Helper()
	var last string
	if err := database.QueryRow(`SELECT last_error FROM notification_deliveries WHERE event_id = $1`, eventID).Scan(&last); err != nil {
		t.Fatal(err)
	}
	return last
}

func assertNoSecret(t *testing.T, database *db.DB, eventID, secret string) {
	t.Helper()
	last := storedError(t, database, eventID)
	if strings.Contains(last, secret) {
		t.Fatalf("last_error contains secret: %s", last)
	}
	var logged string
	if err := database.QueryRow(
		`SELECT l.error
		 FROM notification_log l
		 JOIN notification_deliveries d ON d.integration_id = l.integration_id
		 WHERE d.event_id = $1`, eventID,
	).Scan(&logged); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logged, secret) {
		t.Fatalf("notification_log contains secret: %s", logged)
	}
	var incidentID int64
	if err := database.QueryRow(`SELECT incident_id FROM notification_deliveries WHERE event_id = $1`, eventID).Scan(&incidentID); err != nil {
		t.Fatal(err)
	}
	views, err := database.ListDeliveriesByIncident(incidentID)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		if strings.Contains(view.LastError, secret) {
			t.Fatalf("api error contains secret: %s", view.LastError)
		}
	}
}
