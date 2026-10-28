package monitor

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
)

func TestSyncWaitsAndDropsTheOldURL(t *testing.T) {
	oldSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	newSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(oldSrv.Close)
	t.Cleanup(newSrv.Close)

	database := db.OpenTest(t)
	sched := New(database)
	entered := make(chan struct{})
	hold := make(chan struct{})
	var once sync.Once
	var closeHold sync.Once
	release := func() { closeHold.Do(func() { close(hold) }) }
	t.Cleanup(sched.Stop)
	t.Cleanup(release)
	sched.beforeStore = func() {
		once.Do(func() {
			close(entered)
			<-hold
		})
	}
	svc := config.Service{
		ID: "svc", Name: "API", URL: oldSrv.URL,
		Check: config.ServiceCheck{Interval: 3600, Timeout: 1, RetryAttempts: 1},
	}
	sched.Sync([]config.Service{svc})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("old check did not reach the store")
	}
	svc.URL = newSrv.URL
	replaced := make(chan struct{})
	go func() {
		sched.Sync([]config.Service{svc})
		close(replaced)
	}()
	select {
	case <-replaced:
		t.Fatal("Sync returned before the old worker finished")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case <-replaced:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement Sync did not finish")
	}
	deadline := time.Now().Add(3 * time.Second)
	var payload string
	for {
		err := database.QueryRow(`SELECT payload::text FROM outbox_events`).Scan(&payload)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no check stored: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Contains(payload, oldSrv.URL) || !strings.Contains(payload, newSrv.URL) {
		t.Fatalf("payload = %s", payload)
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM checks`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("checks = %d, %v", n, err)
	}
}
