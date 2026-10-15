// Package query is the single composition layer for AlertFlow's reads.
//
// Every caller — the Fiber handlers, the diagnostic-context builder that seeds
// an Agent run, and the Agent-facing read endpoints — goes through here, so the
// same query is not re-derived in three places. It owns query shapes, time
// windows, pagination bounds and (from the caching phase on) reuse.
//
// Two rules keep the seam honest:
//
//   - It never imports Fiber or anything HTTP. Handlers keep auth, input
//     validation and status codes; this package only composes data.
//   - It reuses the SQL in internal/db rather than re-implementing it. If a
//     query is missing, add it to internal/db and call it from here.
package query

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/ecakeman/alertflow/internal/cache"
	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
)

// ErrServiceNotFound means the config has no service with that id.
var ErrServiceNotFound = errors.New("service not found")

// ErrIncidentNotFound means the database has no incident with that id. It is
// the sentinel from internal/db, re-exported so callers need not import db just
// to compare.
var ErrIncidentNotFound = db.ErrNotFound

// Page-size bounds. They live here so the HTTP handler, the diagnostic-context
// builder and the Agent's on-demand reads all clamp identically.
const (
	DefaultCheckPageSize    = 100
	MaxCheckPageSize        = 500
	DefaultIncidentPageSize = 20
	MaxIncidentPageSize     = 200
)

// ConfigSource returns the live config snapshot. It is a function rather than a
// *config.Config so that a config edit (swapped copy-on-write under the
// server's lock) is visible without rebuilding the Service.
type ConfigSource func() *config.Config

// Service composes monitoring reads over the database and the config snapshot.
// A zero Cfg is tolerated (treated as an empty config) so a process that only
// reads database-side data can still build one.
type Service struct {
	DB  *db.DB
	Cfg ConfigSource
	// Cache is the optional read cache. Nil (no REDIS_URL) disables caching:
	// every cache call degrades to a direct PostgreSQL read.
	Cache *cache.Store
}

// New builds a query service. cfg may be nil in tests that only exercise
// database-backed reads.
func New(database *db.DB, cfg ConfigSource) *Service {
	return &Service{DB: database, Cfg: cfg}
}

func (s *Service) config() *config.Config {
	if s.Cfg == nil {
		return &config.Config{}
	}
	return s.Cfg()
}

// Find returns the configured service, or nil when it is not in the config.
func (s *Service) Find(id string) *config.Service {
	return s.config().Find(id)
}

// Services returns every configured service in config order.
func (s *Service) Services() []config.Service {
	return s.config().Services
}

// RetentionWindow returns the unix cutoff for the current retention setting.
func (s *Service) RetentionWindow() int64 {
	days := s.config().Settings.Check.RetentionDays
	if days < 1 {
		days = config.DefaultRetentionDays
	}
	return time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
}

// ServiceStatus is one service definition plus its aggregated runtime metrics.
// Metrics is nil when the service has no history yet.
type ServiceStatus struct {
	Service config.Service
	Metrics *db.ServiceMetrics
}

// ServiceList returns every configured service with its metrics, in config
// order, over the retention window.
func (s *Service) ServiceList() ([]ServiceStatus, error) {
	cfg := s.config()
	metrics, err := s.DB.MetricsForAll(s.RetentionWindow(), 24)
	if err != nil {
		return nil, err
	}
	out := make([]ServiceStatus, 0, len(cfg.Services))
	for _, svc := range cfg.Services {
		out = append(out, ServiceStatus{Service: svc, Metrics: metrics[svc.ID]})
	}
	return out, nil
}

// ServiceStatusFor returns one service with its metrics over the retention
// window. ErrServiceNotFound when the id is not in the config.
func (s *Service) ServiceStatusFor(id string) (*ServiceStatus, error) {
	svc := s.Find(id)
	if svc == nil {
		return nil, ErrServiceNotFound
	}
	metrics, err := s.DB.MetricsForAll(s.RetentionWindow(), 24)
	if err != nil {
		// The list handler tolerated this error; a metrics read is an
		// enhancement, not a reason to fail the service's own page.
		return &ServiceStatus{Service: *svc}, nil
	}
	return &ServiceStatus{Service: *svc, Metrics: metrics[id]}, nil
}

// UptimeWindows is a service's uptime percentage over three fixed windows.
type UptimeWindows struct {
	Days7   float64
	Days30  float64
	Days365 float64
}

// MetricsResult is everything a service's metrics view needs: the definition,
// its aggregates, a bucketed series, multi-window uptime and the TLS snapshot.
type MetricsResult struct {
	Service       config.Service
	Status        *db.ServiceMetrics
	Series        []db.SeriesPoint
	From          int64
	To            int64
	BucketSeconds int64
	Uptime        UptimeWindows
	TLS           *db.ServiceTLS
}

// ServiceMetrics composes one service's metrics view for a ?range= key.
//
// Only two failures are returned: an unknown service, and a series query that
// did not run. The aggregates, uptime windows and TLS snapshot degrade to zero
// values on error, matching the long-standing behaviour of the endpoint — a
// missing sparkline must not blank the whole page.
//
// The composed result is cached (keyed on the service, the range and a
// quantised "now"), because it is the most expensive read the API serves.
func (s *Service) ServiceMetrics(ctx context.Context, id, rangeKey string, now time.Time) (*MetricsResult, error) {
	svc := s.Find(id)
	if svc == nil {
		return nil, ErrServiceNotFound
	}
	live := *svc
	gen := s.Cache.Generation(ctx, metricsScope(id))
	key := cache.Key(cacheVersion, "metrics", gen, id, rangeKey, keyTime(now, id+"|"+rangeKey))
	res, err := cache.Remember(ctx, s.Cache, key, metricsCacheTTL, func(ctx context.Context) (*MetricsResult, error) {
		return s.loadServiceMetrics(live, id, rangeKey, now)
	})
	if err != nil {
		return nil, err
	}
	// The service definition is config, not data: always serve the live one so
	// an edit is never hidden behind a cached copy.
	res.Service = live
	return res, nil
}

func (s *Service) loadServiceMetrics(svc config.Service, id, rangeKey string, now time.Time) (*MetricsResult, error) {
	since := RangeSince(now, rangeKey)
	bucket := ChooseBucket(now.Unix()-since.Unix(), svc.Check.Interval)

	metrics, _ := s.DB.MetricsForAll(since.Unix(), 60)
	series, err := s.DB.SeriesFor(id, since.Unix(), now.Unix(), bucket)
	if err != nil {
		return nil, err
	}
	if series == nil {
		series = []db.SeriesPoint{}
	}

	day := 24 * time.Hour
	up7, _, _ := s.DB.UptimeSince(id, now.Add(-7*day).Unix())
	up30, _, _ := s.DB.UptimeSince(id, now.Add(-30*day).Unix())
	up365, _, _ := s.DB.UptimeSince(id, now.Add(-365*day).Unix())
	tlsInfo, _ := s.DB.GetServiceTLS(id)

	return &MetricsResult{
		Service:       svc,
		Status:        metrics[id],
		Series:        series,
		From:          since.Unix(),
		To:            now.Unix(),
		BucketSeconds: bucket,
		Uptime:        UptimeWindows{Days7: up7, Days30: up30, Days365: up365},
		TLS:           tlsInfo,
	}, nil
}

// CheckPage is a page of stored check cycles plus the cursor for the next one
// (nil once there is nothing older to fetch).
type CheckPage struct {
	Checks     []db.Check
	NextBefore *int64
}

// CheckHistory returns a service's stored cycles, newest first. beforeID is an
// exclusive keyset cursor (0 for the first page). limit is clamped into
// [1, MaxCheckPageSize]; a non-positive limit means the default page size.
//
// Cached per (service, before, limit). The key carries every parameter that
// changes the page, so two different pages can never share an entry.
func (s *Service) CheckHistory(ctx context.Context, id string, beforeID int64, limit int) (*CheckPage, error) {
	if s.Find(id) == nil {
		return nil, ErrServiceNotFound
	}
	if limit <= 0 {
		limit = DefaultCheckPageSize
	}
	if limit > MaxCheckPageSize {
		limit = MaxCheckPageSize
	}
	gen := s.Cache.Generation(ctx, checksScope(id))
	key := cache.Key(cacheVersion, "checks", gen, id,
		strconv.FormatInt(beforeID, 10), strconv.Itoa(limit))
	return cache.Remember(ctx, s.Cache, key, checksCacheTTL, func(ctx context.Context) (*CheckPage, error) {
		return s.loadCheckHistory(id, beforeID, limit)
	})
}

func (s *Service) loadCheckHistory(id string, beforeID int64, limit int) (*CheckPage, error) {
	rows, err := s.DB.ListChecks(id, beforeID, limit)
	if err != nil {
		return nil, err
	}
	page := &CheckPage{Checks: rows}
	// Only advertise a cursor on a full page; a short page is the end.
	if len(rows) == limit && limit > 0 {
		next := rows[len(rows)-1].ID
		page.NextBefore = &next
	}
	return page, nil
}

// IncidentFilter selects a page of incidents. Empty fields mean "no filter".
type IncidentFilter struct {
	ServiceID string
	Status    string
	Severity  string
	Limit     int
	Offset    int
}

// IncidentPage is a page of incidents plus the total matching the same filter
// (ignoring limit/offset), so a client can render page numbers.
type IncidentPage struct {
	Incidents []db.Incident
	Total     int
}

// Incidents returns a filtered page of incidents, newest first, with the total
// the filter matches. Limit <= 0 means "no limit" (the count is still returned).
func (s *Service) Incidents(f IncidentFilter) (*IncidentPage, error) {
	total, err := s.DB.CountIncidents(f.ServiceID, f.Status, f.Severity)
	if err != nil {
		return nil, err
	}
	list, err := s.DB.ListIncidents(f.ServiceID, f.Status, f.Severity, f.Limit, f.Offset)
	if err != nil {
		return nil, err
	}
	return &IncidentPage{Incidents: list, Total: total}, nil
}

// IncidentDetail is one incident with its comments.
type IncidentDetail struct {
	Incident db.Incident
	Comments []db.IncidentComment
}

// IncidentDetail loads one incident and everything shown beside it.
func (s *Service) IncidentDetail(id int64) (*IncidentDetail, error) {
	inc, err := s.DB.GetIncident(id)
	if err != nil {
		return nil, err
	}
	comments, err := s.DB.ListIncidentComments(id)
	if err != nil {
		return nil, err
	}
	if comments == nil {
		comments = []db.IncidentComment{}
	}
	return &IncidentDetail{Incident: *inc, Comments: comments}, nil
}

// DashboardStats returns the aggregates the main dashboard's tiles need beyond
// what the service list already carries — currently the count of distinct
// services that logged a warning cycle inside settings.dashboard.
// warning_period_hours.
//
// Cached under one invalidation scope, since it aggregates across services, and
// keyed on the configured window plus a quantised "now".
func (s *Service) DashboardStats(ctx context.Context, now time.Time) (int, error) {
	hours := s.config().Settings.Dashboard.WarningPeriodHours
	if hours < 1 {
		hours = 1
	}
	gen := s.Cache.Generation(ctx, dashScope)
	key := cache.Key(cacheVersion, "dash", gen, strconv.Itoa(hours), keyTime(now, "dash"))
	return cache.Remember(ctx, s.Cache, key, dashboardCacheTTL, func(ctx context.Context) (int, error) {
		since := now.Add(-time.Duration(hours) * time.Hour).Unix()
		return s.DB.WarningServiceCountSince(since)
	})
}
