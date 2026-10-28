package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/monitor"
)

func TestSavingAServiceDoesNotStartChecks(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	database := db.OpenTest(t)
	dir := t.TempDir()
	if err := config.Save(dir, &config.Config{}); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	sched := monitor.New(database)
	t.Cleanup(sched.Stop)
	s := &Server{configDir: dir, cfg: loaded, database: database, sched: sched}
	sched.Configured = s.serviceConfigured
	if err := s.updateConfig(func(cfg *config.Config) error {
		cfg.Services = append(cfg.Services, config.Service{
			ID: "svc", Name: "API", URL: upstream.URL,
			Check: config.ServiceCheck{Interval: 5, Timeout: 1, RetryAttempts: 1},
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A resident loop checks once immediately. Waiting past the interval would
	// also have recorded the next cycle.
	time.Sleep(6 * time.Second)
	if n := checkCount(t, database); n != 0 {
		t.Fatalf("checks = %d, server started a loop", n)
	}
	svc := s.config().Find("svc")
	if svc == nil {
		t.Fatal("saved service missing")
	}
	s.sched.CheckNow(*svc)
	if n := checkCount(t, database); n != 1 {
		t.Fatalf("checks after CheckNow = %d", n)
	}
}

func checkCount(t *testing.T, database *db.DB) int {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM checks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
