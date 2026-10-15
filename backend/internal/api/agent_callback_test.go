package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
)

func agentCallbackApp(t *testing.T, token string) (*fiber.App, *db.DB) {
	t.Helper()
	database := db.OpenTest(t)
	s := &Server{
		cfg: &config.Config{Services: []config.Service{{
			ID: "svc", Name: "API", URL: "https://example.com/health",
		}}},
		database:           database,
		agentCallbackToken: token,
	}
	attachQuery(s)
	s.app = fiber.New(fiber.Config{BodyLimit: maxBodyBytes, ErrorHandler: errorHandler})
	s.routes()
	return s.app, database
}

func postAgentCallback(t *testing.T, app *fiber.App, auth, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/webhooks/agent", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func callbackBody(messageID, messageType, runID string, incidentID int64) string {
	return fmt.Sprintf(`{
		"schema_version": "v2",
		"message_id": %q,
		"message_type": %q,
		"correlation_id": "incident:%d",
		"payload": {
			"run_id": %q,
			"incident_id": %d,
			"service_id": "svc",
			"event_type": "incident_start",
			"mode": "agent",
			"status": "completed",
			"report": {"summary": "the target stopped answering", "status": "completed"},
			"evidence": {"chk:11": {"id": "chk:11"}},
			"usage": {"tokens": 1200},
			"started_at": "2026-10-11T10:00:00Z",
			"finished_at": "2026-10-11T10:01:00Z"
		}
	}`, messageID, messageType, incidentID, runID, incidentID)
}

func TestAgentCallbackStoresAndDedupes(t *testing.T) {
	app, database := agentCallbackApp(t, "callback-secret")
	inc, err := database.CreateIncident("svc", "auto", 1_700_000_000, nil, nil)
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}

	body := callbackBody("run:abc:v1", "run.completed", "abc", inc.ID)
	status, out := postAgentCallback(t, app, "Bearer callback-secret", body)
	if status != http.StatusOK || !strings.Contains(out, `"duplicate":false`) {
		t.Fatalf("first callback = %d %s", status, out)
	}
	status, out = postAgentCallback(t, app, "Bearer callback-secret", body)
	if status != http.StatusOK || !strings.Contains(out, `"duplicate":true`) {
		t.Fatalf("repeat callback = %d %s", status, out)
	}

	rec, err := database.LatestAgentRunForIncident(inc.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if rec == nil {
		t.Fatalf("no stored run")
	}
	if rec.RunID != "abc" || rec.Status != "completed" || rec.EventType != "incident_start" {
		t.Fatalf("record = %+v", rec)
	}
	var report map[string]any
	if err := json.Unmarshal(rec.Report, &report); err != nil {
		t.Fatalf("report is not JSON: %v", err)
	}
	if report["summary"] != "the target stopped answering" {
		t.Fatalf("report = %v", report)
	}
	if rec.StartedAt == nil || rec.FinishedAt == nil {
		t.Fatalf("timestamps not parsed: %+v", rec)
	}
	if rec.UpdatedAt == 0 || rec.CreatedAt == 0 {
		t.Fatalf("row timestamps missing: %+v", rec)
	}
}

func TestAgentCallbackKeepsTheNewestRunForTheIncident(t *testing.T) {
	app, database := agentCallbackApp(t, "callback-secret")
	inc, err := database.CreateIncident("svc", "auto", 1_700_000_000, nil, nil)
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}
	if status, out := postAgentCallback(t, app, "Bearer callback-secret",
		callbackBody("run:abc:started", "run.started", "abc", inc.ID)); status != http.StatusOK {
		t.Fatalf("started = %d %s", status, out)
	}
	if status, out := postAgentCallback(t, app, "Bearer callback-secret",
		callbackBody("run:abc:v1", "run.completed", "abc", inc.ID)); status != http.StatusOK {
		t.Fatalf("completed = %d %s", status, out)
	}
	rec, err := database.LatestAgentRunForIncident(inc.ID)
	if err != nil || rec == nil {
		t.Fatalf("read back: %v %+v", err, rec)
	}
	// The completed message is the newer one, so that is what the page shows.
	if rec.MessageType != "run.completed" || len(rec.Report) == 0 {
		t.Fatalf("record = %+v", rec)
	}
}

func TestAgentCallbackRejectsBadRequests(t *testing.T) {
	app, _ := agentCallbackApp(t, "callback-secret")

	if status, _ := postAgentCallback(t, app, "", callbackBody("m1", "run.completed", "abc", 1)); status != http.StatusUnauthorized {
		t.Fatalf("missing token = %d", status)
	}
	if status, _ := postAgentCallback(t, app, "Bearer wrong", callbackBody("m1", "run.completed", "abc", 1)); status != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d", status)
	}
	if status, _ := postAgentCallback(t, app, "Bearer callback-secret", `{`); status != http.StatusBadRequest {
		t.Fatalf("bad json = %d", status)
	}
	if status, out := postAgentCallback(t, app, "Bearer callback-secret",
		`{"schema_version":"v1","message_id":"m","message_type":"run.completed","payload":{"run_id":"r"}}`); status != http.StatusBadRequest {
		t.Fatalf("v1 rejected = %d %s", status, out)
	}
	if status, out := postAgentCallback(t, app, "Bearer callback-secret",
		`{"schema_version":"v2","message_id":"m","message_type":"something.else","payload":{"run_id":"r"}}`); status != http.StatusBadRequest {
		t.Fatalf("unknown type = %d %s", status, out)
	}
	if status, out := postAgentCallback(t, app, "Bearer callback-secret",
		`{"schema_version":"v2","message_id":"","message_type":"run.completed","payload":{"run_id":"r"}}`); status != http.StatusBadRequest {
		t.Fatalf("missing message_id = %d %s", status, out)
	}
	if status, out := postAgentCallback(t, app, "Bearer callback-secret",
		`{"schema_version":"v2","message_id":"m","message_type":"run.completed","payload":{}}`); status != http.StatusBadRequest {
		t.Fatalf("missing run_id = %d %s", status, out)
	}
}

func TestAgentCallbackDisabledWithoutToken(t *testing.T) {
	app, _ := agentCallbackApp(t, "")
	status, _ := postAgentCallback(t, app, "Bearer anything", callbackBody("m", "run.completed", "abc", 1))
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
}

// TestAgentCallbackDoesNotFallThroughToTheSPA pins that an unmatched /webhooks
// path answers JSON, not index.html — a webhook sender must never receive HTML.
func TestAgentCallbackDoesNotFallThroughToTheSPA(t *testing.T) {
	app, _ := agentCallbackApp(t, "callback-secret")
	req, err := http.NewRequest(http.MethodGet, "/webhooks/agent", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(data), "<html") {
		t.Fatalf("webhook path served the SPA: %s", data)
	}
}

func chatCallbackBody(messageType, reply, failure string) string {
	return fmt.Sprintf(`{
		"schema_version": "v2",
		"message_id": "chat:chat-1:%s",
		"message_type": %q,
		"correlation_id": "incident:1",
		"payload": {
			"request_id": "chat-1",
			"incident_id": 1,
			"reply": %q,
			"evidence_ids": ["chk:11"],
			"error": %q
		}
	}`, strings.TrimPrefix(messageType, "chat."), messageType, reply, failure)
}

func TestAgentChatCallbackAnswersAndDedupes(t *testing.T) {
	app, database := agentCallbackApp(t, "callback-secret")
	inc, err := database.CreateIncident("svc", "auto", 1_700_000_000, nil, nil)
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}
	if _, err := database.CreateIntegration(
		db.AgentWebhookType, "agent", true, false, json.RawMessage(`{"url":"http://agent.invalid"}`),
	); err != nil {
		t.Fatalf("create integration: %v", err)
	}
	if _, _, err := database.CreateChatRequest(
		db.ChatRequestRow{RequestID: "chat-1", IncidentID: inc.ID, Message: "why?"},
		[]byte(`{"event":"chat_requested"}`), 1_700_000_000); err != nil {
		t.Fatalf("create chat request: %v", err)
	}

	body := chatCallbackBody("chat.completed", "the probe returned 503", "")
	status, out := postAgentCallback(t, app, "Bearer callback-secret", body)
	if status != http.StatusOK || !strings.Contains(out, `"duplicate":false`) {
		t.Fatalf("answer = %d %s", status, out)
	}
	row, err := database.GetChatRequest("chat-1")
	if err != nil || row == nil {
		t.Fatalf("read back: %v %+v", err, row)
	}
	if row.Status != db.ChatAnswered || row.Reply == nil || *row.Reply != "the probe returned 503" {
		t.Fatalf("row = %+v", row)
	}

	status, out = postAgentCallback(t, app, "Bearer callback-secret", body)
	if status != http.StatusOK || !strings.Contains(out, `"duplicate":true`) {
		t.Fatalf("repeat answer = %d %s", status, out)
	}
}

func TestAgentChatCallbackRecordsFailure(t *testing.T) {
	app, database := agentCallbackApp(t, "callback-secret")
	inc, err := database.CreateIncident("svc", "auto", 1_700_000_000, nil, nil)
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}
	if _, err := database.CreateIntegration(
		db.AgentWebhookType, "agent", true, false, json.RawMessage(`{"url":"http://agent.invalid"}`),
	); err != nil {
		t.Fatalf("create integration: %v", err)
	}
	if _, _, err := database.CreateChatRequest(
		db.ChatRequestRow{RequestID: "chat-1", IncidentID: inc.ID, Message: "why?"},
		[]byte(`{}`), 1_700_000_000); err != nil {
		t.Fatalf("create chat request: %v", err)
	}

	body := chatCallbackBody("chat.failed", "", "the agent has no model configured")
	if status, out := postAgentCallback(t, app, "Bearer callback-secret", body); status != http.StatusOK {
		t.Fatalf("failure callback = %d %s", status, out)
	}
	row, err := database.GetChatRequest("chat-1")
	if err != nil || row == nil {
		t.Fatalf("read back: %v %+v", err, row)
	}
	if row.Status != db.ChatFailed || !strings.Contains(row.Error, "model") {
		t.Fatalf("row = %+v", row)
	}
}

func TestAgentChatCallbackRejectsMissingRequestID(t *testing.T) {
	app, _ := agentCallbackApp(t, "callback-secret")
	status, out := postAgentCallback(t, app, "Bearer callback-secret",
		`{"schema_version":"v2","message_id":"m","message_type":"chat.completed","payload":{}}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d %s", status, out)
	}
}
