package query

import (
	"strings"
	"time"

	"github.com/ecakeman/alertflow/internal/db"
)

// Bounds on the diagnostic context. The context seeds an Agent run over a
// webhook, so it must be big enough to start a real investigation and small
// enough to fit a fixed request body. Anything past these limits is dropped and
// recorded in Coverage.Truncated — never silently.
const (
	MaxContextChecks            = 200
	MaxContextMetricPoints      = 200
	MaxContextComments          = 50
	MaxContextRelatedIncidents  = 10
	MaxContextTextRunes         = 2000
	MaxContextLookbackHours     = 24
	DefaultCheckLookbackMinutes = 60

	// contextScanPages bounds how many ListChecks pages the window walk will
	// read, so an incident in the far past cannot make the builder scan the
	// whole retention window one page at a time.
	contextScanPages = 20
)

// Coverage source states: whether a data source answered.
const (
	SourceOK          = "ok"
	SourceEmpty       = "empty"
	SourceUnavailable = "unavailable"
)

// Coverage gap codes. They name what is missing or cut, so an Agent can say
// "unknown because the source was silent" instead of "healthy".
const (
	GapServiceNotInConfig = "SERVICE_NOT_IN_CONFIG"
	GapNoTriggeringCheck  = "NO_TRIGGERING_CHECK"
	GapChecksTruncated    = "CHECKS_TRUNCATED"
	GapChecksUnavailable  = "CHECKS_UNAVAILABLE"
	GapMetricsTruncated   = "METRICS_TRUNCATED"
	GapMetricsUnavailable = "METRICS_UNAVAILABLE"
	GapCommentsTruncated  = "COMMENTS_TRUNCATED"
)

// DiagnosticContext is the default evidence AlertFlow hands the Agent so a run
// can start without re-reading what the Go side already knows.
//
// Field names are camelCase to match the Agent's existing read shapes (the
// /internal/agent/v1 responses), so the same normalisation applies on both
// paths. The envelope that carries this object is versioned separately.
type DiagnosticContext struct {
	Incident         *IncidentContext  `json:"incident"`
	Service          ServiceContext    `json:"service"`
	TriggeringCheck  *CheckContext     `json:"triggeringCheck,omitempty"`
	RecentChecks     []CheckContext    `json:"recentChecks"`
	Metrics          MetricsContext    `json:"metrics"`
	RelatedIncidents []IncidentSummary `json:"relatedIncidents"`
	TimeWindow       TimeWindow        `json:"timeWindow"`
	Coverage         Coverage          `json:"coverage"`
}

// IncidentContext is the incident plus the notes left on it.
type IncidentContext struct {
	ID          int64            `json:"id"`
	ServiceID   string           `json:"serviceId"`
	ServiceName string           `json:"serviceName"`
	Status      string           `json:"status"`
	Severity    string           `json:"severity"`
	Title       string           `json:"title"`
	StartedAt   string           `json:"startedAt"`
	ResolvedAt  *string          `json:"resolvedAt"`
	Comments    []CommentContext `json:"comments"`
}

// CommentContext is one note on an incident.
type CommentContext struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

// ServiceContext is the service definition the Agent needs to reason about a
// check cadence and retry policy. It carries no secrets.
type ServiceContext struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	URL     string          `json:"url"`
	Present bool            `json:"present"`
	Check   CheckConfigView `json:"check"`
}

// CheckConfigView is the health-check CONFIGURATION, not a stored check row.
type CheckConfigView struct {
	Interval       int    `json:"interval"`
	Method         string `json:"method"`
	Timeout        int    `json:"timeout"`
	ExpectedStatus []int  `json:"expectedStatus"`
	RetryAttempts  int    `json:"retryAttempts"`
	RetryDelays    []int  `json:"retryDelays"`
}

// CheckContext is one stored check cycle.
type CheckContext struct {
	ID         int64  `json:"id"`
	Ts         string `json:"ts"`
	Status     string `json:"status"`
	LatencyMs  *int   `json:"latencyMs"`
	StatusCode *int   `json:"statusCode"`
	Error      string `json:"error"`
	Attempts   int    `json:"attempts"`
}

// MetricsContext is the bucketed latency series plus the current TLS snapshot.
type MetricsContext struct {
	From          string          `json:"from"`
	To            string          `json:"to"`
	BucketSeconds int64           `json:"bucketSeconds"`
	Series        []SeriesContext `json:"series"`
	TLS           *TLSContext     `json:"tls,omitempty"`
}

// SeriesContext is one bucket of the metrics series.
type SeriesContext struct {
	Ts         int64    `json:"ts"`
	AvgLatency *float64 `json:"avgLatency"`
	Errors     int      `json:"errors"`
	Warnings   int      `json:"warnings"`
}

// TLSContext is the certificate snapshot, mirroring toTLSDTO on the HTTP path.
type TLSContext struct {
	CheckedAt  string  `json:"checkedAt"`
	ValidFrom  *string `json:"validFrom"`
	ValidUntil *string `json:"validUntil"`
	Issuer     string  `json:"issuer"`
	Subject    string  `json:"subject"`
	DaysLeft   *int    `json:"daysLeft"`
	Error      string  `json:"error"`
}

// IncidentSummary is a related incident, without its comments.
type IncidentSummary struct {
	ID         int64   `json:"id"`
	ServiceID  string  `json:"serviceId"`
	Status     string  `json:"status"`
	Severity   string  `json:"severity"`
	Title      string  `json:"title"`
	StartedAt  string  `json:"startedAt"`
	ResolvedAt *string `json:"resolvedAt"`
}

// TimeWindow states exactly which slice of time the context covers, anchored on
// the event rather than on "now at delivery time". A delayed delivery must not
// re-anchor the window on the delivery moment.
type TimeWindow struct {
	EventTime       string `json:"eventTime"`
	From            string `json:"from"`
	To              string `json:"to"`
	LookbackSeconds int64  `json:"lookbackSeconds"`
}

// Coverage records what the context does and does not contain, so an Agent can
// distinguish "no problem" from "no data".
type Coverage struct {
	Complete  bool              `json:"complete"`
	Truncated []string          `json:"truncated"`
	Gaps      []string          `json:"gaps"`
	Sources   map[string]string `json:"sources"`
}

// ContextOptions selects what to assemble. IncidentID is required; EventTime is
// the moment the incident (or the triggering check) happened and is used to
// anchor every window — pass it explicitly rather than letting the builder
// default to time.Now().
type ContextOptions struct {
	IncidentID int64
	EventTime  time.Time
	// CheckLookbackMinutes widens the check window before the event. Zero uses
	// DefaultCheckLookbackMinutes.
	CheckLookbackMinutes int
}

// DiagnosticContext assembles the bounded default context for one incident.
//
// It returns ErrIncidentNotFound when the incident is gone. Every other source
// failure degrades: the field is left empty and Coverage records why, because a
// missing series must not stop a diagnosis that can still be made from the rest.
func (s *Service) DiagnosticContext(opts ContextOptions) (*DiagnosticContext, error) {
	detail, err := s.IncidentDetail(opts.IncidentID)
	if err != nil {
		return nil, err
	}
	inc := detail.Incident

	eventTime := opts.EventTime
	if eventTime.IsZero() {
		eventTime = time.Unix(inc.StartedAt, 0)
	}
	// The check window starts at the incident (or the lookback, whichever is
	// older) and never reaches further back than the metrics window.
	lookback := opts.CheckLookbackMinutes
	if lookback <= 0 {
		lookback = DefaultCheckLookbackMinutes
	}
	checksFrom := eventTime.Add(-time.Duration(lookback) * time.Minute).Unix()
	if inc.StartedAt < checksFrom {
		checksFrom = inc.StartedAt
	}
	metricsFrom := eventTime.Add(-MaxContextLookbackHours * time.Hour).Unix()
	if checksFrom < metricsFrom {
		checksFrom = metricsFrom
	}

	ctx := &DiagnosticContext{
		Incident: &IncidentContext{
			ID:          inc.ID,
			ServiceID:   inc.ServiceID,
			ServiceName: s.serviceName(inc.ServiceID),
			Status:      inc.Status,
			Severity:    inc.Severity,
			Title:       clipText(inc.Title),
			StartedAt:   iso(inc.StartedAt),
			ResolvedAt:  isoPtr(inc.ResolvedAt),
		},
		RecentChecks:     []CheckContext{},
		RelatedIncidents: []IncidentSummary{},
		TimeWindow: TimeWindow{
			EventTime:       iso(eventTime.Unix()),
			From:            iso(metricsFrom),
			To:              iso(eventTime.Unix()),
			LookbackSeconds: eventTime.Unix() - metricsFrom,
		},
		Coverage: Coverage{
			Sources:   map[string]string{},
			Truncated: []string{},
			Gaps:      []string{},
		},
	}

	ctx.Incident.Comments = s.contextComments(detail.Comments, &ctx.Coverage)

	ctx.Service = s.contextService(inc.ServiceID, &ctx.Coverage)
	ctx.RecentChecks, ctx.TriggeringCheck = s.contextChecks(inc.ServiceID, checksFrom, eventTime.Unix(), &ctx.Coverage)
	ctx.Metrics = s.contextMetrics(inc.ServiceID, metricsFrom, eventTime.Unix(), &ctx.Coverage)
	ctx.RelatedIncidents = s.contextRelated(inc, &ctx.Coverage)

	ctx.Coverage.Complete = len(ctx.Coverage.Gaps) == 0 && len(ctx.Coverage.Truncated) == 0
	return ctx, nil
}

func (s *Service) serviceName(id string) string {
	if svc := s.Find(id); svc != nil {
		return svc.Name
	}
	return ""
}

func (s *Service) contextService(id string, cov *Coverage) ServiceContext {
	svc := s.Find(id)
	if svc == nil {
		cov.Sources["service"] = SourceUnavailable
		cov.Gaps = append(cov.Gaps, GapServiceNotInConfig)
		return ServiceContext{ID: id}
	}
	cov.Sources["service"] = SourceOK
	expected := svc.Check.ExpectedStatus
	if expected == nil {
		expected = []int{}
	}
	delays := svc.Check.RetryDelays
	if delays == nil {
		delays = []int{}
	}
	return ServiceContext{
		ID:      svc.ID,
		Name:    svc.Name,
		URL:     svc.URL,
		Present: true,
		Check: CheckConfigView{
			Interval:       svc.Check.Interval,
			Method:         svc.Check.Method,
			Timeout:        svc.Check.Timeout,
			ExpectedStatus: expected,
			RetryAttempts:  svc.Check.RetryAttempts,
			RetryDelays:    delays,
		},
	}
}

func (s *Service) contextComments(comments []db.IncidentComment, cov *Coverage) []CommentContext {
	if len(comments) == 0 {
		cov.Sources["comments"] = SourceEmpty
		return []CommentContext{}
	}
	cov.Sources["comments"] = SourceOK
	if len(comments) > MaxContextComments {
		comments = comments[len(comments)-MaxContextComments:]
		cov.Truncated = append(cov.Truncated, GapCommentsTruncated)
	}
	out := make([]CommentContext, 0, len(comments))
	for _, cm := range comments {
		out = append(out, CommentContext{
			ID:        cm.ID,
			Username:  cm.Username,
			Body:      clipText(cm.Body),
			CreatedAt: iso(cm.CreatedAt),
		})
	}
	return out
}

func (s *Service) contextChecks(serviceID string, from, to int64, cov *Coverage) ([]CheckContext, *CheckContext) {
	var (
		collected []db.Check
		before    int64
		trigger   *db.Check
		stop      bool
	)
	for page := 0; page < contextScanPages && !stop; page++ {
		rows, err := s.DB.ListChecks(serviceID, before, MaxCheckPageSize)
		if err != nil {
			cov.Sources["checks"] = SourceUnavailable
			cov.Gaps = append(cov.Gaps, GapChecksUnavailable)
			return []CheckContext{}, nil
		}
		if len(rows) == 0 {
			break
		}
		for i := range rows {
			row := rows[i]
			if row.Ts > to {
				continue
			}
			if row.Ts < from {
				stop = true
				break
			}
			if trigger == nil {
				t := row
				trigger = &t
			}
			if len(collected) >= MaxContextChecks {
				cov.Truncated = append(cov.Truncated, GapChecksTruncated)
				stop = true
				break
			}
			collected = append(collected, row)
		}
		before = rows[len(rows)-1].ID
		if len(rows) < MaxCheckPageSize {
			break
		}
	}

	if len(collected) == 0 {
		cov.Sources["checks"] = SourceEmpty
	} else {
		cov.Sources["checks"] = SourceOK
	}
	if trigger == nil {
		cov.Gaps = append(cov.Gaps, GapNoTriggeringCheck)
	}

	// ListChecks returns newest-first; flip to chronological for the prompt.
	out := make([]CheckContext, 0, len(collected))
	for i := len(collected) - 1; i >= 0; i-- {
		out = append(out, toCheckContext(collected[i]))
	}
	if trigger == nil {
		return out, nil
	}
	tc := toCheckContext(*trigger)
	return out, &tc
}

func toCheckContext(c db.Check) CheckContext {
	return CheckContext{
		ID: c.ID, Ts: iso(c.Ts), Status: c.Status,
		LatencyMs: c.LatencyMs, StatusCode: c.StatusCode, Error: clipText(c.Error), Attempts: c.Attempts,
	}
}

func (s *Service) contextMetrics(serviceID string, from, to int64, cov *Coverage) MetricsContext {
	span := to - from
	if span < 1 {
		span = 1
	}
	bucket := ChooseBucket(span, 0)
	points, err := s.DB.SeriesFor(serviceID, from, to, bucket)
	if err != nil {
		cov.Sources["metrics"] = SourceUnavailable
		cov.Gaps = append(cov.Gaps, GapMetricsUnavailable)
		return MetricsContext{From: iso(from), To: iso(to), BucketSeconds: bucket, Series: []SeriesContext{}}
	}
	if len(points) == 0 {
		cov.Sources["metrics"] = SourceEmpty
	} else {
		cov.Sources["metrics"] = SourceOK
	}
	if len(points) > MaxContextMetricPoints {
		points = points[len(points)-MaxContextMetricPoints:]
		cov.Truncated = append(cov.Truncated, GapMetricsTruncated)
	}
	series := make([]SeriesContext, 0, len(points))
	for _, p := range points {
		series = append(series, SeriesContext{Ts: p.Ts, AvgLatency: p.AvgLatency, Errors: p.Errors, Warnings: p.Warnings})
	}
	out := MetricsContext{From: iso(from), To: iso(to), BucketSeconds: bucket, Series: series}

	tlsInfo, err := s.DB.GetServiceTLS(serviceID)
	if err != nil {
		cov.Gaps = append(cov.Gaps, GapMetricsUnavailable)
		return out
	}
	out.TLS = toTLSContext(tlsInfo)
	return out
}

func toTLSContext(t *db.ServiceTLS) *TLSContext {
	if t == nil {
		return nil
	}
	out := &TLSContext{
		CheckedAt: iso(t.CheckedAt),
		Issuer:    t.Issuer,
		Subject:   t.Subject,
		Error:     t.Error,
	}
	if t.ValidFrom > 0 {
		out.ValidFrom = isoPtr(&t.ValidFrom)
	}
	if t.ValidUntil > 0 {
		out.ValidUntil = isoPtr(&t.ValidUntil)
		days := int(time.Until(time.Unix(t.ValidUntil, 0)).Hours() / 24)
		out.DaysLeft = &days
	}
	return out
}

func (s *Service) contextRelated(inc db.Incident, cov *Coverage) []IncidentSummary {
	// One extra row so a full page can be reported as truncated.
	rows, err := s.DB.ListIncidents(inc.ServiceID, "", "", MaxContextRelatedIncidents+1, 0)
	if err != nil {
		cov.Sources["related_incidents"] = SourceUnavailable
		return []IncidentSummary{}
	}
	cov.Sources["related_incidents"] = SourceOK
	out := make([]IncidentSummary, 0, len(rows))
	for _, r := range rows {
		if r.ID == inc.ID {
			continue
		}
		if len(out) >= MaxContextRelatedIncidents {
			cov.Truncated = append(cov.Truncated, "RELATED_INCIDENTS_TRUNCATED")
			break
		}
		out = append(out, IncidentSummary{
			ID: r.ID, ServiceID: r.ServiceID, Status: r.Status, Severity: r.Severity,
			Title: clipText(r.Title), StartedAt: iso(r.StartedAt), ResolvedAt: isoPtr(r.ResolvedAt),
		})
	}
	return out
}

// clipText bounds free text so one enormous comment cannot blow the request
// body. It counts runes, not bytes, so a CJK comment is not cut mid-character.
func clipText(s string) string {
	if len([]rune(s)) <= MaxContextTextRunes {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:MaxContextTextRunes])) + "…"
}

func iso(ts int64) string {
	return time.Unix(ts, 0).UTC().Format(time.RFC3339)
}

func isoPtr(ts *int64) *string {
	if ts == nil {
		return nil
	}
	s := iso(*ts)
	return &s
}
