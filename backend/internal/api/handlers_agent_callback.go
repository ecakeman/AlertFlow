package api

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"log/slog"
	"net/http"
	"strings"
	"time"

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

// Chat answers come back over the same edge but describe a question, not a run.
const (
	agentMsgChatCompleted = "chat.completed"
	agentMsgChatFailed    = "chat.failed"
)

var agentChatCallbackTypes = map[string]bool{
	agentMsgChatCompleted: true,
	agentMsgChatFailed:    true,
}

// agentChatCallbackPayload is the answer to a queued question.
type agentChatCallbackPayload struct {
	RequestID   string          `json:"request_id"`
	IncidentID  *int64          `json:"incident_id"`
	Reply       string          `json:"reply"`
	EvidenceIDs json.RawMessage `json:"evidence_ids"`
	Error       string          `json:"error"`
}

// POST /webhooks/agent → the diagnostic agent pushes a run result back.
//
// This is machine-facing and separate from everything the browser uses: its own
// token, its own body limit, and message_id as the idempotency key. A storage
// failure answers 5xx on purpose, so the agent keeps retrying instead of the
// result being dropped.
func (s *Server) handleAgentCallback(c *gin.Context) error {
	if s.agentCallbackToken == "" {
		return fail(http.StatusServiceUnavailable, "agent callback token is not configured")
	}
	if !bearerMatch(c.GetHeader("Authorization"), s.agentCallbackToken) {
		return fail(http.StatusUnauthorized, "unauthorized")
	}
	body, err := readBody(c)
	if err != nil {
		return err
	}
	if len(body) > agentCallbackBodyLimit {
		return fail(http.StatusRequestEntityTooLarge, "body too large")
	}
	var env agentCallbackEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fail(http.StatusBadRequest, "invalid request body")
	}
	if env.SchemaVersion != "v2" {
		return fail(http.StatusBadRequest, "schema_version must be v2")
	}
	if env.MessageID == "" {
		return fail(http.StatusBadRequest, "message_id is required")
	}
	if !agentCallbackTypes[env.MessageType] && !agentChatCallbackTypes[env.MessageType] {
		return fail(http.StatusBadRequest, "unsupported message_type")
	}
	if agentChatCallbackTypes[env.MessageType] {
		return s.handleAgentChatCallback(c, env)
	}
	var payload agentCallbackPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fail(http.StatusBadRequest, "invalid payload")
	}
	if payload.RunID == "" {
		return fail(http.StatusBadRequest, "run_id is required")
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
		slog.ErrorContext(c.Request.Context(), "agent callback: store failed", "message_id", env.MessageID, "error", err)
		return fail(http.StatusInternalServerError, "could not store the agent result")
	}
	return writeJSON(c, http.StatusOK, map[string]any{
		"ok":         true,
		"duplicate":  !inserted,
		"message_id": env.MessageID,
		"run_id":     payload.RunID,
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

// handleAgentChatCallback stores an answer to a queued question.
//
// A repeated callback changes nothing (the row is already answered) and reports
// duplicate=true — the same at-least-once/idempotent split as the run results.
func (s *Server) handleAgentChatCallback(c *gin.Context, env agentCallbackEnvelope) error {
	var payload agentChatCallbackPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fail(http.StatusBadRequest, "invalid payload")
	}
	if payload.RequestID == "" {
		return fail(http.StatusBadRequest, "request_id is required")
	}
	now := time.Now().Unix()

	var (
		applied bool
		err     error
	)
	if env.MessageType == agentMsgChatFailed {
		reason := strings.TrimSpace(payload.Error)
		if reason == "" {
			reason = "the agent could not answer"
		}
		applied, err = s.conn().FailChatRequest(payload.RequestID, reason, now)
	} else {
		applied, err = s.conn().CompleteChatRequest(payload.RequestID, payload.Reply, payload.EvidenceIDs, now)
	}
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "agent callback: chat store failed", "request_id", payload.RequestID, "error", err)
		return fail(http.StatusInternalServerError, "could not store the chat answer")
	}
	return writeJSON(c, http.StatusOK, map[string]any{
		"ok":         true,
		"duplicate":  !applied,
		"request_id": payload.RequestID,
	})
}
