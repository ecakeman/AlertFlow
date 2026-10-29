package db

import (
	"encoding/json"
	"testing"
)

func TestDeleteIntegrationRemovesPendingDeliveries(t *testing.T) {
	database := OpenTest(t)
	keep := mustChannel(t, database, "keep", true)
	drop := mustChannel(t, database, "drop", true)
	keepEvent := mustDelivery(t, database, "svc-keep", keep.ID)
	dropEvent := mustDelivery(t, database, "svc-drop", drop.ID)

	var def string
	if err := database.QueryRow(
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'notification_deliveries_integration_id_fkey'`,
	).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if def != "FOREIGN KEY (integration_id) REFERENCES integrations(id) ON DELETE CASCADE" {
		t.Fatalf("constraint = %s", def)
	}

	if err := database.DeleteIntegration(drop.ID); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries WHERE integration_id = $1`, drop.ID); n != 0 {
		t.Fatalf("deleted channel left %d deliveries", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries WHERE integration_id = $1 AND status = 'pending'`, keep.ID); n != 1 {
		t.Fatalf("kept channel deliveries = %d, want 1", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries WHERE event_id = $1 AND integration_id = $2`, keepEvent, keep.ID); n != 1 {
		t.Fatalf("kept event delivery = %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries WHERE event_id = $1`, dropEvent); n != 0 {
		t.Fatalf("deleted channel left event rows = %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries WHERE integration_id IS NULL`); n != 0 {
		t.Fatalf("internal tasks = %d", n)
	}
}

func TestReplaceIntegrationsDropsDeliveriesAndKeepsSequence(t *testing.T) {
	database := OpenTest(t)
	old := mustChannel(t, database, "old", true)
	mustDelivery(t, database, "svc", old.ID)

	imported := Integration{
		ID:        7,
		Type:      "webhook",
		Name:      "imported",
		Enabled:   true,
		Config:    json.RawMessage(`{"url":"http://example.invalid/imported"}`),
		CreatedAt: 10,
		UpdatedAt: 11,
	}
	if err := database.ReplaceIntegrations([]Integration{imported}); err != nil {
		t.Fatal(err)
	}
	got, err := database.ListIntegrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 7 || got[0].Name != "imported" {
		t.Fatalf("integrations = %+v", got)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries`); n != 0 {
		t.Fatalf("replace left %d deliveries", n)
	}
	next, err := database.CreateIntegration(AgentWebhookType, "after", true, false, json.RawMessage(`{"url":"http://example.invalid/after"}`))
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != 8 {
		t.Fatalf("next id = %d, want 8", next.ID)
	}

	before, err := database.ListIntegrations()
	if err != nil {
		t.Fatal(err)
	}
	mustDelivery(t, database, "svc-keep", next.ID)
	err = database.ReplaceIntegrations([]Integration{{
		ID: 1, Type: "not-a-channel", Name: "bad", Enabled: true,
		Config: json.RawMessage(`{}`), CreatedAt: 1, UpdatedAt: 1,
	}})
	if err == nil {
		t.Fatal("expected replace to fail")
	}
	after, err := database.ListIntegrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("failed replace changed integrations from %+v to %+v", before, after)
	}
	for i := range before {
		if after[i].ID != before[i].ID || after[i].Name != before[i].Name {
			t.Fatalf("failed replace changed integrations from %+v to %+v", before, after)
		}
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM notification_deliveries WHERE integration_id = $1`, next.ID); n != 1 {
		t.Fatalf("failed replace left %d deliveries for the channel", n)
	}
}

func mustChannel(t *testing.T, database *DB, name string, warnings bool) *Integration {
	t.Helper()
	in, err := database.CreateIntegration(AgentWebhookType, name, true, warnings, json.RawMessage(`{"url":"http://example.invalid/`+name+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func mustDelivery(t *testing.T, database *DB, service string, integrationID int64) string {
	t.Helper()
	eventID := "evt-" + service
	if _, err := database.Exec(
		`INSERT INTO notification_deliveries (
			event_id, integration_id, event_type, payload, status,
			attempt_count, next_attempt_at, created_at, updated_at
		 ) VALUES ($1, $2, 'incident_start', '{}', 'pending', 0, 1, 1, 1)`,
		eventID, integrationID,
	); err != nil {
		t.Fatal(err)
	}
	return eventID
}
