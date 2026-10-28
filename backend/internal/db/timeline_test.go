package db

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestManualIncidentTimelineAndTasks(t *testing.T) {
	database := OpenTest(t)
	good, err := database.CreateIntegration(AgentWebhookType, "agent", true, false, []byte(`{"url":"https://agent.example/hook"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateIntegration(AgentWebhookType, "blank", true, false, []byte(`{"url":""}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateIntegration("webhook", "old", true, false, []byte(`{"url":"https://example.invalid"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateIntegration(AgentWebhookType, "second", true, false, []byte(`{"url":"http://agent.example/other"}`)); err != nil {
		t.Fatal(err)
	}
	resolved := int64(2500)
	before := time.Now().Unix()
	inc, err := database.CreateManualIncident(ManualIncident{
		ServiceID: "svc", ServiceName: "API", ServiceURL: "https://example.com",
		StartedAt: 2000, ResolvedAt: &resolved,
	})
	if err != nil {
		t.Fatal(err)
	}
	if inc.Status != "resolved" || inc.Source != "manual" {
		t.Fatalf("incident = %+v", inc)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE incident_id = $1`, inc.ID); n != 2 {
		t.Fatalf("timeline = %d, want 2", n)
	}
	rows, err := database.Query(`SELECT event_type, occurred_at, observed_at, recorded_at FROM runtime_events WHERE incident_id = $1 ORDER BY occurred_at`, inc.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var occurred, observed, recorded int64
		if err := rows.Scan(&kind, &occurred, &observed, &recorded); err != nil {
			t.Fatal(err)
		}
		want := int64(2000)
		if kind == "incident.resolved" {
			want = resolved
		}
		if occurred != want || observed < before || recorded < before || observed == occurred {
			t.Fatalf("%s times occurred=%d observed=%d recorded=%d", kind, occurred, observed, recorded)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries WHERE incident_id = $1 AND integration_id = $2`, inc.ID, good.ID); n != 1 {
		t.Fatalf("agent tasks = %d", n)
	}
	var payload string
	if err := database.QueryRow(`SELECT payload::text FROM notification_deliveries WHERE incident_id = $1 LIMIT 1`, inc.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"source": "manual"`) || !strings.Contains(payload, incidentOpenedKey(inc.ID)) {
		t.Fatalf("payload = %s", payload)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries`); n != 2 {
		t.Fatalf("deliveries = %d, want 2", n)
	}

	open, err := database.CreateManualIncident(ManualIncident{ServiceID: "other", ServiceName: "Other", StartedAt: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE incident_id = $1 AND event_type = 'incident.opened'`, open.ID); n != 1 {
		t.Fatalf("open timeline = %d", n)
	}
	if _, err := database.UpdateIncident(open.ID, "renamed", 3000, nil); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE incident_id = $1`, open.ID); n != 1 {
		t.Fatalf("title edit wrote timeline: %d", n)
	}
	at := int64(4000)
	if _, err := database.UpdateIncident(open.ID, "renamed", 3000, &at); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE incident_id = $1 AND event_type = 'incident.resolved'`, open.ID); n != 1 {
		t.Fatalf("resolve timeline = %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries WHERE incident_id = $1`, open.ID); n != 2 {
		t.Fatalf("resolve created another task: %d", n)
	}
	if err := database.DeleteIncident(open.ID); err != nil {
		t.Fatal(err)
	}
	var linked int
	if err := database.QueryRow(`SELECT COUNT(*) FROM runtime_events WHERE incident_id = $1`, open.ID).Scan(&linked); err != nil || linked != 0 {
		t.Fatalf("timeline rows still point at deleted incident: %d, %v", linked, err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE service_id = 'other'`); n != 2 {
		t.Fatalf("timeline rows removed with the incident: %d", n)
	}
}

func TestTimelineRetentionDays(t *testing.T) {
	t.Setenv("ALERTFLOW_TIMELINE_RETENTION_DAYS", "15")
	if TimelineRetentionDays() != 15 {
		t.Fatalf("days = %d", TimelineRetentionDays())
	}
	t.Setenv("ALERTFLOW_TIMELINE_RETENTION_DAYS", "90")
	if TimelineRetentionDays() != 30 {
		t.Fatalf("invalid days = %d", TimelineRetentionDays())
	}
}

func TestDeleteServiceRemovesTimeline(t *testing.T) {
	database := OpenTest(t)
	inc, err := database.CreateManualIncident(ManualIncident{ServiceID: "svc", StartedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteServiceRecords("svc"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.GetIncident(inc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("incident err = %v", err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE service_id = 'svc'`); n != 0 {
		t.Fatalf("timeline left = %d", n)
	}
}
