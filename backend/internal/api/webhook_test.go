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

func TestAlertWebhookRejectsBadRequests(t *testing.T) {
	app, _ := webhookApp(t, "")
	status, _ := postAlert(t, app, "", `{}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured status = %d", status)
	}

	app, database := webhookApp(t, "secret-token")
	status, _ = postAlert(t, app, "", `{"source":"grafana"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d", status)
	}
	status, _ = postAlert(t, app, "Bearer wrong", `{"source":"grafana"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", status)
	}
	status, body := postAlert(t, app, "Bearer secret-token", `{`)
	if status != http.StatusBadRequest || !strings.Contains(body, "invalid request body") {
		t.Fatalf("bad json = %d %s", status, body)
	}
	status, body = postAlert(t, app, "Bearer secret-token", `{"source":"grafana","event_id":"e1","alert_key":"cpu","service_id":"missing","status":"firing","occurred_at":10,"title":"CPU"}`)
	if status != http.StatusBadRequest || !strings.Contains(body, "unknown service") {
		t.Fatalf("unknown service = %d %s", status, body)
	}
	status, body = postAlert(t, app, "Bearer secret-token", `{"source":"grafana","event_id":"e1","alert_key":"cpu","service_id":"svc","status":"firing","occurred_at":10,"title":"CPU"}`)
	if status != http.StatusAccepted || !strings.Contains(body, `"accepted":true`) {
		t.Fatalf("accept = %d %s", status, body)
	}
	if n := countAlerts(t, database, `SELECT COUNT(*) FROM incidents`); n != 0 {
		t.Fatalf("webhook opened incidents: %d", n)
	}
	status, body = postAlert(t, app, "Bearer secret-token", `{"source":"grafana","event_id":"e1","alert_key":"cpu","service_id":"svc","status":"firing","occurred_at":10,"title":"CPU"}`)
	if status != http.StatusOK || !strings.Contains(body, `"duplicate":true`) {
		t.Fatalf("duplicate = %d %s", status, body)
	}
	if n := countAlerts(t, database, `SELECT COUNT(*) FROM outbox_events`); n != 1 {
		t.Fatalf("outbox = %d", n)
	}
}

func TestAlertWebhookBodyLimit(t *testing.T) {
	app, _ := webhookApp(t, "secret-token")
	payload := `{"source":"grafana","event_id":"e1","alert_key":"cpu","service_id":"svc","status":"firing","occurred_at":10,"title":"` + strings.Repeat("x", webhookBodyLimit) + `"}`
	status, _ := postAlert(t, app, "Bearer secret-token", payload)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", status)
	}
}

func webhookApp(t *testing.T, token string) (*fiber.App, *db.DB) {
	t.Helper()
	database := db.OpenTest(t)
	s := &Server{
		cfg: &config.Config{Services: []config.Service{{
			ID: "svc", Name: "API", URL: "https://example.com/health",
		}}},
		database:     database,
		webhookToken: token,
		webhooks:     newWebhookLimiter(),
	}
	attachQuery(s)
	s.app = fiber.New(fiber.Config{BodyLimit: maxBodyBytes, ErrorHandler: errorHandler})
	s.routes()
	return s.app, database
}

func postAlert(t *testing.T, app *fiber.App, auth, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/api/alerts/webhook", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
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

func countAlerts(t *testing.T, database *db.DB, query string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
