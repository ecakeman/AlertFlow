package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ecakeman/alertflow/backend/internal/db"
	"github.com/ecakeman/alertflow/backend/internal/system"
	"github.com/google/uuid"
)

func testAPI(t *testing.T) *Server {
	t.Helper()
	conn, err := db.Open("postgres://alertflow:alertflow@127.0.0.1:55433/alertflow?sslmode=disable")
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.Exec(`TRUNCATE
		system.episode_input_versions, system.episode_artifact_refs, system.outbox_events,
		system.agent_tasks, system.episode_app_events, system.application_event_receipts,
		system.episodes, system.service_monitor_points, system.service_source_states,
		system.service_monitor_target_states, system.service_monitor_targets, system.services
		CASCADE`); err != nil {
		t.Fatal(err)
	}
	return &Server{Store: system.New(conn)}
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status %d body %s: %v", rec.Code, rec.Body.String(), err)
	}
	if body["error_code"] == "" || body["message"] == "" {
		t.Fatalf("error body = %#v", body)
	}
	return body
}

func TestRoutesAndMethodNotAllowed(t *testing.T) {
	h := testAPI(t).Handler()
	routes := []struct{ method, path string }{
		{http.MethodGet, "/healthz"},
		{http.MethodPost, "/api/v1/services"},
		{http.MethodGet, "/api/v1/services/" + uuid.NewString()},
		{http.MethodPatch, "/api/v1/services/" + uuid.NewString()},
		{http.MethodGet, "/api/v1/services/" + uuid.NewString() + "/timeline"},
		{http.MethodPost, "/api/v1/services/" + uuid.NewString() + "/events"},
		{http.MethodGet, "/api/v1/episodes/" + uuid.NewString()},
		{http.MethodGet, "/api/v1/episodes/" + uuid.NewString() + "/timeline"},
		{http.MethodGet, "/api/v1/episodes/" + uuid.NewString() + "/artifacts"},
		{http.MethodPost, "/api/v1/episodes/" + uuid.NewString() + "/retry-diagnosis"},
		{http.MethodPost, "/api/v1/episodes/" + uuid.NewString() + "/retry-archive"},
	}
	for _, route := range routes {
		rec := do(t, h, route.method, route.path, "")
		if rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), "404 page not found") {
			t.Fatalf("%s %s was not matched", route.method, route.path)
		}
	}
	rec := do(t, h, http.MethodDelete, "/healthz", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method mismatch = %d", rec.Code)
	}
	if got := do(t, h, http.MethodGet, "/healthz", ""); got.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing generated request id")
	}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "req-fixed")
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Header().Get("X-Request-ID") != "req-fixed" {
		t.Fatalf("request id = %s", got.Header().Get("X-Request-ID"))
	}
}

func TestCreateAndGetService(t *testing.T) {
	h := testAPI(t).Handler()
	body := `{"service_key":"orders-api","name":"orders","authority_producer_id":"order-api","targets":[{"target_key":"api","target_name":"API","target_type":"HTTP","address":"http://orders/health","interval_ms":1000,"timeout_ms":200,"failure_threshold":2,"recovery_threshold":1}]}`
	rec := do(t, h, http.MethodPost, "/api/v1/services", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %d %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id, _ := created["service_id"].(string)
	got := do(t, h, http.MethodGet, "/api/v1/services/"+id, "")
	if got.Code != http.StatusOK {
		t.Fatalf("get %d %s", got.Code, got.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(got.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view["service_key"] != "orders-api" || view["monitor_status"] != "UNKNOWN" {
		t.Fatalf("view = %#v", view)
	}
}

func TestValidationErrors(t *testing.T) {
	srv := testAPI(t)
	h := srv.Handler()
	if rec := do(t, h, http.MethodGet, "/api/v1/services/not-a-uuid", ""); rec.Code != 400 || errBody(t, rec)["message"] != "invalid service_id" {
		t.Fatalf("uuid %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodPost, "/api/v1/services", "{"); rec.Code != 400 || errBody(t, rec)["error_code"] != "VALIDATION" {
		t.Fatalf("json %d %s", rec.Code, rec.Body.String())
	}
	unknown := `{"service_key":"a","name":"b","authority_producer_id":"c","unknown":1,"targets":[{"target_key":"api","target_name":"API","target_type":"HTTP","address":"http://x","interval_ms":1,"timeout_ms":1,"failure_threshold":1,"recovery_threshold":1}]}`
	if rec := do(t, h, http.MethodPost, "/api/v1/services", unknown); rec.Code != 400 || errBody(t, rec)["error_code"] != "VALIDATION" {
		t.Fatalf("unknown field %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/episodes/not-a-uuid", ""); rec.Code != 400 || errBody(t, rec)["message"] != "invalid episode_id" {
		t.Fatalf("episode uuid %d %s", rec.Code, rec.Body.String())
	}
	missing := uuid.NewString()
	if rec := do(t, h, http.MethodGet, "/api/v1/episodes/"+missing, ""); errBody(t, rec)["error_code"] == "" {
		t.Fatal("episode error has no code")
	}
	if rec := do(t, h, http.MethodPost, "/api/v1/episodes/"+missing+"/retry-diagnosis", ""); errBody(t, rec)["message"] == "" {
		t.Fatal("retry diagnosis has no message")
	}
	if rec := do(t, h, http.MethodPost, "/api/v1/episodes/"+missing+"/retry-archive", ""); errBody(t, rec)["message"] == "" {
		t.Fatal("retry archive has no message")
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/episodes/"+missing+"/artifacts", ""); rec.Code != http.StatusServiceUnavailable || errBody(t, rec)["error_code"] != "AGENT_UNAVAILABLE" {
		t.Fatalf("artifact %d %s", rec.Code, rec.Body.String())
	}
	srv.Artifacts = artifactStub{}
	h = srv.Handler()
	rec := do(t, h, http.MethodGet, "/api/v1/episodes/"+missing+"/artifacts?type=DIAGNOSIS_REPORT", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("artifact ok %d %s", rec.Code, rec.Body.String())
	}
	var artifact map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact["content_text"] != "investigation" {
		t.Fatalf("artifact = %#v", artifact)
	}
}

func TestEventBodyLimit(t *testing.T) {
	h := testAPI(t).Handler()
	id := uuid.NewString()
	payload := `{"event_id":"e","producer_id":"p","event_time":"2026-11-01T00:00:00Z","event_kind":"DIAGNOSTIC_INFO","schema_version":1,"payload":{"message":"` + strings.Repeat("a", 1<<20) + `"}}`
	if len(payload) <= 1<<20 {
		t.Fatal("fixture is not over the limit")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/services/"+id+"/events", bytes.NewReader([]byte(payload)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 || errBody(t, rec)["message"] != "invalid json" {
		t.Fatalf("oversize %d %s", rec.Code, rec.Body.String())
	}
}

type artifactStub struct{}

func (artifactStub) GetArtifact(*http.Request, uuid.UUID, string) (any, error) {
	return map[string]any{"content_text": "investigation"}, nil
}
