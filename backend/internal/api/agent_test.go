package api

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
)

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

func agentApp(t *testing.T, token string) *fiber.App {
	t.Helper()
	database := db.OpenTest(t)
	s := &Server{
		cfg: &config.Config{Services: []config.Service{{
			ID: "svc", Name: "API", URL: "https://example.com/health",
		}}},
		database:   database,
		agentToken: token,
		webhooks:   newWebhookLimiter(),
	}
	s.app = fiber.New(fiber.Config{BodyLimit: maxBodyBytes, ErrorHandler: errorHandler})
	s.routes()
	return s.app
}

func getAgent(t *testing.T, app *fiber.App, path, auth string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func postAgent(t *testing.T, app *fiber.App, path, auth string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}
