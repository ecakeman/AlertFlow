package query

import (
	"context"
	"hash/crc32"
	"strconv"
	"time"
)

// Cache tuning.
//
// Metrics series and reusable aggregates get the longer end of the window;
// the check page and the dashboard tile get a shorter one, so a just-resolved
// incident shows up quickly. Current incident state is never cached at all, so
// these bounds only ever affect derived views — never correctness.
const (
	cacheVersion      = "v1"
	metricsCacheTTL   = 15 * time.Second
	checksCacheTTL    = 10 * time.Second
	dashboardCacheTTL = 10 * time.Second

	// keyBucket quantises "now" for cache keys. Without it every request would
	// build a fresh key and nothing would ever hit; with it, results are stable
	// for the length of a bucket and identical parameters produce identical
	// results.
	keyBucket = 10 * time.Second
)

// dashScope is the invalidation scope for the dashboard tile, which aggregates
// across services and so cannot be scoped to one of them.
const dashScope = "dash:all"

func metricsScope(serviceID string) string { return "metrics:" + serviceID }
func checksScope(serviceID string) string  { return "checks:" + serviceID }

// keyTime renders an instant for use in a cache key: a wall-clock bucket,
// shifted by a per-key offset.
//
// The bucket itself exists so that "now" is stable for a while — without it
// every request would build a fresh key and nothing would ever hit. But an
// unshifted bucket makes every key rotate on the same wall-clock boundary, so
// the entire cache misses at the same instant. That is a cache avalanche, and
// it is self-inflicted: it scales with the number of distinct keys, not with
// traffic, so a demo with three keys never shows it and a system with a few
// hundred would.
//
// The offset is derived from the key's identity, so it is stable for a given
// key (the hit rate is unchanged) and different keys rebuild at different
// moments inside the window. It affects the key only: nothing in the query
// depends on it.
func keyTime(t time.Time, identity string) string {
	shift := time.Duration(offsetSeconds(identity)) * time.Second
	return strconv.FormatInt(t.Add(-shift).Truncate(keyBucket).Unix(), 10)
}

// offsetSeconds picks the second inside the bucket window at which this key
// rotates. The hash only chooses a slot; a collision just means two keys share
// an offset, which is harmless.
func offsetSeconds(identity string) int64 {
	width := int64(keyBucket / time.Second)
	if width <= 1 {
		return 0
	}
	return int64(crc32.ChecksumIEEE([]byte(identity))) % width
}

// InvalidateService retires every cached view a new check for serviceID can
// change. Each bump is a single INCR on a generation counter — no key scan, no
// lock, and old entries simply age out on their TTL.
func (s *Service) InvalidateService(ctx context.Context, serviceID string) {
	if s.Cache == nil {
		return
	}
	s.Cache.Bump(ctx, metricsScope(serviceID))
	s.Cache.Bump(ctx, checksScope(serviceID))
	s.Cache.Bump(ctx, dashScope)
}
