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
