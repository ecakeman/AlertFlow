package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v3"
)

// agentReportDTO is the diagnostic agent's report for an incident, proxied from
// the agent service so the browser never sees the agent's API token. When the
// agent is not configured, has no run for the incident, or is unreachable the
// response is Available=false with a short Reason instead of an error status —
// a missing diagnosis is a normal state, not a failure of the incident page.
type agentReportDTO struct {
	Available bool            `json:"available"`
	Reason    string          `json:"reason,omitempty"`
	RunID     string          `json:"runId,omitempty"`
	Status    string          `json:"status,omitempty"`
	EventType string          `json:"eventType,omitempty"`
	CreatedAt string          `json:"createdAt,omitempty"`
	Usage     json.RawMessage `json:"usage,omitempty"`
	Report    json.RawMessage `json:"report,omitempty"`
	Evidence  json.RawMessage `json:"evidence,omitempty"`
}

// agentRun is the subset of an agent run row this proxy reads.
type agentRun struct {
	ID         string          `json:"id"`
	IncidentID int64           `json:"incident_id"`
	EventType  string          `json:"event_type"`
	Status     string          `json:"status"`
	CreatedAt  string          `json:"created_at"`
	Usage      json.RawMessage `json:"usage"`
}

type agentRunReport struct {
	Report   json.RawMessage `json:"report"`
	Evidence json.RawMessage `json:"evidence"`
}

// agentHTTPClient is shared across requests; agent lookups are short and local.
var agentHTTPClient = &http.Client{Timeout: 8 * time.Second}

// GET /api/incidents/:id/agent-report → the diagnostic agent's report for this
// incident. The agent keeps its own store; AlertFlow only proxies a read-only
// lookup so the UI can show the diagnosis beside the incident.
func (s *Server) handleIncidentAgentReport(c fiber.Ctx) error {
	inc, err := s.incidentParam(c)
	if err != nil {
		return err
	}
	if s.agentBaseURL == "" || s.agentAPIToken == "" {
		return c.JSON(agentReportDTO{Available: false, Reason: "agent not configured"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	run, err := s.agentRunForIncident(ctx, inc.ID)
	if err != nil {
		return c.JSON(agentReportDTO{Available: false, Reason: "agent unreachable"})
	}
	if run == nil {
		return c.JSON(agentReportDTO{Available: false, Reason: "no agent run for this incident"})
	}

	out := agentReportDTO{
		Available: true,
		RunID:     run.ID,
		Status:    run.Status,
		EventType: run.EventType,
		CreatedAt: run.CreatedAt,
		Usage:     run.Usage,
	}
	// A report may not exist yet (run still in flight); that is not an error.
	if rep, err := s.agentReport(ctx, run.ID); err == nil && rep != nil {
		out.Report = rep.Report
		out.Evidence = rep.Evidence
	}
	return c.JSON(out)
}

// agentGet performs an authenticated GET against the agent API, decoding the
// JSON body into dst. The body is capped to keep a misbehaving agent bounded.
func (s *Server) agentGet(ctx context.Context, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.agentBaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.agentAPIToken)
	resp, err := agentHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return errors.New("agent: unexpected status")
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(dst)
}

// agentRunForIncident returns the newest agent run for an incident, or nil when
// the agent has none (e.g. a warning incident, or one predating the agent).
func (s *Server) agentRunForIncident(ctx context.Context, incidentID int64) (*agentRun, error) {
	var list struct {
		Runs []agentRun `json:"runs"`
	}
	if err := s.agentGet(ctx, "/api/v1/runs", &list); err != nil {
		return nil, err
	}
	var best *agentRun
	for i := range list.Runs {
		r := &list.Runs[i]
		if r.IncidentID != incidentID {
			continue
		}
		if best == nil || r.CreatedAt > best.CreatedAt {
			best = r
		}
	}
	return best, nil
}

func (s *Server) agentReport(ctx context.Context, runID string) (*agentRunReport, error) {
	var rep agentRunReport
	if err := s.agentGet(ctx, "/api/v1/runs/"+runID+"/report", &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}
