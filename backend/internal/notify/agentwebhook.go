package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ecakeman/alertflow/internal/query"
)

// AgentWebhookType is the integration type reserved for the diagnostic Agent.
// It is deliberately distinct from "webhook": a generic webhook must never
// receive the internal diagnostic context, and only this type does.
const AgentWebhookType = "agent_webhook"

// AgentSchemaVersion is the version of the AlertFlow→Agent message contract.
const AgentSchemaVersion = "v2"

// AgentMaxPayloadBytes bounds one diagnostic message. It matches the Agent's
// inbound body limit; anything larger is trimmed here, with the trimming
// recorded in the context's coverage, rather than being sent and rejected.
const AgentMaxPayloadBytes = 512 << 10

// Message types on the AlertFlow→Agent edge.
const (
	AgentMsgRequested = "diagnostic.requested"
	AgentMsgResolved  = "diagnostic.resolved"
	AgentMsgWarning   = "diagnostic.warning"
	AgentMsgTest      = "diagnostic.test"
)

// AgentMessage is the versioned envelope AlertFlow pushes to the Agent.
//
// message_id is stable for one logical delivery, so a retry after a crash —
// or the same incident being re-delivered — is recognised by the Agent as the
// same task rather than a new one. correlation_id ties every message about one
// incident together.
type AgentMessage struct {
	SchemaVersion string       `json:"schema_version"`
	MessageID     string       `json:"message_id"`
	MessageType   string       `json:"message_type"`
	CorrelationID string       `json:"correlation_id"`
	SentAt        string       `json:"sent_at"`
	Payload       AgentPayload `json:"payload"`
}

// AgentPayload carries the event summary and, for a diagnostic request, the
// default context assembled before delivery.
type AgentPayload struct {
	Event             AgentEvent               `json:"event"`
	DiagnosticContext *query.DiagnosticContext `json:"diagnostic_context,omitempty"`
}

// AgentEvent summarises the transition that triggered the message. It mirrors
// the fields the generic webhook has always sent, so an Agent that logged the
// old payload still finds what it used to read.
type AgentEvent struct {
	EventID     string `json:"event_id"`
	Event       string `json:"event"`
	IncidentID  int64  `json:"incident_id,omitempty"`
	ServiceID   string `json:"service_id"`
	ServiceName string `json:"service_name"`
	ServiceURL  string `json:"service_url"`
	Status      string `json:"status"`
	Severity    string `json:"severity"`
	OccurredAt  string `json:"occurred_at"`
	StartedAt   string `json:"started_at,omitempty"`
	ResolvedAt  string `json:"resolved_at,omitempty"`
	Attempts    int    `json:"attempts,omitempty"`
	Error       string `json:"error,omitempty"`
	CheckID     int64  `json:"check_id,omitempty"`
}

// AgentSender builds and posts the v2 diagnostic message. It is the only
// sender that reads the monitoring database, because it is the only one that
// has to assemble a context. All secrets stay out of the payload.
type AgentSender struct {
	// Q is the same read-composition layer the API uses, so the Agent's default
	// context and the UI can never disagree about what an incident looks like.
	Q *query.Service
	// MaxBytes bounds the encoded message. Zero means AgentMaxPayloadBytes.
	MaxBytes int
}

// Send assembles and delivers one Agent message.
func (s AgentSender) Send(ctx context.Context, raw json.RawMessage, msg Message) error {
	var cfg webhookConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	if cfg.URL == "" {
		return errors.New("agent_webhook: url is required")
	}
	if s.Q == nil {
		return errors.New("agent_webhook: query service is not configured")
	}

	message := s.build(ctx, msg, cfg)
	limit := s.MaxBytes
	if limit <= 0 {
		limit = AgentMaxPayloadBytes
	}
	body, err := encodeAgentMessage(message, limit)
	if err != nil {
		return err
	}

	method := strings.ToUpper(strings.TrimSpace(cfg.Method))
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.URL, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	return doRequest(req)
}

// build assembles the envelope. A warning has no incident to anchor a context
// on, and the integration test has no incident at all, so both send the event
// alone.
func (s AgentSender) build(ctx context.Context, msg Message, cfg webhookConfig) *AgentMessage {
	messageType := AgentMsgRequested
	eventTime := msg.StartedAt
	switch msg.Event {
	case EventWarning:
		messageType = AgentMsgWarning
		eventTime = msg.OccurredAt
	case EventIncidentResolve:
		messageType = AgentMsgResolved
		if msg.ResolvedAt != nil {
			eventTime = *msg.ResolvedAt
		} else {
			eventTime = msg.OccurredAt
		}
	}
	if msg.IncidentID == 0 {
		// No incident: a channel test, or a warning. There is nothing to anchor
		// a diagnostic context on, and inventing one would create a junk run.
		messageType = AgentMsgTest
		if msg.Event == EventWarning {
			messageType = AgentMsgWarning
		}
	}

	message := &AgentMessage{
		SchemaVersion: AgentSchemaVersion,
		MessageID:     agentMessageID(msg),
		MessageType:   messageType,
		CorrelationID: agentCorrelationID(msg),
		SentAt:        time.Now().UTC().Format(time.RFC3339),
		Payload: AgentPayload{
			Event: AgentEvent{
				EventID:     agentEventID(msg),
				Event:       string(msg.Event),
				IncidentID:  msg.IncidentID,
				ServiceID:   msg.ServiceID,
				ServiceName: msg.ServiceName,
				ServiceURL:  msg.ServiceURL,
				Status:      agentStatus(msg),
				Severity:    severityOf(msg.Event),
				OccurredAt:  msg.eventTime().UTC().Format(time.RFC3339),
				Attempts:    msg.Attempts,
				Error:       msg.Error,
				CheckID:     msg.CheckID,
			},
		},
	}
	if !msg.StartedAt.IsZero() {
		message.Payload.Event.StartedAt = msg.StartedAt.UTC().Format(time.RFC3339)
	}
	if msg.ResolvedAt != nil {
		message.Payload.Event.ResolvedAt = msg.ResolvedAt.UTC().Format(time.RFC3339)
	}

	// Only a real incident gets a diagnostic context, and it is built now — not
	// inside the transaction that recorded the incident, and never stored in
	// the outbox.
	if msg.IncidentID != 0 && msg.Event != EventWarning {
		if built, err := s.Q.DiagnosticContext(query.ContextOptions{
			IncidentID: msg.IncidentID,
			EventTime:  eventTime,
		}); err == nil {
			message.Payload.DiagnosticContext = built
		} else {
			// The incident may already be gone. The event alone still tells the
			// Agent something happened; the missing context is the Agent's cue
			// to query for itself.
			message.Payload.Event.Error = strings.TrimSpace(
				message.Payload.Event.Error + " diagnostic context unavailable: " + err.Error())
		}
	}
	return message
}

func agentMessageID(msg Message) string {
	if msg.IncidentID != 0 {
		return string(msg.Event) + ":" + strconv.FormatInt(msg.IncidentID, 10)
	}
	if msg.DeliveryID != 0 {
		return "delivery:" + strconv.FormatInt(msg.DeliveryID, 10)
	}
	return string(msg.Event)
}

func agentCorrelationID(msg Message) string {
	if msg.IncidentID != 0 {
		return "incident:" + strconv.FormatInt(msg.IncidentID, 10)
	}
	return "delivery:" + strconv.FormatInt(msg.DeliveryID, 10)
}

func agentEventID(msg Message) string {
	if msg.IncidentID != 0 {
		return string(msg.Event) + ":" + strconv.FormatInt(msg.IncidentID, 10)
	}
	return "delivery:" + strconv.FormatInt(msg.DeliveryID, 10)
}

func agentStatus(msg Message) string {
	switch {
	case msg.Down():
		return "down"
	case msg.Warning():
		return "warning"
	default:
		return "recovered"
	}
}

// encodeAgentMessage marshals the message, trimming the diagnostic context
// until it fits. Each trimming step is recorded in coverage.truncated, so the
// Agent can tell "the data does not exist" from "the data did not fit".
func encodeAgentMessage(message *AgentMessage, limit int) ([]byte, error) {
	body, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	if len(body) <= limit {
		return body, nil
	}
	ctx := message.Payload.DiagnosticContext
	if ctx == nil {
		return nil, fmt.Errorf("agent_webhook: event payload is %d bytes, over the %d limit", len(body), limit)
	}
	for _, trim := range agentTrimSteps {
		body, err = json.Marshal(message)
		if err != nil {
			return nil, err
		}
		if len(body) <= limit {
			return body, nil
		}
		trim(ctx)
	}
	body, err = json.Marshal(message)
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		// Refusing here is deliberate: a permanent error stops the delivery
		// worker from retrying something that can never fit.
		return nil, fmt.Errorf("agent_webhook: diagnostic context is %d bytes, over the %d limit", len(body), limit)
	}
	return body, nil
}

// agentTrimSteps drops the least load-bearing context first: prose before
// measurements, measurements before the incident itself.
var agentTrimSteps = []func(*query.DiagnosticContext){
	func(c *query.DiagnosticContext) {
		if len(c.Incident.Comments) > 10 {
			c.Incident.Comments = c.Incident.Comments[len(c.Incident.Comments)-10:]
			markTruncated(c, "COMMENTS_TRIMMED_FOR_PAYLOAD")
		}
	},
	func(c *query.DiagnosticContext) {
		if len(c.RelatedIncidents) > 5 {
			c.RelatedIncidents = c.RelatedIncidents[:5]
			markTruncated(c, "RELATED_INCIDENTS_TRIMMED_FOR_PAYLOAD")
		}
	},
	func(c *query.DiagnosticContext) {
		if len(c.Metrics.Series) > 96 {
			c.Metrics.Series = c.Metrics.Series[len(c.Metrics.Series)-96:]
			markTruncated(c, "METRICS_TRIMMED_FOR_PAYLOAD")
		}
	},
	func(c *query.DiagnosticContext) {
		if len(c.RecentChecks) > 50 {
			c.RecentChecks = c.RecentChecks[len(c.RecentChecks)-50:]
			markTruncated(c, "CHECKS_TRIMMED_FOR_PAYLOAD")
		}
	},
	func(c *query.DiagnosticContext) {
		if len(c.Incident.Comments) > 0 {
			c.Incident.Comments = nil
			markTruncated(c, "COMMENTS_DROPPED_FOR_PAYLOAD")
		}
	},
	func(c *query.DiagnosticContext) {
		if len(c.RecentChecks) > 10 {
			c.RecentChecks = c.RecentChecks[len(c.RecentChecks)-10:]
			markTruncated(c, "CHECKS_TRIMMED_FOR_PAYLOAD")
		}
	},
	func(c *query.DiagnosticContext) {
		if len(c.Metrics.Series) > 0 {
			c.Metrics.Series = nil
			markTruncated(c, "METRICS_DROPPED_FOR_PAYLOAD")
		}
	},
	func(c *query.DiagnosticContext) {
		if len(c.RelatedIncidents) > 0 {
			c.RelatedIncidents = nil
			markTruncated(c, "RELATED_INCIDENTS_DROPPED_FOR_PAYLOAD")
		}
	},
}

func markTruncated(c *query.DiagnosticContext, code string) {
	for _, existing := range c.Coverage.Truncated {
		if existing == code {
			return
		}
	}
	c.Coverage.Truncated = append(c.Coverage.Truncated, code)
	c.Coverage.Complete = false
}
