package query

import (
	"context"
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

// keyTime renders an instant for use in a cache key: truncated to the key
// bucket, so callers inside the same bucket share one entry.
func keyTime(t time.Time) string {
	return strconv.FormatInt(t.Truncate(keyBucket).Unix(), 10)
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
