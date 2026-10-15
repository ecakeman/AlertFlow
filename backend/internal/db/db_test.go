package db

import (
	"context"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	return OpenTest(t)
}

func TestMigrationsCreateTables(t *testing.T) {
	database := openTestDB(t)
	want := []string{
		"users", "sessions", "checks",
		"service_tls", "incidents", "incident_comments",
		"integrations", "notification_log", "outbox_events",
		"processed_events", "dead_letter_events", "service_runtime_state", "notification_deliveries",
		"agent_runs", "agent_chat_requests",
	}
	for _, table := range want {
		var name string
		err := database.QueryRow(
			`SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1`,
			table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q not created: %v", table, err)
		}
	}
	// External alert ingestion was removed; its tables must not come back.
	for _, table := range []string{"external_alert_events", "external_alerts"} {
		var name string
		if err := database.QueryRow(
			`SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1`,
			table,
		).Scan(&name); err == nil {
			t.Errorf("table %q still exists after the external-alert cleanup", table)
		}
	}
}

func TestInsertCheckRollsBack(t *testing.T) {
	database := openTestDB(t)
	tx, err := database.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO checks (service_id, ts, status, attempts) VALUES ($1, $2, $3, $4)`,
		"svc", int64(100), StatusOnline, 1,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM checks WHERE service_id = $1`, "svc").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("rolled-back check still visible, count=%d", n)
	}
	// A committed insert in a later transaction is visible.
	ctx := context.Background()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("ping after rollback: %v", err)
	}
	if err := database.InsertCheck("svc", 200, StatusOffline, nil, nil, "down", 1); err != nil {
		t.Fatalf("insert after rollback: %v", err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM checks WHERE service_id = $1`, "svc").Scan(&n); err != nil {
		t.Fatalf("count after commit: %v", err)
	}
	if n != 1 {
		t.Fatalf("committed check count=%d, want 1", n)
	}
}
