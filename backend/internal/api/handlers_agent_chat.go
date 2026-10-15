package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/ecakeman/alertflow/internal/db"
)

type agentChatTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type agentChatRequest struct {
	Message string          `json:"message"`
	History []agentChatTurn `json:"history"`
	// RequestID is optional. A client that retries a POST can supply the same id
	// and get the original request back instead of queueing a second question.
	RequestID string `json:"requestId"`
}

// agentHTTPClient is shared by the server-side calls to the agent's API. It
// lives here because the chat proxy is the only caller left.
var agentHTTPClient = &http.Client{Timeout: 8 * time.Second}

// maxChatHistory bounds the transcript sent along with a question.
const maxChatHistory = 8

// agentChatRequestDTO is one queued question and its answer.
type agentChatRequestDTO struct {
	RequestID   string          `json:"requestId"`
	IncidentID  int64           `json:"incidentId"`
	Status      string          `json:"status"`
	Message     string          `json:"message"`
	Reply       string          `json:"reply,omitempty"`
	EvidenceIDs json.RawMessage `json:"evidenceIds,omitempty"`
	Error       string          `json:"error,omitempty"`
	CreatedAt   string          `json:"createdAt"`
	UpdatedAt   string          `json:"updatedAt"`
}

func toAgentChatRequestDTO(row *db.ChatRequestRow) agentChatRequestDTO {
	out := agentChatRequestDTO{
		RequestID:   row.RequestID,
		IncidentID:  row.IncidentID,
		Status:      row.Status,
		Message:     row.Message,
		EvidenceIDs: row.EvidenceIDs,
		Error:       row.Error,
		CreatedAt:   iso(row.CreatedAt),
		UpdatedAt:   iso(row.UpdatedAt),
	}
	if row.Reply != nil {
		out.Reply = *row.Reply
	}
	return out
}

// POST /api/incidents/:id/agent-chat/requests → queue a troubleshooting question.
//
// The question is stored and handed to the agent over the same durable delivery
// path as a notification, then answered asynchronously over /webhooks/agent.
// The page polls GET .../requests/:requestId. Nothing here waits on a model.
func (s *Server) handleCreateAgentChatRequest(c fiber.Ctx) error {
	inc, err := s.incidentParam(c)
	if err != nil {
		return err
	}
	var in agentChatRequest
	if err := decode(c, &in); err != nil {
		return err
	}
	message := strings.TrimSpace(in.Message)
	if message == "" {
		return fiber.NewError(fiber.StatusBadRequest, "message is required")
	}
	enabled, err := s.agentChannelEnabled()
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "could not check the agent channel")
	}
	if !enabled {
		return fiber.NewError(fiber.StatusServiceUnavailable, "no agent channel is enabled")
	}

	history := in.History
	if len(history) > maxChatHistory {
		history = history[len(history)-maxChatHistory:]
	}
	historyJSON, err := json.Marshal(history)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "could not encode the request")
	}

	requestID := strings.TrimSpace(in.RequestID)
	if requestID == "" {
		requestID = newChatRequestID()
	}
	svc := s.config().Find(inc.ServiceID)
	snapshot := db.DeliverySnapshot{
		Event:      db.EventChatRequested,
		EventID:    requestID,
		IncidentID: &inc.ID,
		ServiceID:  inc.ServiceID,
		OccurredAt: inc.StartedAt,
		Chat: &db.ChatSnapshot{
			RequestID: requestID,
			Message:   message,
			History:   historyJSON,
		},
	}
	if svc != nil {
		snapshot.ServiceName = svc.Name
		snapshot.ServiceURL = svc.URL
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "could not encode the request")
	}

	now := time.Now().Unix()
	var userID *int64
	if u := userLocal(c); u != nil {
		userID = &u.ID
	}
	row, created, err := s.conn().CreateChatRequest(db.ChatRequestRow{
		RequestID: requestID, IncidentID: inc.ID, UserID: userID, Message: message,
	}, payload, now)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "could not queue the request")
	}
	// A request with no delivery row can never be answered — say so now rather
	// than leaving the page polling a question nobody will read.
	if row.DeliveryID == nil && row.Status == db.ChatPending {
		if _, ferr := s.conn().FailChatRequest(requestID, "no agent channel is enabled", now); ferr == nil {
			row, _ = s.conn().GetChatRequest(requestID)
		}
	}
	status := fiber.StatusAccepted
	if !created {
		status = fiber.StatusOK
	}
	return c.Status(status).JSON(toAgentChatRequestDTO(row))
}

// GET /api/incidents/:id/agent-chat/requests/:requestId → one request's state.
func (s *Server) handleGetAgentChatRequest(c fiber.Ctx) error {
	inc, err := s.incidentParam(c)
	if err != nil {
		return err
	}
	row, err := s.conn().GetChatRequest(c.Params("requestId"))
	if errors.Is(err, db.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "chat request not found")
	}
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "could not load the chat request")
	}
	if row.IncidentID != inc.ID {
		// The request exists but belongs to another incident: from this
		// incident's point of view it does not exist.
		return fiber.NewError(fiber.StatusNotFound, "chat request not found")
	}
	return c.JSON(toAgentChatRequestDTO(row))
}

// GET /api/incidents/:id/agent-chat/requests → the incident's recent requests,
// oldest first, so a page reload restores the conversation.
func (s *Server) handleListAgentChatRequests(c fiber.Ctx) error {
	inc, err := s.incidentParam(c)
	if err != nil {
		return err
	}
	rows, err := s.conn().ListChatRequests(inc.ID, 20)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "could not load the chat requests")
	}
	out := make([]agentChatRequestDTO, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		out = append(out, toAgentChatRequestDTO(&rows[i]))
	}
	return c.JSON(fiber.Map{"requests": out})
}

// agentChannelEnabled reports whether any agent channel would pick up a queued
// chat request. It is a pre-check: the delivery exists only if one is enabled.
func (s *Server) agentChannelEnabled() (bool, error) {
	list, err := s.conn().ListEnabledIntegrations()
	if err != nil {
		return false, err
	}
	for _, in := range list {
		if in.Type == db.AgentWebhookType {
			return true, nil
		}
	}
	return false, nil
}

func newChatRequestID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(err)
	}
	return "chat-" + hex.EncodeToString(buf[:])
}

// --- Legacy synchronous path ------------------------------------------------
//
// Kept until the asynchronous flow has been verified end to end, per the
// migration plan. The UI no longer calls it.

// POST /api/incidents/:id/agent-chat → proxy a troubleshooting question to the
// diagnostic Agent for this incident, synchronously.
//
// Server-side on purpose: the browser never sees AGENT_API_TOKEN, and the Agent
// answers read-only (evidence + report + long-term memory, no tools, no writes).
func (s *Server) handleIncidentAgentChat(c fiber.Ctx) error {
	inc, err := s.incidentParam(c)
	if err != nil {
		return err
	}
	if s.agentBaseURL == "" || s.agentAPIToken == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "agent is not configured")
	}
	var in agentChatRequest
	if err := decode(c, &in); err != nil {
		return err
	}
	if strings.TrimSpace(in.Message) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "message is required")
	}
	history := in.History
	if len(history) > maxChatHistory {
		history = history[len(history)-maxChatHistory:]
	}
	payload, err := json.Marshal(fiber.Map{"message": in.Message, "history": history})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "could not encode request")
	}

	// An answer is one model call; give it room but not unbounded.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	url := fmt.Sprintf("%s/api/v1/incidents/%d/chat", s.agentBaseURL, inc.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "could not build request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.agentAPIToken)

	resp, err := agentHTTPClient.Do(req)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "agent unreachable")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fiber.NewError(fiber.StatusBadGateway, "agent returned an error")
	}
	var out struct {
		Reply       string   `json:"reply"`
		EvidenceIDs []string `json:"evidence_ids"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "unexpected agent response")
	}
	return c.JSON(fiber.Map{"reply": out.Reply, "evidenceIds": out.EvidenceIDs})
}
