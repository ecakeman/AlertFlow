package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
)

func TestRetiredAgentHTTPLeavesBrowserAPI(t *testing.T) {
	database := db.OpenTest(t)
	s := &Server{
		cfg:      &config.Config{Services: []config.Service{{ID: "svc", Name: "API", URL: "https://example.com/health"}}},
		database: database,
	}
	attachQuery(s)
	app := s.newEngine()

	for _, path := range []string{"/webhooks/agent", "/internal/agent/v1/incidents", "/internal/agent/v1/services/svc/checks"} {
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "<html") {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhooks/agent", strings.NewReader(`{}`)))
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("callback = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/setup/status", nil))
	if rec.Code == http.StatusNotFound || strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("setup = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/services", nil))
	if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("services = %d %s", rec.Code, rec.Body.String())
	}
}
