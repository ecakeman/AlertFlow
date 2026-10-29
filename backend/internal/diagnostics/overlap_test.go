package diagnostics

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
)

func TestDiagnosticWindowOverlapsFiveMinutes(t *testing.T) {
	database := db.OpenTest(t)
	fixed := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	var gotSince, gotUntil string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSince = r.URL.Query().Get("since")
		gotUntil = r.URL.Query().Get("until")
		http.Error(w, "down", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	collector := New(database)
	collector.now = func() time.Time { return fixed }
	svc := config.Service{ID: "svc", Diagnostics: config.ServiceDiagnostics{URL: srv.URL, Interval: 60}}
	if err := collector.Poll(t.Context(), svc); err != nil {
		t.Fatal(err)
	}
	if gotUntil != "2026-10-28T12:00:00Z" || gotSince != "2026-10-28T11:59:00Z" {
		t.Fatalf("first window since=%s until=%s", gotSince, gotUntil)
	}
	if _, err := database.Exec(
		`UPDATE runtime_source_state SET last_success_at = $1, status = 'ok' WHERE service_id = 'svc'`,
		fixed.Unix(),
	); err != nil {
		t.Fatal(err)
	}
	collector.now = func() time.Time { return fixed.Add(time.Hour) }
	if err := collector.Poll(t.Context(), svc); err != nil {
		t.Fatal(err)
	}
	if gotSince != "2026-10-28T11:55:00Z" || gotUntil != "2026-10-28T13:00:00Z" {
		t.Fatalf("overlap window since=%s until=%s", gotSince, gotUntil)
	}
	if last, err := database.LastDiagnosticSuccess("svc"); err != nil || last != fixed.Unix() {
		t.Fatalf("failed pull moved the watermark to %d (%v)", last, err)
	}
}

func TestOverlapDedupAndLateItem(t *testing.T) {
	database := db.OpenTest(t)
	okBody := readFixture(t, "ok.json")
	late := []byte(`{"schema_version":"v2","items":[{"kind":"recent_error","name":"db","summary":"connection timed out","observed_at":"2026-10-28T12:00:00Z"},{"kind":"recent_error","name":"cache","summary":"late","observed_at":"2026-10-28T11:56:00Z"}]}`)
	body := okBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	collector := New(database)
	start := time.Date(2026, 10, 28, 12, 1, 0, 0, time.UTC)
	collector.now = func() time.Time { return start }
	svc := config.Service{ID: "svc", Diagnostics: config.ServiceDiagnostics{URL: srv.URL, Interval: 60}}
	if err := collector.Poll(t.Context(), svc); err != nil {
		t.Fatal(err)
	}
	if err := collector.Poll(t.Context(), svc); err != nil {
		t.Fatal(err)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM runtime_events`); n != 1 {
		t.Fatalf("duplicate overlap wrote %d events", n)
	}
	body = late
	collector.now = func() time.Time { return start.Add(time.Minute) }
	if err := collector.Poll(t.Context(), svc); err != nil {
		t.Fatal(err)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM runtime_events WHERE summary = 'late'`); n != 1 {
		t.Fatalf("late item count = %d", n)
	}
	if n := count(t, database, `SELECT COUNT(*) FROM runtime_events`); n != 2 {
		t.Fatalf("events = %d", n)
	}
}

func TestSharedDiagnosticsFixtures(t *testing.T) {
	when := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	if got := ParseSnapshot(readFixture(t, "ok.json"), when).Coverage; got != "ok" {
		t.Fatalf("ok = %s", got)
	}
	if got := ParseSnapshot(readFixture(t, "empty.json"), when).Coverage; got != "empty" {
		t.Fatalf("empty = %s", got)
	}
	if got := ParseSnapshot(readFixture(t, "partial.json"), when).Coverage; got != "partial" {
		t.Fatalf("partial = %s", got)
	}
	if got := ParseSnapshot(readFixture(t, "invalid.json"), when).Coverage; got != "invalid" {
		t.Fatalf("invalid = %s", got)
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "..", "docs", "contracts", "fixtures", "application-diagnostics-v2", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
