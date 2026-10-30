package agentmessage

// Package agentmessage builds the versioned AlertFlow→Agent message envelope.
//
// It is transport independent: the HTTP adapter (notify.AgentSender) and the
// gRPC task client (deliver.GRPCTasks) both serialise through this package, so
// both transports emit byte-identical payloads. The only dependency is the
// read-composition layer, used to assemble the default diagnostic context.
//
// The envelope semantics are part of the Agent contract and are pinned by
// notify's TestAgentMessageGoldenBytes: do not change field order, json tags or
// trimming order without updating that contract.

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/ecakeman/alertflow/internal/query"
)

// SchemaVersion is the version of the AlertFlow→Agent message contract.
const SchemaVersion = "v2"

// MaxPayloadBytes bounds one message. It matches the Agent's inbound body
// limit; anything larger is trimmed here, with the trimming recorded in the
// context's coverage, rather than being sent and rejected.
const MaxPayloadBytes = 512 << 10

// MaxChatMessageRunes bounds one question. The agent clips too; this keeps an
// oversized question from ever leaving here.
const MaxChatMessageRunes = 4000

// Message types on the AlertFlow→Agent edge.
const (
	MsgRequested = "diagnostic.requested"
	MsgResolved  = "diagnostic.resolved"
	MsgWarning   = "diagnostic.warning"
	MsgTest      = "diagnostic.test"
	// MsgChatRequested asks the agent a read-only troubleshooting question
	// about an incident it has already diagnosed.
	MsgChatRequested = "chat.requested"
)

// Event values carried inside the envelope. They mirror notify's event
// constants; notify aliases these so both packages compare the same strings.
const (
	EventIncidentStart   = "incident_start"
	EventIncidentResolve = "incident_resolve"
	EventWarning         = "warning"
	EventChatRequested   = "chat_requested"
)

// now supplies sent_at when the caller does not pin it. Tests in this package
// may replace it; production leaves it as time.Now.
var now = time.Now

// Message is the versioned envelope AlertFlow pushes to the Agent.
//
// message_id is stable for one logical delivery, so a retry after a crash —
// or the same incident being re-delivered — is recognised by the Agent as the
// same task rather than a new one. correlation_id ties every message about one
// incident together.
type Message struct {
	SchemaVersion string  `json:"schema_version"`
	MessageID     string  `json:"message_id"`
	MessageType   string  `json:"message_type"`
	CorrelationID string  `json:"correlation_id"`
	SentAt        string  `json:"sent_at"`
	Payload       Payload `json:"payload"`
}

// Payload carries the event summary and, for a diagnostic request, the default
// context assembled before delivery.
type Payload struct {
	Event             Event                    `json:"event"`
	DiagnosticContext *query.DiagnosticContext `json:"diagnostic_context,omitempty"`
}

// Event summarises the transition that triggered the message. It mirrors the
// fields the generic webhook has always sent, so an Agent that logged the old
// payload still finds what it used to read.
type Event struct {
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

// ChatMessage is the chat.requested envelope. It is a separate type from
// Message because a question has no incident transition to describe — and
// sharing the diagnostic envelope would risk it being read as one.
type ChatMessage struct {
	SchemaVersion string      `json:"schema_version"`
	MessageID     string      `json:"message_id"`
	MessageType   string      `json:"message_type"`
	CorrelationID string      `json:"correlation_id"`
	SentAt        string      `json:"sent_at"`
	Payload       ChatPayload `json:"payload"`
}

// ChatPayload carries one question and the conversation so far.
type ChatPayload struct {
	RequestID  string          `json:"request_id"`
	IncidentID int64           `json:"incident_id"`
	ServiceID  string          `json:"service_id"`
	Message    string          `json:"message"`
	History    json.RawMessage `json:"history,omitempty"`
}

// DiagnosticInput is the transport-neutral description of one diagnostic
// message. The adapters map their own message type onto it.
type DiagnosticInput struct {
	Event       string
	IncidentID  int64
	DeliveryID  int64
	ServiceID   string
	ServiceName string
	ServiceURL  string
	OccurredAt  time.Time
	StartedAt   time.Time
	ResolvedAt  *time.Time
	Attempts    int
	Error       string
	CheckID     int64
	// SentAt pins the envelope timestamp. Zero uses now().
	SentAt time.Time
}

// ChatInput is the transport-neutral description of one chat.requested message.
type ChatInput struct {
	RequestID  string
	IncidentID int64
	ServiceID  string
	Message    string
	History    json.RawMessage
	// SentAt pins the envelope timestamp. Zero uses now().
	SentAt time.Time
}

// BuildDiagnostic assembles one diagnostic envelope.
//
// A warning has no incident to anchor a context on, and the integration test
// has no incident at all, so both send the event alone. A real incident gets
// its context built now — not inside the transaction that recorded the
// incident, and never stored in the outbox.
func BuildDiagnostic(q *query.Service, in DiagnosticInput) *Message {
	message := &Message{
		SchemaVersion: SchemaVersion,
		MessageID:     messageID(in),
		MessageType:   diagnosticMessageType(in),
		CorrelationID: correlationID(in),
		SentAt:        sentAt(in.SentAt),
		Payload: Payload{
			Event: Event{
				EventID:     eventID(in),
				Event:       in.Event,
				IncidentID:  in.IncidentID,
				ServiceID:   in.ServiceID,
				ServiceName: in.ServiceName,
				ServiceURL:  in.ServiceURL,
				Status:      status(in.Event),
				Severity:    Severity(in.Event),
				OccurredAt:  formatTime(occurredAt(in)),
				Attempts:    in.Attempts,
				Error:       in.Error,
				CheckID:     in.CheckID,
			},
		},
	}
	if !in.StartedAt.IsZero() {
		message.Payload.Event.StartedAt = formatTime(in.StartedAt)
	}
	if in.ResolvedAt != nil {
		message.Payload.Event.ResolvedAt = formatTime(*in.ResolvedAt)
	}

	if in.IncidentID != 0 && in.Event != EventWarning && q != nil {
		if built, err := q.DiagnosticContext(query.ContextOptions{
			IncidentID: in.IncidentID,
			EventTime:  eventTime(in),
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

// BuildChat assembles one chat.requested envelope. History defaults to an empty
// JSON array so the Agent always receives a well-formed conversation.
func BuildChat(in ChatInput) *ChatMessage {
	history := in.History
	if len(history) == 0 {
		history = json.RawMessage("[]")
	}
	return &ChatMessage{
		SchemaVersion: SchemaVersion,
		MessageID:     "chat:" + in.RequestID,
		MessageType:   MsgChatRequested,
		CorrelationID: "incident:" + strconv.FormatInt(in.IncidentID, 10),
		SentAt:        sentAt(in.SentAt),
		Payload: ChatPayload{
			RequestID:  in.RequestID,
			IncidentID: in.IncidentID,
			ServiceID:  in.ServiceID,
			Message:    clipRunes(in.Message, MaxChatMessageRunes),
			History:    history,
		},
	}
}

// Severity maps an event to the incident severity the Agent records.
func Severity(event string) string {
	if event == EventWarning {
		return "warning"
	}
	return "outage"
}

func status(event string) string {
	switch event {
	case EventIncidentStart:
		return "down"
	case EventWarning:
		return "warning"
	default:
		return "recovered"
	}
}

// diagnosticMessageType picks message_type from the event.
func diagnosticMessageType(in DiagnosticInput) string {
	messageType := MsgRequested
	switch in.Event {
	case EventWarning:
		messageType = MsgWarning
	case EventIncidentResolve:
		messageType = MsgResolved
	}
	if in.IncidentID == 0 {
		// No incident: a channel test, or a warning. There is nothing to anchor
		// a diagnostic context on, and inventing one would create a junk run.
		messageType = MsgTest
		if in.Event == EventWarning {
			messageType = MsgWarning
		}
	}
	return messageType
}

// eventTime is the moment the diagnostic context is anchored on: the event
// that caused the transition, never the delivery moment.
func eventTime(in DiagnosticInput) time.Time {
	switch in.Event {
	case EventWarning:
		return in.OccurredAt
	case EventIncidentResolve:
		if in.ResolvedAt != nil {
			return *in.ResolvedAt
		}
		return in.OccurredAt
	default:
		return in.StartedAt
	}
}

// occurredAt mirrors notify.Message.eventTime: prefer the check moment, fall
// back to the start of the incident.
func occurredAt(in DiagnosticInput) time.Time {
	if !in.OccurredAt.IsZero() {
		return in.OccurredAt
	}
	return in.StartedAt
}

func sentAt(pinned time.Time) string {
	if pinned.IsZero() {
		return formatTime(now())
	}
	return formatTime(pinned)
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func messageID(in DiagnosticInput) string {
	if in.IncidentID != 0 {
		return in.Event + ":" + strconv.FormatInt(in.IncidentID, 10)
	}
	if in.DeliveryID != 0 {
		return "delivery:" + strconv.FormatInt(in.DeliveryID, 10)
	}
	return in.Event
}

func correlationID(in DiagnosticInput) string {
	if in.IncidentID != 0 {
		return "incident:" + strconv.FormatInt(in.IncidentID, 10)
	}
	return "delivery:" + strconv.FormatInt(in.DeliveryID, 10)
}

func eventID(in DiagnosticInput) string {
	return messageID(in)
}

// clipRunes truncates by rune count and marks the cut with an ellipsis.
func clipRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

var errQueryServiceMissing = errors.New("agent_webhook: query service is not configured")
