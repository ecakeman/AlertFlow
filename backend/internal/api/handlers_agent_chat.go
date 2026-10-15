package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
)

type agentChatTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// agentHTTPClient is shared by the server-side calls to the agent's API. It
// lives here because the chat proxy is the only caller left.
var agentHTTPClient = &http.Client{Timeout: 8 * time.Second}

type agentChatRequest struct {
	Message string          `json:"message"`
	History []agentChatTurn `json:"history"`
}

// POST /api/incidents/:id/agent-chat → proxy a troubleshooting question to the
// diagnostic Agent for this incident.
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
	if len(history) > 8 {
		history = history[len(history)-8:]
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
