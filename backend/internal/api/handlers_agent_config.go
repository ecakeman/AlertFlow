package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// GET /internal/agent/v1/config → the Agent's runtime config, unmasked.
//
// The Agent fetches this once per run so model endpoints and API keys edited in
// the UI take effect without a redeploy. Authenticated with ALERTFLOW_AGENT_TOKEN
// (the same bearer the Agent uses for its other internal reads).
func (s *Server) handleAgentRuntimeConfig(c *gin.Context) error {
	a := s.config().Settings.Agent
	memory := true
	if a.MemoryEnabled != nil {
		memory = *a.MemoryEnabled
	}
	return writeJSON(c, http.StatusOK, map[string]any{
		"memory_enabled":        memory,
		"context_budget_tokens": a.ContextBudgetTokens,
		"llm": map[string]any{
			"base_url": a.LLM.BaseURL, "api_key": a.LLM.APIKey, "model": a.LLM.Model,
		},
		"embed": map[string]any{
			"base_url": a.Embed.BaseURL, "api_key": a.Embed.APIKey, "model": a.Embed.Model, "dim": a.Embed.Dim,
		},
		"rerank": map[string]any{
			"base_url": a.Rerank.BaseURL, "api_key": a.Rerank.APIKey, "model": a.Rerank.Model, "style": a.Rerank.Style,
		},
	})
}
