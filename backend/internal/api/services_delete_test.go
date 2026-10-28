package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
)

func TestDeleteServiceRemovesHistoryBeforeConfig(t *testing.T) {
	database := db.OpenTest(t)
	dir := t.TempDir()
	cfg := &config.Config{Services: []config.Service{{
		ID: "svc", Name: "API", URL: "http://127.0.0.1:1",
		Check: config.ServiceCheck{Interval: 3600, Timeout: 1, RetryAttempts: 1},
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
	if _, err := database.CreateManualIncident(db.ManualIncident{ServiceID: "svc", ServiceName: "API", StartedAt: now - 3600}); err != nil {
		t.Fatal(err)
	}
	if err := database.InsertCheck("svc", now, db.StatusOffline, nil, nil, "down", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(
		`INSERT INTO service_runtime_state (service_id, current_status, last_event_id, last_check_id, last_event_at, updated_at)
		 VALUES ('svc', 'offline', 'evt', 1, $1, $1)`, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(
		`INSERT INTO outbox_events (event_id, event_type, schema_version, message_key, payload, created_at, next_attempt_at)
		 VALUES ('evt-svc', 'monitor.check.completed', 1, 'svc', '{"service_id":"svc"}', $1, $1)`, now,
	); err != nil {
		t.Fatal(err)
	}

	status, _ := integrationRequest(t, app, http.MethodDelete, "/api/services/svc", token, "")
	if status != http.StatusNoContent {
		t.Fatalf("delete = %d", status)
	}
	if s.config().Find("svc") != nil {
		t.Fatal("service still in config")
	}
	onDisk, err := config.Load(dir)
	if err != nil || onDisk.Find("svc") != nil {
		t.Fatalf("config on disk = %v, %v", onDisk, err)
	}
	for _, table := range []string{"incidents", "runtime_events", "checks", "service_runtime_state", "outbox_events"} {
		var n int
		if err := database.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s rows = %d, %v", table, n, err)
		}
	}
}

func TestDeleteServiceKeepsConfigWhenCleanupFails(t *testing.T) {
	database := db.OpenTest(t)
	dir := t.TempDir()
	cfg := &config.Config{Services: []config.Service{{
		ID: "svc", Name: "API", URL: "https://example.com",
	}}}
	if err := config.Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{configDir: dir, cfg: loaded, database: database}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/services/svc", nil)
	c.Params = gin.Params{{Key: "id", Value: "svc"}}

	err = s.handleDeleteService(c)
	if err == nil {
		t.Fatal("delete succeeded after the database was closed")
	}
	if s.config().Find("svc") == nil {
		t.Fatal("config dropped the service after cleanup failed")
	}
	onDisk, err := config.Load(dir)
	if err != nil || onDisk.Find("svc") == nil {
		t.Fatalf("config on disk = %v, %v", onDisk, err)
	}
}

func TestUpdateServiceKeepsDiagnostics(t *testing.T) {
	database := db.OpenTest(t)
	dir := t.TempDir()
	cfg := &config.Config{Services: []config.Service{{
		ID: "svc", Name: "API", URL: "https://example.com",
		Check: config.ServiceCheck{Interval: 3600, Timeout: 1, RetryAttempts: 1},
		Diagnostics: config.ServiceDiagnostics{
			URL: "https://diag.example/snapshot", TokenEnv: "DIAGNOSTICS_TOKEN", Interval: 60,
		},
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
	user, err := database.CreateUser("admin", "hash", "admin")
	if err != nil {
		t.Fatal(err)
	}
	const token = "session-token"
	now := time.Now().Unix()
	if err := database.CreateSession(token, user.ID, now, now+3600); err != nil {
		t.Fatal(err)
	}
	status, body := integrationRequest(t, app, http.MethodPut, "/api/services/svc", token,
		`{"name":"Renamed","url":"https://example.com","interval":3600}`)
	if status != http.StatusOK {
		t.Fatalf("update = %d %s", status, body)
	}
	onDisk, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := onDisk.Find("svc")
	if svc == nil || svc.Name != "Renamed" || svc.Diagnostics.URL != "https://diag.example/snapshot" || svc.Diagnostics.TokenEnv != "DIAGNOSTICS_TOKEN" {
		t.Fatalf("service = %+v", svc)
	}
	if svc.Kubernetes.WorkloadName != "api" || svc.Kubernetes.Namespace != "lab" {
		t.Fatalf("kubernetes = %+v", svc.Kubernetes)
	}
}
