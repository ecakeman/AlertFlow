package api

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/ecakeman/alertflow/internal/db"
)

// agentCallbackBodyLimit bounds one callback. A report plus its cited evidence
// is the largest thing either side sends, so the limit sits well above the
// webhook body limit and still finite.
const agentCallbackBodyLimit = 1 << 20

// agentCallbackEnvelope is the v2 message the agent pushes back.
type agentCallbackEnvelope struct {
	SchemaVersion string          `json:"schema_version"`
	MessageID     string          `json:"message_id"`
	MessageType   string          `json:"message_type"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

type agentCallbackPayload struct {
	RunID      string          `json:"run_id"`
	IncidentID *int64          `json:"incident_id"`
	ServiceID  string          `json:"service_id"`
	EventType  string          `json:"event_type"`
	Mode       string          `json:"mode"`
	Status     string          `json:"status"`
	Report     json.RawMessage `json:"report"`
	Evidence   json.RawMessage `json:"evidence"`
	Usage      json.RawMessage `json:"usage"`
	Error      string          `json:"error"`
	StartedAt  string          `json:"started_at"`
	FinishedAt string          `json:"finished_at"`
}

// agentCallbackTypes are the run-lifecycle messages this edge accepts. An
// unknown type is rejected loudly: silently accepting it would leave the agent
// believing something was stored when it was not.
var agentCallbackTypes = map[string]bool{
	"run.started":   true,
	"run.completed": true,
	"run.failed":    true,
}

// POST /webhooks/agent → the diagnostic agent pushes a run result back.
//
// This is a different edge from POST /api/alerts/webhook (external alerts
// coming in): its own token, its own body limit, and message_id as the
// idempotency key. A storage failure answers 5xx on purpose, so the agent keeps
// retrying instead of the result being dropped.
func (s *Server) handleAgentCallback(c fiber.Ctx) error {
	if s.agentCallbackToken == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "agent callback token is not configured")
	}
	if !bearerMatch(c.Get("Authorization"), s.agentCallbackToken) {
		return fiber.NewError(fiber.StatusUnauthorized, "unauthorized")
	}
	body := c.Body()
	if len(body) > agentCallbackBodyLimit {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, "body too large")
	}
	var env agentCallbackEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if env.SchemaVersion != "v2" {
		return fiber.NewError(fiber.StatusBadRequest, "schema_version must be v2")
	}
	if env.MessageID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "message_id is required")
	}
	if !agentCallbackTypes[env.MessageType] {
		return fiber.NewError(fiber.StatusBadRequest, "unsupported message_type")
	}
	var payload agentCallbackPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid payload")
	}
	if payload.RunID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "run_id is required")
	}

	inserted, err := s.conn().SaveAgentRun(db.AgentRunRecord{
		MessageID:     env.MessageID,
		RunID:         payload.RunID,
		MessageType:   env.MessageType,
		CorrelationID: env.CorrelationID,
		IncidentID:    payload.IncidentID,
		ServiceID:     payload.ServiceID,
		EventType:     payload.EventType,
		Mode:          payload.Mode,
		Status:        payload.Status,
		Report:        payload.Report,
		Evidence:      payload.Evidence,
		Usage:         payload.Usage,
		Error:         payload.Error,
		StartedAt:     parseAgentTime(payload.StartedAt),
		FinishedAt:    parseAgentTime(payload.FinishedAt),
	})
	if err != nil {
		slog.ErrorContext(c.Context(), "agent callback: store failed", "message_id", env.MessageID, "error", err)
		return fiber.NewError(fiber.StatusInternalServerError, "could not store the agent result")
	}
	return c.JSON(fiber.Map{
		"ok":        true,
		"duplicate": !inserted,
		"message_id": env.MessageID,
		"run_id":    payload.RunID,
	})
}

// parseAgentTime turns an RFC3339 stamp into unix seconds. An absent or
// unparsable stamp becomes nil rather than an error: the result still matters
// even if its timestamp does not.
func parseAgentTime(raw string) *int64 {
	if raw == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}
	seconds := parsed.Unix()
	return &seconds
}
