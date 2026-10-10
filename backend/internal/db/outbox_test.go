package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ecakeman/alertflow/internal/event"
)

func TestRecordCheckAndOutboxCommitsTogether(t *testing.T) {
	database := openTestDB(t)
	ev, err := event.NewCheckCompleted("api", "API", "https://example.com/health", 1000, StatusOffline, nil, nil, "connection timeout", 3)
	if err != nil {
		t.Fatal(err)
	}
	checkID, err := database.RecordCheckAndOutbox(ev)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetOutboxByEventID(ev.EventID)
	if err != nil || stored == nil {
		t.Fatalf("outbox row: %v", err)
	}
	if stored.PublishedAt != nil {
		t.Fatal("new event was marked published")
	}
	var got event.CheckCompleted
	if err := json.Unmarshal(stored.Payload, &got); err != nil {
		t.Fatal(err)
	}
	again, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var round event.CheckCompleted
	if err := json.Unmarshal(again, &round); err != nil {
		t.Fatal(err)
	}
	if got.EventID != ev.EventID || round.EventID != ev.EventID || stored.EventID != ev.EventID {
		t.Fatalf("event id changed: stored %s payload %s round %s", stored.EventID, got.EventID, round.EventID)
	}
	if got.CheckID != checkID || round.CheckID != checkID {
		t.Fatalf("check id payload=%d round=%d row=%d", got.CheckID, round.CheckID, checkID)
	}
	if got.ServiceName != "API" || got.Attempts != 3 || got.Status != StatusOffline {
		t.Fatalf("payload lost check detail: %+v", got)
	}
}

func TestRecordCheckAndOutboxRollsBack(t *testing.T) {
	database := openTestDB(t)
	ev, err := event.NewCheckCompleted("api", "API", "https://example.com/health", 1000, StatusOnline, nil, nil, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecordCheckAndOutbox(ev); err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecordCheckAndOutbox(ev); err == nil {
		t.Fatal("duplicate event id committed")
	}
	bad, err := event.NewCheckCompleted("api", "API", "https://example.com/health", 1001, "bogus", nil, nil, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecordCheckAndOutbox(bad); err == nil {
		t.Fatal("invalid status committed")
	}
	var checks, events int
	if err := database.QueryRow(`SELECT COUNT(*) FROM checks`).Scan(&checks); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM outbox_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if checks != 1 || events != 1 {
		t.Fatalf("partial write: checks=%d outbox=%d", checks, events)
	}
}

func TestOutboxLeaseAndFailure(t *testing.T) {
	database := openTestDB(t)
	ev, err := event.NewCheckCompleted("api", "API", "https://example.com/health", 1000, StatusOnline, nil, nil, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.RecordCheckAndOutbox(ev); err != nil {
		t.Fatal(err)
	}

	first, err := database.ClaimOutbox("publisher", 1000, 2000, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].EventID != ev.EventID || first[0].LeaseOwner != "publisher" {
		t.Fatalf("claim = %+v", first)
	}
	held, err := database.ClaimOutbox("other", 1500, 2500, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 0 {
		t.Fatalf("leased event claimed again: %+v", held)
	}
	expired, err := database.ClaimOutbox("publisher", 2000, 3000, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].EventID != ev.EventID {
		t.Fatalf("expired lease not reclaimed: %+v", expired)
	}

	if err := database.RecordOutboxFailure(expired[0].ID, "publisher", 2500, "broker down", 5000); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetOutboxByEventID(ev.EventID)
	if err != nil || stored == nil {
		t.Fatal(err)
	}
	if stored.PublishedAt != nil || stored.AttemptCount != 1 || stored.LastError != "broker down" || stored.LeaseOwner != "" || stored.NextAttemptAt != 5000 {
		t.Fatalf("failure record = %+v", stored)
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM outbox_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("failure deleted the row, count=%d", n)
	}
	early, err := database.ClaimOutbox("publisher", 4000, 4500, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(early) != 0 {
		t.Fatalf("claimed before next attempt: %+v", early)
	}

	again, err := database.ClaimOutbox("publisher", 5000, 7000, 10)
	if err != nil || len(again) != 1 {
		t.Fatalf("reclaim after failure: %v %+v", err, again)
	}
	if err := database.MarkOutboxPublished(again[0].ID, "other", 5500, 6000); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stale owner marked published: %v", err)
	}
	if err := database.MarkOutboxPublished(again[0].ID, "publisher", 5500, 6000); err != nil {
		t.Fatal(err)
	}
	ready, err := database.ClaimOutbox("publisher", 6000, 7000, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 0 {
		t.Fatalf("published event still claimable: %+v", ready)
	}
	if err := database.RecordOutboxFailure(stored.ID, "publisher", 6500, "late", 8000); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("failure on a published event: %v", err)
	}
}
