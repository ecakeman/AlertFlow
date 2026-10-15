package query

import (
	"context"
	"fmt"
	"os"
	"strconv"
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

// TestOffsetSpreadsAcrossTheBucketWindow is the avalanche guard. An unshifted
// bucket makes every key rotate on the same wall-clock boundary, so the whole
// cache rebuilds at once; that is invisible with a handful of keys and grows
// with the number of distinct keys, not with traffic.
func TestOffsetSpreadsAcrossTheBucketWindow(t *testing.T) {
	width := int64(keyBucket / time.Second)
	seen := map[int64]bool{}
	for i := 0; i < 200; i++ {
		off := offsetSeconds(fmt.Sprintf("service-%d|24h", i))
		if off < 0 || off >= width {
			t.Fatalf("offset %d is outside [0,%d)", off, width)
		}
		seen[off] = true
	}
	if int64(len(seen)) != width {
		t.Fatalf("200 keys used %d of the %d rotation slots; rebuilds are not spread", len(seen), width)
	}
}

// TestKeyTimeStaysBucketAligned keeps the shift from turning into an arbitrary
// timestamp — the value still has to be a real bucket boundary, or the key
// would change on every request and nothing would ever hit.
func TestKeyTimeStaysBucketAligned(t *testing.T) {
	width := int64(keyBucket / time.Second)
	base := time.Unix(1_700_000_123, 0) // deliberately off a bucket boundary
	for i := 0; i < 50; i++ {
		raw, err := strconv.ParseInt(keyTime(base, fmt.Sprintf("svc-%d|1h", i)), 10, 64)
		if err != nil {
			t.Fatalf("keyTime returned a non-integer: %v", err)
		}
		if raw%width != 0 {
			t.Fatalf("keyTime produced %d, which is not a multiple of %ds", raw, width)
		}
	}
}

func TestKeyTimeSpreadsKeysAtTheSameInstant(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		seen[keyTime(base, fmt.Sprintf("svc-%d|24h", i))] = true
	}
	if len(seen) < 2 {
		t.Fatalf("every key landed on the same bucket at the same instant: %v", seen)
	}
}

func TestKeyTimeIsStablePerKey(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	if a, b := keyTime(base, "svc|24h"), keyTime(base, "svc|24h"); a != b {
		t.Fatalf("the same key at the same instant gave %s then %s", a, b)
	}
	// A full window later it must have rotated, or the cache would never refresh.
	if a, b := keyTime(base, "svc|24h"), keyTime(base.Add(keyBucket), "svc|24h"); a == b {
		t.Fatalf("the key did not rotate after a full bucket: %s", a)
	}
}

// TestKeyTimeRotatesKeysAtDifferentSeconds is the property the shift exists for:
// within one window, keys must not all change bucket on the same tick. Without
// it every key rebuilds at once, which is the avalanche.
func TestKeyTimeRotatesKeysAtDifferentSeconds(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	rotation := map[int64]int{}
	for i := 0; i < 20; i++ {
		seed := int64(i)
		identity := fmt.Sprintf("svc-%d|24h", i)
		prev := keyTime(base, identity)
		for s := 1; s < int(keyBucket/time.Second); s++ {
			if keyTime(base.Add(time.Duration(s)*time.Second), identity) != prev {
				rotation[seed] = s
				break
			}
		}
	}
	distinct := map[int]bool{}
	for _, second := range rotation {
		distinct[second] = true
	}
	if len(distinct) < 2 {
		t.Fatalf("every key rotated on the same second: %v", rotation)
	}
	t.Logf("rotation seconds across %d keys: %v", len(rotation), rotation)
}
