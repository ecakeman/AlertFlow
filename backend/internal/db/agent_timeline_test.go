package db

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAgentRunTimelineIsIdempotent(t *testing.T) {
	database := OpenTest(t)
	started := int64(1_700_000_000)
	rec := AgentRunRecord{
		MessageID: "m-start", RunID: "run-1", MessageType: "run.started",
		ServiceID: "svc", Status: "running", StartedAt: &started,
	}
	inserted, err := database.SaveAgentRun(rec)
	if err != nil || !inserted {
		t.Fatalf("insert = %v %v", inserted, err)
	}
	inserted, err = database.SaveAgentRun(rec)
	if err != nil || inserted {
		t.Fatalf("replay = %v %v", inserted, err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM agent_runs`); n != 1 {
		t.Fatalf("runs = %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_key = 'agent:m-start'`); n != 1 {
		t.Fatalf("timeline = %d", n)
	}
	var occurred, observed int64
	var details string
	if err := database.QueryRow(`SELECT occurred_at, observed_at, details::text FROM runtime_events WHERE event_key = 'agent:m-start'`).Scan(&occurred, &observed, &details); err != nil {
		t.Fatal(err)
	}
	if occurred != started || observed < time.Now().Unix()-60 || !json.Valid([]byte(details)) {
		t.Fatalf("times occurred=%d observed=%d details=%s", occurred, observed, details)
	}
	if json.Unmarshal([]byte(details), &map[string]any{}) != nil {
		t.Fatal(details)
	}
	bare := rec
	bare.MessageID = "m-nosvc"
	bare.ServiceID = ""
	if _, err := database.SaveAgentRun(bare); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_key = 'agent:m-nosvc'`); n != 0 {
		t.Fatalf("guessed service timeline = %d", n)
	}
}

func TestAgentRunRollsBackWhenTimelineFails(t *testing.T) {
	database := OpenTest(t)
	missing := int64(999999)
	_, err := database.SaveAgentRun(AgentRunRecord{
		MessageID: "m-bad", RunID: "run-1", MessageType: "run.failed",
		ServiceID: "svc", IncidentID: &missing, Error: "boom",
	})
	if err == nil {
		t.Fatal("expected the missing incident to reject the timeline row")
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM agent_runs`); n != 0 {
		t.Fatalf("run survived the rollback: %d", n)
	}
}

func TestReplayFillsAMissingTimelineRow(t *testing.T) {
	database := OpenTest(t)
	if _, err := database.Exec(
		`INSERT INTO agent_runs (
			message_id, run_id, message_type, service_id, report, created_at, updated_at
		 ) VALUES ('m-old', 'run-1', 'run.completed', 'svc', '{"root":"cause"}', 10, 10)`,
	); err != nil {
		t.Fatal(err)
	}
	inserted, err := database.SaveAgentRun(AgentRunRecord{
		MessageID: "m-old", RunID: "run-1", MessageType: "run.completed", ServiceID: "svc",
		Report: json.RawMessage(`{"root":"replaced"}`),
	})
	if err != nil || inserted {
		t.Fatalf("replay = %v %v", inserted, err)
	}
	var report string
	if err := database.QueryRow(`SELECT report::text FROM agent_runs WHERE message_id = 'm-old'`).Scan(&report); err != nil {
		t.Fatal(err)
	}
	if report != `{"root": "cause"}` && report != `{"root":"cause"}` {
		t.Fatalf("report rewritten: %s", report)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'agent.run.completed'`); n != 1 {
		t.Fatalf("timeline = %d", n)
	}
}
