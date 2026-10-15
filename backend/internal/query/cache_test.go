package query

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ecakeman/alertflow/internal/cache"
	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
)

// testCache points at a dedicated Redis database (separate from the cache
// package's) and skips when Redis is unavailable.
func testCache(t *testing.T) *cache.Store {
	t.Helper()
	url := os.Getenv("ALERTFLOW_TEST_REDIS_URL")
	if url == "" {
		url = "redis://127.0.0.1:6379/14"
	}
	s, err := cache.New(url)
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Ping(ctx); err != nil {
		t.Skipf("redis not reachable at %s: %v", url, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func cacheTestService(t *testing.T, database *db.DB, id string) (*Service, *config.Config) {
	t.Helper()
	cfg := &config.Config{Services: []config.Service{{
		ID: id, Name: "API", URL: "https://example.com/health",
		Check: config.ServiceCheck{Interval: 30, Method: "GET", Timeout: 5},
	}}}
	return New(database, func() *config.Config { return cfg }), cfg
}

// TestServiceMetricsCachesAndInvalidates is the acceptance test for the cache:
// a warm read is served from Redis, and a new check is invisible until the
// service's cached views are invalidated.
func TestServiceMetricsCachesAndInvalidates(t *testing.T) {
	database := db.OpenTest(t)
	s, _ := cacheTestService(t, database, "cachesvc")
	s.Cache = testCache(t)
	ctx := context.Background()

	now := time.Now()
	if err := database.InsertCheck("cachesvc", now.Unix()-3600, db.StatusOnline, nil, nil, "", 1); err != nil {
		t.Fatalf("insert check: %v", err)
	}

	first, err := s.ServiceMetrics(ctx, "cachesvc", "1h", now)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(first.Series) != 1 {
		t.Fatalf("first series has %d points, want 1", len(first.Series))
	}

	// A newer check lands, but the cache still answers with the earlier page.
	if err := database.InsertCheck("cachesvc", now.Unix(), db.StatusOnline, nil, nil, "", 1); err != nil {
		t.Fatalf("insert check: %v", err)
	}
	hit, err := s.ServiceMetrics(ctx, "cachesvc", "1h", now)
	if err != nil {
		t.Fatalf("cached read: %v", err)
	}
	if len(hit.Series) != 1 {
		t.Fatalf("cached series has %d points, want the cached 1", len(hit.Series))
	}

	// Invalidating the service makes the next read see the new check.
	s.InvalidateService(ctx, "cachesvc")
	fresh, err := s.ServiceMetrics(ctx, "cachesvc", "1h", now)
	if err != nil {
		t.Fatalf("read after invalidation: %v", err)
	}
	if len(fresh.Series) != 2 {
		t.Fatalf("series after invalidation has %d points, want 2", len(fresh.Series))
	}
}

// TestServiceMetricsSurvivesUnreachableCache proves the cache is an enhancement:
// with Redis pointing nowhere, reads still work and are not short-circuited.
func TestServiceMetricsSurvivesUnreachableCache(t *testing.T) {
	database := db.OpenTest(t)
	s, _ := cacheTestService(t, database, "nocache")
	store, err := cache.New("redis://127.0.0.1:6399/0")
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	defer store.Close()
	s.Cache = store

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Now()
	if err := database.InsertCheck("nocache", now.Unix()-3600, db.StatusOnline, nil, nil, "", 1); err != nil {
		t.Fatalf("insert check: %v", err)
	}
	for i := 0; i < 2; i++ {
		res, err := s.ServiceMetrics(ctx, "nocache", "1h", now)
		if err != nil {
			t.Fatalf("read %d failed with a broken cache: %v", i, err)
		}
		if len(res.Series) != 1 {
			t.Fatalf("read %d returned %d points, want 1", i, len(res.Series))
		}
	}
}

// TestCheckHistoryKeyedByParams checks that two different pages of the same
// service never share a cache entry.
func TestCheckHistoryKeyedByParams(t *testing.T) {
	database := db.OpenTest(t)
	s, _ := cacheTestService(t, database, "pages")
	s.Cache = testCache(t)
	ctx := context.Background()

	base := time.Now().Unix() - 1000
	for i := 0; i < 5; i++ {
		if err := database.InsertCheck("pages", base+int64(i), db.StatusOnline, nil, nil, "", 1); err != nil {
			t.Fatalf("insert check: %v", err)
		}
	}
	page1, err := s.CheckHistory(ctx, "pages", 0, 2)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	page2, err := s.CheckHistory(ctx, "pages", 0, 3)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page1.Checks) != 2 || len(page2.Checks) != 3 {
		t.Fatalf("pages returned %d and %d rows, want 2 and 3", len(page1.Checks), len(page2.Checks))
	}
}
