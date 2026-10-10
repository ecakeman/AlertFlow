package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/notify"
)

func TestFinishCycleFailureLeavesStatusAndSkipsNotify(t *testing.T) {
	database := db.OpenTest(t)
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
	}))
	defer srv.Close()
	cfg, _ := json.Marshal(map[string]string{"url": srv.URL})
	if _, err := database.CreateIntegration("webhook", "on", true, false, cfg); err != nil {
		t.Fatal(err)
	}

	sched := New(database, notify.NewDispatcher(database))
	w := &worker{
		svc:        config.Service{ID: "svc", Name: "Svc", URL: "http://svc", Check: config.ServiceCheck{Interval: 60, Timeout: 5, RetryAttempts: 1}},
		lastStatus: db.StatusOnline,
	}
	sched.finishCycle(context.Background(), w, w.svc, db.StatusOnline, cycleResult{
		Result:   Result{Status: "bogus"},
		Attempts: 1,
	}, 1000)

	if w.lastStatus != db.StatusOnline {
		t.Fatalf("lastStatus advanced to %s", w.lastStatus)
	}
	for _, table := range []string{"checks", "outbox_events", "incidents", "notification_log"} {
		var n int
		if err := database.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s has %d rows after a failed record", table, n)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt64(&hits) != 0 {
		t.Fatalf("notification fired %d times", hits)
	}
}

func TestCheckCyclesDoNotOverlap(t *testing.T) {
	database := db.OpenTest(t)
	var mu sync.Mutex
	var inFlight, maxInFlight int
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		once.Do(func() { close(entered) })
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sched := New(database, nil)
	w := &worker{svc: config.Service{
		ID: "svc", Name: "Svc", URL: srv.URL,
		Check: config.ServiceCheck{Interval: 60, Timeout: 5, RetryAttempts: 1},
	}}
	go sched.check(context.Background(), w)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first check did not reach the server")
	}
	done := make(chan struct{})
	go func() {
		sched.check(context.Background(), w)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("second check ran while the first still held the cycle")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second check did not finish")
	}
	if maxInFlight != 1 {
		t.Fatalf("overlapping HTTP checks: %d", maxInFlight)
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM checks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("checks=%d, want 2", n)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("unpublished outbox rows=%d, want 2", n)
	}
}
