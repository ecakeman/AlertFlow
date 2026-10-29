package db

import (
	"testing"
	"time"
)

func TestAgentOffWritesTimelineWithoutTasks(t *testing.T) {
	database := OpenTest(t)
	t.Setenv("ALERTFLOW_AGENT_GRPC_TARGET", "")

	ev := newEvent(t, "svc", StatusOffline, 1, time.Now().Unix())
	if err := database.ApplyCheckEvent(ev); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM incidents WHERE service_id = 'svc'`); n != 1 {
		t.Fatalf("incidents = %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE service_id = 'svc'`); n != 1 {
		t.Fatalf("check timeline = %d", n)
	}

	inc, err := database.CreateManualIncident(ManualIncident{
		ServiceID: "manual", ServiceName: "Manual", StartedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE incident_id = $1`, inc.ID); n != 1 {
		t.Fatalf("manual timeline = %d", n)
	}

	row, created, err := database.CreateChatRequest(
		ChatRequestRow{RequestID: "chat-off", IncidentID: inc.ID, Message: "why?"},
		[]byte(`{"event":"chat_requested"}`), time.Now().Unix(),
	)
	if err != nil || !created || row.DeliveryID != nil {
		t.Fatalf("chat created=%v delivery=%v err=%v", created, row, err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries`); n != 0 {
		t.Fatalf("tasks = %d", n)
	}
}
