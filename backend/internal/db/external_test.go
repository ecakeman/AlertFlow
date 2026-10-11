package db

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/ecakeman/alertflow/internal/event"
)

func TestAcceptExternalAlertDoesNotOpenIncident(t *testing.T) {
	database := OpenTest(t)
	in := firingAlert("e1", 2_000)
	dup, err := database.AcceptExternalAlert(in)
	if err != nil || dup {
		t.Fatalf("accept duplicate=%v err=%v", dup, err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM outbox_events WHERE event_type = 'external.alert'`); n != 1 {
		t.Fatalf("outbox = %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM incidents`); n != 0 {
		t.Fatalf("incidents = %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries`); n != 0 {
		t.Fatalf("deliveries = %d", n)
	}
	dup, err = database.AcceptExternalAlert(in)
	if err != nil || !dup {
		t.Fatalf("second accept duplicate=%v err=%v", dup, err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM outbox_events`); n != 1 {
		t.Fatalf("outbox after duplicate = %d", n)
	}
}

func TestExternalFiringThenResolved(t *testing.T) {
	database := OpenTest(t)
	mustPages(t, database)
	if err := database.ApplyExternalAlert(mustExternal(t, database, firingAlert("e1", 2_000))); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM external_alerts WHERE source = 'grafana' AND alert_key = 'cpu'`).Scan(&status); err != nil || status != "firing" {
		t.Fatalf("alert = %s, %v", status, err)
	}
	ongoing, err := database.GetOngoingIncident("svc")
	if err != nil || ongoing == nil || ongoing.Title != "CPU high" {
		t.Fatalf("ongoing = %+v, %v", ongoing, err)
	}
	if err := database.ApplyExternalAlert(mustExternal(t, database, resolvedAlert("e2", 3_000))); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT status FROM external_alerts WHERE alert_key = 'cpu'`).Scan(&status); err != nil || status != "resolved" {
		t.Fatalf("alert = %s, %v", status, err)
	}
	if inc, err := database.GetOngoingIncident("svc"); err != nil || inc != nil {
		t.Fatalf("incident still ongoing: %+v %v", inc, err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries WHERE event_type = 'incident_resolve'`); n != 1 {
		t.Fatalf("resolve deliveries = %d", n)
	}
	var httpStatus string
	if err := database.QueryRow(`SELECT current_status FROM service_runtime_state WHERE service_id = 'svc'`).Scan(&httpStatus); err != nil || httpStatus != StatusOnline {
		t.Fatalf("http status = %s, %v", httpStatus, err)
	}
}

func TestDuplicateFiringDoesNotOpenAnotherIncident(t *testing.T) {
	database := OpenTest(t)
	mustPages(t, database)
	first := mustExternal(t, database, firingAlert("e1", 2_000))
	again := firingAlert("e2", 2_100)
	if err := database.ApplyExternalAlert(first); err != nil {
		t.Fatal(err)
	}
	second := mustExternal(t, database, again)
	if err := database.ApplyExternalAlert(second); err != nil {
		t.Fatal(err)
	}
	if err := database.ApplyExternalAlert(second); !errors.Is(err, ErrAlreadyProcessed) {
		t.Fatalf("replay = %v", err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM incidents WHERE status = 'ongoing'`); n != 1 {
		t.Fatalf("ongoing = %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries WHERE event_type = 'incident_start'`); n != 1 {
		t.Fatalf("start deliveries = %d", n)
	}
}

func TestStaleExternalResolveDoesNotRewindFiring(t *testing.T) {
	database := OpenTest(t)
	mustPages(t, database)
	if err := database.ApplyExternalAlert(mustExternal(t, database, firingAlert("e-new", 3_000))); err != nil {
		t.Fatal(err)
	}
	if err := database.ApplyExternalAlert(mustExternal(t, database, resolvedAlert("e-old", 2_000))); err != nil {
		t.Fatal(err)
	}
	if err := database.ApplyExternalAlert(mustExternal(t, database, resolvedAlert("e-same", 3_000))); err != nil {
		t.Fatal(err)
	}
	var status string
	var last int64
	if err := database.QueryRow(`SELECT status, last_occurred_at FROM external_alerts WHERE alert_key = 'cpu'`).Scan(&status, &last); err != nil {
		t.Fatal(err)
	}
	if status != "firing" || last != 3_000 {
		t.Fatalf("alert = %s at %d", status, last)
	}
	if inc, err := database.GetOngoingIncident("svc"); err != nil || inc == nil {
		t.Fatalf("incident rewound: %+v %v", inc, err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries WHERE event_type = 'incident_resolve'`); n != 0 {
		t.Fatalf("resolve deliveries = %d", n)
	}
}

func TestHTTPRecoveryDoesNotCloseExternalAlert(t *testing.T) {
	database := OpenTest(t)
	mustPages(t, database)
	if err := database.ApplyExternalAlert(mustExternal(t, database, firingAlert("e1", 2_000))); err != nil {
		t.Fatal(err)
	}
	if err := database.ApplyCheckEvent(newEvent(t, "svc", StatusOffline, 1, 2_100)); err != nil {
		t.Fatal(err)
	}
	if err := database.ApplyCheckEvent(newEvent(t, "svc", StatusOnline, 2, 2_200)); err != nil {
		t.Fatal(err)
	}
	ongoing, err := database.GetOngoingIncident("svc")
	if err != nil || ongoing == nil {
		t.Fatalf("incident closed while the external alert is firing: %+v %v", ongoing, err)
	}
	var httpStatus string
	if err := database.QueryRow(`SELECT current_status FROM service_runtime_state WHERE service_id = 'svc'`).Scan(&httpStatus); err != nil || httpStatus != StatusOnline {
		t.Fatalf("http status = %s, %v", httpStatus, err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries WHERE event_type = 'incident_resolve'`); n != 0 {
		t.Fatalf("resolve deliveries = %d", n)
	}
}

func TestExternalResolveDoesNotCloseOfflineHTTP(t *testing.T) {
	database := OpenTest(t)
	mustPages(t, database)
	if err := database.ApplyCheckEvent(newEvent(t, "svc", StatusOffline, 1, 2_000)); err != nil {
		t.Fatal(err)
	}
	if err := database.ApplyExternalAlert(mustExternal(t, database, firingAlert("e1", 2_100))); err != nil {
		t.Fatal(err)
	}
	if err := database.ApplyExternalAlert(mustExternal(t, database, resolvedAlert("e2", 2_200))); err != nil {
		t.Fatal(err)
	}
	if inc, err := database.GetOngoingIncident("svc"); err != nil || inc == nil {
		t.Fatalf("incident closed while HTTP is offline: %+v %v", inc, err)
	}
	if err := database.ApplyCheckEvent(newEvent(t, "svc", StatusOnline, 2, 2_300)); err != nil {
		t.Fatal(err)
	}
	if inc, err := database.GetOngoingIncident("svc"); err != nil || inc != nil {
		t.Fatalf("incident after both recovered: %+v %v", inc, err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries WHERE event_type = 'incident_resolve'`); n != 1 {
		t.Fatalf("resolve deliveries = %d", n)
	}
}

func firingAlert(eventID string, at int64) ExternalAlertIn {
	return ExternalAlertIn{
		Source: "grafana", EventID: eventID, AlertKey: "cpu", ServiceID: "svc",
		ServiceName: "API", ServiceURL: "https://example.com", Status: "firing",
		Title: "CPU high", Description: "load", OccurredAt: at,
	}
}

func resolvedAlert(eventID string, at int64) ExternalAlertIn {
	in := firingAlert(eventID, at)
	in.Status = "resolved"
	return in
}

func mustExternal(t *testing.T, database *DB, in ExternalAlertIn) event.ExternalAlert {
	t.Helper()
	dup, err := database.AcceptExternalAlert(in)
	if err != nil || dup {
		t.Fatalf("accept %s duplicate=%v err=%v", in.EventID, dup, err)
	}
	var raw []byte
	if err := database.QueryRow(`SELECT payload FROM outbox_events WHERE event_id = $1`, event.ExternalOutboxID(in.Source, in.EventID)).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var ev event.ExternalAlert
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func mustPages(t *testing.T, database *DB) {
	t.Helper()
	if _, err := database.CreateIntegration("webhook", "pages", true, false, json.RawMessage(`{"url":"http://example.invalid"}`)); err != nil {
		t.Fatal(err)
	}
}
