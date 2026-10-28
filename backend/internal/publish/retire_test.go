package publish

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/event"
)

func TestPublishSkipsARetiredServiceBeforeKafka(t *testing.T) {
	database := db.OpenTest(t)
	ev, err := event.NewCheckCompleted("svc", "API", "https://old.example", 1_000, db.StatusOffline, nil, nil, "down", 1)
	if err != nil {
		t.Fatal(err)
	}
	checkID, err := database.RecordCheckAndOutbox(ev)
	if err != nil {
		t.Fatal(err)
	}
	ev.CheckID = checkID
	now := time.Now().Unix()
	rows, err := database.ClaimOutbox("pub", now, now+30, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim = %v, %v", rows, err)
	}
	if err := database.DeleteServiceRecords("svc"); err != nil {
		t.Fatal(err)
	}
	p := &Publisher{DB: database, Owner: "pub"}
	if err := p.publishOne(context.Background(), rows[0]); err != nil {
		t.Fatal(err)
	}
	if err := database.ApplyCheckEvent(ev); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM incidents`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("incidents = %d, %v", n, err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM service_runtime_state`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("runtime = %d, %v", n, err)
	}
}

// The retirement read and the broker write are not one action. A delete that
// lands after the read still produces one Kafka message. The consumer drops it.
func TestPublishCanSendOnceAfterTheRetirementCheck(t *testing.T) {
	database := db.OpenTest(t)
	ev, err := event.NewCheckCompleted("svc", "API", "https://old.example", 1_000, db.StatusOffline, nil, nil, "down", 1)
	if err != nil {
		t.Fatal(err)
	}
	checkID, err := database.RecordCheckAndOutbox(ev)
	if err != nil {
		t.Fatal(err)
	}
	ev.CheckID = checkID
	now := time.Now().Unix()
	rows, err := database.ClaimOutbox("pub", now, now+30, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim = %v, %v", rows, err)
	}
	sent := 0
	p := &Publisher{DB: database, Owner: "pub", writer: lateWriter{send: func() error {
		sent++
		return database.DeleteServiceRecords("svc")
	}}}
	if err := p.publishOne(context.Background(), rows[0]); err == nil {
		t.Fatal("publish recorded a row the delete had already removed")
	}
	if sent != 1 {
		t.Fatalf("kafka sends = %d", sent)
	}
	if err := database.ApplyCheckEvent(ev); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM incidents`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("incidents = %d, %v", n, err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM processed_events`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("processed = %d, %v", n, err)
	}
}

func TestSameSecondReaddPublishesTheNewLife(t *testing.T) {
	database := db.OpenTest(t)
	ts := time.Now().Unix()
	old, err := event.NewCheckCompleted("svc", "API", "https://old.example", ts, db.StatusOffline, nil, nil, "down", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecordCheckAndOutbox(old); err != nil {
		t.Fatal(err)
	}
	claimed, err := database.ClaimOutbox("pub", ts, ts+30, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim old = %v, %v", claimed, err)
	}
	if err := database.DeleteServiceRecords("svc"); err != nil {
		t.Fatal(err)
	}
	next, err := event.NewCheckCompleted("svc", "API", "https://new.example", ts, db.StatusOffline, nil, nil, "down", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecordCheckAndOutbox(next); err != nil {
		t.Fatal(err)
	}
	again, err := database.ClaimOutbox("pub", ts, ts+30, 1)
	if err != nil || len(again) != 1 {
		t.Fatalf("claim new = %v, %v", again, err)
	}
	sent := 0
	p := &Publisher{DB: database, Owner: "pub", writer: countWriter{n: &sent}}
	if err := p.publishOne(context.Background(), again[0]); err != nil {
		t.Fatal(err)
	}
	if sent != 1 {
		t.Fatalf("kafka sends = %d", sent)
	}
	var fresh event.CheckCompleted
	if err := json.Unmarshal(again[0].Payload, &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.Epoch == nil || *fresh.Epoch != 1 {
		t.Fatalf("epoch = %v", fresh.Epoch)
	}
	if err := database.ApplyCheckEvent(fresh); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM incidents`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("incidents = %d, %v", n, err)
	}
	var previous event.CheckCompleted
	if err := json.Unmarshal(claimed[0].Payload, &previous); err != nil {
		t.Fatal(err)
	}
	if err := database.ApplyCheckEvent(previous); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM incidents`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("incidents after the old event = %d, %v", n, err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM service_runtime_state`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("runtime = %d, %v", n, err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM processed_events`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("processed = %d, %v", n, err)
	}
}

type countWriter struct{ n *int }

func (w countWriter) WriteMessages(context.Context, ...kafka.Message) error {
	*w.n++
	return nil
}

func (countWriter) Close() error { return nil }

type lateWriter struct {
	send func() error
}

func (w lateWriter) WriteMessages(context.Context, ...kafka.Message) error { return w.send() }

func (lateWriter) Close() error { return nil }
