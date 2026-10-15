package query

import (
	"testing"
	"time"

	"github.com/ecakeman/alertflow/internal/config"
	"github.com/ecakeman/alertflow/internal/db"
)

// ctxTestConfig is the config snapshot the context builder reads through.
func ctxTestConfig() *config.Config {
	return &config.Config{
		Services: []config.Service{{
			ID: "svc", Name: "API", URL: "https://example.com/health",
			Check: config.ServiceCheck{Interval: 30, Method: "GET", Timeout: 5},
		}},
	}
}

func TestDiagnosticContextAnchorsOnEventTime(t *testing.T) {
	database := db.OpenTest(t)
	cfg := ctxTestConfig()
	s := New(database, func() *config.Config { return cfg })

	event := time.Unix(1_700_000_000, 0)
	// One check either side of the event, plus two before it.
	for _, offset := range []int64{-120, -60, 0, 60} {
		if err := database.InsertCheck("svc", event.Unix()+offset, db.StatusOffline, nil, nil, "boom", 1); err != nil {
			t.Fatalf("insert check: %v", err)
		}
	}
	inc, err := database.CreateIncident("svc", "auto", event.Unix(), nil, nil)
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}

	ctx, err := s.DiagnosticContext(ContextOptions{IncidentID: inc.ID, EventTime: event, Now: event})
	if err != nil {
		t.Fatalf("DiagnosticContext: %v", err)
	}

	if ctx.Incident == nil || ctx.Incident.ID != inc.ID {
		t.Fatalf("incident = %+v", ctx.Incident)
	}
	if !ctx.Service.Present || ctx.Service.Name != "API" {
		t.Fatalf("service = %+v", ctx.Service)
	}
	// The check at the event itself is the trigger; the later one is out of window.
	if ctx.TriggeringCheck == nil {
		t.Fatalf("expected a triggering check")
	}
	if got := ctx.TriggeringCheck.Ts; got != iso(event.Unix()) {
		t.Fatalf("trigger ts = %s, want %s", got, iso(event.Unix()))
	}
	// The window ends at the event, so the check after it is excluded.
	for _, c := range ctx.RecentChecks {
		if c.Ts == iso(event.Unix()+60) {
			t.Fatalf("check after the event leaked into the window: %+v", c)
		}
	}
	// Chronological order.
	for i := 1; i < len(ctx.RecentChecks); i++ {
		if ctx.RecentChecks[i-1].Ts > ctx.RecentChecks[i].Ts {
			t.Fatalf("recent checks not chronological: %v then %v", ctx.RecentChecks[i-1].Ts, ctx.RecentChecks[i].Ts)
		}
	}
	if ctx.TimeWindow.EventTime != iso(event.Unix()) {
		t.Fatalf("event time = %s", ctx.TimeWindow.EventTime)
	}
	// A delayed delivery must not move the window: passing a much later Now
	// leaves the anchor and the upper bound untouched.
	later, err := s.DiagnosticContext(ContextOptions{IncidentID: inc.ID, EventTime: event, Now: event.Add(2 * time.Hour)})
	if err != nil {
		t.Fatalf("DiagnosticContext(later): %v", err)
	}
	if later.TimeWindow != ctx.TimeWindow {
		t.Fatalf("window moved with delivery time: %+v vs %+v", later.TimeWindow, ctx.TimeWindow)
	}
}

func TestDiagnosticContextTruncatesChecks(t *testing.T) {
	database := db.OpenTest(t)
	cfg := ctxTestConfig()
	s := New(database, func() *config.Config { return cfg })

	event := time.Unix(1_700_000_000, 0)
	// More checks inside the window than the context may carry. Insert oldest
	// first so the append-only id order matches the ts order, exactly as the
	// scheduler produces them.
	for i := MaxContextChecks + 25; i >= 0; i-- {
		ts := event.Unix() - int64(i)
		if err := database.InsertCheck("svc", ts, db.StatusOnline, nil, nil, "", 1); err != nil {
			t.Fatalf("insert check: %v", err)
		}
	}
	inc, err := database.CreateIncident("svc", "auto", event.Unix(), nil, nil)
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}

	ctx, err := s.DiagnosticContext(ContextOptions{
		IncidentID: inc.ID, EventTime: event, Now: event, CheckLookbackMinutes: 24 * 60,
	})
	if err != nil {
		t.Fatalf("DiagnosticContext: %v", err)
	}
	if len(ctx.RecentChecks) != MaxContextChecks {
		t.Fatalf("kept %d checks, want %d", len(ctx.RecentChecks), MaxContextChecks)
	}
	if !contains(ctx.Coverage.Truncated, GapChecksTruncated) {
		t.Fatalf("truncation not recorded: %+v", ctx.Coverage)
	}
	if ctx.Coverage.Complete {
		t.Fatalf("truncated context reported as complete")
	}
}

func TestDiagnosticContextMissingServiceIsAGap(t *testing.T) {
	database := db.OpenTest(t)
	// Config knows nothing about the service the incident points at.
	s := New(database, func() *config.Config { return &config.Config{} })

	event := time.Unix(1_700_000_000, 0)
	inc, err := database.CreateIncident("gone", "auto", event.Unix(), nil, nil)
	if err != nil {
		t.Fatalf("create incident: %v", err)
	}
	ctx, err := s.DiagnosticContext(ContextOptions{IncidentID: inc.ID, EventTime: event, Now: event})
	if err != nil {
		t.Fatalf("DiagnosticContext: %v", err)
	}
	if ctx.Service.Present {
		t.Fatalf("service reported present: %+v", ctx.Service)
	}
	if !contains(ctx.Coverage.Gaps, GapServiceNotInConfig) {
		t.Fatalf("missing service not recorded as a gap: %+v", ctx.Coverage)
	}
	if ctx.Coverage.Sources["service"] != SourceUnavailable {
		t.Fatalf("source = %q, want %q", ctx.Coverage.Sources["service"], SourceUnavailable)
	}
}

func TestDiagnosticContextUnknownIncident(t *testing.T) {
	database := db.OpenTest(t)
	s := New(database, func() *config.Config { return ctxTestConfig() })
	if _, err := s.DiagnosticContext(ContextOptions{IncidentID: 999999}); err != ErrIncidentNotFound {
		t.Fatalf("err = %v, want ErrIncidentNotFound", err)
	}
}

func TestClipTextCountsRunes(t *testing.T) {
	long := ""
	for i := 0; i < MaxContextTextRunes+50; i++ {
		long += "中"
	}
	got := clipText(long)
	if r := []rune(got); len(r) != MaxContextTextRunes+1 { // +1 for the ellipsis
		t.Fatalf("clipped to %d runes, want %d", len(r), MaxContextTextRunes+1)
	}
	if short := clipText("ok"); short != "ok" {
		t.Fatalf("short text changed: %q", short)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
