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
)

// Store is a Redis client plus the in-process state the cache needs.
type Store struct {
	rdb     redis.UniversalClient
	group   singleflight.Group
	timeout time.Duration
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
	c, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	n, err := s.rdb.Get(c, genPrefix+scope).Int64()
	if errors.Is(err, redis.Nil) {
		return 0
	}
	if err != nil {
		s.warn("generation", scope, err)
		return 0
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
	c, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if err := s.rdb.Incr(c, genPrefix+scope).Err(); err != nil {
		s.warn("bump", scope, err)
	}
}

// getJSON loads key into dst, reporting whether it hit. A decode failure drops
// the entry: corrupt cache data must never reach a caller as data.
func (s *Store) getJSON(ctx context.Context, key string, dst any) bool {
	if !s.Enabled() {
		return false
	}
	c, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	raw, err := s.rdb.Get(c, key).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			s.warn("get", key, err)
		}
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
	c, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if err := s.rdb.Set(c, key, raw, ttl).Err(); err != nil {
		s.warn("set", key, err)
	}
}

func (s *Store) delete(ctx context.Context, keys ...string) {
	if !s.Enabled() || len(keys) == 0 {
		return
	}
	c, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if err := s.rdb.Del(c, keys...).Err(); err != nil {
		s.warn("delete", keys[0], err)
	}
}

// warn logs a cache problem at debug level. A down Redis is an expected state,
// not an incident, so it must not fill the log at info level.
func (s *Store) warn(op, key string, err error) {
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
