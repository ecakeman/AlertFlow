package db

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestKubernetesStateCanReturnWithoutDroppingTheEvent(t *testing.T) {
	database := openTestDB(t)
	present := func() bool { return true }
	apply := func(fp, version string, active bool) {
		t.Helper()
		err := database.ApplyKubernetesObject(KubernetesObject{
			ServiceID: "svc", ClusterID: "local", Namespace: "lab", Kind: "Pod",
			Name: "api-0", UID: "pod-1", Fingerprint: fp, ResourceVersion: version,
			Active: active, Live: true, ObservedAt: time.Now().Unix(),
			Severity: "warning", Summary: fp, Snapshot: json.RawMessage(`{}`),
		}, present)
		if err != nil {
			t.Fatal(err)
		}
	}
	apply("A", "1", true)
	apply("B", "2", true)
	apply("A", "3", true)
	apply("A", "3", true)
	apply("B", "4", true)
	apply("B", "4", true)
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.pod.first_seen'`); n != 1 {
		t.Fatalf("first_seen = %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.pod.changed'`); n != 3 {
		t.Fatalf("changed = %d", n)
	}
	apply("B", "5", false)
	apply("A", "6", true)
	apply("A", "7", false)
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_events WHERE event_type = 'kubernetes.pod.inactive'`); n != 2 {
		t.Fatalf("inactive = %d", n)
	}
}

func TestKubernetesSourceWriteDropsWhenTheServiceIsGone(t *testing.T) {
	database := openTestDB(t)
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := lockService(tx, "svc"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- database.SetKubernetesSource("svc", "unavailable", "down", func() bool { return false })
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
			t.Fatalf("unavailable = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unavailable write did not finish after the lock was released")
	}
	if err := database.SetKubernetesSource("svc", "gap", "watch expired", func() bool { return false }); !errors.Is(err, ErrServiceGone) {
		t.Fatalf("gap = %v", err)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM runtime_source_state`); n != 0 {
		t.Fatalf("source rows = %d", n)
	}
}
