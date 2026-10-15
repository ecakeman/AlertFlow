package db

import (
	"encoding/json"
	"testing"
)

func TestSaveAgentRunIsIdempotentByMessageID(t *testing.T) {
	database := OpenTest(t)
	inc, err := database.CreateIncident("svc", "auto", 1_700_000_000, nil, nil)
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}
	started := int64(1_700_000_000)

	rec := AgentRunRecord{
		MessageID:     "run:abc:v1",
		RunID:         "abc",
		MessageType:   "run.completed",
		CorrelationID: "incident:1",
		IncidentID:    &inc.ID,
		ServiceID:     "svc",
		EventType:     "incident_start",
		Status:        "completed",
		Report:        json.RawMessage(`{"summary":"down"}`),
		Usage:         json.RawMessage(`{"tokens":10}`),
		StartedAt:     &started,
	}
	inserted, err := database.SaveAgentRun(rec)
	if err != nil || !inserted {
		t.Fatalf("first save: inserted=%v err=%v", inserted, err)
	}
	// A redelivery of the same message must not create a second row.
	inserted, err = database.SaveAgentRun(rec)
	if err != nil {
		t.Fatalf("repeat save: %v", err)
	}
	if inserted {
		t.Fatalf("a redelivered message inserted a second row")
	}

	got, err := database.LatestAgentRunForIncident(inc.ID)
	if err != nil || got == nil {
		t.Fatalf("read back: %v %+v", err, got)
	}
	if got.MessageID != "run:abc:v1" || got.Status != "completed" {
		t.Fatalf("record = %+v", got)
	}
}

func TestSaveAgentRunWithoutReportKeepsNulls(t *testing.T) {
	database := OpenTest(t)
	inc, err := database.CreateIncident("svc", "auto", 1_700_000_000, nil, nil)
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}
	if _, err := database.SaveAgentRun(AgentRunRecord{
		MessageID: "run:abc:started", RunID: "abc", MessageType: "run.started",
		IncidentID: &inc.ID, ServiceID: "svc", Status: "running",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := database.LatestAgentRunForIncident(inc.ID)
	if err != nil || got == nil {
		t.Fatalf("read back: %v %+v", err, got)
	}
	// "no report yet" must stay distinguishable from "an empty report".
	if got.Report != nil {
		t.Fatalf("report = %s, want nil", got.Report)
	}
	if got.StartedAt != nil {
		t.Fatalf("started_at = %v, want nil", got.StartedAt)
	}
}

func TestLatestAgentRunForIncidentIsEmptyWhenNone(t *testing.T) {
	database := OpenTest(t)
	inc, err := database.CreateIncident("svc", "auto", 1_700_000_000, nil, nil)
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}
	got, err := database.LatestAgentRunForIncident(inc.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != nil {
		t.Fatalf("got %+v, want nil", got)
	}
}
