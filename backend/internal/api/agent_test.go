package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/query"
)

// attachQuery wires the read-composition layer onto a Server built by hand.
// New() does this in production; the test helpers construct the struct directly
// (they have no config dir on disk), so they must attach it themselves before
// routes() is called.
func attachQuery(s *Server) {
	s.queries = query.New(s.database, s.config)
}

func TestAgentRoutesRequireToken(t *testing.T) {
	app := agentApp(t, "")
	status, body := getAgent(t, app, "/internal/agent/v1/incidents", "")
	if status != http.StatusServiceUnavailable || strings.Contains(body, "<html") {
		t.Fatalf("unconfigured = %d %s", status, body)
	}
	status, body = getAgent(t, app, "/internal/agent/v1/missing", "Bearer secret")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured unknown = %d %s", status, body)
	}
}

func TestAgentRoutesReadWithoutSession(t *testing.T) {
	app := agentApp(t, "agent-secret")
	status, body := getAgent(t, app, "/internal/agent/v1/incidents", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("missing = %d %s", status, body)
	}
	status, body = getAgent(t, app, "/internal/agent/v1/incidents", "Bearer wrong")
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong = %d %s", status, body)
	}
	status, body = getAgent(t, app, "/internal/agent/v1/incidents", "Bearer agent-secret")
	if status != http.StatusOK || !strings.Contains(body, "incidents") {
		t.Fatalf("list = %d %s", status, body)
	}
	status, body = getAgent(t, app, "/internal/agent/v1/nope", "Bearer agent-secret")
	if status != http.StatusNotFound || strings.Contains(body, "<html") {
		t.Fatalf("unknown = %d %s", status, body)
	}
	status, body = postAgent(t, app, "/internal/agent/v1/incidents", "Bearer agent-secret")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("post = %d %s", status, body)
	}
}

func TestWrongMethodKeepsTheOldFallback(t *testing.T) {
	app := agentApp(t, "agent-secret")

	status, body := requestAgent(t, app, http.MethodDelete, "/api/services", "")
	if status != http.StatusNotFound || strings.Contains(body, "<html") {
		t.Fatalf("api wrong method = %d %s", status, body)
	}
	status, body = requestAgent(t, app, http.MethodGet, "/webhooks/agent", "")
	if status != http.StatusNotFound || strings.Contains(body, "<html") {
		t.Fatalf("webhook wrong method = %d %s", status, body)
	}

	status, body = postAgent(t, app, "/internal/agent/v1/incidents", "")
	if status != http.StatusUnauthorized || strings.Contains(body, "<html") {
		t.Fatalf("internal wrong method without token = %d %s", status, body)
	}
	status, body = postAgent(t, app, "/internal/agent/v1/incidents", "Bearer wrong")
	if status != http.StatusUnauthorized {
		t.Fatalf("internal wrong method with bad token = %d %s", status, body)
	}
	status, body = postAgent(t, app, "/internal/agent/v1/missing", "Bearer agent-secret")
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("internal unknown post = %d %s", status, body)
	}

	unconfigured := agentApp(t, "")
	status, body = postAgent(t, unconfigured, "/internal/agent/v1/incidents", "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("internal wrong method with empty token = %d %s", status, body)
	}
}

func requestAgent(t *testing.T, app *gin.Engine, method, path, auth string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := serve(app, req)
	return rec.Code, rec.Body.String()
}

func agentApp(t *testing.T, token string) *gin.Engine {
	t.Helper()
	database := db.OpenTest(t)
	s := &Server{
		cfg: &config.Config{Services: []config.Service{{
			ID: "svc", Name: "API", URL: "https://example.com/health",
		}}},
		database:   database,
		agentToken: token,
	}
	attachQuery(s)
	return s.newEngine()
}

func serve(app *gin.Engine, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)
	return rec
}

func getAgent(t *testing.T, app *gin.Engine, path, auth string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := serve(app, req)
	return rec.Code, rec.Body.String()
}

func postAgent(t *testing.T, app *gin.Engine, path, auth string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	rec := serve(app, req)
	return rec.Code, rec.Body.String()
}
