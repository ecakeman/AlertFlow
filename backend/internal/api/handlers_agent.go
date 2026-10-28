package api

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"net/http"
)

// agentReportDTO is the diagnostic agent's result for an incident, read from
// AlertFlow's own database rather than proxied from the agent.
//
// A missing diagnosis is a normal state, not a failure of the incident page:
// when there is nothing to show the response is Available=false with a short
// Reason instead of an error status.
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

// GET /api/incidents/:id/agent-report → the diagnostic agent's result for this
// incident.
//
// The agent pushes its result to POST /webhooks/agent, so this read works
// whether or not the agent is online right now. A run that has started but not
// finished yet is Available=true with no Report — the UI's "still running"
// state.
func (s *Server) handleIncidentAgentReport(c *gin.Context) error {
	inc, err := s.incidentParam(c)
	if err != nil {
		return err
	}
	if s.agentCallbackToken == "" {
		return writeJSON(c, http.StatusOK, agentReportDTO{Available: false, Reason: "agent not configured"})
	}
	rec, err := s.conn().LatestAgentRunForIncident(inc.ID)
	if err != nil {
		return fail(http.StatusInternalServerError, "could not load the agent report")
	}
	if rec == nil {
		return writeJSON(c, http.StatusOK, agentReportDTO{Available: false, Reason: "no agent run for this incident"})
	}
	return writeJSON(c, http.StatusOK, agentReportDTO{
		Available: true,
		RunID:     rec.RunID,
		Status:    rec.Status,
		EventType: rec.EventType,
		// updated_at, not created_at: the newest message about the run is what
		// the page should be describing.
		CreatedAt: iso(rec.UpdatedAt),
		Usage:     rec.Usage,
		Report:    rec.Report,
		Evidence:  rec.Evidence,
	})
}
