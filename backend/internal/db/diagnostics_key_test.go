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
	if err := database.ApplyDiagnostics(DiagnosticPull{ServiceID: "svc", Status: "ok", Success: true, ObservedAt: 1_700_000_100, Items: []DiagnosticItem{base}}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE service_id = 'svc' AND event_type = 'diagnostics.recent_error' AND event_key NOT LIKE 'diagnostics:%:%:%'`); n != 2 {
		t.Fatalf("fingerprint rows = %d", n)
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
	key := diagnosticEventKey("svc", identified)
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_key = $1`, key); n != 1 {
		t.Fatalf("replay rows = %d", n)
	}

	otherService := identified
	otherService.Summary = "other service"
	if err := database.ApplyDiagnostics(DiagnosticPull{ServiceID: "other", Status: "ok", Success: true, ObservedAt: 1_700_000_400, Items: []DiagnosticItem{otherService}}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_key = $1`, diagnosticEventKey("other", otherService)); n != 1 {
		t.Fatalf("other service rows = %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'diagnostics.recent_error' AND summary IN ('first', 'other service')`); n != 2 {
		t.Fatalf("same event_id across services = %d", n)
	}
}

func TestDiagnosticUpgradeKeepsLegacyEventID(t *testing.T) {
	database := OpenTest(t)
	if _, err := database.Exec(
		`INSERT INTO runtime_events (
			event_key, service_id, source, event_type, severity, title, summary,
			occurred_at, observed_at, recorded_at
		 ) VALUES ('diagnostics:evt-old','svc','application_diagnostics','diagnostics.recent_error','error','db','old',1,1,1)`,
	); err != nil {
		t.Fatal(err)
	}
	item := DiagnosticItem{
		Kind: "recent_error", Name: "db", Status: "unhealthy", Summary: "again", ObservedAt: 1_700_000_000, EventID: "evt-old",
	}
	if err := database.ApplyDiagnostics(DiagnosticPull{ServiceID: "svc", Status: "ok", Success: true, ObservedAt: item.ObservedAt, Items: []DiagnosticItem{item}}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE service_id = 'svc' AND event_type = 'diagnostics.recent_error'`); n != 1 {
		t.Fatalf("upgraded rows = %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_key = $1`, diagnosticEventKey("svc", item)); n != 0 {
		t.Fatalf("new key rows = %d", n)
	}
	if err := database.ApplyDiagnostics(DiagnosticPull{ServiceID: "other", Status: "ok", Success: true, ObservedAt: item.ObservedAt, Items: []DiagnosticItem{item}}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_key = $1`, diagnosticEventKey("other", item)); n != 1 {
		t.Fatalf("other service rows = %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'diagnostics.recent_error' AND (event_key = 'diagnostics:evt-old' OR event_key = $1)`, diagnosticEventKey("other", item)); n != 2 {
		t.Fatalf("same event_id across services = %d", n)
	}
}

func TestDiagnosticUpgradeKeepsLegacyRecovery(t *testing.T) {
	database := OpenTest(t)
	if _, err := database.Exec(
		`INSERT INTO runtime_source_state (
			service_id, source, enabled, status, last_attempt_at, cursor, updated_at
		 ) VALUES ('svc', 'application_diagnostics', TRUE, 'ok', 1, '{"items":{"component_health":{"db":"unhealthy"}}}'::jsonb, 1)`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(
		`INSERT INTO runtime_events (
			event_key, service_id, source, event_type, severity, title, summary,
			occurred_at, observed_at, recorded_at
		 ) VALUES ('diagnostics:evt-r:recovered','svc','application_diagnostics','diagnostics.component_health.recovered','info','db recovered','up',1,1,1)`,
	); err != nil {
		t.Fatal(err)
	}
	item := DiagnosticItem{
		Kind: "component_health", Name: "db", Status: "healthy", Summary: "up", ObservedAt: 1_700_000_000, EventID: "evt-r",
	}
	if err := database.ApplyDiagnostics(DiagnosticPull{ServiceID: "svc", Status: "ok", Success: true, ObservedAt: item.ObservedAt, Items: []DiagnosticItem{item}}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE service_id = 'svc' AND event_type = 'diagnostics.component_health.recovered'`); n != 1 {
		t.Fatalf("recovery rows = %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_key = $1`, diagnosticEventKey("svc", item)+":recovered"); n != 0 {
		t.Fatalf("new recovery key rows = %d", n)
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
