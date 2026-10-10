package consume

import (
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"

	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/event"
)

func TestHandleDeadLetterAndDatabaseFailure(t *testing.T) {
	database := db.OpenTest(t)
	cons, err := New(database, "127.0.0.1:1", "alert-events-test", "test-group")
	if err != nil {
		t.Fatal(err)
	}

	commit, err := cons.Handle(kafka.Message{Topic: "alert-events-test", Partition: 1, Offset: 4, Value: []byte("not-json")})
	if err != nil || !commit {
		t.Fatalf("dead letter commit=%v err=%v", commit, err)
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM dead_letter_events WHERE topic = 'alert-events-test' AND offset_value = 4`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("dead letters = %d, %v", n, err)
	}

	ev, err := event.NewCheckCompleted("svc", "Name", "https://example.com", 10, "bogus", nil, nil, "down", 1)
	if err != nil {
		t.Fatal(err)
	}
	ev.CheckID = 1
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	commit, err = cons.Handle(kafka.Message{Topic: "alert-events-test", Key: []byte("svc"), Value: raw})
	if commit || err == nil {
		t.Fatalf("database failure commit=%v err=%v", commit, err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM processed_events`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("processed after rollback = %d, %v", n, err)
	}
}

func TestHandleExternalAlert(t *testing.T) {
	database := db.OpenTest(t)
	cons, err := New(database, "127.0.0.1:1", "alert-events-test", "test-group")
	if err != nil {
		t.Fatal(err)
	}
	in := db.ExternalAlertIn{
		Source: "grafana", EventID: "e1", AlertKey: "cpu", ServiceID: "svc",
		ServiceName: "API", ServiceURL: "https://example.com", Status: "firing",
		Title: "CPU high", OccurredAt: 2_000,
	}
	if _, err := database.AcceptExternalAlert(in); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := database.QueryRow(`SELECT payload FROM outbox_events`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	commit, err := cons.Handle(kafka.Message{Topic: "alert-events-test", Key: []byte("svc"), Value: raw})
	if err != nil || !commit {
		t.Fatalf("commit=%v err=%v", commit, err)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM incidents WHERE service_id = 'svc'`).Scan(&status); err != nil || status != "ongoing" {
		t.Fatalf("incident = %s, %v", status, err)
	}
	var checks int
	if err := database.QueryRow(`SELECT COUNT(*) FROM checks`).Scan(&checks); err != nil || checks != 0 {
		t.Fatalf("checks = %d, %v", checks, err)
	}
}
