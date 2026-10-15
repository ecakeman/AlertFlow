// Package cache is a small Redis-backed, cache-aside store for read results.
//
// It is an enhancement, never a dependency. Every operation degrades to "not
// cached" when Redis is missing, slow, or holding garbage, so the API keeps
// serving from PostgreSQL alone. Nothing here is allowed to turn a cache
// problem into a request failure.
//
// Two deliberate limits: it is only ever wired into the query layer (one place
// owns the keys and the invalidation), and it stores read results only — never
// current incident state, delivery state, credentials, or anything an Agent
// treats as fresh evidence.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

const (
	// opTimeout bounds one Redis round-trip. A slow cache must not slow the API:
	// past this the caller falls through to PostgreSQL.
	opTimeout = 250 * time.Millisecond

	// genPrefix namespaces the invalidation counters. A counter is one small
	// integer per scope and never expires; bumping it retires every key built
	// from the previous generation without scanning for them.
	genPrefix = "af:gen:"

	// breakerThreshold/breakerCooldown are a conventional circuit breaker.
	//
	// Without one, a down Redis is worse than no cache at all: every read waits
	// out opTimeout before falling through to PostgreSQL. Measured under load,
	// that is ~250 ms per cached read against ~150 µs when Redis is healthy —
	// and the penalty is a pure wait, so throughput only recovers by adding
	// concurrency. After this many consecutive failures the store stops calling
	// Redis for the cooldown, goes straight to the loader, and probes again when
	// the cooldown expires.
	breakerThreshold = 5
	breakerCooldown  = 5 * time.Second
)

// errBreakerOpen means the breaker tripped: Redis is skipped this time.
var errBreakerOpen = errors.New("cache: breaker open")

// Store is a Redis client plus the in-process state the cache needs.
type Store struct {
	rdb     redis.UniversalClient
	group   singleflight.Group
	timeout time.Duration

	failures  atomic.Int32
	openUntil atomic.Int64
	// probing marks the half-open probe: while set, other callers keep skipping
	// Redis instead of piling on.
	probing atomic.Bool
}

// New opens a Redis client. An empty url returns (nil, nil), meaning caching is
// disabled — every Store method is nil-safe, so callers need no branch.
func New(url string) (*Store, error) {
	if strings.TrimSpace(url) == "" {
		return nil, nil
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	// A cache read is already allowed to fail — the caller falls through to
	// PostgreSQL by design — so retrying it only adds latency and load. The
	// default of 3 retries, each bounded by opTimeout, turned one slow moment
	// into a 750 ms stall (3 × 250 ms), and under overload the retries fed back
	// into the load. Measured with an open-loop run at 20k rps: p50 fell from
	// 230 ms to 2.5 ms, and the 750 ms tail disappeared, once retries were off.
	// An explicit max_retries in the URL still wins.
	if opts.MaxRetries == 0 {
		opts.MaxRetries = -1
	}
	return &Store{rdb: redis.NewClient(opts), timeout: opTimeout}, nil
}

// Enabled reports whether a Redis client is attached.
func (s *Store) Enabled() bool { return s != nil && s.rdb != nil }

// Close releases the client. It is nil-safe.
func (s *Store) Close() error {
	if !s.Enabled() {
		return nil
	}
	return s.rdb.Close()
}

// Ping checks connectivity once at startup. A failure is reported but is not an
// error: the API runs without a cache.
func (s *Store) Ping(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}
	c, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return s.rdb.Ping(c).Err()
}

// Key builds a cache key. version, namespace and generation make it
// self-describing and safely invalidatable; every parameter that changes the
// result must be passed in parts, so two different queries can never collide.
func Key(version, namespace string, gen int64, parts ...string) string {
	var b strings.Builder
	b.Grow(32)
	b.WriteString("af:")
	b.WriteString(version)
	b.WriteString(":")
	b.WriteString(namespace)
	b.WriteString(":g")
	b.WriteString(strconv.FormatInt(gen, 10))
	for _, p := range parts {
		b.WriteString(":")
		b.WriteString(p)
	}
	return b.String()
}

// Generation returns the current invalidation counter for scope (0 when
// caching is off or Redis cannot answer). Callers put it in the key, so a Bump
// makes every previously built key unreachable.
func (s *Store) Generation(ctx context.Context, scope string) int64 {
	if !s.Enabled() {
		return 0
	}
	var n int64
	err := s.call(ctx, func(c context.Context) error {
		v, err := s.rdb.Get(c, genPrefix+scope).Int64()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
		n = v
		return nil
	})
	if err != nil {
		s.warn("generation", scope, err)
	}
	return n
}

// Bump retires every key under scope. It is a single INCR, so it never scans
// for keys and cannot block on a large keyspace. If Redis is unreachable the
// bump is lost, and the affected entries simply age out on their TTL.
func (s *Store) Bump(ctx context.Context, scope string) {
	if !s.Enabled() {
		return
	}
	if err := s.call(ctx, func(c context.Context) error {
		return s.rdb.Incr(c, genPrefix+scope).Err()
	}); err != nil {
		s.warn("bump", scope, err)
	}
}

// getJSON loads key into dst, reporting whether it hit. A decode failure drops
// the entry: corrupt cache data must never reach a caller as data.
func (s *Store) getJSON(ctx context.Context, key string, dst any) bool {
	if !s.Enabled() {
		return false
	}
	var raw []byte
	err := s.call(ctx, func(c context.Context) error {
		v, err := s.rdb.Get(c, key).Bytes()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
		raw = v
		return nil
	})
	if err != nil {
		s.warn("get", key, err)
		return false
	}
	if raw == nil {
		return false
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		s.warn("decode", key, err)
		s.delete(ctx, key)
		return false
	}
	return true
}

func (s *Store) setJSON(ctx context.Context, key string, value any, ttl time.Duration) {
	if !s.Enabled() {
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		s.warn("encode", key, err)
		return
	}
	if err := s.call(ctx, func(c context.Context) error {
		return s.rdb.Set(c, key, raw, ttl).Err()
	}); err != nil {
		s.warn("set", key, err)
	}
}

func (s *Store) delete(ctx context.Context, keys ...string) {
	if !s.Enabled() || len(keys) == 0 {
		return
	}
	if err := s.call(ctx, func(c context.Context) error {
		return s.rdb.Del(c, keys...).Err()
	}); err != nil {
		s.warn("delete", keys[0], err)
	}
}

// call runs one Redis command unless the breaker is open, and records the
// outcome. When the breaker is open the round-trip is skipped entirely, so the
// caller falls straight through to the loader instead of waiting out a timeout.
func (s *Store) call(ctx context.Context, fn func(context.Context) error) error {
	if !s.allow() {
		return errBreakerOpen
	}
	c, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	err := fn(c)
	s.note(err == nil)
	return err
}

// allow is the closed/open/half-open decision. While the breaker is open
// nothing reaches Redis. Once the cooldown expires, exactly ONE caller wins the
// claim and becomes the probe; everyone else keeps skipping Redis until it
// reports back.
//
// That probe is the whole point. Without it, every caller retried the moment the
// cooldown lapsed, and under load the breaker re-tripped hundreds of times a
// second — measured with an open-loop run at 10k rps: 674 trips and 674 log
// lines inside one second.
func (s *Store) allow() bool {
	until := s.openUntil.Load()
	if until == 0 {
		return true
	}
	if time.Now().UnixNano() < until {
		return false
	}
	return s.probing.CompareAndSwap(false, true)
}

// available reports the breaker's state without claiming the probe. Tests and
// the startup log use it; callers use allow.
func (s *Store) available() bool {
	until := s.openUntil.Load()
	return until == 0 || time.Now().UnixNano() >= until
}

func (s *Store) note(ok bool) {
	wasProbing := s.probing.Swap(false)
	if ok {
		s.failures.Store(0)
		if s.openUntil.Swap(0) != 0 {
			slog.Info("cache: Redis recovered; resuming")
		}
		return
	}
	if wasProbing {
		// The probe answered "still down". Re-open for another cooldown, with no
		// log: the transition was already reported when it first opened.
		s.failures.Store(0)
		s.openUntil.Store(time.Now().Add(breakerCooldown).UnixNano())
		return
	}
	if s.failures.Add(1) >= breakerThreshold {
		s.failures.Store(0)
		if s.openUntil.Swap(time.Now().Add(breakerCooldown).UnixNano()) == 0 {
			slog.Warn("cache: pausing Redis after consecutive failures", "cooldown", breakerCooldown.String())
		}
	}
}

// warn logs a cache problem at debug level. A down Redis is an expected state,
// not an incident, so it must not fill the log at info level. A tripped breaker
// is not logged here at all: it would repeat once per request.
func (s *Store) warn(op, key string, err error) {
	if errors.Is(err, errBreakerOpen) {
		return
	}
	slog.Debug("cache: "+op, "key", key, "error", err)
}

// Remember is cache-aside with single-flight collapse: it returns the cached
// value for key, or calls load exactly once — concurrent callers asking for the
// same key share that one call — and stores the result.
//
// A nil Store is fine: load runs every time. A load error is returned and
// nothing is cached, so a failed query is never remembered.
func Remember[T any](ctx context.Context, s *Store, key string, ttl time.Duration, load func(context.Context) (T, error)) (T, error) {
	var zero T
	if !s.Enabled() {
		return load(ctx)
	}
	var hit T
	if s.getJSON(ctx, key, &hit) {
		return hit, nil
	}

	// Detach from the caller's cancellation: the flight is shared, so one
	// request giving up must not cancel the load for everyone waiting on it.
	work := context.WithoutCancel(ctx)

	value, err, _ := s.group.Do(key, func() (any, error) {
		// A second look inside the flight: another caller may have filled the
		// key between our miss and our turn.
		var again T
		if s.getJSON(work, key, &again) {
			return again, nil
		}
		loaded, err := load(work)
		if err != nil {
			return nil, err
		}
		s.setJSON(work, key, loaded, ttl)
		return loaded, nil
	})
	if err != nil {
		return zero, err
	}
	out, ok := value.(T)
	if !ok {
		return zero, errors.New("cache: unexpected value type")
	}
	return out, nil
}
