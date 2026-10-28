package publish

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
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

func TestPublishHoldsTheServiceLockUntilTheSendReturns(t *testing.T) {
	database := db.OpenTest(t)
	row := claimOffline(t, database, 1_000)
	entered := make(chan struct{})
	release := make(chan struct{})
	var sends atomic.Int32
	p := &Publisher{DB: database, Owner: "pub", writer: gateWriter{
		enter: entered, release: release, sends: &sends,
	}}
	pubDone := make(chan error, 1)
	go func() { pubDone <- p.publishOne(context.Background(), row) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publish did not reach the broker write")
	}
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- database.DeleteServiceRecords("svc") }()
	waitUntilDeleteBlocks(t, database, deleteDone)
	close(release)
	if err := waitErr(t, pubDone, "publish"); err != nil {
		t.Fatal(err)
	}
	if err := waitErr(t, deleteDone, "delete"); err != nil {
		t.Fatal(err)
	}
	if err := p.publishOne(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if sends.Load() != 1 {
		t.Fatalf("kafka sends = %d", sends.Load())
	}
}

func TestPublishAfterDeleteDoesNotSendTheCachedRow(t *testing.T) {
	database := db.OpenTest(t)
	row := claimOffline(t, database, 1_000)
	if err := database.DeleteServiceRecords("svc"); err != nil {
		t.Fatal(err)
	}
	var sends atomic.Int32
	p := &Publisher{DB: database, Owner: "pub", writer: gateWriter{sends: &sends}}
	if err := p.publishOne(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if sends.Load() != 0 {
		t.Fatalf("kafka sends = %d", sends.Load())
	}
}

func TestPublishErrorReleasesTheLockAndDoesNotRecreateTheRow(t *testing.T) {
	database := db.OpenTest(t)
	row := claimOffline(t, database, 1_000)
	entered := make(chan struct{})
	release := make(chan struct{})
	var sends atomic.Int32
	p := &Publisher{DB: database, Owner: "pub", writer: gateWriter{
		enter: entered, release: release, sends: &sends, err: errors.New("broker down"),
	}}
	pubDone := make(chan error, 1)
	go func() { pubDone <- p.publishOne(context.Background(), row) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publish did not reach the broker write")
	}
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- database.DeleteServiceRecords("svc") }()
	waitUntilDeleteBlocks(t, database, deleteDone)
	close(release)
	if err := waitErr(t, pubDone, "publish"); err == nil {
		t.Fatal("publish succeeded after the broker write failed")
	}
	if err := waitErr(t, deleteDone, "delete"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM outbox_events`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("outbox rows = %d, %v", n, err)
	}
	if err := p.publishOne(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if sends.Load() != 1 {
		t.Fatalf("kafka sends = %d", sends.Load())
	}
}

func claimOffline(t *testing.T, database *db.DB, ts int64) db.OutboxEvent {
	t.Helper()
	ev, err := event.NewCheckCompleted("svc", "API", "https://old.example", ts, db.StatusOffline, nil, nil, "down", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecordCheckAndOutbox(ev); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	rows, err := database.ClaimOutbox("pub", now, now+30, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim = %v, %v", rows, err)
	}
	return rows[0]
}

func waitUntilDeleteBlocks(t *testing.T, database *db.DB, deleteDone <-chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-deleteDone:
			t.Fatalf("delete finished while the publish lock was held: %v", err)
		default:
		}
		var waiting int
		err := database.QueryRow(`
			SELECT COUNT(*)
			FROM pg_locks l
			JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE l.locktype = 'advisory' AND NOT l.granted AND a.datname = current_database()`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("delete did not wait on the service lock")
		}
		time.Sleep(15 * time.Millisecond)
	}
}

func waitErr(t *testing.T, done <-chan error, name string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not finish", name)
		return nil
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

type gateWriter struct {
	enter   chan struct{}
	release chan struct{}
	sends   *atomic.Int32
	err     error
}

func (w gateWriter) WriteMessages(context.Context, ...kafka.Message) error {
	if w.sends != nil {
		w.sends.Add(1)
	}
	if w.enter != nil {
		close(w.enter)
	}
	if w.release != nil {
		<-w.release
	}
	return w.err
}

func (gateWriter) Close() error { return nil }
