package db

import "testing"

func TestDiagnosticIdentityKeepsDistinctContentAndReplaysOneEventID(t *testing.T) {
	database := OpenTest(t)
	base := DiagnosticItem{
		Kind: "recent_error", Name: "db", Status: "unhealthy", Summary: "timeout", ObservedAt: 1_700_000_000,
	}
	other := base
	other.Summary = "refused"
	if err := database.ApplyDiagnostics(DiagnosticPull{ServiceID: "svc", Status: "ok", Success: true, ObservedAt: 1_700_000_100, Items: []DiagnosticItem{base}}); err != nil {
		t.Fatal(err)
	}
	if err := database.ApplyDiagnostics(DiagnosticPull{ServiceID: "svc", Status: "ok", Success: true, ObservedAt: 1_700_000_100, Items: []DiagnosticItem{other}}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE service_id = 'svc' AND event_type = 'diagnostics.recent_error'`); n != 2 {
		t.Fatalf("distinct content rows = %d", n)
	}

	identified := base
	identified.EventID = "evt-1"
	identified.Summary = "first"
	if err := database.ApplyDiagnostics(DiagnosticPull{ServiceID: "svc", Status: "ok", Success: true, ObservedAt: 1_700_000_200, Items: []DiagnosticItem{identified}}); err != nil {
		t.Fatal(err)
	}
	identified.Summary = "changed"
	if err := database.ApplyDiagnostics(DiagnosticPull{ServiceID: "svc", Status: "ok", Success: true, ObservedAt: 1_700_000_300, Items: []DiagnosticItem{identified}}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_key = 'diagnostics:evt-1'`); n != 1 {
		t.Fatalf("event_id rows = %d", n)
	}
}

func TestDiagnosticLegacyItemStillStores(t *testing.T) {
	database := OpenTest(t)
	item := DiagnosticItem{Kind: "recent_error", Name: "db", Summary: "timeout", ObservedAt: 1_700_000_000}
	if err := database.ApplyDiagnostics(DiagnosticPull{ServiceID: "svc", Status: "ok", Success: true, ObservedAt: item.ObservedAt, Items: []DiagnosticItem{item}}); err != nil {
		t.Fatal(err)
	}
	if err := database.ApplyDiagnostics(DiagnosticPull{ServiceID: "svc", Status: "ok", Success: true, ObservedAt: item.ObservedAt, Items: []DiagnosticItem{item}}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE service_id = 'svc'`); n != 1 {
		t.Fatalf("legacy rows = %d", n)
	}
}
