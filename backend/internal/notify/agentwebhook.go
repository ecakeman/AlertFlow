package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ecakeman/alertflow/internal/agentmessage"
	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/query"
)

// webhookConfig is the stored shape of an agent_webhook integration.
type webhookConfig struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
}

// AgentWebhookType is the integration type reserved for the diagnostic Agent.
// It is deliberately distinct from "webhook": a generic webhook must never
// receive the internal diagnostic context, and only this type does.
const AgentWebhookType = db.AgentWebhookType

// The envelope is built in internal/agentmessage so the HTTP and gRPC
// transports emit byte-identical payloads. These aliases keep the existing
// callers and tests on the same types.
type (
	// AgentMessage is the versioned envelope AlertFlow pushes to the Agent.
	AgentMessage = agentmessage.Message
	// AgentPayload carries the event summary and, for a diagnostic request,
	// the default context assembled before delivery.
	AgentPayload = agentmessage.Payload
	// AgentEvent summarises the transition that triggered the message.
	AgentEvent = agentmessage.Event
)

// AgentSchemaVersion is the version of the AlertFlow→Agent message contract.
const AgentSchemaVersion = agentmessage.SchemaVersion

// AgentMaxPayloadBytes bounds one diagnostic message.
const AgentMaxPayloadBytes = agentmessage.MaxPayloadBytes

// Message types on the AlertFlow→Agent edge.
const (
	AgentMsgRequested     = agentmessage.MsgRequested
	AgentMsgResolved      = agentmessage.MsgResolved
	AgentMsgWarning       = agentmessage.MsgWarning
	AgentMsgTest          = agentmessage.MsgTest
	AgentMsgChatRequested = agentmessage.MsgChatRequested
)

// agentClock supplies sent_at. Tests pin it so the encoded bytes are
// deterministic; production leaves it as time.Now.
var agentClock = time.Now

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

	diag, chat := AgentMessageInputs(msg)
	body, err := agentmessage.Encode(s.Q, diag, chat, s.MaxBytes)
	if err != nil {
		return err
	}
	return postAgentJSON(ctx, cfg, body)
}

// AgentMessageInputs maps one notification message onto the transport-neutral
// builder input. It is exported so the gRPC task client builds exactly the same
// envelope as the HTTP sender.
func AgentMessageInputs(msg Message) (agentmessage.DiagnosticInput, *agentmessage.ChatInput) {
	sentAt := agentClock()
	diag := agentmessage.DiagnosticInput{
		Event:       string(msg.Event),
		IncidentID:  msg.IncidentID,
		DeliveryID:  msg.DeliveryID,
		ServiceID:   msg.ServiceID,
		ServiceName: msg.ServiceName,
		ServiceURL:  msg.ServiceURL,
		OccurredAt:  msg.OccurredAt,
		StartedAt:   msg.StartedAt,
		ResolvedAt:  msg.ResolvedAt,
		Attempts:    msg.Attempts,
		Error:       msg.Error,
		CheckID:     msg.CheckID,
		SentAt:      sentAt,
	}
	if msg.Chat == nil {
		return diag, nil
	}
	// It carries no diagnostic context: the agent already holds this incident's
	// evidence from the diagnostic run, and answers from that plus its long-term
	// memory.
	return diag, &agentmessage.ChatInput{
		RequestID:  msg.Chat.RequestID,
		IncidentID: msg.IncidentID,
		ServiceID:  msg.ServiceID,
		Message:    msg.Chat.Message,
		History:    msg.Chat.History,
		SentAt:     sentAt,
	}
}

// postAgentJSON sends an already-encoded message to the agent.
func postAgentJSON(ctx context.Context, cfg webhookConfig, body []byte) error {
	method := strings.ToUpper(strings.TrimSpace(cfg.Method))
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	return doRequest(req)
}
