package cache

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testStore points at a dedicated Redis database so a test run cannot disturb
// real data, and skips when Redis is not available.
func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("ALERTFLOW_TEST_REDIS_URL")
	if url == "" {
		url = "redis://127.0.0.1:6379/15"
	}
	s, err := New(url)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Ping(ctx); err != nil {
		t.Skipf("redis not reachable at %s: %v", url, err)
	}
	t.Cleanup(func() {
		_ = s.rdb.FlushDB(context.Background()).Err()
		_ = s.Close()
	})
	return s
}

func key(t *testing.T, ns string, parts ...string) string {
	t.Helper()
	return Key("v1", ns, 0, parts...)
}

func TestRememberMissThenHit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	var loads int32
	load := func(context.Context) (string, error) {
		atomic.AddInt32(&loads, 1)
		return "value", nil
	}
	k := key(t, "t", "a")

	first, err := Remember(ctx, s, k, time.Minute, load)
	if err != nil || first != "value" {
		t.Fatalf("first = %q, %v", first, err)
	}
	second, err := Remember(ctx, s, k, time.Minute, load)
	if err != nil || second != "value" {
		t.Fatalf("second = %q, %v", second, err)
	}
	if n := atomic.LoadInt32(&loads); n != 1 {
		t.Fatalf("loader ran %d times, want 1 (second call should hit the cache)", n)
	}
}

func TestRememberDifferentKeysDoNotShare(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for part, want := range map[string]string{"x": "one", "y": "two"} {
		got, err := Remember(ctx, s, key(t, "t", part), time.Minute, func(context.Context) (string, error) {
			return want, nil
		})
		if err != nil || got != want {
			t.Fatalf("key %q = %q, %v; want %q", part, got, err, want)
		}
	}
	// A parameter that changes the result must change the key.
	if Key("v1", "t", 0, "a") == Key("v1", "t", 0, "b") {
		t.Fatal("distinct parameters produced the same key")
	}
	if Key("v1", "t", 0, "a") == Key("v1", "t", 1, "a") {
		t.Fatal("distinct generations produced the same key")
	}
	if Key("v1", "t", 0, "a") == Key("v2", "t", 0, "a") {
		t.Fatal("distinct versions produced the same key")
	}
}

func TestRememberCollapsesConcurrentMisses(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	var loads int32
	const callers = 24

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = Remember(ctx, s, key(t, "sf", "one"), time.Minute, func(context.Context) (int, error) {
				atomic.AddInt32(&loads, 1)
				time.Sleep(50 * time.Millisecond)
				return 7, nil
			})
		}()
	}
	close(start)
	wg.Wait()

	if n := atomic.LoadInt32(&loads); n != 1 {
		t.Fatalf("loader ran %d times for %d concurrent callers, want 1", n, callers)
	}
}

func TestBumpRetiresKeys(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := "metrics:svc"
	var loads int32
	load := func(context.Context) (int, error) {
		atomic.AddInt32(&loads, 1)
		return int(atomic.LoadInt32(&loads)), nil
	}
	first, err := Remember(ctx, s, Key("v1", "metrics", s.Generation(ctx, scope), "svc"), time.Minute, load)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	s.Bump(ctx, scope)
	second, err := Remember(ctx, s, Key("v1", "metrics", s.Generation(ctx, scope), "svc"), time.Minute, load)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first == second {
		t.Fatalf("bump did not retire the key: both reads returned %d", first)
	}
	if n := atomic.LoadInt32(&loads); n != 2 {
		t.Fatalf("loader ran %d times, want 2", n)
	}
}

func TestNilStoreAlwaysLoads(t *testing.T) {
	var s *Store
	if s.Enabled() {
		t.Fatal("nil store reported enabled")
	}
	for i := 0; i < 3; i++ {
		v, err := Remember(context.Background(), s, "any", time.Minute, func(context.Context) (string, error) {
			return "direct", nil
		})
		if err != nil || v != "direct" {
			t.Fatalf("nil store returned %q, %v", v, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("nil store Close: %v", err)
	}
}

func TestUnreachableRedisDegrades(t *testing.T) {
	s, err := New("redis://127.0.0.1:6399/0") // nothing listens here
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var loads int32
	for i := 0; i < 2; i++ {
		v, err := Remember(ctx, s, "k", time.Minute, func(context.Context) (string, error) {
			atomic.AddInt32(&loads, 1)
			return "from-db", nil
		})
		if err != nil {
			t.Fatalf("an unreachable cache must not fail the read: %v", err)
		}
		if v != "from-db" {
			t.Fatalf("value = %q", v)
		}
	}
	if n := atomic.LoadInt32(&loads); n != 2 {
		t.Fatalf("loader ran %d times, want 2 (nothing can be cached)", n)
	}
	// Generations and bumps must also be harmless.
	if g := s.Generation(ctx, "scope"); g != 0 {
		t.Fatalf("generation = %d, want 0", g)
	}
	s.Bump(ctx, "scope")
}

func TestCorruptValueIsDroppedAndReloaded(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	k := key(t, "corrupt", "one")
	if err := s.rdb.Set(ctx, k, "not json", time.Minute).Err(); err != nil {
		t.Fatalf("seed corrupt value: %v", err)
	}
	var loads int32
	v, err := Remember(ctx, s, k, time.Minute, func(context.Context) (string, error) {
		atomic.AddInt32(&loads, 1)
		return "good", nil
	})
	if err != nil || v != "good" {
		t.Fatalf("Remember = %q, %v", v, err)
	}
	if n := atomic.LoadInt32(&loads); n != 1 {
		t.Fatalf("loader ran %d times, want 1", n)
	}
	// The corrupt entry must be gone, not merely skipped.
	if err := s.rdb.Get(ctx, k).Err(); err != nil {
		t.Fatalf("corrupt key still present: %v", err)
	}
}

func TestGenerationStartsAtZero(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if g := s.Generation(ctx, "brand:new"); g != 0 {
		t.Fatalf("generation = %d, want 0", g)
	}
	s.Bump(ctx, "brand:new")
	if g := s.Generation(ctx, "brand:new"); g != 1 {
		t.Fatalf("generation after one bump = %d, want 1", g)
	}
}

// TestBreakerOpensAfterConsecutiveFailures pins the behaviour the load test
// found: without it, a down Redis costs one op timeout per read, which measured
// ~250 ms against ~150 µs when healthy — far worse than having no cache.
func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	s, err := New("redis://127.0.0.1:6399/0") // nothing listens here
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	for i := 0; i < breakerThreshold; i++ {
		s.Generation(ctx, "breaker")
	}
	if s.available() {
		t.Fatalf("breaker did not open after %d consecutive failures", breakerThreshold)
	}

	// While it is open the round-trip must be skipped entirely: reads stay fast
	// and the loader still answers.
	start := time.Now()
	for i := 0; i < 200; i++ {
		if _, err := Remember(ctx, s, "k", time.Minute, func(context.Context) (string, error) {
			return "from-db", nil
		}); err != nil {
			t.Fatalf("remember with the breaker open: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("200 reads took %v with the breaker open; they must not wait on Redis", elapsed)
	}
}

func TestBreakerNeedsConsecutiveFailures(t *testing.T) {
	s, err := New("redis://127.0.0.1:6399/0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	for i := 0; i < breakerThreshold-1; i++ {
		s.note(false)
	}
	s.note(true) // a success resets the run
	for i := 0; i < breakerThreshold-1; i++ {
		s.note(false)
	}
	if !s.available() {
		t.Fatal("breaker opened on failures that were not consecutive")
	}
	if got := s.failures.Load(); got != breakerThreshold-1 {
		t.Fatalf("failures = %d, want %d", got, breakerThreshold-1)
	}
}

func TestBreakerClosesAfterCooldown(t *testing.T) {
	s, err := New("redis://127.0.0.1:6399/0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	for i := 0; i < breakerThreshold; i++ {
		s.note(false)
	}
	if s.available() {
		t.Fatal("breaker did not open")
	}
	// Pretend the cooldown elapsed: the next call probes Redis again.
	s.openUntil.Store(time.Now().Add(-time.Second).UnixNano())
	if !s.available() {
		t.Fatal("breaker did not close after the cooldown")
	}
}
