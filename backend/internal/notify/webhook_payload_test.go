package notify

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDefaultWebhookKeepsStatusAndAddsContractFields(t *testing.T) {
	started := time.Unix(1_700_000_000, 0).UTC()
	body, err := renderWebhookBody("", Message{
		Event:       EventIncidentStart,
		IncidentID:  12,
		ServiceID:   "api",
		ServiceName: "API",
		ServiceURL:  "https://example.com/health",
		StartedAt:   started,
		Attempts:    3,
		Error:       "connection refused",
		CheckID:     99,
		DeliveryID:  7,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got["event"] != "incident_start" || got["service"] != "API" || got["status"] != "down" {
		t.Fatalf("legacy fields = %#v", got)
	}
	if got["event_id"] != "incident_start:12" || got["schema_version"] != "v1" {
		t.Fatalf("contract id = %#v", got)
	}
	if got["check_id"] != float64(99) || got["error"] != "connection refused" {
		t.Fatalf("check fields = %#v", got)
	}
	if _, ok := got["resolved_at"]; ok {
		t.Fatalf("start payload included resolved_at: %#v", got)
	}

	resolved := started.Add(time.Minute)
	body, err = renderWebhookBody("", Message{
		Event: EventIncidentResolve, IncidentID: 12, ServiceName: "API", ServiceURL: "https://example.com",
		OccurredAt: resolved, StartedAt: started, ResolvedAt: &resolved,
	})
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]any{}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != "recovered" || got["resolved_at"] != got["occurred_at"] || got["started_at"] == got["occurred_at"] {
		t.Fatalf("resolve times = %#v", got)
	}
	if got["started_at"] != started.Format(time.RFC3339) || got["occurred_at"] != resolved.Format(time.RFC3339) {
		t.Fatalf("resolve times = %#v", got)
	}
	if _, ok := got["check_id"]; ok {
		t.Fatalf("external resolve should omit check_id: %#v", got)
	}

	body, err = renderWebhookBody("", Message{Event: EventWarning, ServiceName: "API", StartedAt: started, Attempts: 2, DeliveryID: 4})
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]any{}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != "warning" || got["event_id"] != "delivery:4" || got["attempts"] != float64(2) {
		t.Fatalf("warning = %#v", got)
	}
	if _, ok := got["started_at"]; ok {
		t.Fatalf("warning invented a fault start: %#v", got)
	}
	if _, ok := got["resolved_at"]; ok {
		t.Fatalf("warning invented a recovery: %#v", got)
	}
	if strings.Contains(body, "offline") {
		t.Fatalf("status was rewritten: %s", body)
	}
}
