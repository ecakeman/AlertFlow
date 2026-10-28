package db

import (
	"errors"
	"testing"
	"time"

	"github.com/ecakeman/alertflow/internal/event"
)

func TestWriteWaitsForTheServiceLockThenDrops(t *testing.T) {
	database := openTestDB(t)
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := lockService(tx, "svc"); err != nil {
		t.Fatal(err)
	}
	ev, err := event.NewCheckCompleted("svc", "API", "https://example.com", time.Now().Unix(), StatusOnline, nil, nil, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := database.RecordCheckAndOutboxPresent(ev, func() bool { return false }, &ServiceTLS{
			CheckedAt: ev.OccurredAt, Issuer: "ca", Subject: "example",
		})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("write finished while the service lock was held: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrServiceGone) {
			t.Fatalf("write = %v, want ErrServiceGone", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("write did not finish after the lock was released")
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM checks`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("checks = %d, %v", n, err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM service_tls`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("tls rows = %d, %v", n, err)
	}
}

func TestDiagnosticsWriteDropsWhenTheServiceIsGone(t *testing.T) {
	database := openTestDB(t)
	err := database.ApplyDiagnosticsPresent(DiagnosticPull{
		ServiceID:  "svc",
		Status:     "ok",
		ObservedAt: time.Now().Unix(),
		Success:    true,
	}, func() bool { return false })
	if !errors.Is(err, ErrServiceGone) {
		t.Fatalf("apply = %v", err)
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM runtime_source_state`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("source rows = %d, %v", n, err)
	}
}
