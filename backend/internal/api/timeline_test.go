package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
)

func TestServiceTimelinePagesAndCoverage(t *testing.T) {
	database := db.OpenTest(t)
	dir := t.TempDir()
	cfg := &config.Config{Services: []config.Service{{
		ID: "svc", Name: "API", URL: "https://example.com",
		Diagnostics: config.ServiceDiagnostics{URL: "https://diag.example/snapshot"},
		Kubernetes: config.ServiceKubernetes{
			ClusterID: "local", Namespace: "lab", WorkloadKind: "Deployment", WorkloadName: "api",
		},
	}}}
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{configDir: dir, cfg: loaded, database: database}
	attachQuery(s)
	app := s.newEngine()
	token := "session-token"
	user, err := database.CreateUser("admin", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if err := database.CreateSession(token, user.ID, now, now+3600); err != nil {
		t.Fatal(err)
	}
	for i, ts := range []int64{now - 30, now - 20, now - 10} {
		if _, err := database.Exec(
			`INSERT INTO runtime_events (
				event_key, service_id, source, event_type, severity, title, occurred_at, observed_at, recorded_at, details
			 ) VALUES ($1, 'svc', 'incident', 'incident.opened', 'error', $2, $3, $3, $3, '{"kind":"probe"}')`,
			"k"+strconv.Itoa(i), "event "+strconv.Itoa(i), ts,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Exec(
		`INSERT INTO runtime_source_state (
			service_id, source, enabled, status, last_error_safe, cursor, updated_at
		 ) VALUES ('svc', 'kubernetes', TRUE, 'gap', 'watch expired', '{"secret":"hidden"}', $1)`, now,
	); err != nil {
		t.Fatal(err)
	}

	from := time.Unix(now-3600, 0).UTC().Format(time.RFC3339)
	to := time.Unix(now, 0).UTC().Format(time.RFC3339)
	status, body := integrationRequest(t, app, http.MethodGet, "/api/services/svc/timeline?limit=2&from="+url.QueryEscape(from)+"&to="+url.QueryEscape(to), token, "")
	if status != http.StatusOK {
		t.Fatalf("first page = %d %s", status, body)
	}
	var page struct {
		Events []struct {
			ID int64 `json:"id"`
		} `json:"events"`
		NextCursor *string `json:"nextCursor"`
		Coverage   map[string]struct {
			Status string `json:"status"`
		} `json:"coverage"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.NextCursor == nil {
		t.Fatalf("page = %+v", page)
	}
	if stringsContain(body, "hidden") || stringsContain(body, `"cursor"`) {
		t.Fatalf("response leaked the source cursor: %s", body)
	}
	if page.Coverage["kubernetes"].Status != "gap" || page.Coverage["application_diagnostics"].Status != "empty" {
		t.Fatalf("coverage = %+v", page.Coverage)
	}
	status, body = integrationRequest(t, app, http.MethodGet, "/api/services/svc/timeline?limit=2&from="+url.QueryEscape(from)+"&to="+url.QueryEscape(to)+"&cursor="+url.QueryEscape(*page.NextCursor), token, "")
	if status != http.StatusOK {
		t.Fatalf("second page = %d %s", status, body)
	}
	var next struct {
		Events []struct {
			ID int64 `json:"id"`
		} `json:"events"`
		NextCursor *string `json:"nextCursor"`
	}
	if err := json.Unmarshal([]byte(body), &next); err != nil {
		t.Fatal(err)
	}
	if len(next.Events) != 1 || next.NextCursor != nil || next.Events[0].ID == page.Events[0].ID || next.Events[0].ID == page.Events[1].ID {
		t.Fatalf("next = %+v first = %+v", next, page.Events)
	}

	status, _ = integrationRequest(t, app, http.MethodGet, "/api/services/missing/timeline", token, "")
	if status != http.StatusNotFound {
		t.Fatalf("missing = %d", status)
	}
	status, _ = integrationRequest(t, app, http.MethodGet, "/api/services/svc/timeline?sources=nope", token, "")
	if status != http.StatusBadRequest {
		t.Fatalf("source = %d", status)
	}
	status, _ = integrationRequest(t, app, http.MethodGet, "/api/services/svc/timeline?from="+url.QueryEscape(to)+"&to="+url.QueryEscape(from), token, "")
	if status != http.StatusBadRequest {
		t.Fatalf("range = %d", status)
	}
	status, _ = integrationRequest(t, app, http.MethodGet, "/api/services/svc/timeline?cursor=bm9wZQ", token, "")
	if status != http.StatusBadRequest {
		t.Fatalf("cursor = %d", status)
	}
}

func TestServiceTimelineClampsToRetention(t *testing.T) {
	t.Setenv("ALERTFLOW_TIMELINE_RETENTION_DAYS", "7")
	database := db.OpenTest(t)
	dir := t.TempDir()
	cfg := &config.Config{Services: []config.Service{{ID: "svc", Name: "API", URL: "https://example.com"}}}
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{configDir: dir, cfg: loaded, database: database}
	attachQuery(s)
	app := s.newEngine()
	token := "session-token"
	user, err := database.CreateUser("reader", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := database.CreateSession(token, user.ID, now.Unix(), now.Unix()+3600); err != nil {
		t.Fatal(err)
	}
	from := now.Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	to := now.UTC().Format(time.RFC3339)
	status, body := integrationRequest(t, app, http.MethodGet, "/api/services/svc/timeline?from="+url.QueryEscape(from)+"&to="+url.QueryEscape(to), token, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d %s", status, body)
	}
	var page struct {
		Window struct {
			From          string `json:"from"`
			RetentionDays int    `json:"retentionDays"`
		} `json:"window"`
		Coverage map[string]struct {
			Status string `json:"status"`
		} `json:"coverage"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if page.Window.RetentionDays != 7 {
		t.Fatalf("retention = %d", page.Window.RetentionDays)
	}
	got, err := time.Parse(time.RFC3339, page.Window.From)
	if err != nil {
		t.Fatal(err)
	}
	if got.Before(now.Add(-8*24*time.Hour)) || got.After(now.Add(-6*24*time.Hour)) {
		t.Fatalf("from = %s", page.Window.From)
	}
	if page.Coverage["kubernetes"].Status != "disabled" {
		t.Fatalf("coverage = %+v", page.Coverage)
	}
}

func TestServiceTimelineClipsStoredText(t *testing.T) {
	database := db.OpenTest(t)
	dir := t.TempDir()
	cfg := &config.Config{Services: []config.Service{{ID: "svc", Name: "API", URL: "https://example.com"}}}
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{configDir: dir, cfg: loaded, database: database}
	attachQuery(s)
	app := s.newEngine()
	token := "session-token-long"
	user, err := database.CreateUser("admin", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if err := database.CreateSession(token, user.ID, now, now+3600); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("长", 1100)
	if _, err := database.Exec(
		`INSERT INTO runtime_events (
			event_key, service_id, source, event_type, severity, title, summary, occurred_at, observed_at, recorded_at, details
		 ) VALUES ('long', 'svc', 'incident', 'incident.opened', 'error', $1, $1, $2, $2, $2, '{}')`,
		long, now,
	); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := database.QueryRow(`SELECT summary FROM runtime_events WHERE event_key = 'long'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len([]rune(stored)) != 1100 {
		t.Fatalf("stored runes = %d", len([]rune(stored)))
	}
	from := time.Unix(now-60, 0).UTC().Format(time.RFC3339)
	to := time.Unix(now+60, 0).UTC().Format(time.RFC3339)
	status, body := integrationRequest(t, app, http.MethodGet, "/api/services/svc/timeline?from="+url.QueryEscape(from)+"&to="+url.QueryEscape(to), token, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d %s", status, body)
	}
	var page struct {
		Events []struct {
			Title   string `json:"title"`
			Summary string `json:"summary"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || len([]rune(page.Events[0].Summary)) != 1024 || len([]rune(page.Events[0].Title)) != 1024 {
		t.Fatalf("events = %+v", page.Events)
	}
	if strings.Contains(body, strings.Repeat("长", 1025)) {
		t.Fatal("response kept the tail")
	}
}

func stringsContain(s, part string) bool {
	return len(part) > 0 && len(s) >= len(part) && (s == part || len(s) > 0 && contains(s, part))
}

func contains(s, part string) bool {
	for i := 0; i+len(part) <= len(s); i++ {
		if s[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
