package db

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ecakeman/alertflow/internal/event"
)

func TestApplyCheckEventTransitions(t *testing.T) {
	database := OpenTest(t)
	secret := "super-secret-token"
	if _, err := database.CreateIntegration(AgentWebhookType, "pages", true, false, json.RawMessage(`{"url":"http://example.invalid","token":"`+secret+`"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateIntegration(AgentWebhookType, "warnings", true, true, json.RawMessage(`{"url":"http://example.invalid/warn","token":"`+secret+`"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateIntegration("telegram", "legacy", true, true, json.RawMessage(`{"botToken":"`+secret+`","chatId":"1"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateIntegration("webhook", "off", false, true, json.RawMessage(`{"url":"http://example.invalid/off"}`)); err != nil {
		t.Fatal(err)
	}

	down := newEvent(t, "svc", StatusOffline, 10, 1000)
	if err := database.ApplyCheckEvent(down); err != nil {
		t.Fatal(err)
	}
	ongoing, err := database.GetOngoingIncident("svc")
	if err != nil || ongoing == nil {
		t.Fatalf("ongoing = %+v, %v", ongoing, err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries WHERE event_id = $1 AND status = 'pending' AND event_type = 'incident_start'`, down.EventID); n != 2 {
		t.Fatalf("start deliveries = %d, want 2", n)
	}
	var payload string
	if err := database.QueryRow(`SELECT payload::text FROM notification_deliveries WHERE event_id = $1 LIMIT 1`, down.EventID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, secret) {
		t.Fatalf("delivery payload contains an integration secret: %s", payload)
	}

	if err := database.ApplyCheckEvent(down); !errors.Is(err, ErrAlreadyProcessed) {
		t.Fatalf("replay = %v", err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM incidents WHERE service_id = 'svc' AND status = 'ongoing'`); n != 1 {
		t.Fatalf("ongoing after replay = %d", n)
	}

	back := newEvent(t, "svc", StatusWarning, 11, 1100)
	if err := database.ApplyCheckEvent(back); err != nil {
		t.Fatal(err)
	}
	if got, _ := database.GetOngoingIncident("svc"); got != nil {
		t.Fatal("offline→warning left an ongoing incident")
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries WHERE event_id = $1 AND event_type = 'incident_resolve'`, back.EventID); n != 2 {
		t.Fatalf("resolve deliveries = %d", n)
	}
	var resolvePayload string
	if err := database.QueryRow(`SELECT payload::text FROM notification_deliveries WHERE event_id = $1 AND event_type = 'incident_resolve' LIMIT 1`, back.EventID).Scan(&resolvePayload); err != nil {
		t.Fatal(err)
	}
	var resolveSnap DeliverySnapshot
	if err := json.Unmarshal([]byte(resolvePayload), &resolveSnap); err != nil {
		t.Fatal(err)
	}
	if resolveSnap.StartedAt != 1000 || resolveSnap.OccurredAt != 1100 || resolveSnap.ResolvedAt == nil || *resolveSnap.ResolvedAt != 1100 {
		t.Fatalf("resolve snapshot = %+v", resolveSnap)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries WHERE event_id = $1 AND event_type = 'warning'`, back.EventID); n != 0 {
		t.Fatalf("recovery created warning deliveries: %d", n)
	}

	blip := newEvent(t, "other", StatusWarning, 12, 1200)
	if err := database.ApplyCheckEvent(blip); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries WHERE event_id = $1 AND event_type = 'warning'`, blip.EventID); n != 1 {
		t.Fatalf("warning deliveries = %d, want 1", n)
	}
	var warnPayload string
	if err := database.QueryRow(`SELECT payload::text FROM notification_deliveries WHERE event_id = $1 AND event_type = 'warning'`, blip.EventID).Scan(&warnPayload); err != nil {
		t.Fatal(err)
	}
	var warnSnap DeliverySnapshot
	if err := json.Unmarshal([]byte(warnPayload), &warnSnap); err != nil {
		t.Fatal(err)
	}
	if warnSnap.OccurredAt != 1200 || warnSnap.StartedAt != 0 || warnSnap.ResolvedAt != nil {
		t.Fatalf("warning snapshot = %+v", warnSnap)
	}
}

func TestApplyCheckEventDoesNotRewind(t *testing.T) {
	database := OpenTest(t)
	newer := newEvent(t, "svc", StatusOffline, 20, 2000)
	older := newEvent(t, "svc", StatusOnline, 5, 500)
	if err := database.ApplyCheckEvent(newer); err != nil {
		t.Fatal(err)
	}
	if err := database.ApplyCheckEvent(older); err != nil {
		t.Fatal(err)
	}
	ongoing, err := database.GetOngoingIncident("svc")
	if err != nil || ongoing == nil {
		t.Fatalf("older event resolved the outage: %+v %v", ongoing, err)
	}
	var status string
	var lastCheck int64
	if err := database.QueryRow(`SELECT current_status, last_check_id FROM service_runtime_state WHERE service_id = 'svc'`).Scan(&status, &lastCheck); err != nil {
		t.Fatal(err)
	}
	if status != StatusOffline || lastCheck != 20 {
		t.Fatalf("state = %s check %d", status, lastCheck)
	}
}

func TestApplyCheckEventUsesHistoryAndOngoing(t *testing.T) {
	database := OpenTest(t)
	if err := database.InsertCheck("hist", 10, StatusOffline, nil, nil, "down", 1); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := database.QueryRow(`SELECT id FROM checks WHERE service_id = 'hist'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	again := newEvent(t, "hist", StatusOffline, id+1, 20)
	if err := database.ApplyCheckEvent(again); err != nil {
		t.Fatal(err)
	}
	if got, _ := database.GetOngoingIncident("hist"); got != nil {
		t.Fatal("repeated offline from check history opened another incident")
	}

	if _, err := database.CreateIncident("gap", "auto", 30, nil, nil); err != nil {
		t.Fatal(err)
	}
	recovered := newEvent(t, "gap", StatusOnline, 40, 40)
	if err := database.ApplyCheckEvent(recovered); err != nil {
		t.Fatal(err)
	}
	if got, _ := database.GetOngoingIncident("gap"); got != nil {
		t.Fatal("missing runtime state left the ongoing incident in place")
	}
}

func TestApplyCheckEventRollsBack(t *testing.T) {
	database := OpenTest(t)
	ev := newEvent(t, "svc", "bogus", 1, 1)
	if err := database.ApplyCheckEvent(ev); err == nil {
		t.Fatal("expected constraint failure")
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM processed_events`); n != 0 {
		t.Fatalf("processed_events = %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM incidents`); n != 0 {
		t.Fatalf("incidents = %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM service_runtime_state`); n != 0 {
		t.Fatalf("runtime state = %d", n)
	}
}

func newEvent(t *testing.T, service, status string, checkID, ts int64) event.CheckCompleted {
	t.Helper()
	ev, err := event.NewCheckCompleted(service, "Name", "https://example.com/health", ts, status, nil, nil, "down", 2)
	if err != nil {
		t.Fatal(err)
	}
	ev.CheckID = checkID
	return ev
}

func countRows(t *testing.T, database *DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := database.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
