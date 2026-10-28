package diagnostics

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
)

func TestPollRecordsAbnormalItemsOnly(t *testing.T) {
	database := db.OpenTest(t)
	observed := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	later := time.Now().UTC().Format(time.RFC3339)
	var body atomic.Value
	body.Store(`{"schema_version":"v1","items":[]}`)
	var sawToken atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("since") == "" || r.URL.Query().Get("until") == "" {
			t.Errorf("missing window")
		}
		if r.Header.Get("Authorization") == "Bearer secret" {
			sawToken.Store(true)
		}
		payload := body.Load().(string)
		if payload == "down" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)

	t.Setenv("DIAGNOSTICS_TOKEN", "secret")
	collector := New(database)
	svc := config.Service{
		ID: "svc", Name: "API", URL: "https://example.com",
		Diagnostics: config.ServiceDiagnostics{URL: srv.URL, TokenEnv: "DIAGNOSTICS_TOKEN", Interval: 60},
	}

	body.Store("down")
	if err := collector.Poll(t.Context(), svc); err != nil {
		t.Fatal(err)
	}
	if status := sourceStatus(t, database); status != "unavailable" {
		t.Fatalf("status = %s", status)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM runtime_events`); n != 0 {
		t.Fatalf("failure wrote events: %d", n)
	}

	body.Store(`{"schema_version":"nope"}`)
	if err := collector.Poll(t.Context(), svc); err != nil {
		t.Fatal(err)
	}
	if status := sourceStatus(t, database); status != "invalid" {
		t.Fatalf("status = %s", status)
	}

	body.Store(`{"schema_version":"v2","items":[{"kind":"component_health","name":"db","status":"healthy","observed_at":"` + observed + `"}]}`)
	if err := collector.Poll(t.Context(), svc); err != nil {
		t.Fatal(err)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM runtime_events`); n != 0 {
		t.Fatalf("healthy wrote events: %d", n)
	}

	body.Store(`{"schema_version":"v1","items":[{"kind":"component_health","name":"db","status":"unhealthy","observed_at":"` + observed + `","summary":"down"}]}`)
	if err := collector.Poll(t.Context(), svc); err != nil {
		t.Fatal(err)
	}
	if err := collector.Poll(t.Context(), svc); err != nil {
		t.Fatal(err)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'diagnostics.component_health'`); n != 1 {
		t.Fatalf("unhealthy events = %d", n)
	}

	body.Store(`{"schema_version":"v1","items":[{"kind":"dependency_health","name":"cache","status":"degraded","observed_at":"` + observed + `"}]}`)
	if err := collector.Poll(t.Context(), svc); err != nil {
		t.Fatal(err)
	}
	body.Store(`{"schema_version":"v1","items":[{"kind":"dependency_health","name":"cache","status":"healthy","observed_at":"` + later + `"}]}`)
	if err := collector.Poll(t.Context(), svc); err != nil {
		t.Fatal(err)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'diagnostics.dependency_health.recovered'`); n != 1 {
		t.Fatalf("recovered events = %d", n)
	}

	body.Store(`{"schema_version":"v1","items":[{"kind":"recent_error","name":"timeout","summary":"slow","observed_at":"` + later + `"}]}`)
	if err := collector.Poll(t.Context(), svc); err != nil {
		t.Fatal(err)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'diagnostics.recent_error'`); n != 1 {
		t.Fatalf("recent_error events = %d", n)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM incidents`); n != 0 {
		t.Fatalf("incidents = %d", n)
	}
	if !sawToken.Load() {
		t.Fatal("token was not sent")
	}
	var occurred, seen int64
	if err := database.QueryRow(`SELECT occurred_at, observed_at FROM runtime_events WHERE event_type = 'diagnostics.component_health'`).Scan(&occurred, &seen); err != nil {
		t.Fatal(err)
	}
	itemAt, err := time.Parse(time.RFC3339, observed)
	if err != nil {
		t.Fatal(err)
	}
	if occurred != itemAt.Unix() || seen <= occurred {
		t.Fatalf("occurred %d observed %d item %d", occurred, seen, itemAt.Unix())
	}
}

func TestSaturationSnapshotWritesTimeline(t *testing.T) {
	database := db.OpenTest(t)
	observed := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	var body atomic.Value
	body.Store(`{"schema_version":"v2","items":[{"kind":"saturation","name":"pool"}]}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	t.Cleanup(srv.Close)
	collector := New(database)
	svc := config.Service{
		ID: "svc", Name: "API", URL: "https://example.com",
		Diagnostics: config.ServiceDiagnostics{URL: srv.URL, Interval: 60},
	}
	if err := collector.Poll(t.Context(), svc); err != nil {
		t.Fatal(err)
	}
	if status := sourceStatus(t, database); status != "invalid" {
		t.Fatalf("status = %s", status)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM runtime_events`); n != 0 {
		t.Fatalf("statusless saturation wrote events: %d", n)
	}
	body.Store(`{"schema_version":"v2","items":[{"kind":"saturation","name":"pool","status":"unhealthy","observed_at":"` + observed + `","summary":"full"}]}`)
	if err := collector.Poll(t.Context(), svc); err != nil {
		t.Fatal(err)
	}
	var severity string
	if err := database.QueryRow(`SELECT severity FROM runtime_events WHERE event_type = 'diagnostics.saturation'`).Scan(&severity); err != nil {
		t.Fatal(err)
	}
	if severity != "error" {
		t.Fatalf("severity = %s", severity)
	}
	later := time.Now().UTC().Format(time.RFC3339)
	body.Store(`{"schema_version":"v2","items":[{"kind":"saturation","name":"pool","status":"healthy","observed_at":"` + later + `"}]}`)
	if err := collector.Poll(t.Context(), svc); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT severity FROM runtime_events WHERE event_type = 'diagnostics.saturation.recovered'`).Scan(&severity); err != nil {
		t.Fatal(err)
	}
	if severity != "info" {
		t.Fatalf("recovery severity = %s", severity)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM incidents`); n != 0 {
		t.Fatalf("incidents = %d", n)
	}
}

func TestSyncWaitsUntilTheOldWorkerExits(t *testing.T) {
	database := db.OpenTest(t)
	observed := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	later := time.Now().UTC().Format(time.RFC3339)
	firstBody := `{"schema_version":"v2","items":[{"kind":"saturation","name":"pool","status":"unhealthy","observed_at":"` + observed + `"}]}`
	secondBody := `{"schema_version":"v2","items":[{"kind":"saturation","name":"pool","status":"degraded","observed_at":"` + later + `"}]}`
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(firstBody))
	}))
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(secondBody))
	}))
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)
	hold := make(chan struct{})
	var inflight atomic.Int32
	var peak atomic.Int32
	collector := New(database)
	t.Cleanup(collector.Stop)
	collector.beforeStore = func() {
		n := inflight.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-hold
		inflight.Add(-1)
	}
	svc := config.Service{
		ID: "svc", Name: "API", URL: "https://example.com",
		Diagnostics: config.ServiceDiagnostics{URL: first.URL, Interval: 60},
	}
	collector.Sync([]config.Service{svc})
	waitInflight(t, &inflight)
	replaced := make(chan struct{})
	go func() {
		next := svc
		next.Diagnostics.URL = second.URL
		collector.Sync([]config.Service{next})
		close(replaced)
	}()
	select {
	case <-replaced:
		t.Fatal("replacement returned while the previous poll was still inside beforeStore")
	case <-time.After(150 * time.Millisecond):
	}
	if peak.Load() != 1 {
		t.Fatalf("overlapping polls = %d", peak.Load())
	}
	close(hold)
	select {
	case <-replaced:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not finish after the old poll was released")
	}
	if peak.Load() != 1 {
		t.Fatalf("peak polls = %d", peak.Load())
	}
	waitForCount(t, database, `SELECT COUNT(*) FROM runtime_events`, 1)
	var severity string
	if err := database.QueryRow(`SELECT severity FROM runtime_events WHERE event_type = 'diagnostics.saturation'`).Scan(&severity); err != nil {
		t.Fatal(err)
	}
	if severity != "warning" {
		t.Fatalf("replacement severity = %s, want warning for degraded", severity)
	}
	removed := make(chan struct{})
	go func() {
		collector.Sync(nil)
		close(removed)
	}()
	select {
	case <-removed:
	case <-time.After(5 * time.Second):
		t.Fatal("removing the collector did not wait for its worker")
	}
	before := count(t, database, `SELECT COUNT(*) FROM runtime_events`)
	time.Sleep(200 * time.Millisecond)
	if n := count(t, database, `SELECT COUNT(*) FROM runtime_events`); n != before {
		t.Fatalf("events grew after the collector was removed: %d -> %d", before, n)
	}
}

func waitForCount(t *testing.T, database *db.DB, query string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if n := count(t, database, query); n == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("count = %d, want %d", n, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitInflight(t *testing.T, inflight *atomic.Int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for inflight.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("poll did not reach beforeStore")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func sourceStatus(t *testing.T, database *db.DB) string {
	t.Helper()
	var status string
	if err := database.QueryRow(`SELECT status FROM runtime_source_state WHERE service_id = 'svc'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func count(t *testing.T, database *db.DB, query string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
