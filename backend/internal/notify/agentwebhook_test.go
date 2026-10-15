package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/query"
)

// agentFixture stands up a recording endpoint plus a database, and returns a
// sender aimed at it along with the integration config to pass to Send.
func agentFixture(t *testing.T) (sender *AgentSender, integration json.RawMessage, database *db.DB, lastBody *[]byte, calls *int64) {
	t.Helper()

	var (
		body []byte
		got  int64
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		captured, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		body = captured
		got++
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	database = db.OpenTest(t)
	cfg := &config.Config{Services: []config.Service{{
		ID: "svc", Name: "API", URL: "https://example.com/health",
		Check: config.ServiceCheck{Interval: 30, Method: "GET", Timeout: 5},
	}}}

	integration, err := json.Marshal(map[string]any{
		"url":     srv.URL,
		"method":  "POST",
		"headers": map[string]string{"Authorization": "Bearer super-secret-token"},
	})
	if err != nil {
		t.Fatalf("encode integration: %v", err)
	}
	return &AgentSender{Q: query.New(database, func() *config.Config { return cfg })},
		integration, database, &body, &got
}

func decodeAgentMessage(t *testing.T, body []byte) AgentMessage {
	t.Helper()
	var got AgentMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode message: %v\n%s", err, body)
	}
	return got
}

func TestAgentSenderSendsV2DiagnosticRequest(t *testing.T) {
	sender, integration, database, body, calls := agentFixture(t)

	event := time.Unix(1_700_000_000, 0)
	if err := database.InsertCheck("svc", event.Unix(), db.StatusOffline, nil, nil, "connection refused", 3); err != nil {
		t.Fatalf("insert check: %v", err)
	}
	inc, err := database.CreateIncident("svc", "auto", event.Unix(), nil, nil)
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}

	if err := sender.Send(context.Background(), integration, Message{
		Event:       EventIncidentStart,
		IncidentID:  inc.ID,
		ServiceID:   "svc",
		ServiceName: "API",
		ServiceURL:  "https://example.com/health",
		OccurredAt:  event,
		StartedAt:   event,
		CheckID:     1,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("endpoint called %d times, want 1", *calls)
	}

	got := decodeAgentMessage(t, *body)
	if got.SchemaVersion != AgentSchemaVersion {
		t.Errorf("schema_version = %q, want %q", got.SchemaVersion, AgentSchemaVersion)
	}
	if got.MessageType != AgentMsgRequested {
		t.Errorf("message_type = %q, want %q", got.MessageType, AgentMsgRequested)
	}
	if want := "incident:" + strconv.FormatInt(inc.ID, 10); got.CorrelationID != want {
		t.Errorf("correlation_id = %q, want %q", got.CorrelationID, want)
	}
	if got.Payload.DiagnosticContext == nil {
		t.Fatalf("no diagnostic context in the payload")
	}
	dc := got.Payload.DiagnosticContext
	if dc.Incident == nil || dc.Incident.ID != inc.ID {
		t.Fatalf("context incident = %+v", dc.Incident)
	}
	if dc.TriggeringCheck == nil {
		t.Fatalf("context has no triggering check")
	}
	if len(dc.RecentChecks) != 1 {
		t.Fatalf("context has %d checks, want 1", len(dc.RecentChecks))
	}
	if dc.Service.Name != "API" {
		t.Fatalf("context service = %+v", dc.Service)
	}
	if got.Payload.Event.Status != "down" || got.Payload.Event.Severity != "outage" {
		t.Fatalf("event = %+v", got.Payload.Event)
	}
	// The integration's own credentials must never appear in the payload.
	if strings.Contains(string(*body), "super-secret-token") {
		t.Fatalf("payload leaked the integration secret")
	}
	if len(*body) > AgentMaxPayloadBytes {
		t.Fatalf("payload is %d bytes, over the %d limit", len(*body), AgentMaxPayloadBytes)
	}
}

// TestAgentSenderMessageIDIsStable proves a redelivery of the same logical
// event is recognisable, which is what lets the Agent stay idempotent.
func TestAgentSenderMessageIDIsStable(t *testing.T) {
	sender, integration, database, body, _ := agentFixture(t)
	event := time.Unix(1_700_000_000, 0)
	inc, err := database.CreateIncident("svc", "auto", event.Unix(), nil, nil)
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}
	msg := Message{
		Event: EventIncidentStart, IncidentID: inc.ID, ServiceID: "svc",
		ServiceName: "API", OccurredAt: event, StartedAt: event,
	}
	ids := map[string]bool{}
	for i := 0; i < 2; i++ {
		if err := sender.Send(context.Background(), integration, msg); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		ids[decodeAgentMessage(t, *body).MessageID] = true
	}
	if len(ids) != 1 {
		t.Fatalf("message_id changed between deliveries: %v", ids)
	}
	if want := "incident_start:" + strconv.FormatInt(inc.ID, 10); !ids[want] {
		t.Fatalf("message_id set = %v, want %q", ids, want)
	}
}

// TestAgentSenderTrimsOversizeContext checks the payload is cut to fit and that
// the cut is announced in coverage rather than silently losing evidence.
func TestAgentSenderTrimsOversizeContext(t *testing.T) {
	sender, integration, database, body, _ := agentFixture(t)
	sender.MaxBytes = 8 << 10

	event := time.Unix(1_700_000_000, 0)
	for i := 0; i < 40; i++ {
		if err := database.InsertCheck("svc", event.Unix()-int64(40-i), db.StatusOnline, nil, nil, "", 1); err != nil {
			t.Fatalf("insert check: %v", err)
		}
	}
	inc, err := database.CreateIncident("svc", "auto", event.Unix(), nil, nil)
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}
	for i := 0; i < 30; i++ {
		if _, err := database.AddIncidentComment(inc.ID, nil, strings.Repeat("很长的备注", 200), event.Unix()); err != nil {
			t.Fatalf("add comment: %v", err)
		}
	}

	if err := sender.Send(context.Background(), integration, Message{
		Event: EventIncidentStart, IncidentID: inc.ID, ServiceID: "svc",
		ServiceName: "API", OccurredAt: event, StartedAt: event,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(*body) > sender.MaxBytes {
		t.Fatalf("payload is %d bytes, over the %d limit", len(*body), sender.MaxBytes)
	}
	dc := decodeAgentMessage(t, *body).Payload.DiagnosticContext
	if dc == nil {
		t.Fatalf("context dropped entirely instead of trimmed")
	}
	if len(dc.Coverage.Truncated) == 0 {
		t.Fatalf("trimming was not recorded in coverage")
	}
	if dc.Coverage.Complete {
		t.Fatalf("a trimmed context reported complete")
	}
}

// TestAgentSenderResolveAndWarning pins the message types for the other two
// transitions, and that a warning carries no incident context.
func TestAgentSenderResolveAndWarning(t *testing.T) {
	sender, integration, database, body, _ := agentFixture(t)
	event := time.Unix(1_700_000_000, 0)
	inc, err := database.CreateIncident("svc", "auto", event.Unix(), nil, nil)
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}
	resolved := event.Add(5 * time.Minute)

	if err := sender.Send(context.Background(), integration, Message{
		Event: EventIncidentResolve, IncidentID: inc.ID, ServiceID: "svc",
		ServiceName: "API", OccurredAt: resolved, StartedAt: event, ResolvedAt: &resolved,
	}); err != nil {
		t.Fatalf("resolve send: %v", err)
	}
	if got := decodeAgentMessage(t, *body); got.MessageType != AgentMsgResolved {
		t.Fatalf("resolve message_type = %q, want %q", got.MessageType, AgentMsgResolved)
	}

	if err := sender.Send(context.Background(), integration, Message{
		Event: EventWarning, ServiceID: "svc", ServiceName: "API",
		OccurredAt: event, Attempts: 2, Error: "timeout", DeliveryID: 7,
	}); err != nil {
		t.Fatalf("warning send: %v", err)
	}
	got := decodeAgentMessage(t, *body)
	if got.MessageType != AgentMsgWarning {
		t.Fatalf("warning message_type = %q, want %q", got.MessageType, AgentMsgWarning)
	}
	if got.Payload.DiagnosticContext != nil {
		t.Fatalf("a warning must not carry an incident context")
	}
}

// TestAgentSenderRequiresAQueryService keeps the failure loud rather than
// sending a context-free message that would look like a healthy incident.
func TestAgentSenderRequiresAQueryService(t *testing.T) {
	var sender AgentSender
	err := sender.Send(context.Background(), json.RawMessage(`{"url":"http://example.invalid"}`), Message{})
	if err == nil || !strings.Contains(err.Error(), "query service") {
		t.Fatalf("err = %v", err)
	}
}
