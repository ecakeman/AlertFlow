package query

import (
	"errors"
	"time"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
)

const (
	DefaultTimelinePageSize = 50
	MaxTimelinePageSize     = 100
	maxTimelineTypes        = 10
)

// ErrTimelineRange means from/to are missing, unordered, or not a real window.
var ErrTimelineRange = errors.New("timeline window is invalid")

// ErrTimelineFilter means a source or type filter is not accepted.
var ErrTimelineFilter = errors.New("timeline filter is invalid")

// TimelineRequest is the parsed query. Zero From/To means the last 24 hours.
type TimelineRequest struct {
	From      time.Time
	To        time.Time
	Sources   []string
	Types     []string
	Limit     int
	CursorAt  int64
	CursorID  int64
	HasCursor bool
	Now       time.Time
}

// TimelineCoverage is one collector's health. Empty means configured but not
// observed yet. Disabled means the service has no mapping and no stored row.
type TimelineCoverage struct {
	Status      string
	LastSuccess *int64
	LastError   string
}

// TimelinePage is one keyset page plus the window that was actually read.
type TimelinePage struct {
	Events        []db.RuntimeEvent
	NextCursorAt  int64
	NextCursorID  int64
	HasNext       bool
	From          time.Time
	To            time.Time
	RetentionDays int
	Coverage      map[string]TimelineCoverage
}

// TimelineSourceAllowed reports whether a client may filter on that source.
func TimelineSourceAllowed(source string) bool {
	switch source {
	case "incident", "application_diagnostics", "kubernetes", "agent":
		return true
	default:
		return false
	}
}

// ServiceTimeline reads one service's events. It is not cached.
func (s *Service) ServiceTimeline(id string, req TimelineRequest) (*TimelinePage, error) {
	svc := s.Find(id)
	if svc == nil {
		return nil, ErrServiceNotFound
	}
	if len(req.Types) > maxTimelineTypes {
		return nil, ErrTimelineFilter
	}
	for _, source := range req.Sources {
		if !TimelineSourceAllowed(source) {
			return nil, ErrTimelineFilter
		}
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	to := req.To
	if to.IsZero() {
		to = now
	}
	from := req.From
	if from.IsZero() {
		from = to.Add(-24 * time.Hour)
	}
	if !from.Before(to) {
		return nil, ErrTimelineRange
	}
	days := db.TimelineRetentionDays()
	floor := now.Add(-time.Duration(days) * 24 * time.Hour)
	if from.Before(floor) {
		from = floor
	}
	limit := req.Limit
	if limit <= 0 {
		limit = DefaultTimelinePageSize
	}
	if limit > MaxTimelinePageSize {
		limit = MaxTimelinePageSize
	}
	sources, err := s.DB.ListRuntimeSources(id)
	if err != nil {
		return nil, err
	}
	if !from.Before(to) {
		from = to
	}
	page := &TimelinePage{
		Events:        []db.RuntimeEvent{},
		From:          from.UTC(),
		To:            to.UTC(),
		RetentionDays: days,
		Coverage:      timelineCoverage(*svc, sources),
	}
	if !from.Before(to) {
		return page, nil
	}
	filterSources := req.Sources
	if len(filterSources) == 0 {
		filterSources = []string{"incident", "application_diagnostics", "kubernetes", "agent"}
	}
	rows, err := s.DB.ListRuntimeEvents(db.TimelineFilter{
		ServiceID: id,
		From:      from.Unix(),
		To:        to.Unix(),
		Sources:   filterSources,
		Types:     req.Types,
		CursorAt:  req.CursorAt,
		CursorID:  req.CursorID,
		HasCursor: req.HasCursor,
		Limit:     limit + 1,
	})
	if err != nil {
		return nil, err
	}
	if len(rows) > limit {
		page.HasNext = true
		rows = rows[:limit]
		last := rows[len(rows)-1]
		page.NextCursorAt = last.OccurredAt
		page.NextCursorID = last.ID
	}
	if rows != nil {
		page.Events = rows
	}
	return page, nil
}

func timelineCoverage(svc config.Service, rows []db.RuntimeSource) map[string]TimelineCoverage {
	found := map[string]db.RuntimeSource{}
	for _, row := range rows {
		found[row.Source] = row
	}
	diagConfigured := svc.Diagnostics.URL != ""
	k8s := svc.Kubernetes
	k8sConfigured := k8s.ClusterID != "" && k8s.Namespace != "" && k8s.WorkloadName != ""
	return map[string]TimelineCoverage{
		"application_diagnostics": oneCoverage(diagConfigured, found, "application_diagnostics"),
		"kubernetes":              oneCoverage(k8sConfigured, found, "kubernetes"),
	}
}

func oneCoverage(configured bool, found map[string]db.RuntimeSource, source string) TimelineCoverage {
	row, ok := found[source]
	if !ok {
		if !configured {
			return TimelineCoverage{Status: "disabled"}
		}
		return TimelineCoverage{Status: "empty"}
	}
	out := TimelineCoverage{Status: row.Status, LastError: row.LastError}
	if row.LastSuccess.Valid {
		v := row.LastSuccess.Int64
		out.LastSuccess = &v
	}
	return out
}
