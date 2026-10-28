package monitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
)

func TestReloadStartsChecksAndStopsWhenTheServiceLeaves(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	database := db.OpenTest(t)
	dir := t.TempDir()
	if err := config.Save(dir, &config.Config{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	loop := &Loop{DB: database, Dir: dir, Every: 100 * time.Millisecond}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = loop.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})

	time.Sleep(300 * time.Millisecond)
	if n := checkRows(t, database); n != 0 {
		t.Fatalf("checks before the service existed = %d", n)
	}
	if err := config.Save(dir, &config.Config{Services: []config.Service{{
		ID: "svc", Name: "API", URL: upstream.URL,
		Check: config.ServiceCheck{Interval: 5, Timeout: 1, RetryAttempts: 1},
	}}}); err != nil {
		t.Fatal(err)
	}
	if n := waitChecks(t, database, 1, 3*time.Second); n != 1 {
		t.Fatalf("checks after the service appeared = %d", n)
	}
	if err := config.Save(dir, &config.Config{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(6 * time.Second)
	if n := checkRows(t, database); n != 1 {
		t.Fatalf("checks after the service was removed = %d", n)
	}
}

func waitChecks(t *testing.T, database *db.DB, want int, budget time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(budget)
	var n int
	for {
		n = checkRows(t, database)
		if n == want || time.Now().After(deadline) {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func checkRows(t *testing.T, database *db.DB) int {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM checks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
