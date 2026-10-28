package api

import (
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ecakeman/alertflow/internal/db"
	"github.com/ecakeman/alertflow/internal/notify"
)

// secretFields lists the sensitive keys per integration type. These are never
// echoed back in responses and are preserved on update when omitted.
var secretFields = map[string][]string{
	"telegram":              {"botToken"},
	"slack":                 {"webhookUrl"},
	"email":                 {"password"},
	"webhook":               {},
	notify.AgentWebhookType: {},
}

// requiredFields lists keys that must be present and non-empty per type.
var requiredFields = map[string][]string{
	"telegram":              {"botToken", "chatId"},
	"slack":                 {"webhookUrl"},
	"email":                 {"host", "from", "to"},
	"webhook":               {"url"},
	notify.AgentWebhookType: {"url"},
}

type integrationInput struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// NotifyWarnings is a plain overwrite — unlike secrets, a blank bool cannot
	// mean "keep", so the client must always send the current value.
	NotifyWarnings bool           `json:"notifyWarnings"`
	Config         map[string]any `json:"config"`
}

// GET /api/integrations → all channels (secrets redacted).
func (s *Server) handleListIntegrations(c *gin.Context) error {
	list, err := s.conn().ListIntegrations()
	if err != nil {
		return fail(http.StatusInternalServerError, "could not load integrations")
	}
	out := make([]integrationDTO, 0, len(list))
	for _, in := range list {
		out = append(out, toIntegrationDTO(in))
	}
	return writeJSON(c, http.StatusOK, out)
}

// POST /api/integrations → create a channel.
func (s *Server) handleCreateIntegration(c *gin.Context) error {
	var in integrationInput
	if err := decode(c, &in); err != nil {
		return err
	}
	if _, ok := requiredFields[in.Type]; !ok {
		return fail(http.StatusBadRequest, "unknown integration type")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return fail(http.StatusBadRequest, "name is required")
	}
	cfg := in.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	if err := validateIntegrationConfig(in.Type, cfg); err != nil {
		return fail(http.StatusBadRequest, err.Error())
	}
	raw, _ := json.Marshal(cfg)
	created, err := s.conn().CreateIntegration(in.Type, name, in.Enabled, in.NotifyWarnings, raw)
	if err != nil {
		return fail(http.StatusInternalServerError, "could not create integration")
	}
	return writeJSON(c, http.StatusCreated, toIntegrationDTO(*created))
}

// PUT /api/integrations/:id → update a channel. Omitted/blank secret fields keep
// their stored value. The type may be changed; validation and secret handling
// then follow the NEW type, since that is what the config will belong to.
func (s *Server) handleUpdateIntegration(c *gin.Context) error {
	existing, err := s.integrationParam(c)
	if err != nil {
		return err
	}
	var in integrationInput
	if err := decode(c, &in); err != nil {
		return err
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return fail(http.StatusBadRequest, "name is required")
	}
	kind := existing.Type
	if t := strings.TrimSpace(in.Type); t != "" && t != kind {
		if _, ok := requiredFields[t]; !ok {
			return fail(http.StatusBadRequest, "unknown integration type")
		}
		kind = t
	}
	merged := map[string]any{}
	_ = json.Unmarshal(existing.Config, &merged)
	secret := secretSet(kind)
	for k, v := range in.Config {
		if secret[k] {
			if str, ok := v.(string); ok && str == "" {
				continue // blank secret → keep the stored value
			}
		}
		merged[k] = v
	}
	if err := validateIntegrationConfig(kind, merged); err != nil {
		return fail(http.StatusBadRequest, err.Error())
	}
	raw, _ := json.Marshal(merged)
	updated, err := s.conn().UpdateIntegration(existing.ID, kind, name, in.Enabled, in.NotifyWarnings, raw)
	if err != nil {
		return fail(http.StatusInternalServerError, "could not update integration")
	}
	return writeJSON(c, http.StatusOK, toIntegrationDTO(*updated))
}

// DELETE /api/integrations/:id → remove a channel.
func (s *Server) handleDeleteIntegration(c *gin.Context) error {
	existing, err := s.integrationParam(c)
	if err != nil {
		return err
	}
	if err := s.conn().DeleteIntegration(existing.ID); err != nil {
		return fail(http.StatusInternalServerError, "could not delete integration")
	}
	return writeEmpty(c, http.StatusNoContent)
}

// POST /api/integrations/:id/test → send one real test notification.
func (s *Server) handleTestIntegration(c *gin.Context) error {
	existing, err := s.integrationParam(c)
	if err != nil {
		return err
	}
	msg := notify.Message{
		Event:       notify.EventIncidentStart,
		ServiceName: "Test notification",
		ServiceURL:  "https://example.com",
		StartedAt:   time.Now(),
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	if err := s.dispatch().Test(ctx, *existing, msg); err != nil {
		return writeJSON(c, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
	}
	return writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

// integrationParam parses :id and loads the integration (404 if absent).
func (s *Server) integrationParam(c *gin.Context) (*db.Integration, error) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return nil, fail(http.StatusBadRequest, "invalid integration id")
	}
	in, err := s.conn().GetIntegration(id)
	if err != nil {
		return nil, fail(http.StatusNotFound, "integration not found")
	}
	return in, nil
}

func secretSet(kind string) map[string]bool {
	set := map[string]bool{}
	for _, f := range secretFields[kind] {
		set[f] = true
	}
	return set
}

// validateIntegrationConfig checks required string fields are present.
func validateIntegrationConfig(kind string, cfg map[string]any) error {
	for _, field := range requiredFields[kind] {
		if v, _ := cfg[field].(string); strings.TrimSpace(v) == "" {
			return fail(http.StatusBadRequest, field+" is required")
		}
	}
	return nil
}
